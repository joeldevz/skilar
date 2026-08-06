package execution

import (
	"context"
	"errors"
	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/orchestration"
	"github.com/joeldevz/skynex/internal/workflow"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func execFixture(t *testing.T) (string, *workflow.SQLiteStore, gitcandidate.ContextSeal, gitcandidate.Candidate) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", repo)
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o600)
	git(t, repo, "add", "a.txt")
	git(t, repo, "commit", "-m", "base")
	seal, _ := gitcandidate.CaptureContext(repo)
	candidate, _ := gitcandidate.Freeze(seal, gitcandidate.Policy{})
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	store.Create(workflow.Workflow{ID: "wf", Route: workflow.RoutePlanned, MinimumRisk: workflow.RiskMedium, BasisTree: candidate.TreeOID})
	w, _ := store.Transition(workflow.Transition{WorkflowID: "wf", ExpectedState: workflow.StateCreated, ExpectedVersion: 0, NextState: workflow.StateDiscovering, IdempotencyKey: "d"})
	store.Transition(workflow.Transition{WorkflowID: "wf", ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReady, IdempotencyKey: "r"})
	return repo, store, seal, candidate
}
func graph() orchestration.ExecutionGraph {
	return orchestration.ExecutionGraph{WorkflowID: "wf", Version: 1, Slices: []orchestration.Slice{{ID: "slice_a", Title: "A", AcceptanceCriteria: []string{"a"}}, {ID: "slice_b", Title: "B", AcceptanceCriteria: []string{"b"}, Dependencies: []string{"slice_a"}}}}
}

