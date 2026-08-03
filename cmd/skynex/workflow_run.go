package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/delivery"
	"github.com/joeldevz/skynex/internal/execution"
	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/orchestration"
	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/verification"
	"github.com/joeldevz/skynex/internal/workflow"
)

type workflowRunInput struct {
	Request, Model, Agent, Executable string
	Acceptance, Checks, AllowedPaths  []string
	Timeout                           time.Duration
	Seal                              gitcandidate.ContextSeal
}

func workflowStart(store *workflow.SQLiteStore, repo string, args []string, out io.Writer) error {
	id, ok := flagValue(args, "--id")
	if !ok || id == "" {
		return errors.New("start requires --id")
	}
	request, ok := flagValue(args, "--request")
	if !ok || strings.TrimSpace(request) == "" {
		return errors.New("start requires --request")
	}
	acceptance := flagValues(args, "--accept")
	checks := flagValues(args, "--check")
	paths := flagValues(args, "--path")
	if len(acceptance) == 0 || len(checks) == 0 || len(paths) == 0 {
		return errors.New("simple start requires explicit --accept, --check, and --path")
	}
	seal, err := gitcandidate.CaptureContext(repo)
	if err != nil {
		return err
	}
	candidate, err := gitcandidate.Freeze(seal, gitcandidate.Policy{})
	if err != nil {
		return err
	}
	if _, err = store.Create(workflow.Workflow{ID: id, Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow, BasisTree: candidate.TreeOID}); err != nil {
		return err
	}
	var override *orchestration.RouteOverride
	if route, exists := flagValue(args, "--route"); exists {
		actor, aok := flagValue(args, "--override-actor")
		reason, rok := flagValue(args, "--override-reason")
		if !aok || !rok || actor == "" || reason == "" {
			return errors.New("route override requires --override-actor and --override-reason")
		}
		override = &orchestration.RouteOverride{Route: workflow.Route(route), Actor: actor, Reason: reason, At: time.Now().UTC()}
	}
	engine := orchestration.NewEngine(store)
	decision, err := engine.Begin(id, orchestration.RouteInput{Clear: true, EstimatedSlices: 1}, override)
	if err != nil {
		return err
	}
	if decision.Route != workflow.RouteSimple {
		return fmt.Errorf("route %s requires discovery/planning support not available in workflow start", decision.Route)
	}
	graph := orchestration.ExecutionGraph{WorkflowID: id, Version: 1, Slices: []orchestration.Slice{{ID: "slice_main", Title: request, AcceptanceCriteria: acceptance}}}
	contract := orchestration.ExecutableContract{Destination: request, AcceptanceCriteria: acceptance}
	if err = engine.Close(id, orchestration.WayfinderGraph{WorkflowID: id, Version: 1}, contract, graph); err != nil {
		return err
	}
	timeout := 10 * time.Minute
	if raw, exists := flagValue(args, "--timeout"); exists {
		timeout, err = time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("invalid --timeout: %w", err)
		}
	}
	input := workflowRunInput{Request: request, Acceptance: acceptance, Checks: checks, AllowedPaths: paths, Model: valueOrEmpty(args, "--model"), Agent: valueOrEmpty(args, "--agent"), Executable: valueOrEmpty(args, "--opencode"), Timeout: timeout, Seal: seal}
	raw, _ := json.Marshal(input)
	if _, err = store.Database().Exec(`INSERT INTO workflow_run_inputs(workflow_id,input) VALUES(?,?)`, id, raw); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\t%s\n", id, workflow.StateReady, decision.Route)
	return nil
}

