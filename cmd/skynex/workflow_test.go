package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/workflow"
)

func TestWorkflowHelpDoesNotRequireGitRepository(t *testing.T) {
	var out bytes.Buffer
	if err := runWorkflowCLI([]string{"--help"}, t.TempDir(), &out); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"start", "run", "review", "deliver", "status", "inspect", "receipt", "approve", "abort"} {
		if !strings.Contains(out.String(), command) {
			t.Fatalf("help missing %q:\n%s", command, out.String())
		}
	}
}

func workflowRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	cmd := exec.Command("git", "init", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return repo
}
func createCLIWorkflow(t *testing.T, repo, id string, state workflow.State) {
	t.Helper()
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Create(workflow.Workflow{ID: id, Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow, BasisTree: "tree"}); err != nil {
		t.Fatal(err)
	}
	if state == workflow.StateCreated {
		return
	}
	w, err := store.Transition(workflow.Transition{WorkflowID: id, ExpectedState: workflow.StateCreated, ExpectedVersion: 0, NextState: workflow.StateDiscovering, IdempotencyKey: "discover"})
	if err != nil {
		t.Fatal(err)
	}
	if state == workflow.StateBlocked {
		_, err = store.Transition(workflow.Transition{WorkflowID: id, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateBlocked, ResumeTarget: workflow.StateDiscovering, IdempotencyKey: "block"})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkflowCLIStatusInspectAndAbortIdempotently(t *testing.T) {
	repo := workflowRepo(t)
	createCLIWorkflow(t, repo, "wf-1", workflow.StateDiscovering)
	var out bytes.Buffer
	if err := runWorkflowCLI([]string{"status"}, repo, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WORKFLOW\tSTATE") || !strings.Contains(out.String(), "wf-1\tdiscovering") {
		t.Fatalf("status=%q", out.String())
	}
	out.Reset()
	if err := runWorkflowCLI([]string{"inspect", "wf-1"}, repo, &out); err != nil {
		t.Fatal(err)
	}
	var inspection workflowInspection
	if err := json.Unmarshal(out.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.Workflow.ID != "wf-1" || len(inspection.Events) != 1 {
		t.Fatalf("inspection=%#v", inspection)
	}
	out.Reset()
	args := []string{"abort", "wf-1", "--idempotency-key", "operator-abort"}
	if err := runWorkflowCLI(args, repo, &out); err != nil {
		t.Fatal(err)
	}
	first := out.String()
	out.Reset()
	if err := runWorkflowCLI(args, repo, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != first {
		t.Fatalf("replay differs: %q %q", first, out.String())
	}
}

func TestWorkflowCLIResumeFailsClosed(t *testing.T) {
	repo := workflowRepo(t)
	createCLIWorkflow(t, repo, "wf-blocked", workflow.StateBlocked)
	err := runWorkflowCLI([]string{"resume", "wf-blocked", "--blocker-id", "blocker", "--idempotency-key", "resume"}, repo, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "blocker ID") {
		t.Fatalf("error=%v", err)
	}
	createCLIWorkflow(t, repo, "wf-ready", workflow.StateDiscovering)
	err = runWorkflowCLI([]string{"resume", "wf-ready", "--blocker-id", "blocker", "--idempotency-key", "resume"}, repo, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "only blocked") {
		t.Fatalf("error=%v", err)
	}
}

func TestWorkflowCLIExportIsAtomicPrivateAndDoesNotStage(t *testing.T) {
	repo := workflowRepo(t)
	createCLIWorkflow(t, repo, "wf-export", workflow.StateDiscovering)
	path := filepath.Join(repo, ".skynex", "exports", "workflow.json")
	var out bytes.Buffer
	if err := runWorkflowCLI([]string{"export", "wf-export", "--out", path}, repo, &out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var exported workflowInspection
	if err = json.Unmarshal(data, &exported); err != nil || exported.Workflow.ID != "wf-export" {
		t.Fatalf("export=%#v err=%v", exported, err)
	}
	cmd := exec.Command("git", "diff", "--cached", "--name-only")
	cmd.Dir = repo
	staged, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 0 {
		t.Fatalf("export staged files: %s", staged)
	}
}

func TestWorkflowCLIReceiptRejectsMissingAuthority(t *testing.T) {
	repo := workflowRepo(t)
	createCLIWorkflow(t, repo, "wf-no-receipt", workflow.StateCreated)
	err := runWorkflowCLI([]string{"receipt", "wf-no-receipt"}, repo, &bytes.Buffer{})
	if !errors.Is(err, review.ErrNoAuthority) {
		t.Fatalf("error=%v", err)
	}
}

func TestWorkflowCLIReceiptShowsCurrentAndHistorical(t *testing.T) {
	repo := workflowRepo(t)
	createCLIWorkflow(t, repo, "wf-receipt", workflow.StateCreated)
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	receipt := review.Receipt{ID: "rcpt-historical", WorkflowID: "wf-receipt", CandidateRecordID: "cand-1", CandidateTreeOID: "tree", PolicyHash: "policy", EngineVersion: "engine"}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Database().Exec(`INSERT INTO review_candidates(id,workflow_id,tree_oid,policy_hash,record) VALUES(?,?,?,?,?)`, "cand-1", "wf-receipt", "tree", "policy", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Database().Exec(`INSERT INTO receipts(id,workflow_id,candidate_record_id,receipt) VALUES(?,?,?,?)`, receipt.ID, receipt.WorkflowID, receipt.CandidateRecordID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Database().Exec(`INSERT INTO receipt_authority(workflow_id,receipt_id) VALUES(?,?)`, receipt.WorkflowID, receipt.ID); err != nil {
		t.Fatal(err)
	}
	store.Close()
	for _, args := range [][]string{{"receipt", "wf-receipt"}, {"receipt", "--id", receipt.ID}} {
		var out bytes.Buffer
		if err = runWorkflowCLI(args, repo, &out); err != nil {
			t.Fatal(err)
		}
		var got review.Receipt
		if err = json.Unmarshal(out.Bytes(), &got); err != nil || got.ID != receipt.ID {
			t.Fatalf("receipt=%#v err=%v", got, err)
		}
	}
}
