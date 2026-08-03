package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/workflow"
)

func openCodeFixture(t *testing.T) (string, *workflow.SQLiteStore, gitcandidate.ContextSeal, Attempt) {
	t.Helper()
	repo, store, seal, candidate := execFixture(t)
	now := time.Now()
	if _, err := store.AcquireLease("worktree:"+seal.WorktreeID, "owner", "token", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{ID: "a1", WorkflowID: "wf", SliceID: "slice_a", WorktreeID: seal.WorktreeID, Owner: "owner", FencingToken: "token", BasisTree: candidate.TreeOID, AllowedPaths: []string{"a.txt"}, OperationID: "op1"}
	scheduler, _ := NewScheduler(store, graph())
	if err := scheduler.Start(attempt); err != nil {
		t.Fatal(err)
	}
	return repo, store, seal, attempt
}

func fakeOpenCode(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func resultJSON(t *testing.T, attempt Attempt, basis string) string {
	t.Helper()
	value, err := json.Marshal(invocationOutput{
		Envelope: workflow.ResultEnvelope{WorkflowID: attempt.WorkflowID, NodeID: attempt.SliceID, AttemptID: attempt.ID, BaseCandidateOID: basis, Status: workflow.AttemptCompleted, EvidenceIDs: []string{"e1"}},
		Patch:    PatchArtifact{Operations: []FileOperation{{Path: "a.txt", Data: []byte("new\n")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

func TestOpenCodeAdapterPatchHandoffAndDisposableWrites(t *testing.T) {
	repo, store, seal, attempt := openCodeFixture(t)
	defer store.Close()
	script := fakeOpenCode(t, "echo worker-secret-token=hidden\nprintf '%s' '"+resultJSON(t, attempt, attempt.BasisTree)+"' > \"$SKYNEX_RESULT_FILE\"\necho disposable > direct.txt")
	adapter := OpenCodeAdapter{Store: store, Options: OpenCodeOptions{Executable: script, Model: "test/model", Timeout: time.Second, MaxOutputBytes: 64}}
	result, err := adapter.Run(context.Background(), OpenCodeRequest{InvocationID: "inv1", Attempt: attempt, Seal: seal, ArtifactIDs: []string{"spec"}, Artifacts: map[string][]byte{"spec": []byte("immutable")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(repo, "direct.txt")); !os.IsNotExist(err) {
		t.Fatalf("worker write escaped disposable worktree: %v", err)
	}
	if _, err = (&Broker{Store: store, Seal: seal}).Apply(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(repo, "a.txt"))
	if string(data) != "new\n" {
		t.Fatalf("patch handoff data=%q", data)
	}
	var model, status, stdoutDigest string
	if err = store.Database().QueryRow(`SELECT model,status,stdout_digest FROM opencode_invocations WHERE invocation_id=?`, "inv1").Scan(&model, &status, &stdoutDigest); err != nil || model != "test/model" || status != "completed" || stdoutDigest == "" {
		t.Fatalf("metadata model=%q status=%q digest=%q err=%v", model, status, stdoutDigest, err)
	}
}

func TestOpenCodeAdapterMaterializesExactUncommittedBasisTree(t *testing.T) {
	repo, store, seal, attempt := openCodeFixture(t)
	defer store.Close()
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("accepted-prior-slice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	basis, err := gitcandidate.Freeze(seal, gitcandidate.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if basis.TreeOID == seal.BaseTreeOID {
		t.Fatal("fixture basis unexpectedly equals base commit tree")
	}
	attempt.BasisTree = basis.TreeOID
	body := "test \"$(cat a.txt)\" = accepted-prior-slice\nprintf '%s' '" + resultJSON(t, attempt, attempt.BasisTree) + "' > \"$SKYNEX_RESULT_FILE\""
	adapter := OpenCodeAdapter{Store: store, Options: OpenCodeOptions{Executable: fakeOpenCode(t, body), Timeout: time.Second}}
	if _, err = adapter.Run(context.Background(), OpenCodeRequest{InvocationID: "exact-basis", Attempt: attempt, Seal: seal}); err != nil {
		t.Fatalf("worker did not observe exact attempt basis: %v", err)
	}
}

func TestOpenCodeAdapterRejectsMalformedAndStale(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"malformed", "echo '{bad' > \"$SKYNEX_RESULT_FILE\"", ErrMalformedWorkerResult},
		{"stale", "printf '%s' 'STALE' > \"$SKYNEX_RESULT_FILE\"", workflow.ErrStaleResult},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, seal, attempt := openCodeFixture(t)
			defer store.Close()
			body := tc.body
			if tc.name == "stale" {
				body = "printf '%s' '" + resultJSON(t, attempt, "wrong-basis") + "' > \"$SKYNEX_RESULT_FILE\""
			}
			adapter := OpenCodeAdapter{Store: store, Options: OpenCodeOptions{Executable: fakeOpenCode(t, body), Timeout: time.Second}}
			_, err := adapter.Run(context.Background(), OpenCodeRequest{InvocationID: tc.name, Attempt: attempt, Seal: seal})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
		})
	}
}

func TestOpenCodeAdapterTimeoutAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		cancel  bool
		want    error
	}{
		{"timeout", 30 * time.Millisecond, false, context.DeadlineExceeded},
		{"cancel", time.Second, true, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, seal, attempt := openCodeFixture(t)
			defer store.Close()
			adapter := OpenCodeAdapter{Store: store, Options: OpenCodeOptions{Executable: fakeOpenCode(t, "sleep 2"), Timeout: tc.timeout}}
			ctx, cancel := context.WithCancel(context.Background())
			if tc.cancel {
				go func() { time.Sleep(20 * time.Millisecond); cancel() }()
			} else {
				defer cancel()
			}
			_, err := adapter.Run(ctx, OpenCodeRequest{InvocationID: tc.name, Attempt: attempt, Seal: seal})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
		})
	}
}