func workflowRun(store *workflow.SQLiteStore, repo string, args []string, out io.Writer) error {
	id, err := requiredWorkflowID(args)
	if err != nil {
		return err
	}
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	if w.State == workflow.StateCandidateFrozen {
		return errors.New("candidate frozen; semantic review runner is not configured")
	}
	if w.State != workflow.StateReady && w.State != workflow.StateExecuting && w.State != workflow.StateVerifying {
		return fmt.Errorf("workflow %s cannot run from %s", id, w.State)
	}
	var raw []byte
	if err = store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, id).Scan(&raw); err != nil {
		return err
	}
	var input workflowRunInput
	if err = json.Unmarshal(raw, &input); err != nil {
		return err
	}
	seal := input.Seal
	if seal.RepositoryRoot == "" || seal.RepositoryRoot != repo {
		return errors.New("persisted workflow context does not match repository")
	}
	if w.State != workflow.StateVerifying {
		var graphRaw []byte
		if err = store.Database().QueryRow(`SELECT graph FROM execution_graphs WHERE workflow_id=? ORDER BY version DESC LIMIT 1`, id).Scan(&graphRaw); err != nil {
			return err
		}
		var graph orchestration.ExecutionGraph
		if err = json.Unmarshal(graphRaw, &graph); err != nil {
			return err
		}
		scheduler, err := execution.NewScheduler(store, graph)
		if err != nil {
			return err
		}
		for {
			var attempt execution.Attempt
			var allowedRaw []byte
			activeErr := store.Database().QueryRow(`SELECT attempt_id,workflow_id,slice_id,worktree_id,owner,fencing_token,basis_tree,allowed_paths,operation_id FROM mutation_attempts WHERE workflow_id=? AND live=1 LIMIT 1`, id).Scan(&attempt.ID, &attempt.WorkflowID, &attempt.SliceID, &attempt.WorktreeID, &attempt.Owner, &attempt.FencingToken, &attempt.BasisTree, &allowedRaw, &attempt.OperationID)
			if activeErr == nil {
				_ = json.Unmarshal(allowedRaw, &attempt.AllowedPaths)
			} else {
				ready, nextErr := scheduler.NextReady()
				if nextErr != nil {
					return nextErr
				}
				if ready == nil {
					break
				}
				candidate, freezeErr := gitcandidate.Freeze(seal, gitcandidate.Policy{})
				if freezeErr != nil {
					return freezeErr
				}
				attemptID := id + ":" + ready.ID
				owner := "workflow-cli"
				token, tokenErr := newFencingToken()
				if tokenErr != nil {
					return tokenErr
				}
				attempt = execution.Attempt{ID: attemptID, WorkflowID: id, SliceID: ready.ID, WorktreeID: seal.WorktreeID, Owner: owner, FencingToken: token, BasisTree: candidate.TreeOID, AllowedPaths: input.AllowedPaths, OperationID: "mutation:" + attemptID}
				if err = scheduler.Start(attempt); err != nil {
					return err
				}
			}
			if _, leaseErr := store.AcquireLease("worktree:"+seal.WorktreeID, attempt.Owner, attempt.FencingToken, time.Now(), time.Now().Add(input.Timeout+time.Minute)); leaseErr != nil {
				_, leaseErr = store.HeartbeatLease("worktree:"+seal.WorktreeID, attempt.Owner, attempt.FencingToken, time.Now(), time.Now().Add(input.Timeout+time.Minute))
				if leaseErr != nil {
					return leaseErr
				}
			}
			adapter := execution.OpenCodeAdapter{Store: store, Options: execution.OpenCodeOptions{Executable: input.Executable, Model: input.Model, Agent: input.Agent, Timeout: input.Timeout}}
			result, err := adapter.Run(context.Background(), execution.OpenCodeRequest{InvocationID: "invoke:" + attempt.ID, Attempt: attempt, Seal: seal, Prompt: input.Request + "\nAcceptance: " + strings.Join(input.Acceptance, "; ")})
			if err != nil {
				return err
			}
			if _, err = (&execution.Broker{Store: store, Seal: seal}).Apply(context.Background(), result); err != nil {
				return err
			}
			if err = scheduler.Complete(id, attempt.SliceID); err != nil {
				return err
			}
		}
	}
	w, err = store.Get(id)
	if err != nil {
		return err
	}
	if w.State != workflow.StateVerifying {
		return fmt.Errorf("execution stopped in %s", w.State)
	}
	plan := verification.Plan{Timeout: input.Timeout}
	for _, command := range input.Checks {
		plan.Checks = append(plan.Checks, verification.Command{Name: "sh", Args: []string{"-c", command}})
	}
	for _, command := range input.Acceptance {
		plan.Acceptance = append(plan.Acceptance, verification.Command{Name: "sh", Args: []string{"-c", command}})
	}
	_, err = (&verification.Runner{Store: store, EngineVersion: "workflow-cli-v1", RiskPolicy: review.RiskPolicy{}}).Run(context.Background(), id, seal, plan)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\n", id, workflow.StateCandidateFrozen)
	fmt.Fprintln(out, "candidate frozen; semantic review runner is not configured")
	return nil
}

