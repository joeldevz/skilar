package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/approval"
	"github.com/joeldevz/skynex/internal/processregistry"
	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/workflow"
)

type workflowInspection struct {
	Workflow              workflow.Workflow               `json:"workflow"`
	Events                []workflow.Event                `json:"events"`
	Receipt               *review.Receipt                 `json:"authoritative_receipt,omitempty"`
	RunInput              json.RawMessage                 `json:"run_input,omitempty"`
	VerificationRun       json.RawMessage                 `json:"verification_run,omitempty"`
	Approvals             []string                        `json:"current_approvals,omitempty"`
	Invocations           []invocationInspection          `json:"invocations,omitempty"`
	ReviewInvocations     []reviewInvocationInspection    `json:"review_invocations,omitempty"`
	Jobs                  []workflowJobInspection         `json:"jobs,omitempty"`
	VerificationRevisions []workflow.VerificationRevision `json:"verification_revisions,omitempty"`
	NextAction            string                          `json:"next_action,omitempty"`
}

type workflowJobInspection struct {
	ID, Operation, State, Error, TerminalState string
	PID                                        int
	CreatedAt, HeartbeatAt, FinishedAt         string
	Attempt, RetriesRemaining                  int
	NextAction                                 string
}

func detachedRetryCommand(operation, id string) string {
	if operation == "review" {
		return fmt.Sprintf("skynex workflow review --id %s --detach", id)
	}
	return fmt.Sprintf("skynex workflow run %s --detach", id)
}

func workflowAbortCommand(id string) string {
	return fmt.Sprintf("skynex workflow abort %s --idempotency-key abort-%s", id, id)
}

// workflowNextAction names the one command an operator should run next. An
// integration conflict is a fail-closed sink that no command advances, so it
// takes precedence over job-level retry accounting and reports the exact abort
// that releases it instead of advising the operator to wait.
func workflowNextAction(state workflow.State, id string, jobState workflow.JobState, operation string, retriesRemaining int) string {
	if state == workflow.StateIntegrationConflict {
		return workflowAbortCommand(id)
	}
	if jobState == workflow.JobFailed && state == workflow.StateBlocked {
		if retriesRemaining > 0 {
			return detachedRetryCommand(operation, id)
		}
		return "manual_resolution_required"
	}
	return "wait"
}

type reviewInvocationInspection struct {
	ID             string `json:"id"`
	CandidateTree  string `json:"candidate_tree"`
	Lens           string `json:"lens"`
	Model          string `json:"model"`
	Status         string `json:"status"`
	StartedAt      string `json:"started_at"`
	FinishedAt     string `json:"finished_at"`
	ErrorPreview   string `json:"error_preview,omitempty"`
	PID            int    `json:"pid,omitempty"`
	HeartbeatAt    string `json:"heartbeat_at,omitempty"`
	LastActivityAt string `json:"last_activity_at,omitempty"`
}

type invocationInspection struct {
	InvocationID  string `json:"invocation_id"`
	AttemptID     string `json:"attempt_id"`
	Status        string `json:"status"`
	PID           int    `json:"pid"`
	StartedAt     string `json:"started_at"`
	HeartbeatAt   string `json:"heartbeat_at"`
	FinishedAt    string `json:"finished_at,omitempty"`
	StdoutPreview string `json:"stdout_preview,omitempty"`
	StderrPreview string `json:"stderr_preview,omitempty"`
}

