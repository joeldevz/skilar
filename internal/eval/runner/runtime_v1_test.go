package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/toolpolicy"
)

const (
	runnerHelperEnabled = "SKYNEX_RUNNER_OPENCODE_HELPER"
	runnerHelperBinary  = "SKYNEX_RUNNER_TEST_BINARY"
	runnerHelperMode    = "SKYNEX_RUNNER_HELPER_MODE"
	runnerHelperTools   = "SKYNEX_RUNNER_TOOL_IDS_BODY"
)

// TestRunnerOpenCodeFactoryHelperProcess is re-executed behind a tiny wrapper
// as an evaluator-owned fake OpenCode server. It implements probe endpoints
// only and can never issue a provider/model request.
func TestRunnerOpenCodeFactoryHelperProcess(t *testing.T) {
	if os.Getenv(runnerHelperEnabled) != "1" {
		return
	}
	port, hostname := helperServeAddress(t, os.Args)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode", "opencode.json")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read effective config: %v", err)
	}
	if observationPath := os.Getenv(runnerCleanProfileObservationFile); observationPath != "" {
		if err := writeRunnerCleanProfileObservation(observationPath, config); err != nil {
			t.Fatalf("write clean-profile observation: %v", err)
		}
	}
	if os.Getenv(runnerHelperMode) == "unsafe-config" {
		var decoded map[string]any
		if err := json.Unmarshal(config, &decoded); err != nil {
			t.Fatal(err)
		}
		decoded["tools"].(map[string]any)["github_push"] = true
		decoded["permission"].(map[string]any)["github_push"] = "allow"
		config, err = json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
	}
	toolBody := os.Getenv(runnerHelperTools)
	if toolBody == "" {
		toolBody = `["read","github_push","ambient_unknown"]`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"healthy":true,"version":"test-v1"}`)
	})
	mux.HandleFunc("/path", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"directory": cwd})
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(config) })
	mux.HandleFunc("/agent", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"name":"orchestrator"}]`)
	})
	mux.HandleFunc("/experimental/tool/ids", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, toolBody) })
	mux.HandleFunc("/provider", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"all":[{"id":"openai","models":{"gpt-5":{"id":"gpt-5","providerID":"openai"}}}],"default":{"openai":"gpt-5"},"connected":["openai"]}`)
	})
	mux.HandleFunc("/doc", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"openapi":"3.1.0","info":{"version":"test-v1"},"paths":{
			"/session":{"post":{}},"/session/{sessionID}":{"get":{}},
			"/session/{sessionID}/children":{"get":{}},
			"/session/{sessionID}/message":{"get":{},"post":{}},
			"/session/status":{"get":{}},"/global/event":{"get":{}},
			"/experimental/tool/ids":{"get":{}},"/provider":{"get":{}}
		}}`)
	})
	listener, err := net.Listen("tcp", net.JoinHostPort(hostname, port))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	authenticated := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != os.Getenv("OPENCODE_SERVER_USERNAME") || password == "" || password != os.Getenv("OPENCODE_SERVER_PASSWORD") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, request)
	})
	server := &http.Server{Handler: authenticated}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serve: %v", err)
	}
}

func helperServeAddress(t *testing.T, args []string) (string, string) {
	t.Helper()
	hostname := "127.0.0.1"
	port := ""
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--port":
			port = args[i+1]
		case "--hostname":
			hostname = args[i+1]
		}
	}
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatalf("invalid helper port %q: %v (args=%v)", port, err, args)
	}
	return port, hostname
}

