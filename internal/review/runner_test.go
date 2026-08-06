package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/workflow"
)

func TestReviewReadOnlyWorktreePreservesExecutableGitMode(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{{"init", repo}, {"-C", repo, "config", "user.email", "test@example.com"}, {"-C", repo, "config", "user.name", "Test"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	script := filepath.Join(repo, "script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", repo, "add", "script.sh"}, {"-C", repo, "commit", "-m", "script"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	wt := filepath.Join(t.TempDir(), "candidate")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", wt, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("add worktree: %v %s", err, out)
	}
	defer func() {
		_ = filepath.Walk(wt, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				if info.IsDir() {
					_ = os.Chmod(path, 0o700)
				} else {
					_ = os.Chmod(path, 0o600)
				}
			}
			return nil
		})
		_, _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).CombinedOutput()
	}()
	script = filepath.Join(wt, "script.sh")
	before, err := exec.Command("git", "-C", wt, "write-tree").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := makeReviewWorktreeReadOnly(wt); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o500 {
		t.Fatalf("script mode=%o want 500", got)
	}
	status, err := exec.Command("git", "-C", wt, "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 0 {
		t.Fatalf("read-only projection dirtied candidate: %q", status)
	}
	after, err := exec.Command("git", "-C", wt, "write-tree").Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("candidate tree changed: %s != %s", before, after)
	}
	mode, err := exec.Command("git", "-C", wt, "ls-files", "-s", "script.sh").Output()
	if err != nil || !strings.HasPrefix(string(mode), "100755 ") {
		t.Fatalf("index mode=%q err=%v", mode, err)
	}
}

func TestReviewReconcilesDeadRunningPIDAsInterrupted(t *testing.T) {
	store, err := workflow.OpenSQLite(filepath.Join(t.TempDir(), "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Create(workflow.Workflow{ID: "wf", Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = store.Database().Exec(`INSERT INTO review_invocations(id,workflow_id,candidate_tree,lens,model,status,output_digest,started_at,finished_at,error_preview,pid,heartbeat_at,last_activity_at,result_json,prompt_hash,policy_hash) VALUES('review:wf:semantic','wf','tree','semantic','','running','',?,'','',99999999,?,?,'','','policy')`, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	runner := OpenCodeReviewRunner{Store: store}
	runner.reconcileInterrupted("wf")
	var status, preview string
	if err := store.Database().QueryRow(`SELECT status,error_preview FROM review_invocations WHERE id='review:wf:semantic'`).Scan(&status, &preview); err != nil || status != "interrupted" || !strings.Contains(preview, "disappeared") {
		t.Fatalf("status=%q preview=%q err=%v", status, preview, err)
	}
}

func TestReviewRecoversCompletedResultWrittenBeforeCallerCheckpoint(t *testing.T) {
	store, err := workflow.OpenSQLite(filepath.Join(t.TempDir(), "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Create(workflow.Workflow{ID: "wf", Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow}); err != nil {
		t.Fatal(err)
	}
	candidate := CandidateRecord{WorkflowID: "wf", TreeOID: "tree", PolicyHash: "policy"}
	prompt := semanticPrompt()
	raw := []byte(`{"requested_risk":"low","selected_lens":"","justification":"recovered"}`)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = store.Database().Exec(`INSERT INTO review_invocations(id,workflow_id,candidate_tree,lens,model,status,output_digest,started_at,finished_at,error_preview,pid,heartbeat_at,last_activity_at,result_json,prompt_hash,policy_hash) VALUES('review:wf:semantic','wf','tree','semantic','model','completed','digest',?,?,'',42,?,?,?,?,'policy')`, now, now, now, now, raw, hash([]byte(prompt)))
	if err != nil {
		t.Fatal(err)
	}
	runner := OpenCodeReviewRunner{Store: store, Options: OpenCodeReviewOptions{Executable: "/does/not/exist", Model: "model"}}
	got, err := runner.invoke(context.Background(), "wf", candidate, "semantic", prompt)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("got=%q err=%v", got, err)
	}
	var parsed semanticOutput
	if detail := validateSemanticOutput(got, &parsed); detail != "" {
		t.Fatal(detail)
	}
	if err := runner.persistCheckpoint("wf", candidate, "semantic", prompt, got); err != nil {
		t.Fatal(err)
	}
	var checkpoints int
	if err := store.Database().QueryRow(`SELECT COUNT(*) FROM review_checkpoints WHERE workflow_id='wf'`).Scan(&checkpoints); err != nil || checkpoints != 1 {
		t.Fatalf("checkpoints=%d err=%v", checkpoints, err)
	}
}