func runWorkflowCLI(args []string, cwd string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: skynex workflow <command>; run `skynex workflow --help` for details")
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		printWorkflowUsage(out)
		return nil
	}
	if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
		return printWorkflowCommandUsage(args[0], out)
	}
	if !workflowCommandKnown(args[0]) {
		return fmt.Errorf("unknown workflow command %q", args[0])
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	path, err := workflow.CanonicalDatabasePath(cwd)
	if err != nil {
		return err
	}
	if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
		if args[0] == "status" {
			if len(args) > 1 {
				return fmt.Errorf("workflow database not found at %s", path)
			}
			fmt.Fprintln(out, "WORKFLOW\tSTATE\tVERSION\tROUTE\tRISK")
			return nil
		}
		if args[0] == "inspect" || args[0] == "receipt" {
			return fmt.Errorf("workflow database not found at %s", path)
		}
		if args[0] == "notifications" && len(args) > 1 && args[1] == "claim" {
			fmt.Fprintln(out, "null")
			return nil
		}
		if args[0] == "notifications" && len(args) > 1 && args[1] == "presence" {
			return nil
		}
	}
	diagnostic := args[0] == "status" || args[0] == "inspect"
	var inspectID string
	if args[0] == "inspect" {
		if inspectID, err = requiredWorkflowID(args[1:]); err != nil {
			return err
		}
	}
	readOnly := diagnostic || args[0] == "receipt" || args[0] == "export"
	var store *workflow.SQLiteStore
	if readOnly {
		if diagnostic {
			store, err = workflow.OpenRepositorySQLiteLiveReadOnly(cwd)
		} else {
			store, err = workflow.OpenRepositorySQLiteReadOnly(cwd)
		}
	} else {
		store, err = workflow.OpenRepositorySQLite(cwd)
	}
	if err != nil {
		return err
	}
	// Keep healthy diagnostics genuinely read-only. If their liveness probe
	// finds an orphaned worker, reopen only then so the command can persist the
	// durable blocker instead of reporting a workflow as executing forever. The
	// probe already enumerated the workflows, so hand that scan to the command
	// rather than repeating it.
	var scan workflowDiagnosticScan
	if readOnly && diagnostic {
		probe, probeErr := workflowDiagnosticProbe(store, args, inspectID)
		if probeErr != nil {
			store.Close()
			return probeErr
		}
		scan = probe
		if probe.needsRecovery {
			store.Close()
			store, err = workflow.OpenRepositorySQLite(cwd)
			if err != nil {
				return err
			}
		}
	}
	defer store.Close()
	reviews := review.NewSQLiteStore(store.Database())
	switch args[0] {
	case "start":
		return workflowStart(store, cwd, args[1:], out)
	case "run":
		return workflowRun(store, cwd, args[1:], out)
	case "worker":
		return workflowWorker(store, cwd, args[1:], out)
	case "notifications":
		return workflowNotifications(store, args[1:], out)
	case "review":
		return workflowReview(store, args[1:], out)
	case "deliver":
		return workflowDeliver(store, args[1:], out, nil)
	case "approve":
		return workflowApprove(store, args[1:], out)
	case "revoke-approval":
		return workflowRevokeApproval(store, args[1:], out)
	case "frontier":
		return workflowFrontier(store, args[1:], out)
	case "answer":
		return workflowAnswer(store, args[1:], out)
	case "close-discovery":
		return workflowCloseDiscovery(store, args[1:], out)
	case "status":
		return workflowStatus(store, args[1:], out, scan)
	case "inspect":
		return workflowInspect(store, reviews, inspectID, out)
	case "receipt":
		return workflowReceipt(reviews, args[1:], out)
	case "abort":
		return workflowAbort(store, args[1:], out)
	case "resume":
		return workflowResume(store, cwd, args[1:], out)
	case "retry-verification":
		return workflowRetryVerification(store, args[1:], out)
	case "export":
		return workflowExport(store, reviews, args[1:], out)
	default:
		return fmt.Errorf("unknown workflow command %q", args[0])
	}
}

// workflowDiagnosticScan carries the workflow enumeration a diagnostic probe
// already performed, so the command it precedes does not repeat it.
type workflowDiagnosticScan struct {
	ids           []string
	enumerated    bool
	needsRecovery bool
}