func TestSchedulerDependencyOrderAndSingleWriter(t *testing.T) {
	_, store, seal, c := execFixture(t)
	defer store.Close()
	s, err := NewScheduler(store, graph())
	if err != nil {
		t.Fatal(err)
	}
	ready, _ := s.NextReady()
	if ready.ID != "slice_a" {
		t.Fatalf("ready=%#v", ready)
	}
	now := time.Now()
	store.AcquireLease("worktree:"+seal.WorktreeID, "owner", "token", now, now.Add(time.Minute))
	a := Attempt{ID: "a1", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "owner", FencingToken: "token", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op1"}
	if err = s.Start(a); err != nil {
		t.Fatal(err)
	}
	if ready, _ = s.NextReady(); ready != nil {
		t.Fatal("scheduled concurrent writer")
	}
	if err = s.Complete("wf", "slice_a"); err != nil {
		t.Fatal(err)
	}
	ready, _ = s.NextReady()
	if ready.ID != "slice_b" {
		t.Fatalf("ready=%#v", ready)
	}
}

func TestSchedulerCompletesFinalSliceAndTransitionAtomically(t *testing.T) {
	_, store, seal, c := execFixture(t)
	defer store.Close()
	s, err := NewScheduler(store, graph())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err = store.AcquireLease("worktree:"+seal.WorktreeID, "owner", "token", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	first := Attempt{ID: "a1", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "owner", FencingToken: "token", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op1"}
	if err = s.Start(first); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete("wf", "slice_a"); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID, second.SliceID, second.OperationID = "a2", "slice_b", "op2"
	if err = s.Start(second); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete("wf", "slice_b"); err != nil {
		t.Fatal(err)
	}
	w, err := store.Get("wf")
	if err != nil || w.State != workflow.StateVerifying {
		t.Fatalf("workflow=%+v err=%v", w, err)
	}
	var completed int
	if err = store.Database().QueryRow(`SELECT COUNT(*) FROM execution_slice_state WHERE workflow_id='wf' AND status='completed'`).Scan(&completed); err != nil || completed != 2 {
		t.Fatalf("completed=%d err=%v", completed, err)
	}
}

func TestSchedulerReconcilesLegacyCompletedExecution(t *testing.T) {
	_, store, seal, c := execFixture(t)
	defer store.Close()
	s, err := NewScheduler(store, graph())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err = store.AcquireLease("worktree:"+seal.WorktreeID, "owner", "token", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	first := Attempt{ID: "a1", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "owner", FencingToken: "token", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op1"}
	if err = s.Start(first); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete("wf", "slice_a"); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID, second.SliceID, second.OperationID = "a2", "slice_b", "op2"
	if err = s.Start(second); err != nil {
		t.Fatal(err)
	}
	// Reproduce the durable state written by the pre-fix two-transaction
	// completion path: the last slice is complete but the workflow is not.
	if _, err = store.Database().Exec(`UPDATE execution_slice_state SET status='completed' WHERE workflow_id='wf' AND slice_id='slice_b'`); err != nil {
		t.Fatal(err)
	}
	advanced, err := s.ReconcileCompletion()
	if err != nil || !advanced {
		t.Fatalf("advanced=%v err=%v", advanced, err)
	}
	w, err := store.Get("wf")
	if err != nil || w.State != workflow.StateVerifying {
		t.Fatalf("workflow=%+v err=%v", w, err)
	}
	if advanced, err = s.ReconcileCompletion(); err != nil || advanced {
		t.Fatalf("second reconcile advanced=%v err=%v", advanced, err)
	}
}

func TestReconcileAdoptsSliceLeftActiveByCompletedMutation(t *testing.T) {
	_, store, seal, c := execFixture(t)
	defer store.Close()
	s, err := NewScheduler(store, graph())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err = store.AcquireLease("worktree:"+seal.WorktreeID, "owner", "token", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = s.Start(Attempt{ID: "a1", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "owner", FencingToken: "token", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op1"}); err != nil {
		t.Fatal(err)
	}
	b := Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}}
	env := workflow.ResultEnvelope{WorkflowID: "wf", NodeID: "slice_a", AttemptID: "a1", BaseCandidateOID: c.TreeOID, Status: workflow.AttemptCompleted, EvidenceIDs: []string{"e1"}}
	if _, err = b.Apply(context.Background(), WorkerResult{Envelope: env, Patch: PatchArtifact{Operations: []FileOperation{{Path: "a.txt", Data: []byte("new\n")}}}, Owner: "owner", FencingToken: "token"}); err != nil {
		t.Fatal(err)
	}
	// The worker dies here: the mutation is durably committed and its attempt is
	// retired, but Complete never ran, so the slice is still active.
	if ready, readyErr := s.NextReady(); readyErr != nil || ready != nil {
		t.Fatalf("ready=%#v err=%v", ready, readyErr)
	}
	repaired, err := s.ReconcileCompletion()
	if err != nil || !repaired {
		t.Fatalf("repaired=%v err=%v", repaired, err)
	}
	ready, err := s.NextReady()
	if err != nil || ready == nil || ready.ID != "slice_b" {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
	w, err := store.Get("wf")
	if err != nil || w.State != workflow.StateExecuting {
		t.Fatalf("workflow=%+v err=%v", w, err)
	}
	if repaired, err = s.ReconcileCompletion(); err != nil || repaired {
		t.Fatalf("second reconcile repaired=%v err=%v", repaired, err)
	}
}

func TestReconcileNeverAdoptsSliceWithLiveAttempt(t *testing.T) {
	_, store, seal, c := execFixture(t)
	defer store.Close()
	s, err := NewScheduler(store, graph())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err = store.AcquireLease("worktree:"+seal.WorktreeID, "owner", "token", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = s.Start(Attempt{ID: "a1", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "owner", FencingToken: "token", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op1"}); err != nil {
		t.Fatal(err)
	}
	repaired, err := s.ReconcileCompletion()
	if err != nil || repaired {
		t.Fatalf("repaired=%v err=%v", repaired, err)
	}
	var status string
	if err = store.Database().QueryRow(`SELECT status FROM execution_slice_state WHERE workflow_id='wf' AND slice_id='slice_a'`).Scan(&status); err != nil || status != "active" {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestExecutionCompletionKeyUsesTheScheduledGraphVersion(t *testing.T) {
	_, store, seal, c := execFixture(t)
	defer store.Close()
	scheduled := graph()
	scheduled.Version = 2
	// A stale persisted graph must not decide the completion lineage; only the
	// version the scheduler actually loaded may.
	if _, err := store.Database().Exec(`INSERT INTO execution_graphs(workflow_id,version,graph) VALUES(?,?,?)`, "wf", 1, "{}"); err != nil {
		t.Fatal(err)
	}
	s, err := NewScheduler(store, scheduled)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err = store.AcquireLease("worktree:"+seal.WorktreeID, "owner", "token", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	first := Attempt{ID: "a1", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "owner", FencingToken: "token", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op1"}
	if err = s.Start(first); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete("wf", "slice_a"); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID, second.SliceID, second.OperationID = "a2", "slice_b", "op2"
	if err = s.Start(second); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete("wf", "slice_b"); err != nil {
		t.Fatal(err)
	}
	var key string
	if err = store.Database().QueryRow(`SELECT idempotency_key FROM transition_events WHERE workflow_id='wf' AND to_state=?`, workflow.StateVerifying).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "execution:complete:v2" {
		t.Fatalf("completion key=%q", key)
	}
	var startKey string
	if err = store.Database().QueryRow(`SELECT idempotency_key FROM transition_events WHERE workflow_id='wf' AND to_state=?`, workflow.StateExecuting).Scan(&startKey); err != nil {
		t.Fatal(err)
	}
	if startKey != "execution:start:v2" {
		t.Fatalf("start key=%q", startKey)
	}
}

func startedAttempt(t *testing.T, s *Scheduler, seal gitcandidate.ContextSeal, tree string) Attempt {
	t.Helper()
	now := time.Now()
	if _, err := s.store.AcquireLease("worktree:"+seal.WorktreeID, "o", "t", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	a := Attempt{ID: "a1", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "o", FencingToken: "t", BasisTree: tree, AllowedPaths: []string{"a.txt"}, OperationID: "op1"}
	if err := s.Start(a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestResumeAttemptAdoptsAPatchThatLandedBeforeTheCrash(t *testing.T) {
	repo, store, seal, c := execFixture(t)
	defer store.Close()
	s, err := NewScheduler(store, graph())
	if err != nil {
		t.Fatal(err)
	}
	a := startedAttempt(t, s, seal, c.TreeOID)
	crash := errors.New("crash after mutation")
	dying := Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}, AfterMutation: func() error { return crash }}
	env := workflow.ResultEnvelope{WorkflowID: "wf", NodeID: "slice_a", AttemptID: "a1", BaseCandidateOID: c.TreeOID, Status: workflow.AttemptCompleted}
	patch := WorkerResult{Envelope: env, Patch: PatchArtifact{Operations: []FileOperation{{Path: "a.txt", Data: []byte("new\n")}}}, Owner: "o", FencingToken: "t"}
	if _, err = dying.Apply(context.Background(), patch); !errors.Is(err, crash) {
		t.Fatalf("apply=%v", err)
	}
	var live int
	if err = store.Database().QueryRow(`SELECT live FROM mutation_attempts WHERE attempt_id='a1'`).Scan(&live); err != nil || live != 1 {
		t.Fatalf("live=%d err=%v", live, err)
	}
	landed, err := gitcandidate.Freeze(seal, gitcandidate.Policy{})
	if err != nil || landed.TreeOID == a.BasisTree {
		t.Fatalf("patch did not land: %s err=%v", landed.TreeOID, err)
	}
	// Re-dispatching this attempt would fail its basis check forever, because
	// the worktree already carries the patch.
	if _, err = (&Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}}).Apply(context.Background(), patch); !errors.Is(err, workflow.ErrStaleResult) {
		t.Fatalf("redispatch=%v", err)
	}
	post, err := s.ResumeAttempt(&Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}}, a)
	if err != nil || post != landed.TreeOID {
		t.Fatalf("post=%q err=%v", post, err)
	}
	var status, sliceStatus string
	if err = store.Database().QueryRow(`SELECT status FROM mutation_operations WHERE operation_id='op1'`).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("operation=%q err=%v", status, err)
	}
	if err = store.Database().QueryRow(`SELECT live FROM mutation_attempts WHERE attempt_id='a1'`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("live=%d err=%v", live, err)
	}
	if err = store.Database().QueryRow(`SELECT status FROM execution_slice_state WHERE workflow_id='wf' AND slice_id='slice_a'`).Scan(&sliceStatus); err != nil || sliceStatus != "completed" {
		t.Fatalf("slice=%q err=%v", sliceStatus, err)
	}
	ready, err := s.NextReady()
	if err != nil || ready == nil || ready.ID != "slice_b" {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
	data, _ := os.ReadFile(filepath.Join(repo, "a.txt"))
	if string(data) != "new\n" {
		t.Fatalf("worktree=%q", data)
	}
}

func TestResumeAttemptRedispatchesWhenThePatchNeverLanded(t *testing.T) {
	_, store, seal, c := execFixture(t)
	defer store.Close()
	s, err := NewScheduler(store, graph())
	if err != nil {
		t.Fatal(err)
	}
	a := startedAttempt(t, s, seal, c.TreeOID)
	crash := errors.New("crash before mutation")
	dying := Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}, AfterIntent: func() error { return crash }}
	env := workflow.ResultEnvelope{WorkflowID: "wf", NodeID: "slice_a", AttemptID: "a1", BaseCandidateOID: c.TreeOID, Status: workflow.AttemptCompleted}
	if _, err = dying.Apply(context.Background(), WorkerResult{Envelope: env, Patch: PatchArtifact{Operations: []FileOperation{{Path: "a.txt", Data: []byte("new\n")}}}, Owner: "o", FencingToken: "t"}); !errors.Is(err, crash) {
		t.Fatalf("apply=%v", err)
	}
	post, err := s.ResumeAttempt(&Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}}, a)
	if err != nil || post != "" {
		t.Fatalf("post=%q err=%v", post, err)
	}
	var live int
	var sliceStatus string
	if err = store.Database().QueryRow(`SELECT live FROM mutation_attempts WHERE attempt_id='a1'`).Scan(&live); err != nil || live != 1 {
		t.Fatalf("live=%d err=%v", live, err)
	}
	if err = store.Database().QueryRow(`SELECT status FROM execution_slice_state WHERE workflow_id='wf' AND slice_id='slice_a'`).Scan(&sliceStatus); err != nil || sliceStatus != "active" {
		t.Fatalf("slice=%q err=%v", sliceStatus, err)
	}
}

func TestResumeAttemptFailsClosedOnAnUnrecognizedTree(t *testing.T) {
	repo, store, seal, c := execFixture(t)
	defer store.Close()
	s, err := NewScheduler(store, graph())
	if err != nil {
		t.Fatal(err)
	}
	a := startedAttempt(t, s, seal, c.TreeOID)
	dying := Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}, AfterIntent: func() error { return errors.New("crash") }}
	env := workflow.ResultEnvelope{WorkflowID: "wf", NodeID: "slice_a", AttemptID: "a1", BaseCandidateOID: c.TreeOID, Status: workflow.AttemptCompleted}
	_, _ = dying.Apply(context.Background(), WorkerResult{Envelope: env, Patch: PatchArtifact{Operations: []FileOperation{{Path: "a.txt", Data: []byte("new\n")}}}, Owner: "o", FencingToken: "t"})
	if err = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("third party\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResumeAttempt(&Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}}, a); err == nil {
		t.Fatal("adopted an unrecognized tree")
	}
	w, getErr := store.Get("wf")
	if getErr != nil || w.State != workflow.StateIntegrationConflict {
		t.Fatalf("workflow=%+v err=%v", w, getErr)
	}
}

func TestStartCommitsActivationAndTheExecutingTransitionTogether(t *testing.T) {
	_, store, seal, c := execFixture(t)
	defer store.Close()
	w, err := store.Get("wf")
	if err != nil {
		t.Fatal(err)
	}
	for i, next := range []workflow.State{workflow.StateExecuting, workflow.StateVerifying} {
		if w, err = store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: next, IdempotencyKey: "drive-" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewScheduler(store, graph())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err = store.AcquireLease("worktree:"+seal.WorktreeID, "o", "t", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = s.Start(Attempt{ID: "a1", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "o", FencingToken: "t", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op1"}); err == nil {
		t.Fatal("activated a slice the workflow state cannot host")
	}
	var status string
	if err = store.Database().QueryRow(`SELECT status FROM execution_slice_state WHERE workflow_id='wf' AND slice_id='slice_a'`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("slice=%q err=%v", status, err)
	}
	var attempts int
	if err = store.Database().QueryRow(`SELECT COUNT(*) FROM mutation_attempts WHERE workflow_id='wf'`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestActivationRejectsAMissingGraphVersionBeforeAnyWrite(t *testing.T) {
	_, store, seal, c := execFixture(t)
	defer store.Close()
	unversioned := graph()
	unversioned.Version = 0
	s, err := NewScheduler(store, unversioned)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err = store.AcquireLease("worktree:"+seal.WorktreeID, "o", "t", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = s.Start(Attempt{ID: "a1", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "o", FencingToken: "t", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op1"}); err == nil {
		t.Fatal("activated without a graph version")
	}
	var status string
	if err = store.Database().QueryRow(`SELECT status FROM execution_slice_state WHERE workflow_id='wf' AND slice_id='slice_a'`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("slice=%q err=%v", status, err)
	}
	w, err := store.Get("wf")
	if err != nil || w.State != workflow.StateReady {
		t.Fatalf("workflow=%+v err=%v", w, err)
	}
}

func TestBrokerPatchStaleForbiddenAndRecovery(t *testing.T) {
	repo, store, seal, c := execFixture(t)
	defer store.Close()
	s, _ := NewScheduler(store, graph())
	now := time.Now()
	store.AcquireLease("worktree:"+seal.WorktreeID, "owner", "token", now, now.Add(time.Minute))
	a := Attempt{ID: "a1", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "owner", FencingToken: "token", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op1"}
	if err := s.Start(a); err != nil {
		t.Fatal(err)
	}
	b := Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}}
	env := workflow.ResultEnvelope{WorkflowID: "wf", NodeID: "slice_a", AttemptID: "a1", BaseCandidateOID: c.TreeOID, Status: workflow.AttemptCompleted, EvidenceIDs: []string{"e1"}}
	if _, err := b.Apply(context.Background(), WorkerResult{Envelope: env, Patch: PatchArtifact{Operations: []FileOperation{{Path: "../escape", Data: []byte("x")}}}, Owner: "owner", FencingToken: "token"}); err == nil {
		t.Fatal("accepted forbidden path")
	}
	if _, err := b.Apply(context.Background(), WorkerResult{Envelope: env, Patch: PatchArtifact{Operations: []FileOperation{{Path: "a.txt", Data: []byte("new\n")}}}, Owner: "owner", FencingToken: "wrong"}); !errors.Is(err, workflow.ErrStaleResult) {
		t.Fatalf("stale=%v", err)
	}
	post, err := b.Apply(context.Background(), WorkerResult{Envelope: env, Patch: PatchArtifact{Operations: []FileOperation{{Path: "a.txt", Data: []byte("new\n")}}}, Owner: "owner", FencingToken: "token"})
	if err != nil || post == c.TreeOID {
		t.Fatalf("post=%s err=%v", post, err)
	}
	if outcome, err := b.Recover("op1"); err != nil || outcome != "post" {
		t.Fatalf("recovery=%s err=%v", outcome, err)
	}
	data, _ := os.ReadFile(filepath.Join(repo, "a.txt"))
	if string(data) != "new\n" {
		t.Fatalf("data=%q", data)
	}
}

func TestBrokerCrashBeforeAfterAndUnknown(t *testing.T) {
	t.Run("before", func(t *testing.T) {
		_, store, seal, c := execFixture(t)
		defer store.Close()
		s, _ := NewScheduler(store, graph())
		now := time.Now()
		store.AcquireLease("worktree:"+seal.WorktreeID, "o", "t", now, now.Add(time.Minute))
		s.Start(Attempt{ID: "a", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "o", FencingToken: "t", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op"})
		crash := errors.New("crash")
		b := Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}, AfterIntent: func() error { return crash }}
		env := workflow.ResultEnvelope{WorkflowID: "wf", AttemptID: "a", BaseCandidateOID: c.TreeOID}
		_, err := b.Apply(context.Background(), WorkerResult{Envelope: env, Patch: PatchArtifact{Operations: []FileOperation{{Path: "a.txt", Data: []byte("new")}}}, Owner: "o", FencingToken: "t"})
		if !errors.Is(err, crash) {
			t.Fatal(err)
		}
		outcome, _ := b.Recover("op")
		if outcome != "pre" {
			t.Fatalf("outcome=%s", outcome)
		}
	})
	t.Run("after", func(t *testing.T) {
		_, store, seal, c := execFixture(t)
		defer store.Close()
		s, _ := NewScheduler(store, graph())
		now := time.Now()
		store.AcquireLease("worktree:"+seal.WorktreeID, "o", "t", now, now.Add(time.Minute))
		s.Start(Attempt{ID: "a", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "o", FencingToken: "t", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op"})
		crash := errors.New("crash after mutation")
		b := Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}, AfterMutation: func() error { return crash }}
		env := workflow.ResultEnvelope{WorkflowID: "wf", AttemptID: "a", BaseCandidateOID: c.TreeOID}
		_, err := b.Apply(context.Background(), WorkerResult{Envelope: env, Patch: PatchArtifact{Operations: []FileOperation{{Path: "a.txt", Data: []byte("new")}}}, Owner: "o", FencingToken: "t"})
		if !errors.Is(err, crash) {
			t.Fatal(err)
		}
		outcome, recoverErr := b.Recover("op")
		if recoverErr != nil || outcome != "post" {
			t.Fatalf("outcome=%s err=%v", outcome, recoverErr)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		repo, store, seal, c := execFixture(t)
		defer store.Close()
		s, _ := NewScheduler(store, graph())
		now := time.Now()
		store.AcquireLease("worktree:"+seal.WorktreeID, "o", "t", now, now.Add(time.Minute))
		s.Start(Attempt{ID: "a", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "o", FencingToken: "t", BasisTree: c.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op"})
		b := Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}, AfterIntent: func() error { return errors.New("crash") }}
		env := workflow.ResultEnvelope{WorkflowID: "wf", AttemptID: "a", BaseCandidateOID: c.TreeOID}
		b.Apply(context.Background(), WorkerResult{Envelope: env, Patch: PatchArtifact{Operations: []FileOperation{{Path: "a.txt", Data: []byte("new")}}}, Owner: "o", FencingToken: "t"})
		os.WriteFile(filepath.Join(repo, "a.txt"), []byte("unknown"), 0o600)
		outcome, err := b.Recover("op")
		if err != nil || outcome != "unknown" {
			t.Fatalf("outcome=%s err=%v", outcome, err)
		}
		w, _ := store.Get("wf")
		if w.State != workflow.StateIntegrationConflict {
			t.Fatalf("state=%s", w.State)
		}
	})
}
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
}
