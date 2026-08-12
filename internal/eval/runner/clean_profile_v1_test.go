package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const runnerCleanProfileObservationFile = "SKYNEX_RUNNER_CLEAN_PROFILE_OBSERVATION"

type runnerCleanProfileObservation struct {
	Environment map[string]string   `json:"environment"`
	Files       map[string][]string `json:"files"`
	Auth        json.RawMessage     `json:"auth"`
	AuthMode    uint32              `json:"auth_mode"`
	Config      json.RawMessage     `json:"config"`
}

func TestOpenCodeFactoryUsesFreshProfileWithOnlyOpenAIOAuth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX executable wrapper")
	}
	ambientRoot := t.TempDir()
	ambientDirectories := map[string]string{
		"HOME":            filepath.Join(ambientRoot, "home"),
		"TMPDIR":          filepath.Join(ambientRoot, "tmp"),
		"XDG_CONFIG_HOME": filepath.Join(ambientRoot, "config"),
		"XDG_DATA_HOME":   filepath.Join(ambientRoot, "data"),
		"XDG_STATE_HOME":  filepath.Join(ambientRoot, "state"),
		"XDG_CACHE_HOME":  filepath.Join(ambientRoot, "cache"),
		"XDG_RUNTIME_DIR": filepath.Join(ambientRoot, "runtime"),
	}
	for key, directory := range ambientDirectories {
		if err := os.MkdirAll(filepath.Join(directory, "opencode"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "opencode", "ambient-"+strings.ToLower(key)), []byte("ambient"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, directory)
	}
	ambientAuth := filepath.Join(ambientDirectories["XDG_DATA_HOME"], "opencode", "auth.json")
	if err := os.WriteFile(ambientAuth, []byte(`{
		"anthropic":{"type":"oauth","access":"anthropic-access","refresh":"anthropic-refresh","expires":4102444800000},
		"openai":{"type":"oauth","access":"ambient-openai-access","refresh":"ambient-openai-refresh","expires":4102444800000,"accountId":"ambient-account"},
		"openrouter":{"type":"api","key":"openrouter-key"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_CONFIG", filepath.Join(ambientRoot, "ambient-opencode.json"))
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"plugin":["ambient-plugin"],"agent":{"ambient":{}},"mcp":{"ambient":{"enabled":true}}}`)
	t.Setenv("OPENCODE_DISABLE_PROJECT_CONFIG", "0")
	t.Setenv("OPENAI_API_KEY", "ambient-api-key")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-anthropic-key")

	authSource := filepath.Join(t.TempDir(), "auth.json")
	authSourceJSON := `{"openai":{"type":"oauth","access":"openai-access","refresh":"openai-refresh","expires":4102444800000,"accountId":"openai-account"}}`
	if err := os.WriteFile(authSource, []byte(authSourceJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	request, effective := openCodeFactoryRequest(t)
	request.Case.Completion.Timeout = "5m"
	observationPath := filepath.Join(t.TempDir(), "first-observation.json")
	factory := openCodeTestFactory(t, map[string]string{runnerCleanProfileObservationFile: observationPath})
	factory.OpenAIOAuthFile = authSource
	runtimeHandle, err := factory.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if err := runtimeHandle.Close(); err != nil {
		t.Fatal(err)
	}
	first := loadRunnerCleanProfileObservation(t, observationPath)
	assertRunnerCleanProfile(t, first, request, effective.Config)

	// Simulate state written by OpenCode during the first run. A subsequent
	// runtime under the same evaluator RunPath must receive different identity
	// directories and cannot see these sessions or cache entries.
	for key, relative := range map[string]string{
		"XDG_DATA_HOME":  "opencode/session/previous-run.json",
		"XDG_STATE_HOME": "opencode/session/previous-run.json",
		"XDG_CACHE_HOME": "opencode/previous-run.cache",
	} {
		path := filepath.Join(first.Environment[key], filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("must-not-cross-runs"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	secondPath := filepath.Join(t.TempDir(), "second-observation.json")
	factory.Env[runnerCleanProfileObservationFile] = secondPath
	secondHandle, err := factory.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	if err := secondHandle.Close(); err != nil {
		t.Fatal(err)
	}
	second := loadRunnerCleanProfileObservation(t, secondPath)
	assertRunnerCleanProfile(t, second, request, effective.Config)
	for _, key := range []string{"HOME", "TMPDIR", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		if filepath.Clean(first.Environment[key]) == filepath.Clean(second.Environment[key]) {
			t.Fatalf("%s was reused across runs: %q", key, first.Environment[key])
		}
	}
	for _, label := range []string{"state", "cache"} {
		if len(second.Files[label]) != 0 {
			t.Fatalf("second run inherited %s: %v", label, second.Files[label])
		}
	}
	if strings.Contains(strings.Join(second.Files["data"], "\n"), "previous-run") {
		t.Fatalf("second run inherited data/session state: %v", second.Files["data"])
	}
	sourceAfter, err := os.ReadFile(authSource)
	if err != nil {
		t.Fatal(err)
	}
	if string(sourceAfter) != authSourceJSON {
		t.Fatal("dedicated OAuth source was mutated across runs")
	}
	if info, err := os.Stat(authSource); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("dedicated OAuth source mode changed to %o", info.Mode().Perm())
	}
}

func writeRunnerCleanProfileObservation(path string, config []byte) error {
	keys := []string{
		"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT",
		"OPENCODE_DISABLE_PROJECT_CONFIG",
		"OPENCODE_DISABLE_MODELS_FETCH",
	}
	environment := make(map[string]string, len(keys))
	for _, key := range keys {
		environment[key] = os.Getenv(key)
	}
	files := make(map[string][]string, 6)
	for label, key := range map[string]string{
		"home": "HOME", "config": "XDG_CONFIG_HOME", "data": "XDG_DATA_HOME", "state": "XDG_STATE_HOME", "cache": "XDG_CACHE_HOME", "runtime": "XDG_RUNTIME_DIR",
	} {
		entries, err := runnerRelativeTree(os.Getenv(key))
		if err != nil {
			return err
		}
		files[label] = entries
	}
	authPath := filepath.Join(os.Getenv("XDG_DATA_HOME"), "opencode", "auth.json")
	auth, err := os.ReadFile(authPath)
	if err != nil {
		return err
	}
	info, err := os.Lstat(authPath)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(runnerCleanProfileObservation{
		Environment: environment, Files: files, Auth: auth,
		AuthMode: uint32(info.Mode().Perm()), Config: append([]byte(nil), config...),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}

func loadRunnerCleanProfileObservation(t *testing.T, path string) runnerCleanProfileObservation {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var observation runnerCleanProfileObservation
	if err := json.Unmarshal(raw, &observation); err != nil {
		t.Fatal(err)
	}
	return observation
}

func assertRunnerCleanProfile(t *testing.T, observation runnerCleanProfileObservation, request RuntimeRequest, expectedConfig []byte) {
	t.Helper()
	if filepath.Clean(observation.Environment["XDG_CONFIG_HOME"]) != filepath.Clean(request.ConfigRoot) {
		t.Fatalf("runtime XDG_CONFIG_HOME = %q, want %q", observation.Environment["XDG_CONFIG_HOME"], request.ConfigRoot)
	}
	for _, key := range []string{"HOME", "TMPDIR", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		assertRunnerPrivateDirectory(t, request.RunPath, observation.Environment[key])
	}
	if got := observation.Environment["OPENCODE_DISABLE_PROJECT_CONFIG"]; got != "1" {
		t.Fatalf("runtime OPENCODE_DISABLE_PROJECT_CONFIG = %q, want evaluator-owned 1", got)
	}
	if got := observation.Environment["OPENCODE_DISABLE_MODELS_FETCH"]; got != "1" {
		t.Fatalf("runtime OPENCODE_DISABLE_MODELS_FETCH = %q, want evaluator-owned 1", got)
	}
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT"} {
		if value := observation.Environment[key]; value != "" {
			t.Fatalf("ambient environment %s leaked as %q", key, value)
		}
	}
	if !reflect.DeepEqual([]byte(observation.Config), expectedConfig) {
		t.Fatal("runtime did not load only the evaluator-owned effective config")
	}
	if strings.Contains(string(observation.Config), "ambient-plugin") || strings.Contains(string(observation.Config), `"ambient"`) {
		t.Fatal("ambient plugin/MCP/agent survived in effective config")
	}
	var config map[string]any
	if err := json.Unmarshal(observation.Config, &config); err != nil {
		t.Fatal(err)
	}
	plugins, ok := config["plugin"].([]any)
	if !ok || len(plugins) != 0 {
		t.Fatalf("effective plugins = %#v", config["plugin"])
	}
	assertRunnerOnlyOpenAIOAuth(t, observation.Auth)
	if observation.AuthMode != 0o600 {
		t.Fatalf("runtime auth.json mode = %o, want 600", observation.AuthMode)
	}
	if got := observation.Files["home"]; len(got) != 0 {
		t.Fatalf("runtime HOME is not clean: %v", got)
	}
	if got := observation.Files["state"]; len(got) != 0 {
		t.Fatalf("runtime state is not clean: %v", got)
	}
	if got := observation.Files["cache"]; len(got) != 0 {
		t.Fatalf("runtime cache is not clean: %v", got)
	}
	if got := observation.Files["runtime"]; len(got) != 0 {
		t.Fatalf("runtime XDG runtime directory is not clean: %v", got)
	}
	if got := observation.Files["data"]; !reflect.DeepEqual(got, []string{"opencode/", "opencode/auth.json"}) {
		t.Fatalf("runtime data contains non-auth state: %v", got)
	}
}

func assertRunnerOnlyOpenAIOAuth(t *testing.T, raw []byte) {
	t.Helper()
	var providers map[string]map[string]any
	if err := json.Unmarshal(raw, &providers); err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers["openai"] == nil {
		t.Fatalf("runtime auth providers = %v, want only openai", sortedRunnerProviderKeys(providers))
	}
	if providers["openai"]["type"] != "oauth" {
		t.Fatalf("runtime OpenAI credential type = %v", providers["openai"]["type"])
	}
	for _, foreignSecret := range []string{"anthropic-access", "anthropic-refresh", "ambient-openai-access", "ambient-openai-refresh", "openrouter-key"} {
		if strings.Contains(string(raw), foreignSecret) {
			t.Fatal("foreign provider secret was copied into runtime auth.json")
		}
	}
}

func assertRunnerPrivateDirectory(t *testing.T, runRoot, path string) {
	t.Helper()
	relative, err := filepath.Rel(runRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("private path %q is outside run root %q", path, runRoot)
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		// lifecycle may already have removed the credential-bearing private
		// directory after the process stopped; containment was recorded by the
		// helper while it was live.
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("private path %q mode = %v", path, info.Mode())
	}
}

func runnerRelativeTree(root string) ([]string, error) {
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			relative += "/"
		}
		entries = append(entries, relative)
		return nil
	})
	sort.Strings(entries)
	return entries, err
}

func sortedRunnerProviderKeys(values map[string]map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