func workflowDiagnosticProbe(store *workflow.SQLiteStore, args []string, inspectID string) (workflowDiagnosticScan, error) {
	var scan workflowDiagnosticScan
	switch args[0] {
	case "inspect":
		scan.ids = []string{inspectID}
	case "status":
		if len(args) > 1 {
			scan.ids = []string{args[1]}
		} else {
			ids, err := allWorkflowIDs(store)
			if err != nil {
				return workflowDiagnosticScan{}, err
			}
			scan.ids = ids
			scan.enumerated = true
		}
	}
	for _, id := range scan.ids {
		stale, err := store.HasStaleWorkflowJobs(id, time.Now())
		if err != nil {
			return workflowDiagnosticScan{}, err
		}
		if stale {
			scan.needsRecovery = true
			break
		}
	}
	return scan, nil
}

func allWorkflowIDs(store *workflow.SQLiteStore) ([]string, error) {
	rows, err := store.Database().Query(`SELECT id FROM workflows ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func workflowCommandKnown(command string) bool {
	switch command {
	case "start", "run", "worker", "notifications", "review", "deliver", "approve", "revoke-approval", "frontier", "answer", "close-discovery", "status", "inspect", "receipt", "abort", "resume", "retry-verification", "export":
		return true
	}
	return false
}

func printWorkflowCommandUsage(command string, out io.Writer) error {
	usage := map[string]string{
		"start":              "Usage: skynex workflow start --id ID --request TEXT --path PATH --check COMMAND --accept COMMAND [--route simple|planned|discovery] [--plan-file FILE] [--wayfinder-file FILE] [--override-actor ACTOR --override-reason REASON] [--model MODEL] [--agent AGENT] [--opencode PATH] [--timeout DURATION]\nSimple requires --id, --request, and repeatable --path, --check, --accept. Planned requires --plan-file. Discovery requires --wayfinder-file.",
		"run":                "Usage: skynex workflow run WORKFLOW_ID [--detach]",
		"notifications":      "Usage: skynex workflow notifications claim --consumer SESSION_ID | ack|release --id ID --claim-token TOKEN",
		"review":             "Usage: skynex workflow review --id WORKFLOW_ID [--detach]",
		"deliver":            "Usage: skynex workflow deliver --id WORKFLOW_ID --message TEXT --idempotency-key KEY [--author-name NAME --author-email EMAIL]",
		"status":             "Usage: skynex workflow status [WORKFLOW_ID]",
		"inspect":            "Usage: skynex workflow inspect WORKFLOW_ID",
		"receipt":            "Usage: skynex workflow receipt WORKFLOW_ID | --id RECEIPT_ID",
		"approve":            "Usage: skynex workflow approve --id WORKFLOW_ID --action ACTION --actor ACTOR --reason TEXT [--expires DURATION]",
		"revoke-approval":    "Usage: skynex workflow revoke-approval --id WORKFLOW_ID --action ACTION --actor ACTOR --reason TEXT",
		"abort":              "Usage: skynex workflow abort WORKFLOW_ID --idempotency-key KEY",
		"resume":             "Usage: skynex workflow resume WORKFLOW_ID --blocker-id ID --idempotency-key KEY",
		"retry-verification": "Usage: skynex workflow retry-verification --id WORKFLOW_ID --check-id EVIDENCE_ID --replacement COMMAND --actor ACTOR --reason TEXT --idempotency-key KEY",
		"export":             "Usage: skynex workflow export WORKFLOW_ID --out PATH",
		"frontier":           "Usage: skynex workflow frontier --id WORKFLOW_ID",
		"answer":             "Usage: skynex workflow answer --id WORKFLOW_ID --node NODE_ID --answer TEXT --actor ACTOR",
		"close-discovery":    "Usage: skynex workflow close-discovery --id WORKFLOW_ID --plan-file FILE",
	}
	value, ok := usage[command]
	if !ok {
		return fmt.Errorf("unknown workflow command %q", command)
	}
	fmt.Fprintln(out, value)
	return nil
}

func printWorkflowUsage(out io.Writer) {
	fmt.Fprintln(out, `Usage: skynex workflow <command> [options]

Commands:
  start               Create a simple, planned, or discovery workflow
  run                 Execute ready slices with OpenCode and verify the result
  notifications       Claim or acknowledge terminal workflow notifications
  review              Review a frozen candidate and issue its receipt
  deliver             Commit the exact receipt-authorized candidate tree
  status              List workflows or show one workflow
  inspect             Show workflow events, inputs, approvals, and authority
  receipt             Show the current or a historical receipt
  approve             Approve an exact high-risk action basis
  revoke-approval     Revoke a current approval
  abort               Stop work and revoke attempts, leases, and approvals
  resume              Reconcile and resume a blocked workflow
  retry-verification  Replace one failed check and verify the same candidate
  export              Export a workflow summary or receipt
  frontier            Show the next blocking discovery question
  answer              Record an attributed discovery answer
  close-discovery     Close discovery with an explicit execution plan

Run skynex workflow <command> --help for command-specific options.`)
}

func workflowStatus(store *workflow.SQLiteStore, args []string, out io.Writer, scan workflowDiagnosticScan) error {
	if len(args) > 0 {
		if err := store.ReconcileStaleWorkflowJobs(args[0], time.Now()); err != nil {
			return err
		}
		w, err := store.Get(args[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "WORKFLOW\tSTATE\tVERSION\tROUTE\tRISK\n%s\t%s\t%d\t%s\t%s\n", w.ID, w.State, w.StateVersion, w.Route, w.MinimumRisk)
		var job workflow.WorkflowJob
		var created, started, heartbeat, finished string
		err = store.Database().QueryRow(`SELECT id,workflow_id,session_id,operation,state,pid,created_at,started_at,heartbeat_at,finished_at,terminal_state,error FROM workflow_jobs WHERE workflow_id=? ORDER BY created_at DESC LIMIT 1`, w.ID).Scan(&job.ID, &job.WorkflowID, &job.SessionID, &job.Operation, &job.State, &job.PID, &created, &started, &heartbeat, &finished, &job.TerminalState, &job.Error)
		next := workflowNextAction(w.State, w.ID, "", "", 0)
		if err == nil {
			attempts, _ := store.WorkflowJobAttempts(w.ID, job.Operation)
			remaining := workflow.MaxWorkflowJobAttempts - attempts
			if remaining < 0 {
				remaining = 0
			}
			next = workflowNextAction(w.State, w.ID, job.State, job.Operation, remaining)
			preview := strings.ReplaceAll(strings.TrimSpace(job.Error), "\n", " ")
			if len(preview) > 500 {
				preview = preview[:500] + "..."
			}
			fmt.Fprintf(out, "JOB\t%s\t%s\toperation=%s\tpid=%d\theartbeat=%s\tterminal=%s\tattempt=%d\tretries_remaining=%d\tnext=%s\terror=%s\n", job.ID, job.State, job.Operation, job.PID, heartbeat, job.TerminalState, attempts, remaining, next, preview)
		}
		if next != "wait" {
			fmt.Fprintf(out, "NEXT\t%s\n", next)
		}
		return nil
	}
	ids := scan.ids
	if !scan.enumerated {
		var err error
		if ids, err = allWorkflowIDs(store); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if err := store.ReconcileStaleWorkflowJobs(id, time.Now()); err != nil {
			return err
		}
	}
	rows, err := store.Database().Query(`SELECT id,state,state_version,route,minimum_risk FROM workflows ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Fprintln(out, "WORKFLOW\tSTATE\tVERSION\tROUTE\tRISK")
	for rows.Next() {
		var w workflow.Workflow
		if err := rows.Scan(&w.ID, &w.State, &w.StateVersion, &w.Route, &w.MinimumRisk); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\t%s\t%d\t%s\t%s\n", w.ID, w.State, w.StateVersion, w.Route, w.MinimumRisk)
	}
	return rows.Err()
}

func workflowInspect(store *workflow.SQLiteStore, reviews *review.SQLiteStore, id string, out io.Writer) error {
	if err := store.ReconcileStaleWorkflowJobs(id, time.Now()); err != nil {
		return err
	}
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	events, err := store.Events(id)
	if err != nil {
		return err
	}
	inspection := workflowInspection{Workflow: w, Events: events}
	if action := workflowNextAction(w.State, id, "", "", 0); action != "wait" {
		inspection.NextAction = action
	}
	if rows, e := store.Database().Query(`SELECT id,operation,state,pid,created_at,heartbeat_at,finished_at,terminal_state,error FROM workflow_jobs WHERE workflow_id=? ORDER BY created_at`, id); e == nil {
		defer rows.Close()
		attemptByOperation := map[string]int{}
		for rows.Next() {
			var j workflowJobInspection
			if rows.Scan(&j.ID, &j.Operation, &j.State, &j.PID, &j.CreatedAt, &j.HeartbeatAt, &j.FinishedAt, &j.TerminalState, &j.Error) == nil {
				attemptByOperation[j.Operation]++
				j.Attempt = attemptByOperation[j.Operation]
				j.RetriesRemaining = workflow.MaxWorkflowJobAttempts - j.Attempt
				if j.RetriesRemaining < 0 {
					j.RetriesRemaining = 0
				}
				if action := workflowNextAction(w.State, id, workflow.JobState(j.State), j.Operation, j.RetriesRemaining); action != "wait" {
					j.NextAction = action
					inspection.NextAction = action
				}
				inspection.Jobs = append(inspection.Jobs, j)
			}
		}
	}
	var input []byte
	if e := store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, id).Scan(&input); e == nil {
		inspection.RunInput = input
	}
	var verificationRun []byte
	if e := store.Database().QueryRow(`SELECT result FROM verification_runs WHERE workflow_id=?`, id).Scan(&verificationRun); e == nil {
		inspection.VerificationRun = verificationRun
	}
	rows, e := store.Database().Query(`SELECT action||':'||approval_id FROM current_approvals WHERE workflow_id=? ORDER BY action`, id)
	if e == nil {
		defer rows.Close()
		for rows.Next() {
			var value string
			if rows.Scan(&value) == nil {
				inspection.Approvals = append(inspection.Approvals, value)
			}
		}
	}
	if receipt, e := reviews.Authority(id); e == nil {
		inspection.Receipt = &receipt
	} else if !errors.Is(e, review.ErrNoAuthority) {
		return e
	}
	if rows, e := store.Database().Query(`SELECT invocation_id,attempt_id,status,pid,started_at,heartbeat_at,finished_at,stdout_preview,stderr_preview FROM invocation_runtime WHERE workflow_id=? ORDER BY started_at`, id); e == nil {
		defer rows.Close()
		for rows.Next() {
			var v invocationInspection
			if rows.Scan(&v.InvocationID, &v.AttemptID, &v.Status, &v.PID, &v.StartedAt, &v.HeartbeatAt, &v.FinishedAt, &v.StdoutPreview, &v.StderrPreview) == nil {
				inspection.Invocations = append(inspection.Invocations, v)
			}
		}
	}
	if rows, e := store.Database().Query(`SELECT id,candidate_tree,lens,model,status,started_at,finished_at,error_preview,pid,heartbeat_at,last_activity_at FROM review_invocations WHERE workflow_id=? ORDER BY started_at`, id); e == nil {
		defer rows.Close()
		for rows.Next() {
			var v reviewInvocationInspection
			if rows.Scan(&v.ID, &v.CandidateTree, &v.Lens, &v.Model, &v.Status, &v.StartedAt, &v.FinishedAt, &v.ErrorPreview, &v.PID, &v.HeartbeatAt, &v.LastActivityAt) == nil {
				inspection.ReviewInvocations = append(inspection.ReviewInvocations, v)
			}
		}
	}
	if rows, e := store.Database().Query(`SELECT workflow_id,revision,candidate_tree,check_id,previous_command,replacement_command,actor,reason,idempotency_key,attempt_id,fencing_token,created_at FROM verification_contract_revisions WHERE workflow_id=? ORDER BY revision`, id); e == nil {
		defer rows.Close()
		for rows.Next() {
			var revision workflow.VerificationRevision
			if rows.Scan(&revision.WorkflowID, &revision.Revision, &revision.CandidateTree, &revision.CheckID, &revision.PreviousCommand, &revision.ReplacementCommand, &revision.Actor, &revision.Reason, &revision.IdempotencyKey, &revision.AttemptID, &revision.FencingToken, &revision.CreatedAt) == nil {
				inspection.VerificationRevisions = append(inspection.VerificationRevisions, revision)
			}
		}
	}
	return writeJSON(out, inspection)
}