func workflowReview(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	id, ok := flagValue(args, "--id")
	var err error
	if !ok {
		id, err = requiredWorkflowID(args)
		if err != nil {
			return err
		}
	}
	var raw []byte
	if err = store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, id).Scan(&raw); err != nil {
		return err
	}
	var input workflowRunInput
	if err = json.Unmarshal(raw, &input); err != nil {
		return err
	}
	model := input.Model
	if model == "" {
		model = "default"
	}
	runner := review.OpenCodeReviewRunner{Store: store, Options: review.OpenCodeReviewOptions{Executable: input.Executable, Model: model, Timeout: input.Timeout}}
	receipt, err := runner.Run(context.Background(), id)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\t%s\n", id, workflow.StateReceipted, receipt.ID)
	return nil
}

func workflowDeliver(store *workflow.SQLiteStore, args []string, out io.Writer, afterCommit func() error) error {
	id, ok := flagValue(args, "--id")
	if !ok || id == "" {
		return errors.New("deliver requires --id")
	}
	message, ok := flagValue(args, "--message")
	if !ok || strings.TrimSpace(message) == "" {
		return errors.New("deliver requires --message")
	}
	key, ok := flagValue(args, "--idempotency-key")
	if !ok || key == "" {
		return errors.New("deliver requires --idempotency-key")
	}
	authorName, hasName := flagValue(args, "--author-name")
	authorEmail, hasEmail := flagValue(args, "--author-email")
	if hasName != hasEmail || (hasName && (authorName == "" || authorEmail == "")) {
		return errors.New("deliver author requires both --author-name and --author-email")
	}
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	if w.State != workflow.StateReceipted {
		return fmt.Errorf("deliver requires receipted state, got %s", w.State)
	}
	reviews := review.NewSQLiteStore(store.Database())
	authority, err := reviews.Authority(id)
	if err != nil {
		return err
	}
	var raw []byte
	if err = store.Database().QueryRow(`SELECT result FROM verification_runs WHERE workflow_id=?`, id).Scan(&raw); err != nil {
		return err
	}
	var verified struct {
		Record review.CandidateRecord
	}
	if err = json.Unmarshal(raw, &verified); err != nil {
		return err
	}
	gate := delivery.Gate{Authority: reviews, Intents: delivery.NewSQLiteIntentStore(store.Database())}
	result, err := gate.Commit(context.Background(), delivery.Request{WorkflowID: id, Candidate: verified.Record, CandidatePolicy: gitcandidate.Policy{}, ExpectedReceiptID: authority.ID, ExpectedPolicyHash: authority.PolicyHash, CompatibleEngineVersion: verified.Record.EngineVersion, Message: message, IdempotencyKey: key, AuthorName: authorName, AuthorEmail: authorEmail})
	if err != nil {
		return err
	}
	if afterCommit != nil {
		if err = afterCommit(); err != nil {
			return err
		}
	}
	w, err = store.Get(id)
	if err != nil {
		return err
	}
	updated, err := store.Transition(workflow.Transition{WorkflowID: id, ExpectedState: workflow.StateReceipted, ExpectedVersion: w.StateVersion, NextState: workflow.StateDelivered, IdempotencyKey: "delivery:" + key, ArtifactIDs: []string{result.CommitOID, result.ReceiptID}})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", id, updated.State, result.CommitOID, result.Ref)
	return nil
}

func flagValues(args []string, name string) []string {
	var values []string
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			values = append(values, args[i+1])
			i++
		} else if strings.HasPrefix(args[i], name+"=") {
			values = append(values, strings.TrimPrefix(args[i], name+"="))
		}
	}
	return values
}

func valueOrEmpty(args []string, name string) string {
	value, _ := flagValue(args, name)
	return value
}

func newFencingToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate fencing token: %w", err)
	}
	return hex.EncodeToString(value), nil
}
