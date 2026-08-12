package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/joeldevz/skynex/internal/approval"
	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/workflow"
)

const evaluatorManagedDetachEnvironment = "SKYNEX_EVAL_MANAGED_DETACH"

var detachedWorkerExecutable = os.Executable
var detachedWorkerStart = startWorkflowWorkerProcess
var signalDetachedJob = signalWorkflowJobPID
var detachedWorkflowCapability = requireDetachedWorkflowSupport

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func workflowRunDetached(store *workflow.SQLiteStore, repo, id string, out io.Writer) error {
	if err := store.ReconcileStaleWorkflowJobs(id, time.Now()); err != nil {
		return err
	}
	// Refuse before a job exists. A worker spawned into a fence conflict would
	// finish as failed, and a failed job is a durable blocker, so queueing
	// against a healthy executor would abort the very run it collided with.
	if err := refuseWhileExecutionFenceIsHeld(store, id); err != nil {
		return err
	}
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	if w.State != workflow.StateReady && w.State != workflow.StateExecuting && w.State != workflow.StateVerifying && w.State != workflow.StateBlocked {
		return fmt.Errorf("workflow %s cannot run from %s", id, w.State)
	}
	input, err := workflowInputFor(store, id)
	if err != nil {
		return err
	}
	if err = workflowRuntimePreflight.Check(context.Background(), workflow.RuntimePreflightRequest{Phase: "run", Executable: input.Executable, Model: input.Model, Agent: input.Agent, ModelExplicit: input.ModelExplicit, AgentExplicit: input.AgentExplicit, WorkDir: repo, RequireResultFile: true, ResultTransport: input.ResultTransport}); err != nil {
		return err
	}
	return workflowQueueDetached(store, repo, id, "run", w, out)
}

func workflowReviewDetached(store *workflow.SQLiteStore, id string, out io.Writer) error {
	if err := store.ReconcileStaleWorkflowJobs(id, time.Now()); err != nil {
		return err
	}
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	if w.State != workflow.StateCandidateFrozen && w.State != workflow.StateReviewing && w.State != workflow.StateBlocked {
		return fmt.Errorf("workflow %s cannot review from %s", id, w.State)
	}
	input, err := workflowInputFor(store, id)
	if err != nil {
		return err
	}
	if err = workflowRuntimePreflight.Check(context.Background(), workflow.RuntimePreflightRequest{Phase: "review", Executable: input.Executable, Model: input.Model, Agent: "workflow-reviewer", ModelExplicit: input.ModelExplicit, AgentExplicit: true, WorkDir: input.Seal.RepositoryRoot, RequireResultFile: true, ResultTransport: input.ResultTransport}); err != nil {
		return err
	}
	return workflowQueueDetached(store, input.Seal.RepositoryRoot, id, "review", w, out)
}

func workflowInputFor(store *workflow.SQLiteStore, id string) (workflowRunInput, error) {
	var raw []byte
	if err := store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, id).Scan(&raw); err != nil {
		return workflowRunInput{}, err
	}
	var input workflowRunInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return workflowRunInput{}, err
	}
	return input, nil
}

func workflowQueueDetached(store *workflow.SQLiteStore, repo, id, operation string, w workflow.Workflow, out io.Writer) error {
	if _, err := evaluatorManagedDetachMode(); err != nil {
		return err
	}
	// Fail before persisting a job or starting a process. On platforms where we
	// cannot guarantee process-group ownership/cancellation, foreground remains
	// available but detached execution must not risk orphaning OpenCode.
	if err := detachedWorkflowCapability(); err != nil {
		return err
	}
	if err := store.ReconcileStaleWorkflowJobs(id, time.Now()); err != nil {
		return err
	}
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	if w.State == workflow.StateBlocked {
		w, err = store.RetryTechnicalWorkflowJob(id, operation, time.Now())
		if err != nil {
			return err
		}
	}
	token, err := newFencingToken()
	if err != nil {
		return err
	}
	jobID := "job-" + token[:24]
	if _, err = store.CreateWorkflowJobOperation(jobID, id, operation, time.Now()); err != nil {
		return fmt.Errorf("queue detached workflow: %w", err)
	}
	if err = store.BindWorkflowJobSession(jobID, os.Getenv("SKYNEX_OPENCODE_SESSION_ID")); err != nil {
		_ = store.FinishWorkflowJob(jobID, workflow.JobFailed, string(w.State), err.Error(), time.Now())
		return err
	}
	executable, err := detachedWorkerExecutable()
	if err != nil {
		_ = store.FinishWorkflowJob(jobID, workflow.JobFailed, string(w.State), err.Error(), time.Now())
		return err
	}
	pid, err := detachedWorkerStart(executable, repo, id, jobID, filepath.Dir(store.Path()))
	if err != nil {
		_ = store.FinishWorkflowJob(jobID, workflow.JobFailed, string(w.State), err.Error(), time.Now())
		return err
	}
	if err = store.SetWorkflowJobPID(jobID, pid); err != nil {
		_ = signalDetachedJob(pid)
		_ = store.FinishWorkflowJob(jobID, workflow.JobFailed, string(w.State), err.Error(), time.Now())
		return err
	}
	fmt.Fprintf(out, "%s\t%s\t%s\tqueued\tpid=%d\n", id, jobID, operation, pid)
	return nil
}

