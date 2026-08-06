package verification

import (
	"bytes"
	"context"
	"errors"
	"github.com/joeldevz/skynex/internal/delivery"
	"github.com/joeldevz/skynex/internal/execution"
	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/orchestration"
	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/workflow"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func verifyFixture(t *testing.T) (string, *workflow.SQLiteStore, gitcandidate.ContextSeal) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", repo)
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o600)
	git(t, repo, "add", "file.txt")
	git(t, repo, "commit", "-m", "base")
	seal, _ := gitcandidate.CaptureContext(repo)
	os.WriteFile(filepath.Join(repo, "file.txt"), []byte("candidate\n"), 0o600)
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	store.Create(workflow.Workflow{ID: "wf", Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow, BasisTree: seal.BaseTreeOID})
	engine := orchestration.NewEngine(store)
	_, err = engine.Begin("wf", orchestration.RouteInput{Clear: true, EstimatedSlices: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := orchestration.ExecutionGraph{WorkflowID: "wf", Version: 1, Slices: []orchestration.Slice{{ID: "slice_feature", Title: "Feature", AcceptanceCriteria: []string{"candidate written"}}}}
	if err = engine.Close("wf", orchestration.WayfinderGraph{WorkflowID: "wf", Version: 1}, orchestration.ExecutableContract{Destination: "candidate", AcceptanceCriteria: []string{"candidate written"}}, graph); err != nil {
		t.Fatal(err)
	}
	basis, err := gitcandidate.Freeze(seal, gitcandidate.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := execution.NewScheduler(store, graph)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err = store.AcquireLease("worktree:"+seal.WorktreeID, "harness", "token", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	attempt := execution.Attempt{ID: "attempt", WorkflowID: "wf", SliceID: "slice_feature", WorktreeID: seal.WorktreeID, Owner: "harness", FencingToken: "token", BasisTree: basis.TreeOID, AllowedPaths: []string{"file.txt"}, OperationID: "operation"}
	if err = scheduler.Start(attempt); err != nil {
		t.Fatal(err)
	}
	broker := execution.Broker{Store: store, Seal: seal, Policy: gitcandidate.Policy{}}
	envelope := workflow.ResultEnvelope{WorkflowID: "wf", NodeID: "slice_feature", AttemptID: "attempt", BaseCandidateOID: basis.TreeOID, Status: workflow.AttemptCompleted, EvidenceIDs: []string{"mutation"}}
	if _, err = broker.Apply(context.Background(), execution.WorkerResult{Envelope: envelope, Patch: execution.PatchArtifact{Operations: []execution.FileOperation{{Path: "file.txt", Data: []byte("candidate\n")}}}, Owner: "harness", FencingToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err = scheduler.Complete("wf", "slice_feature"); err != nil {
		t.Fatal(err)
	}
	return repo, store, seal
}
func runner(store *workflow.SQLiteStore) *Runner {
	return &Runner{Store: store, Policy: gitcandidate.Policy{}, RiskPolicy: review.DefaultRiskPolicy(), EngineVersion: "engine-v1"}
}
func goodPlan() Plan {
	return Plan{Checks: []Command{{Name: "sh", Args: []string{"-c", "test -f file.txt"}}}, Acceptance: []Command{{Name: "sh", Args: []string{"-c", "grep -q candidate file.txt"}}}}
}

func TestEndToEndVerifyReviewReceiptAndExactDelivery(t *testing.T) {
	repo, store, seal := verifyFixture(t)
	defer store.Close()
	result, err := runner(store).Run(context.Background(), "wf", seal, goodPlan())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed || result.Floor.Risk != review.RiskLow {
		t.Fatalf("result=%#v", result)
	}
	if err = ValidateEvidenceBasis(result.Record, result.Evidence); err != nil {
		t.Fatal(err)
	}
	w, _ := store.Get("wf")
	w, _ = store.Transition(workflow.Transition{WorkflowID: "wf", ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReviewing, IdempotencyKey: "review"})
	reviewEvidence := ToReviewEvidence(result.Record, result.Evidence)
	ids := []string{}
	for _, e := range reviewEvidence {
		ids = append(ids, e.ID)
	}
	assessment, err := review.AssessSemantic(result.Record, result.Floor, review.SemanticInput{RequestedRisk: review.RiskLow, Justification: "fake auditable semantic assessment", EvidenceIDs: ids, ModelProvider: "fake", ModelID: "fake-model", ModelVersion: "1", PromptTemplateID: "fake-v1", RenderedRedactedPrompt: "redacted"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reviews := review.NewSQLiteStore(store.Database())
	receipt, err := reviews.Issue(review.IssueRequest{Candidate: result.Record, Floor: result.Floor, Assessment: assessment, Evidence: reviewEvidence, IssuedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	w, _ = store.Transition(workflow.Transition{WorkflowID: "wf", ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReceipted, IdempotencyKey: "receipt"})
	gate := delivery.Gate{Authority: reviews, Intents: delivery.NewSQLiteIntentStore(store.Database())}
	delivered, err := gate.Commit(context.Background(), delivery.Request{WorkflowID: "wf", Candidate: result.Record, CandidatePolicy: gitcandidate.Policy{}, ExpectedReceiptID: receipt.ID, ExpectedPolicyHash: result.Record.PolicyHash, CompatibleEngineVersion: "engine-v1", Message: "managed", IdempotencyKey: "deliver"})
	if err != nil {
		t.Fatal(err)
	}
	if gitOut(t, repo, "rev-parse", "HEAD^{tree}") != delivered.TreeOID {
		t.Fatal("delivered tree mismatch")
	}
	store.Transition(workflow.Transition{WorkflowID: "wf", ExpectedState: workflow.StateReceipted, ExpectedVersion: w.StateVersion, NextState: workflow.StateDelivered, IdempotencyKey: "delivered"})
	final, _ := store.Get("wf")
	if final.State != workflow.StateDelivered {
		t.Fatalf("state=%s", final.State)
	}
}

func TestFailingCheckReplans(t *testing.T) {
	_, store, seal := verifyFixture(t)
	defer store.Close()
	result, err := runner(store).Run(context.Background(), "wf", seal, Plan{Checks: []Command{{Name: "sh", Args: []string{"-c", "exit 7"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed {
		t.Fatal("failure passed")
	}
	w, _ := store.Get("wf")
	if w.State != workflow.StateReplanRequired {
		t.Fatalf("state=%s", w.State)
	}
}

func TestZeroOperationVerificationUsesAdoptedDirtyWorkflowBasis(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", repo)
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "file.txt")
	git(t, repo, "commit", "-m", "base")
	// This pre-existing dirty dependency file is adopted into the workflow basis.
	// A zero-operation run must not reclassify it as a workflow mutation.
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seal, err := gitcandidate.CaptureContext(repo)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := gitcandidate.Freeze(seal, gitcandidate.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if adopted.TreeOID == seal.BaseTreeOID {
		t.Fatal("fixture did not create an adopted dirty basis")
	}
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := store.Create(workflow.Workflow{ID: "zero-op", Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow, BasisTree: adopted.TreeOID})
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []workflow.State{workflow.StateDiscovering, workflow.StateReady, workflow.StateExecuting, workflow.StateVerifying} {
		w, err = store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: next, IdempotencyKey: "to:" + string(next)})
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := runner(store).Run(context.Background(), "zero-op", seal, Plan{Checks: []Command{{Name: "sh", Args: []string{"-c", "test -f package.json"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed {
		t.Fatal("zero-operation verification failed")
	}
	if result.Candidate.TreeOID != adopted.TreeOID {
		t.Fatalf("candidate=%s adopted=%s", result.Candidate.TreeOID, adopted.TreeOID)
	}
	if result.Floor.Risk != review.RiskLow || len(result.Floor.Reasons) != 0 {
		t.Fatalf("adopted basis counted as mutation: %#v", result.Floor)
	}
}

func TestDriftBetweenVerificationAndFreezeReplans(t *testing.T) {
	repo, store, seal := verifyFixture(t)
	defer store.Close()
	r := runner(store)
	r.BeforeTransition = func() error { return os.WriteFile(filepath.Join(repo, "file.txt"), []byte("drift\n"), 0o600) }
	_, err := r.Run(context.Background(), "wf", seal, goodPlan())
	if err == nil {
		t.Fatal("drift accepted")
	}
	w, _ := store.Get("wf")
	if w.State != workflow.StateReplanRequired {
		t.Fatalf("state=%s", w.State)
	}
}
func TestEvidenceMismatchAndIdempotentCrashReplay(t *testing.T) {
	_, store, seal := verifyFixture(t)
	defer store.Close()
	r := runner(store)
	crash := errors.New("crash")
	r.BeforeTransition = func() error { return crash }
	result, err := r.Run(context.Background(), "wf", seal, goodPlan())
	if !errors.Is(err, crash) {
		t.Fatalf("error=%v", err)
	}
	bad := append([]Evidence(nil), result.Evidence...)
	bad[0].CandidateTree = "other"
	if !errors.Is(ValidateEvidenceBasis(result.Record, bad), review.ErrEvidenceMismatch) {
		t.Fatal("basis mismatch accepted")
	}
	r.BeforeTransition = nil
	replayed, err := r.Run(context.Background(), "wf", seal, goodPlan())
	if err != nil || replayed.Record.ID != result.Record.ID {
		t.Fatalf("replay=%#v err=%v", replayed, err)
	}
}
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSpace(out))
}
