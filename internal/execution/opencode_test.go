package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/workflow"
)

func TestOpenCodePromptContainsExactResultContract(t *testing.T) {
	_, store, seal, attempt := openCodeFixture(t)
	defer store.Close()
	captured := filepath.Join(t.TempDir(), "prompt")
	body := "printf '%s' \"$*\" > '" + captured + "'\nprintf '%s' '" + resultJSON(t, attempt, attempt.BasisTree) + "' > \"$SKYNEX_RESULT_FILE\""
	a := OpenCodeAdapter{Store: store, Options: OpenCodeOptions{Executable: fakeOpenCode(t, body), Timeout: time.Second}}
	if _, err := a.Run(context.Background(), OpenCodeRequest{InvocationID: "contract", Attempt: attempt, Seal: seal, Checks: []string{"go test ./..."}, Prompt: "do work"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(raw)
	for _, want := range []string{`"WorkflowID":"wf"`, `"AttemptID":"a1"`, `"NodeID":"slice_a"`, `"BaseCandidateOID":"` + attempt.BasisTree + `"`, `"Status":"completed"`, `"patch"`, `"Operations"`, `"Path"`, `"Data"`, `base64`, `Mode`, `Allowed paths: a.txt`, `Checks: go test ./...`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
}

func TestOpenCodeWorkerGetsWritableIsolatedRuntimeAndCredentials(t *testing.T) {
	_, store, seal, attempt := openCodeFixture(t)
	defer store.Close()
	home := t.TempDir()
	source := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"provider":"token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	body := `test -d "$XDG_DATA_HOME/opencode"
test -d "$XDG_CACHE_HOME"
test -d "$XDG_STATE_HOME"
test -w "$XDG_DATA_HOME/opencode"
test "$(cat "$XDG_DATA_HOME/opencode/auth.json")" = '{"provider":"token"}'
printf '%s' '` + resultJSON(t, attempt, attempt.BasisTree) + `' > "$SKYNEX_RESULT_FILE"`
	adapter := OpenCodeAdapter{Store: store, Options: OpenCodeOptions{Executable: fakeOpenCode(t, body), Timeout: time.Second}}
	if _, err := adapter.Run(context.Background(), OpenCodeRequest{InvocationID: "isolated-runtime", Attempt: attempt, Seal: seal}); err != nil {
		t.Fatal(err)
	}
}

func TestHermeticOpenCodeWorkerProjectsOnlyParentXDGAuthAndSanitizedConfig(t *testing.T) {
	parentData := t.TempDir()
	parentConfig := t.TempDir()
	ambientHome := t.TempDir()
	dataDir := filepath.Join(parentData, "opencode")
	configDir := filepath.Join(parentConfig, "opencode")
	ambientData := filepath.Join(ambientHome, ".local", "share", "opencode")
	for _, dir := range []string{dataDir, configDir, ambientData} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataDir, "auth.json"), []byte(`{"source":"parent-xdg"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"account.json":  `{"must":"not-copy"}`,
		"mcp-auth.json": `{"must":"not-copy"}`,
		"sessions.db":   "must-not-copy",
	} {
		if err := os.WriteFile(filepath.Join(dataDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ambientData, "auth.json"), []byte(`{"source":"ambient-home"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := `{"agent":{"workflow-worker":{"mode":"primary","prompt":"inline worker"}},"plugin":["ambient-plugin"],"mcp":{"neurox":{"type":"local","command":["neurox","mcp"]}}}`
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "ambient-extra.json"), []byte(`{"must":"not-copy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", ambientHome)
	t.Setenv("XDG_DATA_HOME", parentData)
	t.Setenv("XDG_CONFIG_HOME", parentConfig)
	t.Setenv("OPENCODE_DISABLE_PROJECT_CONFIG", "1")
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"plugin":["injected-plugin"]}`)

	runtimeParent := t.TempDir()
	result := filepath.Join(runtimeParent, "result.json")
	env, err := openCodeRuntimeEnv(runtimeParent, result, true)
	if err != nil {
		t.Fatal(err)
	}
	values := testEnvironmentMap(env)
	exactKeys := []string{"PATH", "HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "LANG", "LC_ALL", "TZ", "OPENCODE_DISABLE_PROJECT_CONFIG", "OPENCODE_DISABLE_MODELS_FETCH", "SKYNEX_RESULT_FILE"}
	if len(values) != len(exactKeys) {
		t.Fatalf("hermetic env keys=%v", values)
	}
	for _, key := range exactKeys {
		if _, ok := values[key]; !ok {
			t.Fatalf("hermetic env is missing %s: %v", key, values)
		}
	}
	for _, key := range []string{"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
		value := values[key]
		if value == "" || !filepath.IsAbs(value) || !strings.HasPrefix(value, runtimeParent+string(filepath.Separator)) {
			t.Fatalf("%s=%q is not a private runtime path", key, value)
		}
	}
	if values["HOME"] == ambientHome {
		t.Fatal("ambient HOME survived the hermetic profile")
	}
	if values["OPENCODE_DISABLE_PROJECT_CONFIG"] != "1" || values["OPENCODE_DISABLE_MODELS_FETCH"] != "1" {
		t.Fatalf("OpenCode isolation flags=%#v", values)
	}
	if _, ok := values["OPENCODE_CONFIG_CONTENT"]; ok {
		t.Fatal("ambient OPENCODE_CONFIG_CONTENT survived")
	}
	targetData := filepath.Join(values["XDG_DATA_HOME"], "opencode")
	auth, err := os.ReadFile(filepath.Join(targetData, "auth.json"))
	if err != nil || string(auth) != `{"source":"parent-xdg"}` {
		t.Fatalf("auth=%q err=%v", auth, err)
	}
	if info, statErr := os.Stat(filepath.Join(targetData, "auth.json")); statErr != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("projected auth mode=%v err=%v", info, statErr)
	}
	entries, err := os.ReadDir(targetData)
	if err != nil || len(entries) != 1 || entries[0].Name() != "auth.json" {
		t.Fatalf("copied data entries=%v err=%v", entries, err)
	}
	targetConfig := filepath.Join(values["XDG_CONFIG_HOME"], "opencode")
	entries, err = os.ReadDir(targetConfig)
	if err != nil || len(entries) != 1 || entries[0].Name() != "opencode.json" {
		t.Fatalf("copied config entries=%v err=%v", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(targetConfig, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]any
	if err := json.Unmarshal(raw, &projected); err != nil {
		t.Fatal(err)
	}
	if plugins, ok := projected["plugin"].([]any); !ok || len(plugins) != 0 {
		t.Fatalf("plugins=%#v", projected["plugin"])
	}
	if mcp, ok := projected["mcp"].(map[string]any); !ok || len(mcp) != 0 {
		t.Fatalf("mcp=%#v", projected["mcp"])
	}
	if !strings.Contains(string(raw), "inline worker") || strings.Contains(strings.ToLower(string(raw)), "neurox") {
		t.Fatalf("projected config=%s", raw)
	}

	args := workerOpenCodeArgs("/private/worktree", "prompt", OpenCodeOptions{Model: "openai/model", Agent: "workflow-worker"}, true)
	if len(args) < 2 || args[0] != "run" || args[1] != "--pure" || !strings.Contains(strings.Join(args, " "), "--agent workflow-worker") {
		t.Fatalf("hermetic argv=%q", args)
	}
	if legacy := workerOpenCodeArgs("/worktree", "prompt", OpenCodeOptions{}, false); strings.Contains(strings.Join(legacy, " "), "--pure") {
		t.Fatalf("legacy argv changed=%q", legacy)
	}
}

func TestHermeticOpenCodeWorkerResolvesOnlyExactConfiguredExecutable(t *testing.T) {
	worktree := t.TempDir()
	fixtureExecutable := filepath.Join(worktree, "fake-opencode")
	if err := os.WriteFile(fixtureExecutable, []byte("fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveHermeticOpenCodeExecutable("./fake-opencode", worktree)
	if err != nil || resolved != fixtureExecutable || !filepath.IsAbs(resolved) {
		t.Fatalf("resolved fixture executable=%q err=%v", resolved, err)
	}
	identity, err := snapshotHermeticOpenCodeExecutable(resolved)
	if err != nil || revalidateHermeticOpenCodeExecutable("./fake-opencode", worktree, resolved, identity) != nil {
		t.Fatalf("stable executable identity=%v err=%v", identity, err)
	}
	replacement := filepath.Join(worktree, "replacement-opencode")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixtureExecutable); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, fixtureExecutable); err != nil {
		t.Fatal(err)
	}
	if err := revalidateHermeticOpenCodeExecutable("./fake-opencode", worktree, resolved, identity); err == nil {
		t.Fatal("executable replacement passed launch revalidation")
	}

	absoluteExecutable := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(absoluteExecutable, []byte("fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err = resolveHermeticOpenCodeExecutable(absoluteExecutable, worktree)
	if err != nil || resolved != absoluteExecutable {
		t.Fatalf("resolved absolute executable=%q err=%v", resolved, err)
	}

	for _, declared := range []string{"", "opencode", "../opencode", "./../opencode", ".//fake-opencode", "./fake\\opencode", "/tmp/../tmp/opencode"} {
		if resolved, err := resolveHermeticOpenCodeExecutable(declared, worktree); err == nil {
			t.Fatalf("executable %q resolved to %q", declared, resolved)
		}
	}
	nonExecutable := filepath.Join(worktree, "non-executable")
	if err := os.WriteFile(nonExecutable, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resolved, err := resolveHermeticOpenCodeExecutable("./non-executable", worktree); err == nil {
		t.Fatalf("non-executable file resolved to %q", resolved)
	}
	symlink := filepath.Join(worktree, "linked-opencode")
	if err := os.Symlink("fake-opencode", symlink); err != nil {
		t.Fatal(err)
	}
	if resolved, err := resolveHermeticOpenCodeExecutable("./linked-opencode", worktree); err == nil {
		t.Fatalf("symlink resolved to %q", resolved)
	}
}

func testEnvironmentMap(env []string) map[string]string {
	result := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

func TestOpenCodeInvocationIsObservableWhileRunning(t *testing.T) {
	_, store, seal, attempt := openCodeFixture(t)
	defer store.Close()
	body := "echo incremental-progress\nsleep 2\nprintf '%s' '" + resultJSON(t, attempt, attempt.BasisTree) + "' > \"$SKYNEX_RESULT_FILE\""
	a := OpenCodeAdapter{Store: store, Options: OpenCodeOptions{Executable: fakeOpenCode(t, body), Timeout: time.Second}}
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), OpenCodeRequest{InvocationID: "observable", Attempt: attempt, Seal: seal})
		done <- err
	}()
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		var status, preview string
		var pid int
		var heartbeat string
		err := store.Database().QueryRow(`SELECT status,pid,heartbeat_at,stdout_preview FROM invocation_runtime WHERE invocation_id='observable'`).Scan(&status, &pid, &heartbeat, &preview)
		if err == nil && status == "running" && pid > 0 && heartbeat != "" && strings.Contains(preview, "incremental-progress") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runtime not observable status=%q pid=%d heartbeat=%q preview=%q err=%v", status, pid, heartbeat, preview, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	var status string
	if err := store.Database().QueryRow(`SELECT status FROM invocation_runtime WHERE invocation_id='observable'`).Scan(&status); err != nil || status != "timeout" {
		t.Fatalf("terminal=%q err=%v", status, err)
	}
}

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
			var status string
			if err = store.Database().QueryRow(`SELECT status FROM invocation_runtime WHERE invocation_id=?`, tc.name).Scan(&status); err != nil || status != tc.name {
				t.Fatalf("runtime terminal=%q err=%v", status, err)
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

func TestOpenCodeAdapterRejectsAgentFallback(t *testing.T) {
	_, store, seal, attempt := openCodeFixture(t)
	defer store.Close()
	body := `echo 'agent "research-orchestrator" is a subagent, not a primary agent. Falling back to default agent' >&2
printf '%s' '` + resultJSON(t, attempt, attempt.BasisTree) + `' > "$SKYNEX_RESULT_FILE"`
	adapter := OpenCodeAdapter{Store: store, Options: OpenCodeOptions{Executable: fakeOpenCode(t, body), Agent: "research-orchestrator", Timeout: time.Second}}
	_, err := adapter.Run(context.Background(), OpenCodeRequest{InvocationID: "agent-fallback", Attempt: attempt, Seal: seal})
	if !errors.Is(err, ErrAgentFallback) {
		t.Fatalf("err=%v", err)
	}
	var status, preview string
	if err := store.Database().QueryRow(`SELECT status,stderr_preview FROM invocation_runtime WHERE invocation_id='agent-fallback'`).Scan(&status, &preview); err != nil || status != "agent_rejected" || !strings.Contains(preview, "Falling back") {
		t.Fatalf("status=%q preview=%q err=%v", status, preview, err)
	}
}

func TestOpenCodeAdapterIdleProgressTimeout(t *testing.T) {
	_, store, seal, attempt := openCodeFixture(t)
	defer store.Close()
	adapter := OpenCodeAdapter{Store: store, Options: OpenCodeOptions{Executable: fakeOpenCode(t, "echo step_start\nsleep 2"), Timeout: time.Second, IdleTimeout: 60 * time.Millisecond}}
	started := time.Now()
	_, err := adapter.Run(context.Background(), OpenCodeRequest{InvocationID: "idle", Attempt: attempt, Seal: seal})
	if !errors.Is(err, ErrIdleProgressTimeout) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("idle watchdog was slow: %s", time.Since(started))
	}
	var status, preview string
	if err := store.Database().QueryRow(`SELECT status,stdout_preview FROM invocation_runtime WHERE invocation_id='idle'`).Scan(&status, &preview); err != nil || status != "idle_timeout" || !strings.Contains(preview, "step_start") {
		t.Fatalf("status=%q preview=%q err=%v", status, preview, err)
	}
}

func TestOpenCodeAdapterIdleWatchdogResetsOnRealOutput(t *testing.T) {
	_, store, seal, attempt := openCodeFixture(t)
	defer store.Close()
	body := `for step in 1 2 3 4; do echo "step_$step"; sleep 0.03; done
printf '%s' '` + resultJSON(t, attempt, attempt.BasisTree) + `' > "$SKYNEX_RESULT_FILE"`
	adapter := OpenCodeAdapter{Store: store, Options: OpenCodeOptions{Executable: fakeOpenCode(t, body), Timeout: time.Second, IdleTimeout: 55 * time.Millisecond}}
	if _, err := adapter.Run(context.Background(), OpenCodeRequest{InvocationID: "progressing", Attempt: attempt, Seal: seal}); err != nil {
		t.Fatalf("real output must reset watchdog: %v", err)
	}
}
