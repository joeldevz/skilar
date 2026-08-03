package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/execution"
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
  *"Assess risk"*) printf '{"requested_risk":"` + tc.risk + `","selected_lens":"` + tc.lens + `","justification":"fake assessment"}' > "$SKYNEX_RESULT_FILE"; exit 0 ;;
  *"Review lens"*) printf '{"findings":[]}' > "$SKYNEX_RESULT_FILE"; exit 0 ;;
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

func TestWorkflowOpenCodeReviewRejectsBadResultsAndDrift(t *testing.T) {
	for _, tc := range []struct {
		name, reviewScript string
		drift              bool
	}{
		{"lowering", `printf '{"requested_risk":"","justification":"lower"}' > "$SKYNEX_RESULT_FILE"`, false},
		{"malformed", `echo bad > "$SKYNEX_RESULT_FILE"`, false},
		{"timeout", `sleep 2`, false},
		{"severe", `case "$*" in *"Assess risk"*) printf '{"requested_risk":"high","justification":"high"}' ;; *) printf '{"findings":[{"severity":"severe","message":"boom","reproducible":true,"candidate_caused":true}]}' ;; esac > "$SKYNEX_RESULT_FILE"`, false},
		{"drift", `printf '{"requested_risk":"low","justification":"ok"}' > "$SKYNEX_RESULT_FILE"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, store := cliWorkflowRepo(t)
			defer store.Close()
			fake := cliFakeOpenCode(t, "true")
			args := []string{"--id", "wf", "--request", "change a", "--accept", "true", "--check", "true", "--path", "a.txt", "--opencode", fake, "--model", "fake", "--timeout", "30ms"}
			if err := workflowStart(store, repo, args, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if err := workflowRun(store, repo, []string{"wf"}, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if tc.drift {
				_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("drift\n"), 0o600)
			}
			if err := os.WriteFile(fake, []byte("#!/bin/sh\nset -eu\n"+tc.reviewScript+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := workflowReview(store, []string{"--id", "wf"}, &bytes.Buffer{}); err == nil {
				t.Fatal("review unexpectedly succeeded")
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
