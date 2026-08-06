package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/joeldevz/skynex/internal/approval"
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
	ModelExplicit, AgentExplicit      bool
	ResultTransport                   string
	Acceptance, Checks, AllowedPaths  []string
	Timeout                           time.Duration
	Seal                              gitcandidate.ContextSeal
	SliceConfigs                      map[string]sliceRunConfig
}

const defaultWorkflowWorkerAgent = "workflow-worker"

func workflowAgent(args []string) string {
	if value := valueOrEmpty(args, "--agent"); value != "" {
		return value
	}
	return defaultWorkflowWorkerAgent
}

func workflowRequestNeedsPlanned(request string) bool {
	words := strings.FieldsFunc(strings.ToLower(request), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	sensitiveWord := map[string]bool{
		"secret": true, "secrets": true, "auth": true, "authentication": true, "oauth": true,
		"token": true, "tokens": true, "sdk": true, "dependency": true, "dependencies": true,
		"migration": true, "migrations": true, "payment": true, "payments": true,
	}
	for i, word := range words {
		if sensitiveWord[word] {
			return true
		}
		if word == "api" && i+1 < len(words) && (words[i+1] == "key" || words[i+1] == "keys") {
			return true
		}
	}
	return false
}

type sliceRunConfig struct{ Paths, Checks []string }
type planFile struct {
	Slices []planFileSlice `json:"slices"`
}
type planFileSlice struct {
	ID, Title          string
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Dependencies       []string `json:"dependencies"`
	Paths              []string `json:"paths"`
	Checks             []string `json:"checks"`
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
	routeValue := workflow.RouteSimple
	explicitRoute := false
	if value, exists := flagValue(args, "--route"); exists {
		explicitRoute = true
		routeValue = workflow.Route(value)
	}
	inferredPlanned := workflowRequestNeedsPlanned(request)
	if inferredPlanned && explicitRoute && routeValue == workflow.RouteSimple {
		return errors.New("sensitive workflow request cannot use simple route; use --route planned or discovery")
	}
	if inferredPlanned && !explicitRoute {
		routeValue = workflow.RoutePlanned
	}
	var planned planFile
	if routeValue == workflow.RoutePlanned {
		if file, exists := flagValue(args, "--plan-file"); exists {
			raw, readErr := os.ReadFile(file)
			if readErr != nil {
				return readErr
			}
			if json.Unmarshal(raw, &planned) != nil || len(planned.Slices) == 0 {
				return errors.New("invalid plan file")
			}
		} else if explicitRoute {
			return errors.New("planned start requires --plan-file")
		} else if len(acceptance) == 0 || len(checks) == 0 || len(paths) == 0 {
			return errors.New("inferred planned start requires explicit --accept, --check, and --path")
		}
	} else if routeValue == workflow.RouteDiscovery {
		if _, exists := flagValue(args, "--wayfinder-file"); !exists {
			return errors.New("discovery start requires --wayfinder-file")
		}
	} else if len(acceptance) == 0 || len(checks) == 0 || len(paths) == 0 {
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
	// Persist the recovery basis before any state that can block exists, so a
	// blocker created later always has an exact tree to reconcile against.
	if err = store.UpdateRecoveryBasis(id, func(b *workflow.RecoveryBasis) {
		b.Seal = seal
		b.CandidatePolicy = gitcandidate.Policy{}
		b.PreTreeOID = candidate.TreeOID
	}); err != nil {
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
	estimated := 1
	if len(planned.Slices) > 0 {
		estimated = len(planned.Slices)
	}
	decision, err := engine.Begin(id, orchestration.RouteInput{Clear: routeValue != workflow.RouteDiscovery, EstimatedSlices: estimated, Sensitive: inferredPlanned, BlockingUncertainty: func() []string {
		if routeValue == workflow.RouteDiscovery {
			return []string{"discovery required"}
		}
		return nil
	}()}, override)
	if err != nil {
		return err
	}
	if decision.Route == workflow.RouteDiscovery {
		file, _ := flagValue(args, "--wayfinder-file")
		raw, e := os.ReadFile(file)
		if e != nil {
			return e
		}
		var graph orchestration.WayfinderGraph
		if json.Unmarshal(raw, &graph) != nil {
			return errors.New("invalid wayfinder file")
		}
		graph.WorkflowID = id
		graph.Version = 2
		if e = orchestration.ValidateWayfinder(graph); e != nil {
			return e
		}
		encoded, _ := json.Marshal(graph)
		if _, e = store.Database().Exec(`INSERT INTO wayfinder_graphs(workflow_id,version,graph) VALUES(?,?,?)`, id, graph.Version, encoded); e != nil {
			return e
		}
		_, modelExplicit := flagValue(args, "--model")
		_, agentExplicit := flagValue(args, "--agent")
		input := workflowRunInput{Request: request, Model: valueOrEmpty(args, "--model"), Agent: workflowAgent(args), ModelExplicit: modelExplicit, AgentExplicit: agentExplicit, ResultTransport: workflow.ResultTransportFileV1, Executable: valueOrEmpty(args, "--opencode"), Timeout: 10 * time.Minute, Seal: seal}
		data, _ := json.Marshal(input)
		_, e = store.Database().Exec(`INSERT INTO workflow_run_inputs(workflow_id,input) VALUES(?,?)`, id, data)
		if e != nil {
			return e
		}
		fmt.Fprintf(out, "%s\t%s\t%s\n", id, workflow.StateDiscovering, decision.Route)
		return nil
	}
	graph := orchestration.ExecutionGraph{WorkflowID: id, Version: 1}
	configs := map[string]sliceRunConfig{}
	if decision.Route == workflow.RoutePlanned && len(planned.Slices) > 0 {
		checks = nil
		paths = nil
		for _, s := range planned.Slices {
			graph.Slices = append(graph.Slices, orchestration.Slice{ID: s.ID, Title: s.Title, AcceptanceCriteria: s.AcceptanceCriteria, Dependencies: s.Dependencies})
			if len(s.Paths) == 0 || len(s.Checks) == 0 {
				return errors.New("planned slices require paths and checks")
			}
			configs[s.ID] = sliceRunConfig{Paths: s.Paths, Checks: s.Checks}
			checks = append(checks, s.Checks...)
			paths = append(paths, s.Paths...)
		}
	} else {
		graph.Slices = []orchestration.Slice{{ID: "slice_main", Title: request, AcceptanceCriteria: acceptance}}
		configs["slice_main"] = sliceRunConfig{Paths: paths, Checks: checks}
	}
	if decision.Route == workflow.RoutePlanned && len(planned.Slices) > 0 {
		acceptance = nil
		for _, s := range planned.Slices {
			acceptance = append(acceptance, s.AcceptanceCriteria...)
		}
	}
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
	_, modelExplicit := flagValue(args, "--model")
	_, agentExplicit := flagValue(args, "--agent")
	input := workflowRunInput{Request: request, Acceptance: acceptance, Checks: checks, AllowedPaths: paths, Model: valueOrEmpty(args, "--model"), Agent: workflowAgent(args), ModelExplicit: modelExplicit, AgentExplicit: agentExplicit, ResultTransport: workflow.ResultTransportFileV1, Executable: valueOrEmpty(args, "--opencode"), Timeout: timeout, Seal: seal, SliceConfigs: configs}
	raw, _ := json.Marshal(input)
	if _, err = store.Database().Exec(`INSERT INTO workflow_run_inputs(workflow_id,input) VALUES(?,?)`, id, raw); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\t%s\n", id, workflow.StateReady, decision.Route)
	return nil
}

func workflowRun(store *workflow.SQLiteStore, repo string, args []string, out io.Writer) error {
	if !hasFlag(args, "--detach") {
		id, err := requiredWorkflowID(args)
		if err != nil {
			return err
		}
		if err = refuseWhileDetachedWorkerIsLive(store, id, "run"); err != nil {
			return err
		}
	}
	return workflowRunContext(context.Background(), store, repo, args, out)
}

// refuseWhileDetachedWorkerIsLive keeps a foreground command from inheriting an
// attempt, worktree, or candidate that a healthy detached worker still owns.
// The worker's own owner and fencing token live in the durable attempt row, so
// a second process can reproduce them and heartbeat the same lease; only the
// job liveness record distinguishes the owner from an intruder. The check stays
// read-only: a crashed worker is simply not live here, so the foreground run
// still proceeds without first blocking the workflow.
func refuseWhileDetachedWorkerIsLive(store *workflow.SQLiteStore, id, operation string) error {
	job, live, err := store.LiveWorkflowJob(id, time.Now())
	if err != nil {
		return err
	}
	if !live {
		return nil
	}
	return fmt.Errorf("workflow %s already has a live detached %s worker (job %s, pid %d); wait for its completion notification, or run `skynex workflow abort %s --idempotency-key abort-%s` to stop it before running %s in the foreground", id, job.Operation, job.ID, job.PID, id, id, operation)
}

// refuseWhileExecutionFenceIsHeld rejects an execution request before it can
// create durable state, scoped strictly to this workflow ID. A different
// workflow is never affected, so alternative solutions to the same problem run
// concurrently in their own worktrees and branches.
func refuseWhileExecutionFenceIsHeld(store *workflow.SQLiteStore, id string) error {
	held, err := store.ExecutionFenceHeld(id, time.Now())
	if err != nil {
		return err
	}
	if !held {
		return nil
	}
	return fmt.Errorf("%w: workflow %s is already being executed; wait for it to finish, or retry in %s if that process died", workflow.ErrExecutionFenceHeld, id, workflowExecutionFenceTTL)
}

// workflowExecutionFenceTTL bounds how long a crashed executor can hold a
// workflow. It matches the job liveness window, so a process that dies without
// releasing its fence frees the workflow on the same schedule as a dead worker.
const workflowExecutionFenceTTL = 30 * time.Second

const workflowExecutionFenceHeartbeat = 5 * time.Second

// executionFence is the durable claim one process holds while it executes a
// workflow. The attempt row publishes the worktree owner and fencing token, so
// any second process can reproduce them and heartbeat that lease; this fence is
// keyed to a private per-process identity that nothing else can read back, and
// it covers the detached worker and the foreground CLI alike because both enter
// through workflowRunContext.
type executionFence struct {
	store                       *workflow.SQLiteStore
	workflowID, resource, owner string
	token                       string
	stop, done                  chan struct{}
}

func fenceWorkflowExecution(store *workflow.SQLiteStore, id string) (*executionFence, error) {
	token, err := newFencingToken()
	if err != nil {
		return nil, err
	}
	fence := &executionFence{
		store:      store,
		workflowID: id,
		resource:   workflow.ExecutionFenceResource(id),
		owner:      fmt.Sprintf("execution-pid-%d-%s", os.Getpid(), token[:16]),
		token:      token,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	now := time.Now()
	if _, err = store.AcquireLease(fence.resource, fence.owner, fence.token, now, now.Add(workflowExecutionFenceTTL)); err != nil {
		if errors.Is(err, workflow.ErrLeaseConflict) {
			return nil, fmt.Errorf("%w: workflow %s is already being executed; wait for it to finish, or retry in %s if that process died", workflow.ErrExecutionFenceHeld, id, workflowExecutionFenceTTL)
		}
		return nil, err
	}
	go func() {
		defer close(fence.done)
		ticker := time.NewTicker(workflowExecutionFenceHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-fence.stop:
				return
			case beat := <-ticker.C:
				if _, err := store.HeartbeatLease(fence.resource, fence.owner, fence.token, beat, beat.Add(workflowExecutionFenceTTL)); err == nil {
					continue
				}
				// The claim lapsed. Reclaim it only while it is genuinely free;
				// if another executor took it, Validate stops this run before
				// the next mutation instead of acting on a stale claim.
				_, _ = store.AcquireLease(fence.resource, fence.owner, fence.token, beat, beat.Add(workflowExecutionFenceTTL))
			}
		}
	}()
	return fence, nil
}

// Validate re-proves exclusivity against the durable lease. Every step that
// mutates the worktree or adopts another executor's work calls it first, so a
// heartbeat that silently lapsed cannot let two processes both believe they own
// the workflow.
func (f *executionFence) Validate() error {
	if err := f.store.ValidateLease(f.resource, f.owner, f.token, time.Now()); err != nil {
		return fmt.Errorf("%w for workflow %s; stopping before mutating", workflow.ErrExecutionFenceLost, f.workflowID)
	}
	return nil
}

func (f *executionFence) Release() {
	close(f.stop)
	<-f.done
	_, _ = f.store.Database().Exec(`DELETE FROM leases WHERE resource=? AND owner=? AND fencing_token=?`, f.resource, f.owner, f.token)
}

func workflowRunContext(ctx context.Context, store *workflow.SQLiteStore, repo string, args []string, out io.Writer) error {
	id, err := requiredWorkflowID(args)
	if err != nil {
		return err
	}
	if hasFlag(args, "--detach") {
		return workflowRunDetached(store, repo, id, out)
	}
	fence, err := fenceWorkflowExecution(store, id)
	if err != nil {
		return err
	}
	defer fence.Release()
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
	if err = workflowRuntimePreflight.Check(ctx, workflow.RuntimePreflightRequest{Phase: "run", Executable: input.Executable, Model: input.Model, Agent: input.Agent, ModelExplicit: input.ModelExplicit, AgentExplicit: input.AgentExplicit, WorkDir: repo, RequireResultFile: true, ResultTransport: input.ResultTransport}); err != nil {
		return err
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
		// A worker from an older process may have died after committing the last
		// slice but before advancing the workflow to verification. Repair that
		// durable state before trying to schedule another mutation.
		if _, err = scheduler.ReconcileCompletion(); err != nil {
			return err
		}
		for {
			var attempt execution.Attempt
			var allowedRaw []byte
			activeErr := store.Database().QueryRow(`SELECT attempt_id,workflow_id,slice_id,worktree_id,owner,fencing_token,basis_tree,allowed_paths,operation_id FROM mutation_attempts WHERE workflow_id=? AND live=1 LIMIT 1`, id).Scan(&attempt.ID, &attempt.WorkflowID, &attempt.SliceID, &attempt.WorktreeID, &attempt.Owner, &attempt.FencingToken, &attempt.BasisTree, &allowedRaw, &attempt.OperationID)
			inherited := activeErr == nil
			if inherited {
				_ = json.Unmarshal(allowedRaw, &attempt.AllowedPaths)
			} else {
				ready, nextErr := scheduler.NextReady()
				if nextErr != nil {
					return nextErr
				}
				if ready == nil {
					break
				}
				attemptID := id + ":" + ready.ID
				owner := "workflow-cli"
				token, tokenErr := newFencingToken()
				if tokenErr != nil {
					return tokenErr
				}
				allowed := input.AllowedPaths
				if config, ok := input.SliceConfigs[ready.ID]; ok {
					allowed = config.Paths
				}
				if err = waitForWorkflowLease(ctx, store, "worktree:"+seal.WorktreeID, owner, token, input.Timeout+time.Minute); err != nil {
					return err
				}
				candidate, freezeErr := gitcandidate.Freeze(seal, gitcandidate.Policy{})
				if freezeErr != nil {
					_, _ = store.Database().Exec(`DELETE FROM leases WHERE resource=? AND owner=? AND fencing_token=?`, "worktree:"+seal.WorktreeID, owner, token)
					return freezeErr
				}
				attempt = execution.Attempt{ID: attemptID, WorkflowID: id, SliceID: ready.ID, WorktreeID: seal.WorktreeID, Owner: owner, FencingToken: token, BasisTree: candidate.TreeOID, AllowedPaths: allowed, OperationID: "mutation:" + attemptID}
				if err = scheduler.Start(attempt); err != nil {
					_, _ = store.Database().Exec(`DELETE FROM leases WHERE resource=? AND owner=? AND fencing_token=?`, "worktree:"+seal.WorktreeID, owner, token)
					return err
				}
			}
			if _, leaseErr := store.AcquireLease("worktree:"+seal.WorktreeID, attempt.Owner, attempt.FencingToken, time.Now(), time.Now().Add(input.Timeout+time.Minute)); leaseErr != nil {
				_, leaseErr = store.HeartbeatLease("worktree:"+seal.WorktreeID, attempt.Owner, attempt.FencingToken, time.Now(), time.Now().Add(input.Timeout+time.Minute))
				if leaseErr != nil {
					if err = waitForWorkflowLease(ctx, store, "worktree:"+seal.WorktreeID, attempt.Owner, attempt.FencingToken, input.Timeout+time.Minute); err != nil {
						return err
					}
				}
			}
			// An inherited attempt may belong to a worker that died between
			// writing the patch and committing it. Reconcile the worktree under
			// the lease before dispatching the same attempt again.
			if inherited {
				if err = fence.Validate(); err != nil {
					return err
				}
				adopted, resumeErr := scheduler.ResumeAttempt(&execution.Broker{Store: store, Seal: seal}, attempt)
				if resumeErr != nil {
					return resumeErr
				}
				if adopted != "" {
					if err = store.UpdateRecoveryBasis(id, func(b *workflow.RecoveryBasis) {
						b.Seal = seal
						b.CandidatePolicy = gitcandidate.Policy{}
						b.PreTreeOID = attempt.BasisTree
						b.PostTreeOID = adopted
					}); err != nil {
						return err
					}
					_, _ = store.Database().Exec(`DELETE FROM leases WHERE resource=? AND owner=? AND fencing_token=?`, "worktree:"+seal.WorktreeID, attempt.Owner, attempt.FencingToken)
					continue
				}
			}
			adapter := execution.OpenCodeAdapter{Store: store, Options: execution.OpenCodeOptions{Executable: input.Executable, Model: input.Model, Agent: input.Agent, Timeout: input.Timeout}}
			criteria := input.Acceptance
			checks := input.Checks
			for _, slice := range graph.Slices {
				if slice.ID == attempt.SliceID {
					criteria = slice.AcceptanceCriteria
					if config, ok := input.SliceConfigs[slice.ID]; ok {
						checks = config.Checks
					}
				}
			}
			result, err := adapter.Run(ctx, execution.OpenCodeRequest{InvocationID: "invoke:" + attempt.ID, Attempt: attempt, Seal: seal, Checks: checks, Prompt: input.Request + "\nAcceptance: " + strings.Join(criteria, "; ")})
			if err != nil {
				return err
			}
			if err = fence.Validate(); err != nil {
				return err
			}
			post, err := (&execution.Broker{Store: store, Seal: seal}).Apply(context.Background(), result)
			if err != nil {
				return err
			}
			if err = store.UpdateRecoveryBasis(id, func(b *workflow.RecoveryBasis) {
				b.Seal = seal
				b.CandidatePolicy = gitcandidate.Policy{}
				b.PreTreeOID = attempt.BasisTree
				b.PostTreeOID = post
			}); err != nil {
				return err
			}
			if err = scheduler.Complete(id, attempt.SliceID); err != nil {
				return err
			}
			_, _ = store.Database().Exec(`DELETE FROM leases WHERE resource=? AND owner=? AND fencing_token=?`, "worktree:"+seal.WorktreeID, attempt.Owner, attempt.FencingToken)
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
	verified, err := (&verification.Runner{Store: store, EngineVersion: "workflow-cli-v1", RiskPolicy: review.RiskPolicy{}}).Run(context.Background(), id, seal, plan)
	if err != nil {
		return err
	}
	if w, err = store.Get(id); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\n", id, w.State)
	if !verified.Passed {
		fmt.Fprintln(out, "verification failed; candidate was not frozen")
		return nil
	}
	fmt.Fprintln(out, "candidate frozen; semantic review runner is not configured")
	return nil
}

func waitForWorkflowLease(ctx context.Context, store *workflow.SQLiteStore, resource, owner, token string, duration time.Duration) error {
	if duration <= 0 {
		duration = time.Minute
	}
	for {
		now := time.Now()
		if _, err := store.AcquireLease(resource, owner, token, now, now.Add(duration)); err == nil {
			return nil
		} else if !errors.Is(err, workflow.ErrLeaseConflict) {
			return err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
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
	if hasFlag(args, "--detach") {
		return workflowReviewDetached(store, id, out)
	}
	if err = refuseWhileDetachedWorkerIsLive(store, id, "review"); err != nil {
		return err
	}
	options, err := reviewOptionsForWorkflow(store, id)
	if err != nil {
		return err
	}
	runner := review.OpenCodeReviewRunner{Store: store, Options: options}
	receipt, err := runner.Run(context.Background(), id)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\t%s\n", id, workflow.StateReceipted, receipt.ID)
	return nil
}

var workflowRuntimePreflight = workflow.DefaultRuntimePreflight()

func reviewOptionsForWorkflow(store *workflow.SQLiteStore, id string) (review.OpenCodeReviewOptions, error) {
	var raw []byte
	if err := store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, id).Scan(&raw); err != nil {
		return review.OpenCodeReviewOptions{}, err
	}
	var input workflowRunInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return review.OpenCodeReviewOptions{}, err
	}
	return review.OpenCodeReviewOptions{Executable: input.Executable, Model: input.Model, Timeout: input.Timeout, Preflight: func(ctx context.Context) error {
		return workflowRuntimePreflight.Check(ctx, workflow.RuntimePreflightRequest{Phase: "review", Executable: input.Executable, Model: input.Model, Agent: "workflow-reviewer", ModelExplicit: input.ModelExplicit, AgentExplicit: true, WorkDir: input.Seal.RepositoryRoot, RequireResultFile: true, ResultTransport: input.ResultTransport})
	}}, nil
}

func workflowApprove(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	id, ok := flagValue(args, "--id")
	if !ok || id == "" {
		return errors.New("approve requires --id")
	}
	action, ok := flagValue(args, "--action")
	if !ok || action == "" {
		return errors.New("approve requires --action")
	}
	actor, ok := flagValue(args, "--actor")
	if !ok || actor == "" {
		return errors.New("approve requires --actor")
	}
	reason, ok := flagValue(args, "--reason")
	if !ok || reason == "" {
		return errors.New("approve requires --reason")
	}
	var raw []byte
	var basis, policy string
	if err := store.Database().QueryRow(`SELECT result FROM verification_runs WHERE workflow_id=?`, id).Scan(&raw); err == nil {
		var verified struct{ Record review.CandidateRecord }
		if err = json.Unmarshal(raw, &verified); err != nil {
			return err
		}
		basis = verified.Record.TreeOID
		policy = verified.Record.PolicyHash
	} else {
		if err = store.Database().QueryRow(`SELECT graph FROM wayfinder_graphs WHERE workflow_id=? ORDER BY version DESC LIMIT 1`, id).Scan(&raw); err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		basis = hex.EncodeToString(sum[:])
		policy = "discovery:v1"
	}
	expires := time.Now().Add(time.Hour)
	if value, exists := flagValue(args, "--expires"); exists {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		expires = time.Now().Add(duration)
	}
	artifact, err := approval.Issue(store.Database(), approval.Artifact{Actor: actor, AuthSource: "cli", WorkflowID: id, Action: action, BasisGraphOrCandidate: basis, PolicyHash: policy, Rationale: reason, IssuedAt: time.Now(), ExpiresAt: expires})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\t%s\n", id, action, artifact.ID)
	return nil
}

func workflowRevokeApproval(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	id, ok := flagValue(args, "--id")
	if !ok {
		return errors.New("revoke-approval requires --id")
	}
	action, ok := flagValue(args, "--action")
	if !ok {
		return errors.New("revoke-approval requires --action")
	}
	actor, ok := flagValue(args, "--actor")
	if !ok {
		return errors.New("revoke-approval requires --actor")
	}
	reason, ok := flagValue(args, "--reason")
	if !ok {
		return errors.New("revoke-approval requires --reason")
	}
	if err := approval.Revoke(store.Database(), id, action, actor, reason, time.Now()); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\trevoked\n", id, action)
	return nil
}

func latestWayfinder(store *workflow.SQLiteStore, id string) (orchestration.WayfinderGraph, []byte, error) {
	var raw []byte
	var g orchestration.WayfinderGraph
	err := store.Database().QueryRow(`SELECT graph FROM wayfinder_graphs WHERE workflow_id=? ORDER BY version DESC LIMIT 1`, id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal(raw, &g)
	}
	return g, raw, err
}
func workflowFrontier(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	id, ok := flagValue(args, "--id")
	if !ok {
		id, _ = requiredWorkflowID(args)
	}
	g, _, err := latestWayfinder(store, id)
	if err != nil {
		return err
	}
	node, err := g.Frontier()
	if err != nil {
		return err
	}
	if node == nil {
		fmt.Fprintln(out, "no blocking frontier")
		return nil
	}
	return writeJSON(out, node)
}
func workflowAnswer(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	id, _ := flagValue(args, "--id")
	nodeID, _ := flagValue(args, "--node")
	answer, _ := flagValue(args, "--answer")
	actor, _ := flagValue(args, "--actor")
	if id == "" || nodeID == "" || answer == "" || actor == "" {
		return errors.New("answer requires --id --node --answer --actor")
	}
	g, raw, err := latestWayfinder(store, id)
	if err != nil {
		return err
	}
	found := false
	for i := range g.Nodes {
		if g.Nodes[i].ID != nodeID {
			continue
		}
		found = true
		if g.Nodes[i].Resolved {
			return errors.New("node already resolved")
		}
		if g.Nodes[i].Type == orchestration.NodePrototype {
			sum := sha256.Sum256(raw)
			if _, err = approval.Require(store.Database(), id, "prototype:"+nodeID, hex.EncodeToString(sum[:]), "discovery:v1", time.Now()); err != nil {
				return err
			}
			validation, _ := json.Marshal(approval.PrototypeValidation{ID: "prototype-validation:" + id + ":" + nodeID, WorkflowID: id, PrototypeID: nodeID, Validator: actor, Outcome: answer, EvidenceDigest: hex.EncodeToString(sum[:]), ValidatedAt: time.Now()})
			if _, err = store.Database().Exec(`INSERT OR IGNORE INTO prototype_validations(id,workflow_id,artifact) VALUES(?,?,?)`, "prototype-validation:"+id+":"+nodeID, id, validation); err != nil {
				return err
			}
		}
		g.Nodes[i].Resolved = true
		g.Nodes[i].Answer = answer
		g.Nodes[i].Actor = actor
	}
	if !found {
		return errors.New("unknown wayfinder node")
	}
	g.Version++
	encoded, _ := json.Marshal(g)
	if _, err = store.Database().Exec(`INSERT INTO wayfinder_graphs(workflow_id,version,graph) VALUES(?,?,?)`, id, g.Version, encoded); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\tanswered\n", id, nodeID)
	return nil
}
func workflowCloseDiscovery(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	id, _ := flagValue(args, "--id")
	file, _ := flagValue(args, "--plan-file")
	if id == "" || file == "" {
		return errors.New("close-discovery requires --id --plan-file")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var plan planFile
	if json.Unmarshal(raw, &plan) != nil {
		return errors.New("invalid plan file")
	}
	g, _, err := latestWayfinder(store, id)
	if err != nil {
		return err
	}
	graph := orchestration.ExecutionGraph{WorkflowID: id, Version: 1}
	contract := orchestration.ExecutableContract{Destination: "discovery contract"}
	configs := map[string]sliceRunConfig{}
	var checks, paths []string
	for _, s := range plan.Slices {
		if len(s.Paths) == 0 || len(s.Checks) == 0 {
			return errors.New("planned slices require paths and checks")
		}
		graph.Slices = append(graph.Slices, orchestration.Slice{ID: s.ID, Title: s.Title, AcceptanceCriteria: s.AcceptanceCriteria, Dependencies: s.Dependencies})
		contract.AcceptanceCriteria = append(contract.AcceptanceCriteria, s.AcceptanceCriteria...)
		configs[s.ID] = sliceRunConfig{Paths: s.Paths, Checks: s.Checks}
		checks = append(checks, s.Checks...)
		paths = append(paths, s.Paths...)
	}
	if err = orchestration.NewEngine(store).Close(id, g, contract, graph); err != nil {
		return err
	}
	var inputRaw []byte
	_ = store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, id).Scan(&inputRaw)
	var input workflowRunInput
	_ = json.Unmarshal(inputRaw, &input)
	input.SliceConfigs = configs
	input.Checks = checks
	input.AllowedPaths = paths
	input.Acceptance = contract.AcceptanceCriteria
	updated, _ := json.Marshal(input)
	_, err = store.Database().Exec(`UPDATE workflow_run_inputs SET input=? WHERE workflow_id=?`, updated, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\n", id, workflow.StateReady)
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
	if authority.EffectiveRisk == review.RiskHigh {
		if _, err = approval.Require(store.Database(), id, "delivery", authority.CandidateTreeOID, authority.PolicyHash, time.Now()); err != nil {
			return err
		}
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
