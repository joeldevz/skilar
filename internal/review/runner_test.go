package review

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/workflow"
)

func TestHermeticReviewRuntimeProjectsOnlyParentXDGAuthAndSanitizedConfig(t *testing.T) {
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
	config := `{"agent":{"workflow-reviewer":{"mode":"primary","prompt":"inline reviewer"}},"plugin":["ambient-plugin"],"mcp":{"neurox":{"type":"local","command":["neurox","mcp"]}}}`
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
	env, err := reviewOpenCodeRuntimeEnv(runtimeParent, result, true)
	if err != nil {
		t.Fatal(err)
	}
	values := reviewTestEnvironmentMap(env)
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
	if !strings.Contains(string(raw), "inline reviewer") || strings.Contains(strings.ToLower(string(raw)), "neurox") {
		t.Fatalf("projected config=%s", raw)
	}

	args := reviewOpenCodeArgs("/private/worktree", "prompt", "openai/model", true)
	if len(args) < 2 || args[0] != "run" || args[1] != "--pure" || !strings.Contains(strings.Join(args, " "), "--agent workflow-reviewer") {
		t.Fatalf("hermetic argv=%q", args)
	}
	if legacy := reviewOpenCodeArgs("/worktree", "prompt", "", false); strings.Contains(strings.Join(legacy, " "), "--pure") {
		t.Fatalf("legacy argv changed=%q", legacy)
	}
}

func TestHermeticReviewResolvesOnlyExactConfiguredExecutable(t *testing.T) {
	worktree := t.TempDir()
	fixtureExecutable := filepath.Join(worktree, "fake-opencode")
	if err := os.WriteFile(fixtureExecutable, []byte("fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveHermeticReviewExecutable("./fake-opencode", worktree)
	if err != nil || resolved != fixtureExecutable || !filepath.IsAbs(resolved) {
		t.Fatalf("resolved fixture executable=%q err=%v", resolved, err)
	}
	identity, err := snapshotHermeticReviewExecutable(resolved)
	if err != nil || revalidateHermeticReviewExecutable("./fake-opencode", worktree, resolved, identity) != nil {
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
	if err := revalidateHermeticReviewExecutable("./fake-opencode", worktree, resolved, identity); err == nil {
		t.Fatal("executable replacement passed launch revalidation")
	}

	absoluteExecutable := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(absoluteExecutable, []byte("fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err = resolveHermeticReviewExecutable(absoluteExecutable, worktree)
	if err != nil || resolved != absoluteExecutable {
		t.Fatalf("resolved absolute executable=%q err=%v", resolved, err)
	}

	for _, declared := range []string{"", "opencode", "../opencode", "./../opencode", ".//fake-opencode", "./fake\\opencode", "/tmp/../tmp/opencode"} {
		if resolved, err := resolveHermeticReviewExecutable(declared, worktree); err == nil {
			t.Fatalf("executable %q resolved to %q", declared, resolved)
		}
	}
	nonExecutable := filepath.Join(worktree, "non-executable")
	if err := os.WriteFile(nonExecutable, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resolved, err := resolveHermeticReviewExecutable("./non-executable", worktree); err == nil {
		t.Fatalf("non-executable file resolved to %q", resolved)
	}
	symlink := filepath.Join(worktree, "linked-opencode")
	if err := os.Symlink("fake-opencode", symlink); err != nil {
		t.Fatal(err)
	}
	if resolved, err := resolveHermeticReviewExecutable("./linked-opencode", worktree); err == nil {
		t.Fatalf("symlink resolved to %q", resolved)
	}
}

func reviewTestEnvironmentMap(env []string) map[string]string {
	result := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

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
	floor := RiskFloor{Risk: RiskLow}
	prompt := semanticPrompt(floor)
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
	if detail := validateSemanticOutput(got, floor, &parsed); detail != "" {
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

func TestSemanticPromptCarriesDeterministicMinimumAndRejectsLowerOutput(t *testing.T) {
	floor := RiskFloor{Risk: RiskMedium, Depth: DepthOneLens}
	prompt := semanticPrompt(floor)
	if !strings.Contains(prompt, `deterministic minimum risk is "medium"`) || !strings.Contains(prompt, `MUST be "medium" or higher`) {
		t.Fatalf("prompt does not carry the risk floor: %s", prompt)
	}
	var output semanticOutput
	detail := validateSemanticOutput([]byte(`{"requested_risk":"low","selected_lens":"","justification":"small change"}`), floor, &output)
	if !strings.Contains(detail, `below deterministic minimum "medium"`) {
		t.Fatalf("detail=%q", detail)
	}
	detail = validateSemanticOutput([]byte(`{"requested_risk":"medium","selected_lens":"reliability","justification":"respect floor"}`), floor, &output)
	if detail != "" {
		t.Fatalf("valid medium assessment rejected: %s", detail)
	}
}
