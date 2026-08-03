package delivery

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/workflow"
)

type fixture struct {
	repo       string
	candidate  gitcandidate.Candidate
	record     review.CandidateRecord
	receipt    review.Receipt
	reviews    *review.MemoryStore
	policy     gitcandidate.Policy
	riskPolicy review.RiskPolicy
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	git(t, "", "init", repo)
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "file.txt"), "base\n")
	git(t, repo, "add", "file.txt")
	git(t, repo, "commit", "-m", "base")
	write(t, filepath.Join(repo, "file.txt"), "candidate\n")
	seal, err := gitcandidate.CaptureContext(repo)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := gitcandidate.Freeze(seal, gitcandidate.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	riskPolicy := review.DefaultRiskPolicy()
	record, err := review.NewCandidateRecord("wf-1", candidate, riskPolicy.Hash(), "engine-v1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	floor := review.DeterministicFloor(workflow.RouteSimple, []review.Change{{Path: "file.txt", Kind: review.ChangeText}}, riskPolicy)
	evidence := []review.Evidence{{ID: "accept", Kind: review.EvidenceAcceptance, CandidateRecordID: record.ID, CandidateTreeOID: record.TreeOID, PolicyHash: record.PolicyHash, Digest: "accept-digest"}, {ID: "check", Kind: review.EvidenceCheck, CandidateRecordID: record.ID, CandidateTreeOID: record.TreeOID, PolicyHash: record.PolicyHash, Digest: "check-digest"}}
	assessment, err := review.AssessSemantic(record, floor, review.SemanticInput{RequestedRisk: review.RiskLow, Justification: "small change", EvidenceIDs: []string{"accept", "check"}, ModelProvider: "test", ModelID: "model", ModelVersion: "1", PromptTemplateID: "risk-v1", RenderedRedactedPrompt: "redacted"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reviews := review.NewMemoryStore()
	receipt, err := reviews.Issue(review.IssueRequest{Candidate: record, Floor: floor, Assessment: assessment, Evidence: evidence, IssuedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{repo: repo, candidate: candidate, record: record, receipt: receipt, reviews: reviews, policy: gitcandidate.Policy{}, riskPolicy: riskPolicy}
}

func (f fixture) request() Request {
	return Request{WorkflowID: "wf-1", Candidate: f.record, CandidatePolicy: f.policy, ExpectedReceiptID: f.receipt.ID, ExpectedPolicyHash: f.riskPolicy.Hash(), CompatibleEngineVersion: "engine-v1", Message: "managed commit", IdempotencyKey: "commit-v1"}
}

func TestManagedCommitUsesExactFrozenTreeDespiteLaterWorktreeEdit(t *testing.T) {
	f := newFixture(t)
	gate := Gate{Authority: f.reviews, Intents: NewMemoryIntentStore(), AfterGateCheck: func() { write(t, filepath.Join(f.repo, "file.txt"), "edited during gate\n") }}
	result, err := gate.Commit(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if result.TreeOID != f.candidate.TreeOID {
		t.Fatalf("result tree=%s", result.TreeOID)
	}
	if got := gitOut(t, f.repo, "rev-parse", "HEAD^{tree}"); got != f.candidate.TreeOID {
		t.Fatalf("commit tree=%s want=%s", got, f.candidate.TreeOID)
	}
	if got := gitOut(t, f.repo, "show", "HEAD:file.txt"); got != "candidate" {
		t.Fatalf("committed bytes=%q", got)
	}
	data, err := os.ReadFile(filepath.Join(f.repo, "file.txt"))
	if err != nil || string(data) != "edited during gate\n" {
		t.Fatalf("worktree=%q err=%v", data, err)
	}
}

func TestManagedCommitRetryIsIdempotent(t *testing.T) {
	f := newFixture(t)
	gate := Gate{Authority: f.reviews, Intents: NewMemoryIntentStore()}
	first, err := gate.Commit(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	second, err := gate.Commit(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if !second.Recovered || second.CommitOID != first.CommitOID {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	if got := gitOut(t, f.repo, "rev-list", "--count", "HEAD"); got != "2" {
		t.Fatalf("commit count=%s", got)
	}
}

func TestManagedCommitCASRejectsMovedRef(t *testing.T) {
	f := newFixture(t)
	gate := Gate{Authority: f.reviews, Intents: NewMemoryIntentStore(), AfterGateCheck: func() {
		write(t, filepath.Join(f.repo, "file.txt"), "external\n")
		git(t, f.repo, "add", "file.txt")
		git(t, f.repo, "commit", "-m", "external")
	}}
	_, err := gate.Commit(context.Background(), f.request())
	if !errors.Is(err, ErrRefConflict) {
		t.Fatalf("error=%v", err)
	}
	if got := gitOut(t, f.repo, "show", "HEAD:file.txt"); got != "external" {
		t.Fatalf("CAS overwrote external commit: %q", got)
	}
}

func TestManagedCommitRejectsInvalidAuthorityAndDrift(t *testing.T) {
	t.Run("wrong receipt", func(t *testing.T) {
		f := newFixture(t)
		req := f.request()
		req.ExpectedReceiptID = "wrong"
		_, err := (&Gate{Authority: f.reviews, Intents: NewMemoryIntentStore()}).Commit(context.Background(), req)
		if !errors.Is(err, ErrInvalidAuthority) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("invalidated", func(t *testing.T) {
		f := newFixture(t)
		if err := f.reviews.Invalidate(review.Invalidation{WorkflowID: "wf-1", CandidateRecordID: f.record.ID, Reason: "drift"}); err != nil {
			t.Fatal(err)
		}
		_, err := (&Gate{Authority: f.reviews, Intents: NewMemoryIntentStore()}).Commit(context.Background(), f.request())
		if !errors.Is(err, ErrInvalidAuthority) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("worktree", func(t *testing.T) {
		f := newFixture(t)
		write(t, filepath.Join(f.repo, "file.txt"), "pre-gate drift\n")
		_, err := (&Gate{Authority: f.reviews, Intents: NewMemoryIntentStore()}).Commit(context.Background(), f.request())
		if !errors.Is(err, ErrContextDrift) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("head", func(t *testing.T) {
		f := newFixture(t)
		git(t, f.repo, "checkout", "-b", "other")
		_, err := (&Gate{Authority: f.reviews, Intents: NewMemoryIntentStore()}).Commit(context.Background(), f.request())
		if !errors.Is(err, ErrContextDrift) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("base ref", func(t *testing.T) {
		f := newFixture(t)
		git(t, f.repo, "add", "file.txt")
		git(t, f.repo, "commit", "-m", "move base")
		_, err := (&Gate{Authority: f.reviews, Intents: NewMemoryIntentStore()}).Commit(context.Background(), f.request())
		if !errors.Is(err, ErrContextDrift) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("detached", func(t *testing.T) {
		f := newFixture(t)
		git(t, f.repo, "checkout", "--detach")
		req := f.request()
		req.Candidate.Seal.Detached = true
		req.Candidate.Seal.SymbolicHEAD = ""
		req.Candidate.Seal.BaseRef = ""
		_, err := (&Gate{Authority: f.reviews, Intents: NewMemoryIntentStore()}).Commit(context.Background(), req)
		if !errors.Is(err, ErrContextDrift) {
			t.Fatalf("error=%v", err)
		}
	})
}

func write(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}
func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
func gitOut(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