func TestOpenCodeFactoryProbesAndBindsVerifiedToolCatalog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX executable wrapper")
	}
	request, effective := openCodeFactoryRequest(t)
	factory := openCodeTestFactory(t, nil)

	runtimeHandle, err := factory.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := runtimeHandle.GetToolIDsContext(context.Background()); err != nil {
		t.Fatalf("authenticated operational client failed: %v", err)
	}
	t.Cleanup(func() { _ = runtimeHandle.Close() })
	tools := runtimeHandle.PromptTools()
	want := map[string]bool{"*": false, "ambient_unknown": false, "github_push": false, "read": true}
	if !reflect.DeepEqual(tools, want) {
		t.Fatalf("bound prompt tools = %#v, want %#v", tools, want)
	}
	// PromptTools must be a defensive copy: caller mutation cannot widen the
	// map subsequently sent by the runtime.
	tools["github_push"] = true
	if runtimeHandle.PromptTools()["github_push"] {
		t.Fatal("PromptTools exposed mutable runtime authority")
	}
	catalog, err := runtimeHandle.GetToolIDsContext(context.Background())
	if err != nil {
		t.Fatalf("GetToolIDsContext() error = %v", err)
	}
	if strings.Join(catalog, ",") != "read,github_push,ambient_unknown" {
		t.Fatalf("probed tool catalog = %v", catalog)
	}
	info := runtimeHandle.Info()
	wantToolset, err := contracts.CanonicalDigest(map[string]string{"policy": info.ToolPolicyDigest, "catalog": info.ToolCatalogDigest})
	if err != nil {
		t.Fatal(err)
	}
	if info.ToolPolicyDigest != effective.Digest || info.ToolCatalogDigest == "" || info.ToolsetDigest != wantToolset || info.ConfigDigest != effective.Digest || info.OpenCodeAPI == "" || info.AgentsDigest == "" {
		t.Fatalf("runtime provenance = %#v, policy digest = %s", info, effective.Digest)
	}
}

func TestOpenCodeFactoryFailsClosedOnRuntimeConfigOrCatalogDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a POSIX executable wrapper")
	}
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "resolved config widens authority", env: map[string]string{runnerHelperMode: "unsafe-config"}, want: "violates tool policy"},
		{name: "allowed tool absent from catalog", env: map[string]string{runnerHelperTools: `["github_push","ambient_unknown"]`}, want: "explicitly allowed tool \"read\" is absent"},
		{name: "malformed catalog", env: map[string]string{runnerHelperTools: `{"read":true}`}, want: "decode probed OpenCode tool catalog"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, _ := openCodeFactoryRequest(t)
			factory := openCodeTestFactory(t, test.env)
			if runtimeHandle, err := factory.Start(context.Background(), request); err == nil {
				_ = runtimeHandle.Close()
				t.Fatalf("unsafe runtime unexpectedly started")
			} else if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Start() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestRequireCleanOpenAIOAuthProviders(t *testing.T) {
	t.Parallel()
	for _, connected := range [][]string{{"openai"}, {"openai", "opencode"}, {"opencode", "openai"}} {
		if err := RequireCleanOpenAIOAuthProviders(connected); err != nil {
			t.Fatalf("connected providers %v rejected: %v", connected, err)
		}
	}
	for _, connected := range [][]string{{}, {"opencode"}, {"openai", "anthropic"}, {"openai", "openai"}} {
		if err := RequireCleanOpenAIOAuthProviders(connected); err == nil {
			t.Fatalf("connected providers %v accepted", connected)
		}
	}
}

func openCodeFactoryRequest(t *testing.T) (RuntimeRequest, toolpolicy.Effective) {
	t.Helper()
	effective, err := toolpolicy.Generate(toolpolicy.Input{
		Base:           json.RawMessage(`{"agent":{"orchestrator":{"model":"openai/gpt-5"}}}`),
		AllowedTools:   []string{"read"},
		ForbiddenTools: []string{"github_push"},
	})
	if err != nil {
		t.Fatal(err)
	}
	configRoot := t.TempDir()
	configDir := filepath.Join(configRoot, "opencode")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), effective.Config, 0o600); err != nil {
		t.Fatal(err)
	}
	return RuntimeRequest{
		WorkspacePath: t.TempDir(), RunPath: t.TempDir(), ConfigRoot: configRoot,
		Case: contracts.Case{
			Agent:    contracts.AgentConfig{Name: "orchestrator", Model: "openai/gpt-5"},
			Security: contracts.SecurityConfig{ExecutionMode: contracts.ExecutionTrustedLocal, Network: contracts.NetworkHostUnisolated},
		},
		ToolPolicy: effective,
	}, effective
}

func openCodeTestFactory(t *testing.T, extra map[string]string) OpenCodeFactory {
	t.Helper()
	binary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "fake-opencode")
	script := "#!/bin/sh\nexec \"$" + runnerHelperBinary + "\" -test.run=^TestRunnerOpenCodeFactoryHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{runnerHelperEnabled: "1", runnerHelperBinary: binary}
	for key, value := range extra {
		env[key] = value
	}
	return OpenCodeFactory{
		Binary: wrapper, ExpectedVersion: "test-v1", Env: env,
		StartupTimeout: 5 * time.Second,
	}
}
