package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/execution"
	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/workflow"
)

func cliWorkflowRepo(t *testing.T) (string, *workflow.SQLiteStore) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{{"init", repo}, {"-C", repo, "config", "user.email", "test@example.com"}, {"-C", repo, "config", "user.name", "Test"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", repo, "add", "a.txt"}, {"-C", repo, "commit", "-m", "base"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	return repo, store
}

func cliFakeOpenCode(t *testing.T, prefix string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	body := "#!/bin/sh\nset -eu\n" + prefix + `
tree=$(git write-tree)
printf '{"envelope":{"WorkflowID":"wf","NodeID":"slice_main","AttemptID":"wf:slice_main","BaseCandidateOID":"%s","Status":"completed","EvidenceIDs":["fake"]},"patch":{"Operations":[{"Path":"a.txt","Data":"bmV3Cg==","Mode":384}]}}' "$tree" > "$SKYNEX_RESULT_FILE"
`
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWorkflowStartRunToCandidateFrozen(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "test \"$(cat a.txt)\" = base")
	args := []string{"--id", "wf", "--request", "change a", "--accept", "test \"$(cat a.txt)\" = new", "--check", "test -f a.txt", "--path", "a.txt", "--opencode", fake, "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := workflowRun(store, repo, []string{"wf"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "candidate_frozen") {
		t.Fatalf("out=%q", out.String())
	}
	w, _ := store.Get("wf")
	if w.State != workflow.StateCandidateFrozen {
		t.Fatalf("state=%s", w.State)
	}
}

func TestWorkflowPersistsRecoveryBasisSoBlockedResumeSucceeds(t *testing.T) {
	if !workflow.ResumeSupported() {
		t.Skip("resume requires exclusive worktree locking")
	}
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "test \"$(cat a.txt)\" = base")
	args := []string{"--id", "wf", "--request", "change a", "--accept", "test \"$(cat a.txt)\" = new", "--check", "test -f a.txt", "--path", "a.txt", "--opencode", fake, "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	started, err := store.RecoveryBasis("wf")
	if err != nil {
		t.Fatalf("start did not persist a recovery basis: %v", err)
	}
	if started.Seal.RepositoryRoot != repo || started.PreTreeOID == "" {
		t.Fatalf("basis=%+v", started)
	}
	if err = workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	basis, err := store.RecoveryBasis("wf")
	if err != nil {
		t.Fatal(err)
	}
	if basis.PostTreeOID == "" || basis.PostTreeOID == started.PreTreeOID {
		t.Fatalf("mutation trees were not recorded: %+v", basis)
	}
	if basis.CandidateRecordID == "" || basis.CandidateTreeOID == "" || basis.PolicyHash == "" {
		t.Fatalf("candidate lineage was not recorded: %+v", basis)
	}
	if basis.Seal.RepositoryRoot != repo || basis.PreTreeOID == "" {
		t.Fatalf("merge dropped the start lineage: %+v", basis)
	}
	w, err := store.Get("wf")
	if err != nil || w.State != workflow.StateCandidateFrozen {
		t.Fatalf("workflow=%+v err=%v", w, err)
	}
	if _, err = store.Transition(workflow.Transition{WorkflowID: "wf", ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateBlocked, ResumeTarget: workflow.StateCandidateFrozen, IdempotencyKey: "block", ArtifactIDs: []string{"blocker-1"}}); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.Resume(context.Background(), repo, workflow.ResumeRequest{WorkflowID: "wf", BlockerID: "blocker-1", IdempotencyKey: "resume"})
	if err != nil {
		t.Fatalf("resume failed with a persisted basis: %v", err)
	}
	if resumed.State != workflow.StateCandidateFrozen {
		t.Fatalf("resumed=%s", resumed.State)
	}
}

func TestForegroundRunRefusesToInheritALiveDetachedWorkersAttempt(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	args := []string{"--id", "wf", "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	w, err := store.Get("wf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateExecuting, IdempotencyKey: "to-executing"}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateWorkflowJobOperation("job-live", "wf", "run", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = store.StartWorkflowJob("job-live", os.Getpid(), time.Now()); err != nil {
		t.Fatal(err)
	}
	err = workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("foreground run proceeded against a live detached worker")
	}
	if !strings.Contains(err.Error(), "job-live") || !strings.Contains(err.Error(), "abort") {
		t.Fatalf("refusal is not actionable: %v", err)
	}
	if err = workflowReview(store, []string{"--id", "wf"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "job-live") {
		t.Fatalf("foreground review was not refused: %v", err)
	}
	job, err := store.WorkflowJob("job-live")
	if err != nil || job.State != workflow.JobRunning {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if w, err = store.Get("wf"); err != nil || w.State != workflow.StateExecuting {
		t.Fatalf("workflow=%+v err=%v", w, err)
	}
}

func TestForegroundRunProceedsOnceTheDetachedWorkerIsDead(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "test \"$(cat a.txt)\" = base")
	args := []string{"--id", "wf", "--request", "change a", "--accept", "test \"$(cat a.txt)\" = new", "--check", "test -f a.txt", "--path", "a.txt", "--opencode", fake, "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkflowJobOperation("job-dead", "wf", "run", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.StartWorkflowJob("job-dead", 99999999, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("a dead worker blocked the foreground run: %v", err)
	}
	w, err := store.Get("wf")
	if err != nil || w.State != workflow.StateCandidateFrozen {
		t.Fatalf("workflow=%+v err=%v", w, err)
	}
}

func TestWorkflowRetryVerificationReplacesOnlyFailedCheckAndPreservesCandidate(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	args := []string{"--id", "wf", "--request", "change a", "--accept", "test \"$(cat a.txt)\" = new", "--check", "false", "--check", "test -f a.txt", "--path", "a.txt", "--opencode", fake, "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	w, _ := store.Get("wf")
	if w.State != workflow.StateReplanRequired {
		t.Fatalf("state=%s", w.State)
	}
	var beforeRaw []byte
	if err := store.Database().QueryRow(`SELECT result FROM verification_runs WHERE workflow_id='wf'`).Scan(&beforeRaw); err != nil {
		t.Fatal(err)
	}
	var before struct {
		Candidate struct{ TreeOID string }
		Evidence  []struct {
			ID       string
			Kind     string
			ExitCode int
		}
	}
	if err := json.Unmarshal(beforeRaw, &before); err != nil {
		t.Fatal(err)
	}
	var failedID string
	for _, item := range before.Evidence {
		if item.Kind == "check" && item.ExitCode != 0 {
			failedID = item.ID
		}
	}
	if failedID == "" {
		t.Fatal("missing failed check evidence")
	}
	var invocationsBefore int
	if err := store.Database().QueryRow(`SELECT COUNT(*) FROM opencode_invocations WHERE workflow_id='wf'`).Scan(&invocationsBefore); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := workflowRetryVerification(store, []string{"--id", "wf", "--check-id", failedID, "--replacement", "true", "--actor", "tester", "--reason", "verification environment fixed", "--idempotency-key", "retry:v1"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	w, _ = store.Get("wf")
	if w.State != workflow.StateCandidateFrozen {
		t.Fatalf("state=%s out=%q", w.State, out.String())
	}
	var afterRaw []byte
	if err := store.Database().QueryRow(`SELECT result FROM verification_runs WHERE workflow_id='wf'`).Scan(&afterRaw); err != nil {
		t.Fatal(err)
	}
	var after struct {
		Candidate struct{ TreeOID string }
		Passed    bool
	}
	if err := json.Unmarshal(afterRaw, &after); err != nil {
		t.Fatal(err)
	}
	if !after.Passed || after.Candidate.TreeOID != before.Candidate.TreeOID {
		t.Fatalf("candidate changed or verification failed: before=%s after=%s passed=%v", before.Candidate.TreeOID, after.Candidate.TreeOID, after.Passed)
	}
	var invocationsAfter, history, revisions int
	_ = store.Database().QueryRow(`SELECT COUNT(*) FROM opencode_invocations WHERE workflow_id='wf'`).Scan(&invocationsAfter)
	_ = store.Database().QueryRow(`SELECT COUNT(*) FROM verification_run_history WHERE workflow_id='wf'`).Scan(&history)
	_ = store.Database().QueryRow(`SELECT COUNT(*) FROM verification_contract_revisions WHERE workflow_id='wf'`).Scan(&revisions)
	if invocationsAfter != invocationsBefore || history != 1 || revisions != 1 {
		t.Fatalf("coder reran or provenance missing: invocations=%d/%d history=%d revisions=%d", invocationsBefore, invocationsAfter, history, revisions)
	}
	var rawInput []byte
	if err := store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id='wf'`).Scan(&rawInput); err != nil {
		t.Fatal(err)
	}
	var input workflowRunInput
	_ = json.Unmarshal(rawInput, &input)
	if len(input.Checks) != 2 || input.Checks[0] != "true" || input.Checks[1] != "test -f a.txt" || len(input.Acceptance) != 1 {
		t.Fatalf("contract mutated beyond failed check: %+v", input)
	}
	if err := workflowRetryVerification(store, []string{"--id", "wf", "--check-id", failedID, "--replacement", "true", "--actor", "tester", "--reason", "verification environment fixed", "--idempotency-key", "retry:v1"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("idempotent retry failed: %v", err)
	}
}

func failedCheckEvidenceID(t *testing.T, store *workflow.SQLiteStore) string {
	t.Helper()
	var raw []byte
	if err := store.Database().QueryRow(`SELECT result FROM verification_runs WHERE workflow_id='wf'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Passed   bool
		Evidence []struct {
			ID       string
			Kind     string
			ExitCode int
		}
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Passed {
		t.Fatal("verification unexpectedly passed")
	}
	for _, item := range result.Evidence {
		if item.Kind == "check" && item.ExitCode != 0 {
			return item.ID
		}
	}
	t.Fatal("missing failed check evidence")
	return ""
}

func TestWorkflowRetryVerificationFailingAgainStaysRetryableAndNeverFreezes(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	args := []string{"--id", "wf", "--request", "change a", "--accept", "test \"$(cat a.txt)\" = new", "--check", "false", "--check", "test -f a.txt", "--path", "a.txt", "--opencode", fake, "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var runOut bytes.Buffer
	if err := workflowRun(store, repo, []string{"wf"}, &runOut); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(runOut.String(), "candidate frozen") {
		t.Fatalf("failing verification reported a frozen candidate: %q", runOut.String())
	}
	firstFailed := failedCheckEvidenceID(t, store)

	err := workflowRetryVerification(store, []string{"--id", "wf", "--check-id", firstFailed, "--replacement", "test -f absent", "--actor", "tester", "--reason", "wrong path", "--idempotency-key", "retry:1"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("failing replacement reported success")
	}
	w, _ := store.Get("wf")
	if w.State != workflow.StateReplanRequired {
		t.Fatalf("state=%s", w.State)
	}
	if err = workflowRetryVerification(store, []string{"--id", "wf", "--check-id", firstFailed, "--replacement", "test -f absent", "--actor", "tester", "--reason", "wrong path", "--idempotency-key", "retry:1"}, &bytes.Buffer{}); err == nil {
		t.Fatal("idempotent replay of a failed retry reported success")
	}

	secondFailed := failedCheckEvidenceID(t, store)
	var out bytes.Buffer
	if err = workflowRetryVerification(store, []string{"--id", "wf", "--check-id", secondFailed, "--replacement", "true", "--actor", "tester", "--reason", "environment fixed", "--idempotency-key", "retry:2"}, &out); err != nil {
		t.Fatalf("second retry failed: %v", err)
	}
	if w, _ = store.Get("wf"); w.State != workflow.StateCandidateFrozen {
		t.Fatalf("state=%s out=%q", w.State, out.String())
	}
	var revisions int
	if err = store.Database().QueryRow(`SELECT COUNT(*) FROM verification_contract_revisions WHERE workflow_id='wf'`).Scan(&revisions); err != nil || revisions != 2 {
		t.Fatalf("revisions=%d err=%v", revisions, err)
	}
	var invocations int
	if err = store.Database().QueryRow(`SELECT COUNT(*) FROM opencode_invocations WHERE workflow_id='wf'`).Scan(&invocations); err != nil || invocations != 1 {
		t.Fatalf("coder reran: invocations=%d err=%v", invocations, err)
	}
}

func TestWorkflowAgentDefaultsToDedicatedPrimaryAndPreservesExplicitAgent(t *testing.T) {
	if got := workflowAgent(nil); got != "workflow-worker" {
		t.Fatalf("default agent=%q", got)
	}
	if got := workflowAgent([]string{"--agent", "explicit-primary"}); got != "explicit-primary" {
		t.Fatalf("explicit agent=%q", got)
	}
}

func TestWorkflowRunPreflightFailureDoesNotCreateAttemptLeaseOrJob(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "change", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	old := workflowRuntimePreflight
	defer func() { workflowRuntimePreflight = old }()
	workflowRuntimePreflight = workflow.RuntimePreflight{LookPath: func(string) (string, error) { return "", errors.New("absent") }}
	err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{})
	var preflightErr *workflow.RuntimePreflightError
	if !errors.As(err, &preflightErr) || preflightErr.Code != "opencode_unavailable" {
		t.Fatalf("err=%v", err)
	}
	err = workflowRun(store, repo, []string{"wf", "--detach"}, &bytes.Buffer{})
	if !errors.As(err, &preflightErr) || preflightErr.Code != "opencode_unavailable" {
		t.Fatalf("detached err=%v", err)
	}
	w, _ := store.Get("wf")
	if w.State != workflow.StateReady {
		t.Fatalf("state=%s", w.State)
	}
	for _, table := range []string{"mutation_attempts", "leases", "workflow_jobs", "invocation_runtime"} {
		var count int
		if err := store.Database().QueryRow("SELECT COUNT(*) FROM " + table + " WHERE workflow_id='wf'").Scan(&count); err != nil && table != "leases" {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s=%d", table, count)
		}
	}
}

func TestWorkflowReviewPreflightFailureKeepsCandidateFrozenWithoutInvocation(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "change", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, next := range []workflow.State{workflow.StateExecuting, workflow.StateVerifying, workflow.StateCandidateFrozen} {
		current, _ := store.Get("wf")
		if _, err := store.Transition(workflow.Transition{WorkflowID: "wf", ExpectedState: current.State, ExpectedVersion: current.StateVersion, NextState: next, IdempotencyKey: "test:" + string(next)}); err != nil {
			t.Fatal(err)
		}
	}
	old := workflowRuntimePreflight
	defer func() { workflowRuntimePreflight = old }()
	workflowRuntimePreflight = workflow.RuntimePreflight{LookPath: func(string) (string, error) { return "", errors.New("absent") }}
	err := workflowReview(store, []string{"--id", "wf"}, &bytes.Buffer{})
	var preflightErr *workflow.RuntimePreflightError
	if !errors.As(err, &preflightErr) || preflightErr.Code != "opencode_unavailable" {
		t.Fatalf("err=%v", err)
	}
	w, _ := store.Get("wf")
	if w.State != workflow.StateCandidateFrozen {
		t.Fatalf("state=%s", w.State)
	}
	var count int
	if err := store.Database().QueryRow(`SELECT COUNT(*) FROM review_invocations WHERE workflow_id='wf'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invocations=%d err=%v", count, err)
	}
}

func TestWorkflowRunLegacyInputWithoutDeclaredTransportFailsClosed(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "change", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id='wf'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "ResultTransport")
	raw, _ = json.Marshal(legacy)
	if _, err := store.Database().Exec(`UPDATE workflow_run_inputs SET input=? WHERE workflow_id='wf'`, raw); err != nil {
		t.Fatal(err)
	}
	err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{})
	var preflightErr *workflow.RuntimePreflightError
	if !errors.As(err, &preflightErr) || preflightErr.Code != "result_transport_undeclared" {
		t.Fatalf("err=%v", err)
	}
	w, _ := store.Get("wf")
	if w.State != workflow.StateReady {
		t.Fatalf("state=%s", w.State)
	}
}

func TestWorkflowStartInfersPlannedMediumForSensitiveRequests(t *testing.T) {
	requests := []string{
		"rotate the API keys used by authentication",
		"install an external payment SDK dependency",
		"add a database migration for payments",
	}
	for i, request := range requests {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			repo, store := cliWorkflowRepo(t)
			defer store.Close()
			if err := workflowStart(store, repo, []string{"--id", "wf", "--request", request, "--accept", "true", "--check", "true", "--path", "a.txt"}, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			w, err := store.Get("wf")
			if err != nil {
				t.Fatal(err)
			}
			if w.Route != workflow.RoutePlanned || w.MinimumRisk != workflow.RiskMedium {
				t.Fatalf("route=%s risk=%s", w.Route, w.MinimumRisk)
			}
		})
	}
}

func TestWorkflowStartSensitiveRequestRejectsExplicitSimpleDowngrade(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	err := workflowStart(store, repo, []string{"--id", "wf", "--request", "change authentication token handling", "--route", "simple", "--override-actor", "tester", "--override-reason", "force simple", "--accept", "true", "--check", "true", "--path", "a.txt"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "cannot use simple route") {
		t.Fatalf("err=%v", err)
	}
	if _, getErr := store.Get("wf"); getErr == nil {
		t.Fatal("rejected downgrade created workflow state")
	}
}

func TestWorkflowStartBenignRequestRemainsSimpleLow(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "rename the local heading", "--accept", "true", "--check", "true", "--path", "a.txt"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	w, err := store.Get("wf")
	if err != nil {
		t.Fatal(err)
	}
	if w.Route != workflow.RouteSimple || w.MinimumRisk != workflow.RiskLow {
		t.Fatalf("route=%s risk=%s", w.Route, w.MinimumRisk)
	}
}

func TestWorkflowRunDetachQueuesWorkerAndReturnsImmediately(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	args := []string{"--id", "wf", "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	oldExe, oldStart := detachedWorkerExecutable, detachedWorkerStart
	defer func() { detachedWorkerExecutable, detachedWorkerStart = oldExe, oldStart }()
	detachedWorkerExecutable = func() (string, error) { return "/tmp/skynex-test", nil }
	detachedWorkerStart = func(executable, directory, id, jobID, stateDir string) (int, error) {
		if executable != "/tmp/skynex-test" || directory != repo || id != "wf" || jobID == "" {
			t.Fatalf("spawn args %q %q %q %q", executable, directory, id, jobID)
		}
		return 4321, nil
	}
	t.Setenv("SKYNEX_OPENCODE_SESSION_ID", "session-1")
	var out bytes.Buffer
	if err := workflowRun(store, repo, []string{"wf", "--detach"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "queued\tpid=4321") {
		t.Fatalf("out=%q", out.String())
	}
	var state, session string
	if err := store.Database().QueryRow(`SELECT state,session_id FROM workflow_jobs WHERE workflow_id='wf'`).Scan(&state, &session); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || session != "session-1" {
		t.Fatalf("state=%s session=%s", state, session)
	}
}

func TestWorkflowDetachRejectsUnsupportedPlatformBeforeCreatingJob(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	oldCapability, oldStart := detachedWorkflowCapability, detachedWorkerStart
	defer func() { detachedWorkflowCapability, detachedWorkerStart = oldCapability, oldStart }()
	detachedWorkflowCapability = func() error {
		return errors.New("detached workflow execution is not supported on Windows; run without --detach")
	}
	started := false
	detachedWorkerStart = func(_, _, _, _, _ string) (int, error) {
		started = true
		return 123, nil
	}
	err := workflowRun(store, repo, []string{"wf", "--detach"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "run without --detach") {
		t.Fatalf("detach err=%v", err)
	}
	if started {
		t.Fatal("unsupported detach started a worker")
	}
	var jobs int
	if err := store.Database().QueryRow(`SELECT COUNT(*) FROM workflow_jobs WHERE workflow_id='wf'`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("jobs=%d err=%v", jobs, err)
	}
}

func TestWorkflowReviewDetachQueuesDurableReviewOperation(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "review detached", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--model", "fake", "--timeout", "2s"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	oldExe, oldStart := detachedWorkerExecutable, detachedWorkerStart
	defer func() { detachedWorkerExecutable, detachedWorkerStart = oldExe, oldStart }()
	detachedWorkerExecutable = func() (string, error) { return "/tmp/skynex-test", nil }
	detachedWorkerStart = func(executable, directory, id, jobID, stateDir string) (int, error) {
		if directory != repo || id != "wf" || jobID == "" {
			t.Fatalf("spawn=%q %q %q", directory, id, jobID)
		}
		return 4322, nil
	}
	var out bytes.Buffer
	if err := workflowReview(store, []string{"--id", "wf", "--detach"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "review\tqueued\tpid=4322") {
		t.Fatalf("out=%q", out.String())
	}
	var operation, state string
	if err := store.Database().QueryRow(`SELECT operation,state FROM workflow_jobs WHERE workflow_id='wf'`).Scan(&operation, &state); err != nil || operation != "review" || state != "queued" {
		t.Fatalf("operation=%q state=%q err=%v", operation, state, err)
	}
	if _, err := store.CreateWorkflowJobOperation("second", "wf", "run", time.Now()); err == nil {
		t.Fatal("cross-operation live job uniqueness was bypassed")
	}
}

func TestWorkflowWorkerDispatchesPersistedReviewOperation(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := filepath.Join(t.TempDir(), "opencode")
	body := `#!/bin/sh
set -eu
case "$*" in *"Assess risk"*) printf '{"requested_risk":"low","selected_lens":"","justification":"low"}' > "$SKYNEX_RESULT_FILE"; exit 0;; esac
tree=$(git write-tree)
printf '{"envelope":{"WorkflowID":"wf","NodeID":"slice_main","AttemptID":"wf:slice_main","BaseCandidateOID":"%s","Status":"completed"},"patch":{"Operations":[{"Path":"a.txt","Data":"bmV3Cg==","Mode":384}]}}' "$tree" > "$SKYNEX_RESULT_FILE"
`
	if err := os.WriteFile(fake, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "worker review", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--model", "fake", "--timeout", "2s"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkflowJobOperation("job-review", "wf", "review", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := workflowWorker(store, repo, []string{"wf", "--job", "job-review"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	w, _ := store.Get("wf")
	if w.State != workflow.StateReceipted {
		t.Fatalf("state=%s", w.State)
	}
	job, err := store.WorkflowJob("job-review")
	if err != nil || job.Operation != "review" || job.State != workflow.JobSucceeded {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}

func TestAbortDoesNotSignalStaleDetachedPID(t *testing.T) {
	_, store := cliWorkflowRepo(t)
	defer store.Close()
	if _, err := store.Create(workflow.Workflow{ID: "stale", Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkflowJob("job-stale", "stale", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.StartWorkflowJob("job-stale", 4242, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := store.Database().Exec(`INSERT INTO invocation_runtime(invocation_id,workflow_id,attempt_id,status,pid,started_at,heartbeat_at) VALUES('inv-stale','stale','attempt','running',99,?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	old := signalDetachedJob
	defer func() { signalDetachedJob = old }()
	signals := 0
	signalDetachedJob = func(int) error { signals++; return nil }
	if err := cleanupAbortedWorkflow(store, "stale"); err != nil {
		t.Fatal(err)
	}
	if signals != 0 {
		t.Fatalf("signaled stale/reused pid %d times", signals)
	}
	var runtimeStatus string
	if err := store.Database().QueryRow(`SELECT status FROM invocation_runtime WHERE invocation_id='inv-stale'`).Scan(&runtimeStatus); err != nil || runtimeStatus != "cancelled" {
		t.Fatalf("runtime=%q err=%v", runtimeStatus, err)
	}
}

func TestDetachAcceptsFastChildTerminalBeforeParentPersistsPID(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "change", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	oldExe, oldStart, oldSignal := detachedWorkerExecutable, detachedWorkerStart, signalDetachedJob
	defer func() { detachedWorkerExecutable, detachedWorkerStart, signalDetachedJob = oldExe, oldStart, oldSignal }()
	detachedWorkerExecutable = func() (string, error) { return "/tmp/test", nil }
	signaled := 0
	signalDetachedJob = func(int) error { signaled++; return nil }
	detachedWorkerStart = func(_, _, _, jobID, _ string) (int, error) {
		if err := store.StartWorkflowJob(jobID, 4242, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishWorkflowJob(jobID, workflow.JobFailed, "", "simulated worker exit", time.Now()); err != nil {
			t.Fatal(err)
		}
		return 4242, nil
	}
	if err := workflowRun(store, repo, []string{"wf", "--detach"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("fast child terminal must be accepted: %v", err)
	}
	var live int
	if err := store.Database().QueryRow(`SELECT COUNT(*) FROM workflow_jobs WHERE workflow_id='wf' AND state IN ('queued','running','cancel_requested')`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("live=%d err=%v", live, err)
	}
	if signaled != 0 {
		t.Fatalf("signals=%d", signaled)
	}
}

func TestWorkflowRunInvalidOutputThenResume(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	marker := filepath.Join(t.TempDir(), "first")
	prefix := "if [ ! -f '" + marker + "' ]; then touch '" + marker + "'; echo bad > \"$SKYNEX_RESULT_FILE\"; exit 0; fi"
	fake := cliFakeOpenCode(t, prefix)
	args := []string{"--id", "wf", "--request", "change a", "--accept", "test \"$(cat a.txt)\" = new", "--check", "true", "--path", "a.txt", "--opencode", fake, "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); !errors.Is(err, execution.ErrMalformedWorkerResult) {
		t.Fatalf("first err=%v", err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("resume err=%v", err)
	}
}

func TestWorkflowRunTimeout(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "sleep 2")
	args := []string{"--id", "wf", "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--timeout", "20ms"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err == nil || time.Since(started) > time.Second {
		t.Fatalf("timeout err=%v elapsed=%s", err, time.Since(started))
	}
}

func TestWorkflowRunWaitsForLeaseBeforeActivatingSlice(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "change", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--timeout", "1s"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var inputRaw []byte
	if err := store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id='wf'`).Scan(&inputRaw); err != nil {
		t.Fatal(err)
	}
	var input workflowRunInput
	if err := json.Unmarshal(inputRaw, &input); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := store.AcquireLease("worktree:"+input.Seal.WorktreeID, "other", "other-token", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := workflowRunContext(ctx, store, repo, []string{"wf"}, &bytes.Buffer{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run error=%v", err)
	}
	w, err := store.Get("wf")
	if err != nil {
		t.Fatal(err)
	}
	if w.State != workflow.StateReady {
		t.Fatalf("state=%s", w.State)
	}
	var active, live int
	_ = store.Database().QueryRow(`SELECT COUNT(*) FROM execution_slice_state WHERE workflow_id='wf' AND status='active'`).Scan(&active)
	_ = store.Database().QueryRow(`SELECT COUNT(*) FROM mutation_attempts WHERE workflow_id='wf' AND live=1`).Scan(&live)
	if active != 0 || live != 0 {
		t.Fatalf("active=%d live=%d", active, live)
	}
}

func TestConcurrentWorkflowsSharingWorktreeWaitInsteadOfFailingLease(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	for _, id := range []string{"wf-one", "wf-two"} {
		fake := filepath.Join(t.TempDir(), "opencode")
		body := "#!/bin/sh\nset -eu\nsleep 0.15\ntree=$(git write-tree)\nprintf '{\"envelope\":{\"WorkflowID\":\"" + id + "\",\"NodeID\":\"slice_main\",\"AttemptID\":\"" + id + ":slice_main\",\"BaseCandidateOID\":\"%s\",\"Status\":\"completed\",\"EvidenceIDs\":[\"fake\"]},\"patch\":{\"Operations\":[{\"Path\":\"a.txt\",\"Data\":\"bmV3Cg==\",\"Mode\":384}]}}' \"$tree\" > \"$SKYNEX_RESULT_FILE\"\n"
		if err := os.WriteFile(fake, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := workflowStart(store, repo, []string{"--id", id, "--request", "change", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--timeout", "2s"}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, id := range []string{"wf-one", "wf-two"} {
		id := id
		go func() { <-start; errs <- workflowRun(store, repo, []string{id}, &bytes.Buffer{}) }()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent run: %v", err)
		}
	}
	for _, id := range []string{"wf-one", "wf-two"} {
		w, err := store.Get(id)
		if err != nil || w.State != workflow.StateCandidateFrozen {
			t.Fatalf("%s state=%s err=%v", id, w.State, err)
		}
		var active, live int
		_ = store.Database().QueryRow(`SELECT COUNT(*) FROM execution_slice_state WHERE workflow_id=? AND status='active'`, id).Scan(&active)
		_ = store.Database().QueryRow(`SELECT COUNT(*) FROM mutation_attempts WHERE workflow_id=? AND live=1`, id).Scan(&live)
		if active != 0 || live != 0 {
			t.Fatalf("%s active=%d live=%d", id, active, live)
		}
	}
}

func TestWorkflowRunUsesPersistedSealAndRejectsMovedHEAD(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	args := []string{"--id", "wf", "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--timeout", "1s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("move head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", repo, "add", "other.txt"}, {"-C", repo, "commit", "-m", "move"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err == nil {
		t.Fatal("run resealed moved HEAD instead of rejecting context drift")
	}
	var attempts int
	if err := store.Database().QueryRow(`SELECT COUNT(*) FROM mutation_attempts WHERE workflow_id='wf'`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestWorkflowAttemptsUseDistinctRandomFencingTokens(t *testing.T) {
	var tokens []string
	for _, id := range []string{"wf-a", "wf-b"} {
		repo, store := cliWorkflowRepo(t)
		fake := filepath.Join(t.TempDir(), "bad-opencode")
		if err := os.WriteFile(fake, []byte("#!/bin/sh\necho bad > \"$SKYNEX_RESULT_FILE\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		args := []string{"--id", id, "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--timeout", "1s"}
		if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		_ = workflowRun(store, repo, []string{id}, &bytes.Buffer{})
		var token string
		if err := store.Database().QueryRow(`SELECT fencing_token FROM mutation_attempts WHERE workflow_id=?`, id).Scan(&token); err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
		store.Close()
	}
	if tokens[0] == tokens[1] || len(tokens[0]) != 64 || len(tokens[1]) != 64 || strings.HasPrefix(tokens[0], "lease:") {
		t.Fatalf("tokens are not random and distinct: %q %q", tokens[0], tokens[1])
	}
}

func TestWorkflowOpenCodeReviewDepthsAndReplay(t *testing.T) {
	for _, tc := range []struct {
		risk        string
		lens        string
		invocations int
	}{{"low", "", 1}, {"medium", "reliability", 2}, {"high", "", 5}} {
		t.Run(tc.risk, func(t *testing.T) {
			repo, store := cliWorkflowRepo(t)
			defer store.Close()
			fake := filepath.Join(t.TempDir(), "opencode")
			body := `#!/bin/sh
set -eu
case "$*" in
  *"--agent workflow-reviewer"*"Assess risk"*"requested_risk"*"selected_lens"*) printf '{"requested_risk":"` + tc.risk + `","selected_lens":"` + tc.lens + `","justification":"fake assessment"}' > "$SKYNEX_RESULT_FILE"; exit 0 ;;
  *"--agent workflow-reviewer"*"Review lens"*"findings"*) printf '{"findings":[]}' > "$SKYNEX_RESULT_FILE"; exit 0 ;;
esac
tree=$(git write-tree)
printf '{"envelope":{"WorkflowID":"wf","NodeID":"slice_main","AttemptID":"wf:slice_main","BaseCandidateOID":"%s","Status":"completed"},"patch":{"Operations":[{"Path":"a.txt","Data":"bmV3Cg==","Mode":384}]}}' "$tree" > "$SKYNEX_RESULT_FILE"
`
			if err := os.WriteFile(fake, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			args := []string{"--id", "wf", "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--model", "fake", "--timeout", "2s"}
			if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if tc.risk == "high" {
				if err := workflowApprove(store, []string{"--id", "wf", "--action", "review", "--actor", "tester", "--reason", "high review"}, &bytes.Buffer{}); err != nil {
					t.Fatal(err)
				}
			}
			if err := workflowReview(store, []string{"--id", "wf"}, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := store.Database().QueryRow(`SELECT COUNT(*) FROM review_invocations WHERE workflow_id='wf'`).Scan(&count); err != nil || count != tc.invocations {
				t.Fatalf("invocations=%d err=%v", count, err)
			}
			if err := workflowReview(store, []string{"--id", "wf"}, &bytes.Buffer{}); err != nil {
				t.Fatalf("replay=%v", err)
			}
			if err := store.Database().QueryRow(`SELECT COUNT(*) FROM review_invocations WHERE workflow_id='wf'`).Scan(&count); err != nil || count != tc.invocations {
				t.Fatalf("replay invocations=%d err=%v", count, err)
			}
		})
	}
}

func TestReviewFindingIDsAreNamespacedAcrossWorkflows(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	for _, id := range []string{"wf-one", "wf-two"} {
		data := "b25lCg=="
		if id == "wf-two" {
			data = "dHdvCg=="
		}
		fake := filepath.Join(t.TempDir(), "opencode")
		body := `#!/bin/sh
set -eu
case "$*" in
  *"Assess risk"*) printf '{"requested_risk":"medium","selected_lens":"reliability","justification":"needs lens"}' > "$SKYNEX_RESULT_FILE"; exit 0 ;;
  *"Review lens"*) printf '{"findings":[{"id":"REL-001","severity":"warning","message":"shared model label","reproducible":true,"candidate_caused":true,"evidence_ids":[]}]}' > "$SKYNEX_RESULT_FILE"; exit 0 ;;
esac
tree=$(git write-tree)
printf '{"envelope":{"WorkflowID":"` + id + `","NodeID":"slice_main","AttemptID":"` + id + `:slice_main","BaseCandidateOID":"%s","Status":"completed"},"patch":{"Operations":[{"Path":"a.txt","Data":"` + data + `","Mode":384}]}}' "$tree" > "$SKYNEX_RESULT_FILE"
`
		if err := os.WriteFile(fake, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		args := []string{"--id", id, "--request", "review identity", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--model", "fake", "--timeout", "2s"}
		if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if err := workflowRun(store, repo, []string{id}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if err := workflowReview(store, []string{"--id", id}, &bytes.Buffer{}); err != nil {
			t.Fatalf("%s review: %v", id, err)
		}
		w, _ := store.Get(id)
		if w.State != workflow.StateReceipted {
			t.Fatalf("%s state=%s", id, w.State)
		}
		if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.Database().Query(`SELECT id,workflow_id,finding FROM review_findings ORDER BY workflow_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]string{}
	for rows.Next() {
		var id, workflowID string
		var raw []byte
		if err := rows.Scan(&id, &workflowID, &raw); err != nil {
			t.Fatal(err)
		}
		var finding review.Finding
		if err := json.Unmarshal(raw, &finding); err != nil {
			t.Fatal(err)
		}
		if finding.SourceID != "REL-001" || !strings.Contains(id, workflowID) {
			t.Fatalf("id=%q workflow=%q finding=%+v", id, workflowID, finding)
		}
		seen[workflowID] = id
	}
	if len(seen) != 2 || seen["wf-one"] == seen["wf-two"] {
		t.Fatalf("finding identities=%v", seen)
	}
	var evidence int
	if err := store.Database().QueryRow(`SELECT COUNT(*) FROM review_evidence WHERE id LIKE 'evidence:finding:%'`).Scan(&evidence); err != nil || evidence != 2 {
		t.Fatalf("evidence=%d err=%v", evidence, err)
	}
}

func TestReviewResumeReusesCompletedCheckpointsAndRunsOnlyMissingLens(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := filepath.Join(t.TempDir(), "opencode")
	counts := t.TempDir()
	body := `#!/bin/sh
set -eu
kind=execution
case "$*" in
  *"Assess risk"*) kind=semantic ;;
  *"Review lens risk"*) kind=risk ;;
  *"Review lens readability"*) kind=readability ;;
  *"Review lens reliability"*) kind=reliability ;;
  *"Review lens resilience"*) kind=resilience ;;
esac
if [ "$kind" != execution ]; then
  n=0; [ ! -f "` + counts + `/count-$kind" ] || n=$(cat "` + counts + `/count-$kind")
  echo $((n+1)) > "` + counts + `/count-$kind"
  if [ "$kind" = resilience ] && [ ! -f "` + counts + `/failed-once" ]; then touch "` + counts + `/failed-once"; echo transient >&2; exit 1; fi
  if [ "$kind" = semantic ]; then printf '{"requested_risk":"high","selected_lens":"","justification":"high"}' > "$SKYNEX_RESULT_FILE"; else printf '{"findings":[]}' > "$SKYNEX_RESULT_FILE"; fi
  exit 0
fi
tree=$(git write-tree)
printf '{"envelope":{"WorkflowID":"wf","NodeID":"slice_main","AttemptID":"wf:slice_main","BaseCandidateOID":"%s","Status":"completed"},"patch":{"Operations":[{"Path":"a.txt","Data":"bmV3Cg==","Mode":384}]}}' "$tree" > "$SKYNEX_RESULT_FILE"
`
	if err := os.WriteFile(fake, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "resume review", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--model", "fake", "--timeout", "2s"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowApprove(store, []string{"--id", "wf", "--action", "review", "--actor", "tester", "--reason", "high review"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowReview(store, []string{"--id", "wf"}, &bytes.Buffer{}); err == nil {
		t.Fatal("first review must fail at resilience")
	}
	if err := workflowReview(store, []string{"--id", "wf"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("resume review: %v", err)
	}
	for _, kind := range []string{"semantic", "risk", "readability", "reliability", "resilience"} {
		raw, err := os.ReadFile(filepath.Join(counts, "count-"+kind))
		if err != nil {
			t.Fatal(err)
		}
		want := "1\n"
		if kind == "resilience" {
			want = "2\n"
		}
		if string(raw) != want {
			t.Fatalf("%s calls=%q want=%q", kind, raw, want)
		}
	}
}

func TestReviewIdleWatchdogPersistsObservableTerminalState(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "stall review", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--model", "fake", "--timeout", "2s"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nset -eu\necho step_start\nsleep 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := review.OpenCodeReviewRunner{Store: store, Options: review.OpenCodeReviewOptions{Executable: fake, Model: "fake", Timeout: 2 * time.Second, IdleTimeout: 60 * time.Millisecond}}
	started := time.Now()
	if _, err := runner.Run(context.Background(), "wf"); !errors.Is(err, review.ErrReviewIdleTimeout) {
		t.Fatalf("idle err=%v", err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("idle watchdog slow: %s", time.Since(started))
	}
	var status, preview, heartbeat, activity string
	var pid int
	if err := store.Database().QueryRow(`SELECT status,error_preview,pid,heartbeat_at,last_activity_at FROM review_invocations WHERE workflow_id='wf' AND lens='semantic'`).Scan(&status, &preview, &pid, &heartbeat, &activity); err != nil || status != "idle_timeout" || pid <= 0 || heartbeat == "" || activity == "" || !strings.Contains(preview, "step_start") {
		t.Fatalf("status=%q preview=%q pid=%d heartbeat=%q activity=%q err=%v", status, preview, pid, heartbeat, activity, err)
	}
}

func TestRunningReviewIsObservableAndAbortable(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	if err := workflowStart(store, repo, []string{"--id", "wf", "--request", "abort review", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--model", "fake", "--timeout", "3s"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nset -eu\necho reviewing\nsleep 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := review.OpenCodeReviewRunner{Store: store, Options: review.OpenCodeReviewOptions{Executable: fake, Model: "fake", Timeout: 3 * time.Second}}
	done := make(chan error, 1)
	go func() { _, err := runner.Run(context.Background(), "wf"); done <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		var status string
		var pid int
		err := store.Database().QueryRow(`SELECT status,pid FROM review_invocations WHERE workflow_id='wf' AND lens='semantic'`).Scan(&status, &pid)
		if err == nil && status == "running" && pid > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("review not observable status=%q pid=%d err=%v", status, pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(180 * time.Millisecond)
	var preview string
	if err := store.Database().QueryRow(`SELECT error_preview FROM review_invocations WHERE workflow_id='wf' AND lens='semantic'`).Scan(&preview); err != nil || !strings.Contains(preview, "reviewing") {
		t.Fatalf("live preview=%q err=%v", preview, err)
	}
	if err := cleanupAbortedWorkflow(store, "wf"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("abort err=%v", err)
	}
	var status string
	if err := store.Database().QueryRow(`SELECT status FROM review_invocations WHERE workflow_id='wf' AND lens='semantic'`).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestWorkflowOpenCodeReviewRejectsBadResultsAndDrift(t *testing.T) {
	for _, tc := range []struct {
		name, reviewScript string
		drift              bool
	}{
		{"lowering", `printf '{"requested_risk":"","justification":"lower"}' > "$SKYNEX_RESULT_FILE"`, false},
		{"malformed", `echo bad > "$SKYNEX_RESULT_FILE"`, false},
		{"review-mutation", `chmod u+w a.txt; printf 'tampered\n' > a.txt; printf '{"requested_risk":"low","selected_lens":"","justification":"looks valid"}' > "$SKYNEX_RESULT_FILE"`, false},
		{"lens-mutation", `case "$*" in *"Assess risk"*) printf '{"requested_risk":"medium","selected_lens":"risk","justification":"inspect risk"}' ;; *) chmod u+w a.txt; printf 'tampered by lens\n' > a.txt; printf '{"findings":[]}' ;; esac > "$SKYNEX_RESULT_FILE"`, false},
		{"timeout", `sleep 2`, false},
		{"severe", `case "$*" in *"Assess risk"*) printf '{"requested_risk":"high","justification":"high"}' ;; *) printf '{"findings":[{"severity":"severe","message":"boom","reproducible":true,"candidate_caused":true}]}' ;; esac > "$SKYNEX_RESULT_FILE"`, false},
		{"drift", `printf '{"requested_risk":"low","justification":"ok"}' > "$SKYNEX_RESULT_FILE"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, store := cliWorkflowRepo(t)
			defer store.Close()
			fake := cliFakeOpenCode(t, "true")
			args := []string{"--id", "wf", "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--model", "fake", "--timeout", "2s"}
			if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			// The timeout case targets the review subprocess specifically. Keep
			// execution admission/materialization independent from the deliberately
			// tiny review deadline so race instrumentation or host load cannot make
			// setup fail before the behavior under test is reached.
			if tc.name == "timeout" {
				var raw []byte
				if err := store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id='wf'`).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var input workflowRunInput
				if err := json.Unmarshal(raw, &input); err != nil {
					t.Fatal(err)
				}
				input.Timeout = 30 * time.Millisecond
				raw, _ = json.Marshal(input)
				if _, err := store.Database().Exec(`UPDATE workflow_run_inputs SET input=? WHERE workflow_id='wf'`, raw); err != nil {
					t.Fatal(err)
				}
			}
			if tc.drift {
				_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("drift\n"), 0o600)
			}
			if tc.name == "severe" {
				if err := workflowApprove(store, []string{"--id", "wf", "--action", "review", "--actor", "tester", "--reason", "severe review"}, &bytes.Buffer{}); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(fake, []byte("#!/bin/sh\nset -eu\n"+tc.reviewScript+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			reviewErr := workflowReview(store, []string{"--id", "wf"}, &bytes.Buffer{})
			if reviewErr == nil {
				t.Fatal("review unexpectedly succeeded")
			}
			if tc.name == "timeout" && !errors.Is(reviewErr, context.DeadlineExceeded) {
				t.Fatalf("review timeout error=%v", reviewErr)
			}
			if tc.name == "malformed" {
				var status, preview string
				if err := store.Database().QueryRow(`SELECT status,error_preview FROM review_invocations WHERE workflow_id='wf' AND lens='semantic'`).Scan(&status, &preview); err != nil || status != "malformed" || !strings.Contains(preview, "invalid JSON") || !strings.Contains(preview, "bad") {
					t.Fatalf("malformed diagnostic status=%q preview=%q err=%v", status, preview, err)
				}
				var out bytes.Buffer
				if err := workflowInspect(store, review.NewSQLiteStore(store.Database()), "wf", &out); err != nil || !strings.Contains(out.String(), `"status": "malformed"`) || !strings.Contains(out.String(), "invalid JSON") {
					t.Fatalf("inspect malformed=%q err=%v", out.String(), err)
				}
			}
			if tc.name == "review-mutation" || tc.name == "lens-mutation" {
				if !errors.Is(reviewErr, review.ErrCandidateMismatch) {
					t.Fatalf("mutation error=%v want ErrCandidateMismatch", reviewErr)
				}
				if _, err := review.NewSQLiteStore(store.Database()).Authority("wf"); err == nil {
					t.Fatal("mutated review issued authoritative receipt")
				}
			}
			if tc.name == "severe" || tc.name == "drift" {
				w, _ := store.Get("wf")
				if w.State != workflow.StateReplanRequired {
					t.Fatalf("state=%s", w.State)
				}
			}
		})
	}
}

func TestWorkflowReviewOmitsEmptyModelAndReportsRedactedFailure(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := cliFakeOpenCode(t, "true")
	argsFile := filepath.Join(t.TempDir(), "args")
	args := []string{"--id", "wf", "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nprintf 'token=supersecret review exploded' >&2\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	err := workflowReview(store, []string{"--id", "wf"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("review unexpectedly succeeded")
	}
	if got := err.Error(); !strings.Contains(got, "review exploded") || strings.Contains(got, "supersecret") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("unsafe or opaque error: %q", got)
	}
	invoked, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(invoked), "--model") || strings.Contains(string(invoked), "default") {
		t.Fatalf("empty model was materialized: %q", invoked)
	}
	var inspected workflowInspection
	var out bytes.Buffer
	if err := workflowInspect(store, review.NewSQLiteStore(store.Database()), "wf", &out); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if len(inspected.ReviewInvocations) != 1 || inspected.ReviewInvocations[0].Status != "failed" || inspected.ReviewInvocations[0].Model != "opencode-provider-default" || !strings.Contains(inspected.ReviewInvocations[0].ErrorPreview, "[REDACTED]") {
		t.Fatalf("review invocation not inspectable: %+v", inspected.ReviewInvocations)
	}
}

func TestWorkflowReviewUsesAuditableDefaultModelIdentity(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	fake := filepath.Join(t.TempDir(), "opencode")
	body := `#!/bin/sh
set -eu
case "$*" in
  *"Assess risk"*) printf '{"requested_risk":"low","selected_lens":"","justification":"low risk"}' > "$SKYNEX_RESULT_FILE"; exit 0 ;;
esac
tree=$(git write-tree)
printf '{"envelope":{"WorkflowID":"wf","NodeID":"slice_main","AttemptID":"wf:slice_main","BaseCandidateOID":"%s","Status":"completed"},"patch":{"Operations":[{"Path":"a.txt","Data":"bmV3Cg==","Mode":384}]}}' "$tree" > "$SKYNEX_RESULT_FILE"
`
	if err := os.WriteFile(fake, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"--id", "wf", "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowReview(store, []string{"--id", "wf"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("review with provider-selected model failed: %v", err)
	}
	if _, err := review.NewSQLiteStore(store.Database()).Authority("wf"); err != nil {
		t.Fatal(err)
	}
	var model string
	if err := store.Database().QueryRow(`SELECT model FROM review_invocations WHERE workflow_id='wf' AND lens='semantic'`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "opencode-provider-default" {
		t.Fatalf("model identity=%q", model)
	}
}

func prepareReceiptedWorkflow(t *testing.T) (string, *workflow.SQLiteStore) {
	t.Helper()
	repo, store := cliWorkflowRepo(t)
	fake := filepath.Join(t.TempDir(), "opencode")
	body := `#!/bin/sh
set -eu
case "$*" in
  *"Assess risk"*) printf '{"requested_risk":"low","justification":"low"}' > "$SKYNEX_RESULT_FILE"; exit 0 ;;
esac
tree=$(git write-tree)
printf '{"envelope":{"WorkflowID":"wf","NodeID":"slice_main","AttemptID":"wf:slice_main","BaseCandidateOID":"%s","Status":"completed"},"patch":{"Operations":[{"Path":"a.txt","Data":"bmV3Cg==","Mode":384}]}}' "$tree" > "$SKYNEX_RESULT_FILE"
`
	if err := os.WriteFile(fake, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"--id", "wf", "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--model", "fake", "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowReview(store, []string{"--id", "wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	return repo, store
}

func TestWorkflowDeliverExactTreeAndCrashReplay(t *testing.T) {
	repo, store := prepareReceiptedWorkflow(t)
	defer store.Close()
	crash := errors.New("crash after ref update")
	args := []string{"--id", "wf", "--message", "deliver exact tree", "--idempotency-key", "delivery-1", "--author-name", "Test Author", "--author-email", "author@example.com"}
	if err := workflowDeliver(store, args, &bytes.Buffer{}, func() error { return crash }); !errors.Is(err, crash) {
		t.Fatalf("crash=%v", err)
	}
	first, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	w, _ := store.Get("wf")
	if w.State != workflow.StateReceipted {
		t.Fatalf("state after crash=%s", w.State)
	}
	if err := workflowDeliver(store, args, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("replay=%v", err)
	}
	second, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if string(first) != string(second) {
		t.Fatalf("second commit created: %s != %s", first, second)
	}
	var candidateTree string
	if err := store.Database().QueryRow(`SELECT tree_oid FROM review_candidates WHERE workflow_id='wf'`).Scan(&candidateTree); err != nil {
		t.Fatal(err)
	}
	commitTree, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD^{tree}").Output()
	if strings.TrimSpace(string(commitTree)) != candidateTree {
		t.Fatalf("commit tree=%s candidate=%s", commitTree, candidateTree)
	}
	w, _ = store.Get("wf")
	if w.State != workflow.StateDelivered {
		t.Fatalf("state=%s", w.State)
	}
}

func TestWorkflowDeliverRejectsInvalidAuthorityAndDrift(t *testing.T) {
	t.Run("authority", func(t *testing.T) {
		_, store := prepareReceiptedWorkflow(t)
		defer store.Close()
		if _, err := store.Database().Exec(`DELETE FROM receipt_authority WHERE workflow_id='wf'`); err != nil {
			t.Fatal(err)
		}
		if err := workflowDeliver(store, []string{"--id", "wf", "--message", "x", "--idempotency-key", "k"}, &bytes.Buffer{}, nil); err == nil {
			t.Fatal("invalid authority accepted")
		}
	})
	t.Run("drift", func(t *testing.T) {
		repo, store := prepareReceiptedWorkflow(t)
		defer store.Close()
		if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("drift\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := workflowDeliver(store, []string{"--id", "wf", "--message", "x", "--idempotency-key", "k"}, &bytes.Buffer{}, nil); err == nil {
			t.Fatal("drift accepted")
		}
	})
	t.Run("base-ref-moved", func(t *testing.T) {
		repo, store := prepareReceiptedWorkflow(t)
		defer store.Close()
		if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("move\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"-C", repo, "add", "other.txt"}, {"-C", repo, "commit", "-m", "move base ref"}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		if err := workflowDeliver(store, []string{"--id", "wf", "--message", "x", "--idempotency-key", "k"}, &bytes.Buffer{}, nil); err == nil {
			t.Fatal("moved base ref accepted")
		}
	})
}

func TestWorkflowAbortCancelsOpenCodeAndRejectsLateResult(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	marker := filepath.Join(t.TempDir(), "running")
	fake := filepath.Join(t.TempDir(), "opencode")
	script := "#!/bin/sh\ntouch '" + marker + "'\nsleep 10\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"--id", "wf", "--request", "change", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--timeout", "20s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	abortArgs := []string{"wf", "--idempotency-key", "abort-1"}
	if err := workflowAbort(store, abortArgs, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run succeeded after abort")
		}
	case <-time.After(time.Second):
		t.Fatal("worker was not cancelled")
	}
	if err := workflowAbort(store, abortArgs, &bytes.Buffer{}); err != nil {
		t.Fatalf("idempotent abort=%v", err)
	}
	var attemptID, owner, token, basis string
	if err := store.Database().QueryRow(`SELECT attempt_id,owner,fencing_token,basis_tree FROM mutation_attempts WHERE workflow_id='wf'`).Scan(&attemptID, &owner, &token, &basis); err != nil {
		t.Fatal(err)
	}
	var inputRaw []byte
	_ = store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id='wf'`).Scan(&inputRaw)
	var input workflowRunInput
	_ = json.Unmarshal(inputRaw, &input)
	env := workflow.ResultEnvelope{WorkflowID: "wf", NodeID: "slice_main", AttemptID: attemptID, BaseCandidateOID: basis, Status: workflow.AttemptCompleted}
	_, err := (&execution.Broker{Store: store, Seal: input.Seal}).Apply(context.Background(), execution.WorkerResult{Envelope: env, Owner: owner, FencingToken: token})
	if err == nil {
		t.Fatal("late result accepted")
	}
	var audits int
	_ = store.Database().QueryRow(`SELECT COUNT(*) FROM stale_result_audit WHERE attempt_id=?`, attemptID).Scan(&audits)
	if audits == 0 {
		t.Fatal("late result was not audited")
	}
}

func TestWorkflowPlannedMultiSliceDependencies(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	planPath := filepath.Join(t.TempDir(), "plan.json")
	plan := `{"slices":[{"id":"slice_one","title":"one","acceptance_criteria":["test \"$(cat a.txt)\" = one"],"paths":["a.txt"],"checks":["true"]},{"id":"slice_two","title":"two","acceptance_criteria":["test \"$(cat b.txt)\" = two"],"dependencies":["slice_one"],"paths":["b.txt"],"checks":["true"]}]}`
	_ = os.WriteFile(planPath, []byte(plan), 0o600)
	counter := filepath.Join(t.TempDir(), "count")
	fake := filepath.Join(t.TempDir(), "opencode")
	script := `#!/bin/sh
tree=$(git write-tree)
if [ ! -f '` + counter + `' ]; then touch '` + counter + `'; node=slice_one; path=a.txt; data=b25lCg==; else test "$(cat a.txt)" = one || exit 9; node=slice_two; path=b.txt; data=dHdvCg==; fi
printf '{"envelope":{"WorkflowID":"wf","NodeID":"%s","AttemptID":"wf:%s","BaseCandidateOID":"%s","Status":"completed"},"patch":{"Operations":[{"Path":"%s","Data":"%s","Mode":384}]}}' "$node" "$node" "$tree" "$path" "$data" > "$SKYNEX_RESULT_FILE"`
	_ = os.WriteFile(fake, []byte(script), 0o700)
	args := []string{"--id", "wf", "--request", "planned", "--route", "planned", "--override-actor", "tester", "--override-reason", "explicit plan", "--plan-file", planPath, "--opencode", fake, "--timeout", "2s"}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(repo, "b.txt")); string(raw) != "two\n" {
		t.Fatalf("b=%q", raw)
	}
}

func TestWorkflowDiscoveryFrontierPrototypeAndClose(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	way := filepath.Join(t.TempDir(), "way.json")
	raw := `{"nodes":[{"id":"wfnode_question","type":"grill","question":"Choose?","blocking":true,"unlocks":["a","b"]},{"id":"wfnode_proto","type":"prototype","question":"Validate?","blocking":true}]}`
	_ = os.WriteFile(way, []byte(raw), 0o600)
	args := []string{"--id", "wf", "--request", "discover", "--route", "discovery", "--override-actor", "tester", "--override-reason", "uncertain", "--wayfinder-file", way}
	if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := workflowFrontier(store, []string{"--id", "wf"}, &out); err != nil || !strings.Contains(out.String(), "wfnode_question") {
		t.Fatalf("frontier=%q err=%v", out.String(), err)
	}
	if err := workflowAnswer(store, []string{"--id", "wf", "--node", "wfnode_question", "--answer", "yes", "--actor", "tester"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowApprove(store, []string{"--id", "wf", "--action", "prototype:wfnode_proto", "--actor", "tester", "--reason", "validate"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := workflowAnswer(store, []string{"--id", "wf", "--node", "wfnode_proto", "--answer", "valid", "--actor", "tester"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var validations int
	_ = store.Database().QueryRow(`SELECT COUNT(*) FROM prototype_validations WHERE workflow_id='wf'`).Scan(&validations)
	if validations != 1 {
		t.Fatalf("validations=%d", validations)
	}
	plan := filepath.Join(t.TempDir(), "plan.json")
	_ = os.WriteFile(plan, []byte(`{"slices":[{"id":"slice_after","title":"after","acceptance_criteria":["true"],"paths":["a.txt"],"checks":["true"]}]}`), 0o600)
	if err := workflowCloseDiscovery(store, []string{"--id", "wf", "--plan-file", plan}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	w, _ := store.Get("wf")
	if w.State != workflow.StateReady {
		t.Fatalf("state=%s", w.State)
	}
}

func TestWorkflowPlannedRejectsInvalidDAG(t *testing.T) {
	repo, store := cliWorkflowRepo(t)
	defer store.Close()
	plan := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(plan, []byte(`{"slices":[{"id":"slice_a","title":"a","acceptance_criteria":["true"],"dependencies":["slice_b"],"paths":["a.txt"],"checks":["true"]},{"id":"slice_b","title":"b","acceptance_criteria":["true"],"dependencies":["slice_a"],"paths":["b.txt"],"checks":["true"]}]}`), 0o600)
	err := workflowStart(store, repo, []string{"--id", "wf", "--request", "bad", "--route", "planned", "--override-actor", "t", "--override-reason", "test", "--plan-file", plan}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("cycle accepted")
	}
}
