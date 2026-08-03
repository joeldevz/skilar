package approval

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/workflow"
)

func TestApprovalExactExpiryRevocationAndIdempotency(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	input := Artifact{Actor: "actor", AuthSource: "test", WorkflowID: "wf", Action: "review", BasisGraphOrCandidate: "tree", PolicyHash: "policy", Rationale: "because", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	a, err := Issue(store.Database(), input)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Issue(store.Database(), input)
	if err != nil || again.ID != a.ID {
		t.Fatalf("replay=%#v %v", again, err)
	}
	if _, err = Require(store.Database(), "wf", "review", "tree", "policy", now); err != nil {
		t.Fatal(err)
	}
	if _, err = Require(store.Database(), "wf", "review", "other", "policy", now); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("mismatch=%v", err)
	}
	if _, err = Require(store.Database(), "wf", "review", "tree", "policy", now.Add(2*time.Hour)); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("expiry=%v", err)
	}
	if err = Revoke(store.Database(), "wf", "review", "actor", "changed", now); err != nil {
		t.Fatal(err)
	}
	if _, err = Require(store.Database(), "wf", "review", "tree", "policy", now); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("revoked=%v", err)
	}
}
