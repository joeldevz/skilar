//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	cleanProfileHelperEnabled = "SKYNEX_CLEAN_PROFILE_HELPER"
	cleanProfileHelperBinary  = "SKYNEX_CLEAN_PROFILE_TEST_BINARY"
)

type cleanProfileObservation struct {
	Args        []string            `json:"args"`
	Environment map[string]string   `json:"environment"`
	Files       map[string][]string `json:"files"`
	Auth        json.RawMessage     `json:"auth"`
	AuthMode    uint32              `json:"auth_mode"`
}

// TestCleanProfileLifecycleHelperProcess is a loopback-only fake OpenCode
// server. It never implements session or completion routes; its sole purpose
// is to expose the child process's filesystem/environment view to this test.
func TestCleanProfileLifecycleHelperProcess(t *testing.T) {
	if os.Getenv(cleanProfileHelperEnabled) != "1" {
		return
	}
	port := argumentValue(os.Args, "--port")
	hostname := argumentValue(os.Args, "--hostname")
	if hostname == "" {
		hostname = "127.0.0.1"
	}
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatalf("invalid helper port %q: %v", port, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, request *http.Request) {
		if !validHelperServerAuth(request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"healthy":true,"version":"clean-profile-v1"}`)
	})
	mux.HandleFunc("/observation", func(w http.ResponseWriter, request *http.Request) {
		if !validHelperServerAuth(request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		observation, err := observeCleanProfileProcess()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(observation)
	})

	listener, err := net.Listen("tcp", net.JoinHostPort(hostname, port))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: mux}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serve: %v", err)
	}
}

func TestServerStartsWithCleanConfigAndOnlyOpenAIOAuth(t *testing.T) {
	ambient := materializeAmbientOpenCodeProfile(t)
	for key, value := range ambient.environment {
		t.Setenv(key, value)
	}
	t.Setenv("OPENAI_API_KEY", "ambient-openai-api-key")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-anthropic-api-key")
	t.Setenv("OPENCODE_CONFIG", filepath.Join(ambient.root, "override.json"))
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"plugin":["ambient-inline-plugin"]}`)
	t.Setenv("OPENCODE_DISABLE_PROJECT_CONFIG", "0")
	t.Setenv("CODEX_HOME", filepath.Join(ambient.root, "codex"))
	t.Setenv("NODE_OPTIONS", "--require="+filepath.Join(ambient.root, "ambient-plugin.js"))

	configHome := t.TempDir()
	configDir := filepath.Join(configHome, "opencode")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(`{"agent":{"orchestrator":{"model":"openai/gpt-5"}},"plugin":[],"mcp":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	authSource := filepath.Join(t.TempDir(), "auth.json")
	validUntil := time.Now().Add(24 * time.Hour).UnixMilli()
	authSourceJSON := fmt.Sprintf(`{"openai":{"type":"oauth","access":"openai-access","refresh":"openai-refresh","expires":%d,"accountId":"openai-account"}}`, validUntil)
	if err := os.WriteFile(authSource, []byte(authSourceJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceBefore, err := os.ReadFile(authSource)
	if err != nil {
		t.Fatal(err)
	}
	runDir := t.TempDir()
	workDir := t.TempDir()
	srv := NewServerWithConfig(Config{
		Binary:                     cleanProfileWrapper(t),
		WorkDir:                    workDir,
		RunDir:                     runDir,
		ConfigHome:                 configHome,
		OpenAIOAuthFile:            authSource,
		OpenAIOAuthMinimumValidity: time.Hour,
		Env: map[string]string{
			cleanProfileHelperEnabled: "1",
			cleanProfileHelperBinary:  mustAbsoluteTestBinary(t),
		},
		ExpectedVersion: "clean-profile-v1",
		Timeout:         5 * time.Second,
		HealthTimeout:   time.Second,
		HealthInterval:  10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Stop()
	})

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.BaseURL()+"/observation", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(evaluatorServerUsername, srv.serverPassword)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("observation status %d: %s", response.StatusCode, body)
	}
	var observation cleanProfileObservation
	if err := json.NewDecoder(response.Body).Decode(&observation); err != nil {
		t.Fatal(err)
	}

	if !containsAdjacent(observation.Args, "serve", "--pure") {
		t.Fatalf("OpenCode did not start in pure mode: argv=%v", observation.Args)
	}
	if got := observation.Environment["XDG_CONFIG_HOME"]; filepath.Clean(got) != filepath.Clean(configHome) {
		t.Fatalf("XDG_CONFIG_HOME = %q, want controlled config home %q", got, configHome)
	}
	for _, key := range []string{"HOME", "TMPDIR", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		assertPrivateDirectory(t, runDir, observation.Environment[key])
		if filepath.Clean(observation.Environment[key]) == filepath.Clean(ambient.environment[key]) {
			t.Fatalf("%s reused ambient directory %q", key, observation.Environment[key])
		}
	}
	if got := observation.Environment["OPENCODE_DISABLE_PROJECT_CONFIG"]; got != "1" {
		t.Fatalf("OPENCODE_DISABLE_PROJECT_CONFIG = %q, want evaluator-owned 1", got)
	}
	if got := observation.Environment["OPENCODE_DISABLE_MODELS_FETCH"]; got != "1" {
		t.Fatalf("OPENCODE_DISABLE_MODELS_FETCH = %q, want evaluator-owned 1", got)
	}
	for _, key := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENCODE_CONFIG",
		"OPENCODE_CONFIG_CONTENT", "CODEX_HOME", "NODE_OPTIONS",
	} {
		if value := observation.Environment[key]; value != "" {
			t.Fatalf("ambient variable %s leaked into clean process as %q", key, value)
		}
	}

	assertOnlyOpenAIOAuth(t, observation.Auth, sourceBefore)
	if observation.AuthMode != 0o600 {
		t.Fatalf("private auth.json mode = %o, want 600", observation.AuthMode)
	}
	if got := observation.Files["config"]; !reflect.DeepEqual(got, []string{"opencode/", "opencode/opencode.json"}) {
		t.Fatalf("clean config files = %v", got)
	}
	if got := observation.Files["home"]; len(got) != 0 {
		t.Fatalf("ambient HOME state survived: %v", got)
	}
	if got := observation.Files["state"]; len(got) != 0 {
		t.Fatalf("ambient sessions/state survived: %v", got)
	}
	if got := observation.Files["cache"]; len(got) != 0 {
		t.Fatalf("ambient cache survived: %v", got)
	}
	if got := observation.Files["runtime"]; len(got) != 0 {
		t.Fatalf("ambient runtime state survived: %v", got)
	}
	if got := observation.Files["data"]; !reflect.DeepEqual(got, []string{"opencode/", "opencode/auth.json"}) {
		t.Fatalf("private data profile contains more than OpenAI auth: %v", got)
	}
	cancel()
	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "TMPDIR", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		if _, err := os.Stat(observation.Environment[key]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("credential-bearing private %s was not removed: %v", key, err)
		}
	}

	sourceAfter, err := os.ReadFile(authSource)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sourceAfter, sourceBefore) {
		t.Fatal("source auth.json was mutated")
	}
	sourceInfo, err := os.Stat(authSource)
	if err != nil {
		t.Fatal(err)
	}
	if sourceInfo.Mode().Perm() != 0o600 {
		t.Fatalf("source auth.json mode changed to %o", sourceInfo.Mode().Perm())
	}
}

func validHelperServerAuth(request *http.Request) bool {
	username, password, ok := request.BasicAuth()
	return ok && username == os.Getenv("OPENCODE_SERVER_USERNAME") &&
		password != "" && password == os.Getenv("OPENCODE_SERVER_PASSWORD")
}

func TestOpenAIOAuthSourceFailsClosedBeforeStartingProcess(t *testing.T) {
	validExpiry := time.Now().Add(24 * time.Hour).UnixMilli()
	valid := fmt.Sprintf(`{"openai":{"type":"oauth","access":"access","refresh":"refresh","expires":%d,"accountId":"account"}}`, validExpiry)
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		mutate  func(*testing.T, string) string
	}{
		{name: "malformed JSON", content: `{"openai":`, mode: 0o600},
		{name: "missing OpenAI", content: `{"anthropic":{"type":"oauth","access":"a","refresh":"r","expires":4102444800000}}`, mode: 0o600},
		{name: "multiple providers", content: fmt.Sprintf(`{"openai":{"type":"oauth","access":"access","refresh":"refresh","expires":%d,"accountId":"account"},"anthropic":{"type":"api","key":"foreign"}}`, validExpiry), mode: 0o600},
		{name: "OpenAI API key", content: `{"openai":{"type":"api","key":"secret"}}`, mode: 0o600},
		{name: "missing refresh", content: `{"openai":{"type":"oauth","access":"access","expires":4102444800000,"accountId":"account"}}`, mode: 0o600},
		{name: "unexpected OpenAI field", content: `{"openai":{"type":"oauth","access":"access","refresh":"refresh","expires":4102444800000,"accountId":"account","apiKey":"must-not-copy"}}`, mode: 0o600},
		{name: "already expired", content: fmt.Sprintf(`{"openai":{"type":"oauth","access":"access","refresh":"refresh","expires":%d,"accountId":"account"}}`, time.Now().Add(-time.Minute).UnixMilli()), mode: 0o600},
		{name: "expires before run horizon", content: fmt.Sprintf(`{"openai":{"type":"oauth","access":"access","refresh":"refresh","expires":%d,"accountId":"account"}}`, time.Now().Add(time.Minute).UnixMilli()), mode: 0o600},
		{name: "public permissions", content: valid, mode: 0o644},
		{name: "symlink", content: valid, mode: 0o600, mutate: func(t *testing.T, path string) string {
			link := filepath.Join(t.TempDir(), "auth-link.json")
			if err := os.Symlink(path, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(source, []byte(test.content), test.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(source, test.mode); err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				source = test.mutate(t, source)
			}
			marker := filepath.Join(t.TempDir(), "process-started")
			binary := filepath.Join(t.TempDir(), "must-not-start")
			script := "#!/bin/sh\ntouch \"" + marker + "\"\nexit 97\n"
			if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			srv := NewServerWithConfig(Config{
				Binary: binary, WorkDir: t.TempDir(), RunDir: t.TempDir(),
				ConfigHome: t.TempDir(), OpenAIOAuthFile: source, OpenAIOAuthMinimumValidity: time.Hour,
				Timeout: 250 * time.Millisecond, HealthInterval: 10 * time.Millisecond,
			})
			if err := srv.Start(context.Background()); err == nil {
				_ = srv.Stop()
				t.Fatal("unsafe OAuth source was accepted")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("OpenCode process started before rejecting OAuth source: %v", err)
			}
		})
	}
}

type ambientProfile struct {
	root        string
	environment map[string]string
}

func materializeAmbientOpenCodeProfile(t *testing.T) ambientProfile {
	t.Helper()
	root := t.TempDir()
	directories := map[string]string{
		"HOME":            filepath.Join(root, "home"),
		"TMPDIR":          filepath.Join(root, "tmp"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_DATA_HOME":   filepath.Join(root, "data"),
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_CACHE_HOME":  filepath.Join(root, "cache"),
		"XDG_RUNTIME_DIR": filepath.Join(root, "runtime"),
	}
	for _, directory := range directories {
		if err := os.MkdirAll(filepath.Join(directory, "opencode"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(directories["HOME"], ".opencode", "agents", "ambient.md"):                 "ambient-agent",
		filepath.Join(directories["XDG_CONFIG_HOME"], "opencode", "opencode.json"):              `{"plugin":["ambient-plugin"],"mcp":{"ambient":{"enabled":true}},"agent":{"ambient":{}}}`,
		filepath.Join(directories["XDG_STATE_HOME"], "opencode", "sessions", "ambient-session"): "ambient-session",
		filepath.Join(directories["XDG_CACHE_HOME"], "opencode", "ambient-cache"):               "ambient-cache",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	authFile := filepath.Join(directories["XDG_DATA_HOME"], "opencode", "auth.json")
	auth := `{
		"anthropic":{"type":"oauth","access":"anthropic-access","refresh":"anthropic-refresh","expires":4102444800000},
		"openai":{"type":"oauth","access":"ambient-openai-access","refresh":"ambient-openai-refresh","expires":4102444800000,"accountId":"ambient-openai-account"},
		"openrouter":{"type":"api","key":"openrouter-secret"}
	}`
	if err := os.WriteFile(authFile, []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	return ambientProfile{root: root, environment: directories}
}

func observeCleanProfileProcess() (cleanProfileObservation, error) {
	keys := []string{
		"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "CODEX_HOME", "NODE_OPTIONS",
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
		entries, err := relativeTree(os.Getenv(key))
		if err != nil {
			return cleanProfileObservation{}, err
		}
		files[label] = entries
	}
	authPath := filepath.Join(os.Getenv("XDG_DATA_HOME"), "opencode", "auth.json")
	auth, err := os.ReadFile(authPath)
	if err != nil {
		return cleanProfileObservation{}, err
	}
	info, err := os.Lstat(authPath)
	if err != nil {
		return cleanProfileObservation{}, err
	}
	return cleanProfileObservation{
		Args: append([]string(nil), os.Args...), Environment: environment,
		Files: files, Auth: auth, AuthMode: uint32(info.Mode().Perm()),
	}, nil
}

func relativeTree(root string) ([]string, error) {
	var result []string
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
		result = append(result, relative)
		return nil
	})
	sort.Strings(result)
	return result, err
}

func assertOnlyOpenAIOAuth(t *testing.T, destination, source []byte) {
	t.Helper()
	var got, original map[string]json.RawMessage
	if err := json.Unmarshal(destination, &got); err != nil {
		t.Fatalf("decode private auth.json: %v", err)
	}
	if err := json.Unmarshal(source, &original); err != nil {
		t.Fatalf("decode source auth.json: %v", err)
	}
	if len(got) != 1 || got["openai"] == nil {
		t.Fatalf("private auth providers = %v, want only openai", sortedRawKeys(got))
	}
	var gotOpenAI, sourceOpenAI map[string]any
	if err := json.Unmarshal(got["openai"], &gotOpenAI); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(original["openai"], &sourceOpenAI); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotOpenAI, sourceOpenAI) {
		t.Fatalf("OpenAI OAuth entry changed while filtering: got fields %v, want %v", sortedAnyKeys(gotOpenAI), sortedAnyKeys(sourceOpenAI))
	}
	if gotOpenAI["type"] != "oauth" {
		t.Fatalf("private OpenAI credential type = %v, want oauth", gotOpenAI["type"])
	}
	for _, forbidden := range []string{"anthropic-access", "anthropic-refresh", "ambient-openai-access", "ambient-openai-refresh", "openrouter-secret"} {
		if strings.Contains(string(destination), forbidden) {
			t.Fatalf("foreign provider secret survived in private auth.json")
		}
	}
}

func sortedRawKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedAnyKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func argumentValue(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func containsAdjacent(values []string, first, second string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == first && values[index+1] == second {
			return true
		}
	}
	return false
}

func cleanProfileWrapper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode-clean-profile")
	script := "#!/bin/sh\nexec \"$" + cleanProfileHelperBinary + "\" -test.run=^TestCleanProfileLifecycleHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustAbsoluteTestBinary(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return path
}
