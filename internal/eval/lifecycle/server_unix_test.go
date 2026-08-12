//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const helperProcessEnv = "SKYNEX_LIFECYCLE_HELPER_PROCESS"

// TestLifecycleHelperProcess is re-executed as the fake OpenCode server. It
// also starts a SIGTERM-ignoring descendant so the parent test can prove that
// Stop cleans up the process group, not only the direct server PID.
func TestLifecycleHelperProcess(t *testing.T) {
	if os.Getenv(helperProcessEnv) != "1" {
		return
	}
	if len(os.Args) < 2 {
		t.Fatal("missing helper port")
	}
	port := os.Args[len(os.Args)-1]
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatalf("invalid helper port %q: %v", port, err)
	}
	if os.Getenv("SKYNEX_LIFECYCLE_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}

	childScript := "exec sleep 600"
	if os.Getenv("SKYNEX_LIFECYCLE_CHILD_IGNORE_TERM") == "1" {
		childScript = "trap '' TERM; exec sleep 600"
	}
	child := exec.Command("/bin/sh", "-c", childScript)
	if err := child.Start(); err != nil {
		t.Fatalf("start helper descendant: %v", err)
	}
	pidFile := os.Getenv("SKYNEX_LIFECYCLE_CHILD_PID_FILE")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		t.Fatalf("write child pid: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"healthy":true,"version":"test-v1"}`)
	})
	mux.HandleFunc("/path", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"directory": cwd})
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"inherited":    os.Getenv("SKYNEX_LIFECYCLE_NOT_ALLOWED"),
			"override":     os.Getenv("SKYNEX_LIFECYCLE_OVERRIDE"),
			"home":         os.Getenv("HOME"),
			"tmpdir":       os.Getenv("TMPDIR"),
			"xdg_config":   os.Getenv("XDG_CONFIG_HOME"),
			"xdg_data":     os.Getenv("XDG_DATA_HOME"),
			"xdg_state":    os.Getenv("XDG_STATE_HOME"),
			"xdg_cache":    os.Getenv("XDG_CACHE_HOME"),
			"mcp_manifest": os.Getenv("SKYNEX_EVAL_MCP_PROXY_MANIFEST"),
		})
	})
	mux.HandleFunc("/agent", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `[{"name":"fake-agent"}]`)
	})
	mux.HandleFunc("/experimental/tool/ids", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `["read","write"]`)
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/provider", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"all":[{"id":"test","models":{"model":{"id":"model","providerID":"test"}}}],"default":{"test":"model"},"connected":["test"]}`)
	})
	mux.HandleFunc("/doc", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"openapi":"3.1.0","info":{"version":"test-v1"}}`)
	})

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: mux}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serve: %v", err)
	}
}

func TestServerBindsRunCWDProbesAndCleansEntireProcessGroup(t *testing.T) {
	workDir := t.TempDir()
	runDir := t.TempDir()
	pidFile := filepath.Join(runDir, "child.pid")
	manifestPath := filepath.Join(runDir, "mcp-proxy-manifest.json")
	if err := os.WriteFile(manifestPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKYNEX_LIFECYCLE_NOT_ALLOWED", "must-not-leak")

	ctx, cancel := context.WithCancel(context.Background())
	srv := NewServerWithConfig(Config{
		Port: 0, Hostname: "127.0.0.1", Binary: os.Args[0],
		CommandArgs: []string{"-test.run=^TestLifecycleHelperProcess$", "--", "{port}"},
		WorkDir:     workDir, RunDir: runDir,
		EnvAllowlist: []string{"PATH"},
		Env: map[string]string{
			helperProcessEnv:                     "1",
			"SKYNEX_LIFECYCLE_CHILD_PID_FILE":    pidFile,
			"SKYNEX_LIFECYCLE_CHILD_IGNORE_TERM": "1",
			"SKYNEX_LIFECYCLE_IGNORE_TERM":       "1",
			"SKYNEX_LIFECYCLE_OVERRIDE":          "explicit",
		},
		Timeout: 5 * time.Second, ShutdownTimeout: 200 * time.Millisecond,
		HealthTimeout: time.Second, HealthInterval: 10 * time.Millisecond,
		ExpectedVersion:  "test-v1",
		MCPProxyManifest: manifestPath,
	})
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Stop()
	})

	if !srv.IsRunning() || srv.Port() == 0 || !strings.HasPrefix(srv.BaseURL(), "http://127.0.0.1:") {
		t.Fatalf("server state: running=%v port=%d URL=%q", srv.IsRunning(), srv.Port(), srv.BaseURL())
	}
	if srv.Version() != "test-v1" {
		t.Fatalf("version = %q", srv.Version())
	}
	if srv.RunDir() != runDir {
		t.Fatalf("run dir = %q, want %q", srv.RunDir(), runDir)
	}
	logInfo, err := os.Stat(srv.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := logInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("log mode = %o, want 600", got)
	}

	evidence, err := srv.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Health.Version != "test-v1" || evidence.OpenAPI.SHA256 == "" || evidence.Config.SHA256 == "" || evidence.Tools.SHA256 == "" || evidence.MCP.SHA256 == "" || evidence.Providers.SHA256 == "" {
		t.Fatalf("probe evidence = %#v", evidence)
	}
	var effectiveEnv map[string]string
	if err := json.Unmarshal(evidence.Config.Body, &effectiveEnv); err != nil {
		t.Fatal(err)
	}
	if effectiveEnv["inherited"] != "" || effectiveEnv["override"] != "explicit" {
		t.Fatalf("effective env = %#v", effectiveEnv)
	}
	if effectiveEnv["mcp_manifest"] != manifestPath {
		t.Fatalf("internal MCP manifest env = %q, want %q", effectiveEnv["mcp_manifest"], manifestPath)
	}
	for _, key := range []string{"home", "tmpdir", "xdg_config", "xdg_data", "xdg_state", "xdg_cache"} {
		assertPrivateDirectory(t, runDir, effectiveEnv[key])
	}

	childPIDBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(string(childPIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	if !processAliveNonZombie(childPID) {
		t.Fatalf("helper child %d was not alive before Stop", childPID)
	}
	srv.mu.RLock()
	serverPID := srv.pid
	srv.mu.RUnlock()
	childGroup, err := syscall.Getpgid(childPID)
	if err != nil || childGroup != serverPID {
		t.Fatalf("helper child pgid = %d, lifecycle pgid = %d, err = %v", childGroup, serverPID, err)
	}

	stopStarted := time.Now()
	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(stopStarted); elapsed < 180*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("forced process-group stop took %s", elapsed)
	}
	cancel()
	if err := srv.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if srv.IsRunning() {
		t.Fatal("server still reports running")
	}
	if !waitUntil(2*time.Second, func() bool { return !processAliveNonZombie(childPID) }) {
		t.Fatalf("descendant %d survived process-group cleanup", childPID)
	}
}

func TestServerGracefulStopReapsBeforeGroupAttestation(t *testing.T) {
	workDir := t.TempDir()
	runDir := t.TempDir()
	pidFile := filepath.Join(runDir, "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	srv := NewServerWithConfig(Config{
		Port: 0, Hostname: "127.0.0.1", Binary: os.Args[0],
		CommandArgs:  []string{"-test.run=^TestLifecycleHelperProcess$", "--", "{port}"},
		WorkDir:      workDir,
		RunDir:       runDir,
		EnvAllowlist: []string{"PATH"},
		Env: map[string]string{
			helperProcessEnv:                  "1",
			"SKYNEX_LIFECYCLE_CHILD_PID_FILE": pidFile,
		},
		Timeout: 5 * time.Second, ShutdownTimeout: 2 * time.Second,
		HealthTimeout: time.Second, HealthInterval: 10 * time.Millisecond,
		ExpectedVersion: "test-v1",
	})
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srv.Stop()
	})
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(string(raw))
	if err != nil || !processAliveNonZombie(childPID) {
		t.Fatalf("graceful helper child = %d, err = %v", childPID, err)
	}

	stopStarted := time.Now()
	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(stopStarted); elapsed >= time.Second {
		t.Fatalf("graceful stop consumed shutdown escalation window: %s", elapsed)
	}
	if processGroupAlive(srv.pid) || processAliveNonZombie(childPID) {
		t.Fatalf("graceful stop left process group %d or child %d live", srv.pid, childPID)
	}
}

func TestServerRejectsUncontainedOrUnprotectedInternalMCPManifest(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, string) string
	}{
		{
			name: "outside run",
			prepare: func(t *testing.T, _ string) string {
				path := filepath.Join(t.TempDir(), "manifest.json")
				if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "unsafe mode",
			prepare: func(t *testing.T, runDir string) string {
				path := filepath.Join(runDir, "manifest.json")
				if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runDir := t.TempDir()
			srv := NewServerWithConfig(Config{
				Binary: os.Args[0], WorkDir: t.TempDir(), RunDir: runDir,
				CommandArgs:      []string{"-test.run=^TestLifecycleHelperProcess$", "--", "{port}"},
				MCPProxyManifest: test.prepare(t, runDir),
			})
			if err := srv.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "protected MCP proxy manifest") {
				t.Fatalf("Start error = %v", err)
			}
		})
	}
}

func TestStopDuringLaunchWaitsForPIDPublicationAndReapsProcess(t *testing.T) {
	workDir := t.TempDir()
	runDir := t.TempDir()
	pidFile := filepath.Join(runDir, "child.pid")
	entered := make(chan struct{})
	release := make(chan struct{})

	srv := NewServerWithConfig(Config{
		Port: 0, Hostname: "127.0.0.1", Binary: os.Args[0],
		CommandArgs: []string{"-test.run=^TestLifecycleHelperProcess$", "--", "{port}"},
		WorkDir:     workDir, RunDir: runDir,
		EnvAllowlist: []string{"PATH"},
		Env: map[string]string{
			helperProcessEnv:                  "1",
			"SKYNEX_LIFECYCLE_CHILD_PID_FILE": pidFile,
		},
		Timeout: 5 * time.Second, ShutdownTimeout: 200 * time.Millisecond,
		HealthTimeout: time.Second, HealthInterval: 10 * time.Millisecond,
		ExpectedVersion: "test-v1",
	})
	srv.beforeCommandStart = func() {
		close(entered)
		<-release
	}

	startDone := make(chan error, 1)
	go func() { startDone <- srv.Start(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not reach the command launch fence")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- srv.Stop() }()
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before the launch transaction published a PID: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop during launch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not finish after launch was released")
	}

	select {
	case <-startDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after concurrent Stop")
	}

	srv.mu.RLock()
	pid := srv.pid
	state := srv.state
	srv.mu.RUnlock()
	if pid <= 0 {
		t.Fatal("the command PID was never published")
	}
	if state != stateStopped {
		t.Fatalf("server state = %d, want stopped", state)
	}
	if !waitUntil(2*time.Second, func() bool { return !processAliveNonZombie(pid) }) {
		t.Fatalf("server process %d survived Stop during launch", pid)
	}
}

func TestDefaultArgumentsArePureAndGenericImpureOptOutFailsClosed(t *testing.T) {
	t.Parallel()
	srv := NewServerWithConfig(Config{ExtraArgs: []string{"--log-level", "ERROR"}})
	srv.mu.Lock()
	srv.port = 4321
	srv.workDir = "/fixture"
	got := srv.commandArgsLocked()
	srv.mu.Unlock()
	want := []string{"serve", "--pure", "--port", "4321", "--hostname", "127.0.0.1", "--log-level", "ERROR"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("default argv = %v, want %v", got, want)
	}

	impure := NewServerWithConfig(Config{AllowImpure: true})
	impure.mu.Lock()
	impure.port = 4321
	got = impure.commandArgsLocked()
	impure.mu.Unlock()
	if !strings.Contains(strings.Join(got, " "), "--pure") {
		t.Fatalf("uncontrolled impure argv = %v", got)
	}
	if err := verifyControlledPluginBoundary(impure.cfg); err == nil || !strings.Contains(err.Error(), "uncontrolled") {
		t.Fatalf("generic AllowImpure boundary error = %v", err)
	}
}

func TestControlledPluginIsOnlyPureOptOutAndRequiresExactConfig(t *testing.T) {
	pluginPath := filepath.Join(t.TempDir(), "skynex-workflow.ts")
	content := []byte("export default async function workflow() {}\n")
	if err := os.WriteFile(pluginPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	identity := &ControlledPluginIdentity{Path: pluginPath, ContentDigest: "sha256:" + hex.EncodeToString(sum[:])}
	configHome := t.TempDir()
	configDir := filepath.Join(configHome, "opencode")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pluginURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(pluginPath)}).String()
	writeConfig := func(plugins []string) {
		raw, err := json.Marshal(map[string]any{"plugin": plugins})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig([]string{pluginURL})

	cfg := Config{ConfigHome: configHome, ControlledPlugin: identity}
	if err := verifyControlledPluginBoundary(cfg); err != nil {
		t.Fatalf("valid controlled plugin boundary: %v", err)
	}
	srv := NewServerWithConfig(cfg)
	srv.mu.Lock()
	srv.port = 4321
	got := srv.commandArgsLocked()
	srv.mu.Unlock()
	if strings.Contains(strings.Join(got, " "), "--pure") {
		t.Fatalf("controlled plugin argv = %v", got)
	}

	pure := cfg
	pure.Pure = true
	if err := verifyControlledPluginBoundary(pure); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("controlled plugin plus Pure error = %v", err)
	}
	writeConfig([]string{pluginURL, "file:///ambient.ts"})
	if err := verifyControlledPluginBoundary(cfg); err == nil || !strings.Contains(err.Error(), "exactly") {
		t.Fatalf("additional plugin boundary error = %v", err)
	}
	writeConfig([]string{pluginURL})
	ambientPlugin := filepath.Join(configDir, "ambient.ts")
	if err := os.WriteFile(ambientPlugin, []byte("export default {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyControlledPluginBoundary(cfg); err == nil || !strings.Contains(err.Error(), "only regular opencode.json") {
		t.Fatalf("ambient discovery surface error = %v", err)
	}
}

func TestControlledPluginIsRevalidatedAfterLaunchHook(t *testing.T) {
	pluginPath := filepath.Join(t.TempDir(), "skynex-workflow.ts")
	content := []byte("export default async function workflow() {}\n")
	if err := os.WriteFile(pluginPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	identity := &ControlledPluginIdentity{Path: pluginPath, ContentDigest: "sha256:" + hex.EncodeToString(sum[:])}
	configHome := t.TempDir()
	configDir := filepath.Join(configHome, "opencode")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"plugin": []string{(&url.URL{Scheme: "file", Path: filepath.ToSlash(pluginPath)}).String()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	srv := NewServerWithConfig(Config{
		Binary: os.Args[0], WorkDir: t.TempDir(), RunDir: t.TempDir(), ConfigHome: configHome,
		CommandArgs:      []string{"-test.run=^TestLifecycleHelperProcess$", "--", "{port}"},
		ControlledPlugin: identity,
		Env:              map[string]string{helperProcessEnv: "1"},
	})
	var mutationErr error
	srv.beforeCommandStart = func() {
		mutationErr = os.WriteFile(pluginPath, []byte("export default async function changed() {}\n"), 0o644)
	}
	startErr := srv.Start(context.Background())
	if mutationErr != nil {
		_ = srv.Stop()
		t.Fatal(mutationErr)
	}
	if startErr == nil || !strings.Contains(startErr.Error(), "revalidate controlled plugin before OpenCode launch") {
		_ = srv.Stop()
		t.Fatalf("Start error = %v", startErr)
	}
	srv.mu.RLock()
	pid := srv.pid
	srv.mu.RUnlock()
	if pid != 0 {
		t.Fatalf("process started before plugin revalidation: pid=%d", pid)
	}
}

func TestHealthRejectsNonExactVersion(t *testing.T) {
	server := httptestServer(t, `{"healthy":true,"version":"1.18.16"}`)
	srv := NewServerWithConfig(Config{ExpectedVersion: "1.18.15"})
	srv.mu.Lock()
	srv.state = stateRunning
	srv.baseURL = server.URL
	srv.workDir = t.TempDir()
	srv.mu.Unlock()
	_, err := srv.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), `expected exactly "1.18.15"`) {
		t.Fatalf("health error = %v", err)
	}
}

func httptestServer(t *testing.T, health string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/global/health" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, health)
	}))
	t.Cleanup(server.Close)
	return server
}

func processAliveNonZombie(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat))
	return len(fields) < 3 || fields[2] != "Z"
}

func assertPrivateDirectory(t *testing.T, runDir, directory string) {
	t.Helper()
	if directory == "" {
		t.Fatal("private environment directory is empty")
	}
	relative, err := filepath.Rel(runDir, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("private directory %q is outside run dir %q", directory, runDir)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory %q mode = %v", directory, info.Mode())
	}
}

func waitUntil(timeout time.Duration, condition func() bool) bool {
	if condition() {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if condition() {
				return true
			}
		case <-timer.C:
			return condition()
		}
	}
}