func workflowReceipt(reviews *review.SQLiteStore, args []string, out io.Writer) error {
	if id, ok := flagValue(args, "--id"); ok {
		r, err := reviews.Receipt(id)
		if err != nil {
			return err
		}
		return writeJSON(out, r)
	}
	id, err := requiredWorkflowID(args)
	if err != nil {
		return errors.New("receipt requires a workflow ID or --id RECEIPT_ID")
	}
	r, err := reviews.Authority(id)
	if err != nil {
		return err
	}
	return writeJSON(out, r)
}

func workflowAbort(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	id, err := requiredWorkflowID(args)
	if err != nil {
		return err
	}
	key, ok := flagValue(args, "--idempotency-key")
	if !ok || key == "" {
		return errors.New("abort requires --idempotency-key")
	}
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	if w.State == workflow.StateAborted {
		_ = cleanupAbortedWorkflow(store, id)
		events, eventErr := store.Events(id)
		if eventErr != nil {
			return eventErr
		}
		for _, event := range events {
			if event.IdempotencyKey == key && event.To == workflow.StateAborted {
				updated, replayErr := store.Transition(workflow.Transition{WorkflowID: id, ExpectedState: event.From, ExpectedVersion: event.StateVersion - 1, NextState: workflow.StateAborted, IdempotencyKey: key})
				if replayErr != nil {
					return replayErr
				}
				fmt.Fprintf(out, "%s\t%s\t%d\n", updated.ID, updated.State, updated.StateVersion)
				return nil
			}
		}
	}
	if terminal(w.State) {
		return fmt.Errorf("workflow %s is already terminal in %s", id, w.State)
	}
	updated, err := store.Transition(workflow.Transition{WorkflowID: id, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateAborted, IdempotencyKey: key})
	if err != nil {
		return err
	}
	if err = cleanupAbortedWorkflow(store, id); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\t%d\n", updated.ID, updated.State, updated.StateVersion)
	return nil
}

