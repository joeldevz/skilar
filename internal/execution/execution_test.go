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