func workflowWorker(store *workflow.SQLiteStore, repo string, args []string, out io.Writer) error {
	managedDetach, err := evaluatorManagedDetachMode()
	if err != nil {
		return err
	}
	workerCtx, cancelWorker := detachedWorkerContext(managedDetach)
	defer cancelWorker()
	id, err := requiredWorkflowID(args)
	if err != nil {
		return err
	}
	jobID, ok := flagValue(args, "--job")
	if !ok || jobID == "" {
		return errors.New("worker requires --job")
	}
	if err = store.StartWorkflowJob(jobID, os.Getpid(), time.Now()); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				_ = store.HeartbeatWorkflowJob(jobID, now)
			}
		}
	}()
	job, err := store.WorkflowJob(jobID)
	if err != nil {
		return err
	}
	if job.WorkflowID != id {
		return errors.New("worker job/workflow identity mismatch")
	}
	switch job.Operation {
	case "run":
		err = workflowRunContext(workerCtx, store, repo, []string{id}, out)
	case "review":
		var options review.OpenCodeReviewOptions
		options, err = reviewOptionsForWorkflow(store, id)
		if err == nil {
			runner := review.OpenCodeReviewRunner{Store: store, Options: options}
			_, err = runner.Run(workerCtx, id)
		}
	default:
		err = fmt.Errorf("unsupported persisted worker operation %q", job.Operation)
	}
	w, _ := store.Get(id)
	state := workflow.JobSucceeded
	message := ""
	humanGate := false
	if err != nil {
		state = workflow.JobFailed
		message = err.Error()
		if errors.Is(err, approval.ErrApprovalRequired) {
			state = workflow.JobWaitingApproval
			humanGate = true
		}
		// This worker does not own the workflow: it was refused the execution
		// fence, or it lost one it held and stopped before mutating. Recording
		// either as failed would block the workflow under whichever executor
		// legitimately owns the fence now, turning displacement into a durable
		// blocker.
		if workflow.ExecutionDisplaced(err) {
			state = workflow.JobCancelled
			message = workflow.JobDisplacedErrorPrefix + err.Error()
		}
	}
	if w.State == workflow.StateAborted {
		state = workflow.JobCancelled
	}
	managedCancellation := managedDetach && workerCtx.Err() != nil
	if managedCancellation {
		state = workflow.JobCancelled
		message = "evaluator stopped managed detached worker"
	}
	finishErr := store.FinishWorkflowJob(jobID, state, string(w.State), message, time.Now())
	if humanGate {
		return finishErr
	}
	if managedCancellation {
		return finishErr
	}
	if err != nil {
		return err
	}
	return finishErr
}

func workflowNotifications(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("notifications requires claim or ack")
	}
	switch args[0] {
	case "presence":
		session, ok := flagValue(args[1:], "--session")
		if !ok || session == "" {
			return errors.New("notifications presence requires --session")
		}
		return store.HeartbeatWorkflowSession(session, time.Now())
	case "claim":
		consumer, ok := flagValue(args[1:], "--consumer")
		if !ok || consumer == "" {
			return errors.New("notifications claim requires --consumer")
		}
		var n *workflow.WorkflowNotification
		var err error
		if hasFlag(args[1:], "--allow-rebind") {
			n, err = store.ClaimWorkflowNotificationForActive(consumer, flagValues(args[1:], "--active-session"), time.Now())
		} else {
			n, err = store.ClaimWorkflowNotification(consumer, time.Now())
		}
		if err != nil {
			return err
		}
		if n == nil {
			fmt.Fprintln(out, "null")
			return nil
		}
		return writeJSON(out, n)
	case "ack":
		id, ok := flagValue(args[1:], "--id")
		if !ok || id == "" {
			return errors.New("notifications ack requires --id")
		}
		token, ok := flagValue(args[1:], "--claim-token")
		if !ok || token == "" {
			return errors.New("notifications ack requires --claim-token")
		}
		if err := store.AckWorkflowNotification(id, token, time.Now()); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\tacked\n", id)
		return nil
	case "release":
		id, ok := flagValue(args[1:], "--id")
		if !ok || id == "" {
			return errors.New("notifications release requires --id")
		}
		token, ok := flagValue(args[1:], "--claim-token")
		if !ok || token == "" {
			return errors.New("notifications release requires --claim-token")
		}
		if err := store.ReleaseWorkflowNotification(id, token); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\treleased\n", id)
		return nil
	default:
		return fmt.Errorf("unknown notifications command %q", args[0])
	}
}

func startWorkflowWorkerProcess(executable, repo, id, jobID, stateDir string) (int, error) {
	managedDetach, err := evaluatorManagedDetachMode()
	if err != nil {
		return 0, err
	}
	logDir := filepath.Join(stateDir, "jobs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return 0, err
	}
	logPath := filepath.Join(logDir, jobID+".log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(executable, "workflow", "worker", id, "--job", jobID)
	cmd.Dir = repo
	cmd.Stdin = nil
	cmd.Stdout = log
	cmd.Stderr = log
	configureDetachedProcess(cmd, managedDetach)
	if err = cmd.Start(); err != nil {
		log.Close()
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = log.Close()
	return pid, nil
}

func signalWorkflowJobPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	managedDetach, err := evaluatorManagedDetachMode()
	if err != nil {
		return err
	}
	return signalDetachedProcess(p, pid, managedDetach)
}

func evaluatorManagedDetachMode() (bool, error) {
	value, exists := os.LookupEnv(evaluatorManagedDetachEnvironment)
	if !exists {
		return false, nil
	}
	if value != "1" {
		return false, fmt.Errorf("%s must equal exactly 1 when set", evaluatorManagedDetachEnvironment)
	}
	return true, nil
}

func detachedWorkerContext(managed bool) (context.Context, context.CancelFunc) {
	if !managed {
		return context.WithCancel(context.Background())
	}
	return signal.NotifyContext(context.Background(), managedDetachSignals()...)
}
