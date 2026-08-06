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
	"time"

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

func TestWorkflowCLIInspectReconcilesDeadDetachedWorker(t *testing.T) {
	repo := workflowRepo(t)
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(workflow.Workflow{ID: "wf-dead", Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow, BasisTree: "tree"})
	if err != nil {
		t.Fatal(err)
	}
	for i, next := range []workflow.State{workflow.StateDiscovering, workflow.StateReady, workflow.StateExecuting} {
		w, err = store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: next, IdempotencyKey: "setup-" + string(rune('a'+i))})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.CreateWorkflowJobOperation("job-dead", w.ID, "run", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = store.StartWorkflowJob("job-dead", 99999999, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err = runWorkflowCLI([]string{"inspect", "wf-dead"}, repo, &out); err != nil {
		t.Fatal(err)
	}
	var inspection workflowInspection
	if err = json.Unmarshal(out.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.Workflow.State != workflow.StateBlocked || inspection.Workflow.ResumeTarget != workflow.StateExecuting {
		t.Fatalf("workflow=%+v", inspection.Workflow)
	}
	if len(inspection.Jobs) != 1 || inspection.Jobs[0].State != string(workflow.JobFailed) || inspection.Jobs[0].NextAction == "" {
		t.Fatalf("jobs=%+v", inspection.Jobs)
	}
}

func TestWorkflowCLIStatusListReconcilesFromTheSingleDiagnosticScan(t *testing.T) {
	repo := workflowRepo(t)
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := store.Create(workflow.Workflow{ID: "wf-healthy", Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow, BasisTree: "tree"})
	if err != nil {
		t.Fatal(err)
	}
	dead, err := store.Create(workflow.Workflow{ID: "wf-orphan", Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow, BasisTree: "tree"})
	if err != nil {
		t.Fatal(err)
	}
	for i, next := range []workflow.State{workflow.StateDiscovering, workflow.StateReady, workflow.StateExecuting} {
		if dead, err = store.Transition(workflow.Transition{WorkflowID: dead.ID, ExpectedState: dead.State, ExpectedVersion: dead.StateVersion, NextState: next, IdempotencyKey: "setup-" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.CreateWorkflowJobOperation("job-orphan", dead.ID, "run", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = store.StartWorkflowJob("job-orphan", 99999999, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err = runWorkflowCLI([]string{"status"}, repo, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "wf-orphan\tblocked") {
		t.Fatalf("orphan was not reconciled in the listing: %q", text)
	}
	if !strings.Contains(text, healthy.ID+"\t"+string(workflow.StateCreated)) {
		t.Fatalf("healthy workflow missing from the listing: %q", text)
	}
}

func TestIntegrationConflictReportsTheAbortCommandInsteadOfWait(t *testing.T) {
	repo := workflowRepo(t)
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(workflow.Workflow{ID: "wf-conflict", Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow, BasisTree: "tree"})
	if err != nil {
		t.Fatal(err)
	}
	for i, next := range []workflow.State{workflow.StateDiscovering, workflow.StateReady, workflow.StateExecuting, workflow.StateIntegrationConflict} {
		if w, err = store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: next, IdempotencyKey: "setup-" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	want := "skynex workflow abort wf-conflict --idempotency-key abort-wf-conflict"

	var status bytes.Buffer
	if err = runWorkflowCLI([]string{"status", "wf-conflict"}, repo, &status); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.String(), "NEXT\t"+want) {
		t.Fatalf("status did not report an actionable next: %q", status.String())
	}

	var inspected bytes.Buffer
	if err = runWorkflowCLI([]string{"inspect", "wf-conflict"}, repo, &inspected); err != nil {
		t.Fatal(err)
	}
	var inspection workflowInspection
	if err = json.Unmarshal(inspected.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.NextAction != want {
		t.Fatalf("inspect next_action=%q", inspection.NextAction)
	}
}

func TestHealthyWorkflowStillReportsNoNextAction(t *testing.T) {
	repo := workflowRepo(t)
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(workflow.Workflow{ID: "wf-ok", Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow, BasisTree: "tree"}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	var status bytes.Buffer
	if err = runWorkflowCLI([]string{"status", "wf-ok"}, repo, &status); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status.String(), "NEXT\t") {
		t.Fatalf("healthy workflow demanded an action: %q", status.String())
	}
	var inspected bytes.Buffer
	if err = runWorkflowCLI([]string{"inspect", "wf-ok"}, repo, &inspected); err != nil {
		t.Fatal(err)
	}
	var inspection workflowInspection
	if err = json.Unmarshal(inspected.Bytes(), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.NextAction != "" {
		t.Fatalf("inspect next_action=%q", inspection.NextAction)
	}
}

func TestWorkflowCommandHelpOutsideRepositoryDoesNotCreateDatabase(t *testing.T) {
	dir := t.TempDir()
	commands := []string{"start", "run", "review", "deliver", "status", "inspect", "receipt", "approve", "revoke-approval", "abort", "resume", "export", "frontier", "answer", "close-discovery"}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			var out bytes.Buffer
			if err := runWorkflowCLI([]string{command, "--help"}, dir, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Usage: skynex workflow "+command) {
				t.Fatalf("help=%q", out.String())
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("help created repository state: %v", err)
	}
}

func TestWorkflowStatusAbsentDatabaseIsReadOnly(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	var output bytes.Buffer
	if err := runWorkflowCLI([]string{"status"}, repo, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "WORKFLOW\tSTATE\tVERSION\tROUTE\tRISK\n" {
		t.Fatalf("status=%q", output.String())
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "skynex")); !os.IsNotExist(err) {
		t.Fatalf("status created database directory: %v", err)
	}
	output.Reset()
	if err := runWorkflowCLI([]string{"notifications", "claim", "--consumer", "session-1"}, repo, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "null\n" {
		t.Fatalf("notification claim=%q", output.String())
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "skynex")); !os.IsNotExist(err) {
		t.Fatalf("notification polling created database directory: %v", err)
	}
	if err := runWorkflowCLI([]string{"notifications", "presence", "--session", "session-1"}, repo, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "skynex")); !os.IsNotExist(err) {
		t.Fatalf("presence heartbeat created database directory: %v", err)
	}
	for _, command := range []string{"inspect", "receipt"} {
		if err := runWorkflowCLI([]string{command, "missing"}, repo, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "database not found") {
			t.Fatalf("%s err=%v", command, err)
		}
		if _, err := os.Stat(filepath.Join(repo, ".git", "skynex")); !os.IsNotExist(err) {
			t.Fatalf("%s created database directory", command)
		}
	}
}

func TestWorkflowDiagnosticCommandsDoNotTouchExistingDatabaseDirectory(t *testing.T) {
	repo := workflowRepo(t)
	createCLIWorkflow(t, repo, "wf-read-only", workflow.StateDiscovering)
	databasePath, err := workflow.CanonicalDatabasePath(repo)
	if err != nil {
		t.Fatal(err)
	}
	databaseDir := filepath.Dir(databasePath)
	fixedTime := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name string
		args []string
	}{
		{name: "status", args: []string{"status"}},
		{name: "inspect", args: []string{"inspect", "wf-read-only"}},
		{name: "receipt", args: []string{"receipt", "wf-read-only"}},
		{name: "export", args: []string{"export", "wf-read-only", "--out", filepath.Join(repo, "export.json")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, suffix := range []string{"-wal", "-shm"} {
				if err := os.Remove(databasePath + suffix); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(databaseDir, fixedTime, fixedTime); err != nil {
				t.Fatal(err)
			}
			err := runWorkflowCLI(test.args, repo, &bytes.Buffer{})
			if test.name != "receipt" && err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"-wal", "-shm"} {
				if _, statErr := os.Stat(databasePath + suffix); !os.IsNotExist(statErr) {
					t.Fatalf("diagnostic command left SQLite sidecar %s: %v", suffix, statErr)
				}
			}
			info, statErr := os.Stat(databaseDir)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if !info.ModTime().Equal(fixedTime) {
				t.Fatalf("diagnostic command changed database directory mtime: got %s want %s", info.ModTime(), fixedTime)
			}
		})
	}
}

func TestWorkflowStatusByIDRequiresExistingDatabase(t *testing.T) {
	repo := t.TempDir()
	if output, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	var out bytes.Buffer
	err := runWorkflowCLI([]string{"status", "missing"}, repo, &out)
	if err == nil || !strings.Contains(err.Error(), "workflow database not found") {
		t.Fatalf("status error = %v", err)
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