func cleanupAbortedWorkflow(store *workflow.SQLiteStore, id string) error {
	cancelled := processregistry.CancelWorkflow(id)
	now := time.Now()
	jobs, jobsErr := store.CancelWorkflowJobs(id, now)
	if jobsErr != nil {
		return jobsErr
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	_, _ = store.Database().Exec(`UPDATE invocation_runtime SET status='cancelled',heartbeat_at=?,finished_at=? WHERE workflow_id=? AND status='running'`, stamp, stamp, id)
	_, _ = store.Database().Exec(`UPDATE opencode_invocations SET status='cancelled',finished_at=? WHERE workflow_id=? AND status='running'`, stamp, id)
	if rows, e := store.Database().Query(`SELECT pid,heartbeat_at FROM review_invocations WHERE workflow_id=? AND status='running'`, id); e == nil {
		for rows.Next() {
			var pid int
			var heartbeat string
			if rows.Scan(&pid, &heartbeat) == nil {
				seen, _ := time.Parse(time.RFC3339Nano, heartbeat)
				if pid > 0 && now.Sub(seen) >= 0 && now.Sub(seen) <= 30*time.Second {
					_ = signalDetachedJob(pid)
				}
			}
		}
		rows.Close()
	}
	_, _ = store.Database().Exec(`UPDATE review_invocations SET status='cancelled',finished_at=?,heartbeat_at=?,error_preview='review cancelled by workflow abort' WHERE workflow_id=? AND status='running'`, stamp, stamp, id)
	for _, job := range jobs {
		reference := job.HeartbeatAt
		if reference.IsZero() {
			reference = job.CreatedAt
		}
		// Refuse to signal a stale PID: it may have been reused by an unrelated
		// process. A healthy worker heartbeats every two seconds.
		age := now.Sub(reference)
		if job.PID > 0 && age >= 0 && age <= 30*time.Second {
			_ = signalDetachedJob(job.PID)
		}
		_ = store.FinishWorkflowJob(job.ID, workflow.JobCancelled, string(workflow.StateAborted), "workflow aborted", now)
	}
	_, _ = store.Database().Exec(`UPDATE mutation_attempts SET live=0 WHERE workflow_id=?`, id)
	_, _ = store.Database().Exec(`DELETE FROM leases WHERE resource IN (SELECT 'worktree:'||worktree_id FROM mutation_attempts WHERE workflow_id=?)`, id)
	_ = approval.RevokeAll(store.Database(), id, "workflow-abort", "kill switch", time.Now())
	plan, _ := json.Marshal(map[string]any{"workflow_id": id, "processes_cancelled": cancelled, "attempts_revoked": true, "leases_revoked": true, "approvals_revoked": true})
	_, err := store.Database().Exec(`INSERT INTO abort_cleanup_plans(workflow_id,plan) VALUES(?,?) ON CONFLICT(workflow_id) DO UPDATE SET plan=excluded.plan`, id, plan)
	return err
}

func workflowResume(store *workflow.SQLiteStore, repo string, args []string, out io.Writer) error {
	id, err := requiredWorkflowID(args)
	if err != nil {
		return err
	}
	blocker, ok := flagValue(args, "--blocker-id")
	if !ok || blocker == "" {
		return errors.New("resume requires --blocker-id")
	}
	key, ok := flagValue(args, "--idempotency-key")
	if !ok || key == "" {
		return errors.New("resume requires --idempotency-key")
	}
	updated, err := store.Resume(context.Background(), repo, workflow.ResumeRequest{WorkflowID: id, BlockerID: blocker, IdempotencyKey: key})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\t%d\n", updated.ID, updated.State, updated.StateVersion)
	return nil
}

func workflowExport(store *workflow.SQLiteStore, reviews *review.SQLiteStore, args []string, out io.Writer) error {
	id, err := requiredWorkflowID(args)
	if err != nil {
		return err
	}
	path, ok := flagValue(args, "--out")
	if !ok || path == "" {
		return errors.New("export requires --out PATH")
	}
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	events, err := store.Events(id)
	if err != nil {
		return err
	}
	value := workflowInspection{Workflow: w, Events: events}
	if r, e := reviews.Authority(id); e == nil {
		value.Receipt = &r
	} else if !errors.Is(e, review.ErrNoAuthority) {
		return e
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err = writeAtomic0600(path, data); err != nil {
		return err
	}
	fmt.Fprintln(out, path)
	return nil
}

func writeAtomic0600(path string, data []byte) error {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("export destination is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	temp, err := os.CreateTemp(dir, ".skynex-export-")
	if err != nil {
		return err
	}
	name := temp.Name()
	ok := false
	defer func() {
		temp.Close()
		if !ok {
			os.Remove(name)
		}
	}()
	if err = temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err = temp.Write(data); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}
func requiredWorkflowID(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--id" || arg == "--out" || arg == "--idempotency-key" || arg == "--blocker-id" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "--") && strings.Contains(arg, "=") {
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			return arg, nil
		}
	}
	return "", errors.New("workflow ID is required")
}
func flagValue(args []string, name string) (string, bool) {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1], true
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"="), true
		}
	}
	return "", false
}
func terminal(state workflow.State) bool {
	return state == workflow.StateDelivered || state == workflow.StateAborted || state == workflow.StateFailed
}
func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
