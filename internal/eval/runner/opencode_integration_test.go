//go:build opencode_integration

package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/eval/contracts"
)

// This integration test performs the strongest useful preflight that does not
// invoke a provider: it starts the pinned OpenCode binary with an evaluator-
// owned config, probes the resolved config and API, and binds the complete
// runtime tool catalogue to the fail-closed per-prompt map.
func TestPinnedOpenCodeAcceptsAndEnforcesGeneratedPolicyWithoutModelCall(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	runPath := filepath.Join(root, "runtime")
	configRoot := filepath.Join(runPath, "control", "xdg-config")
	bundle := filepath.Join(configRoot, "opencode")
	for _, directory := range []string{
		workspace, runPath, bundle,
		filepath.Join(bundle, "agents"), filepath.Join(bundle, "commands"), filepath.Join(bundle, "plugins"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bundle, "agents", "eval-probe.md"), []byte("\n  No-model evaluator policy probe.  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "commands", "ambient.md"), []byte("must not be discovered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "plugins", "ambient.ts"), []byte("export const Ambient = {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := json.Marshal(map[string]any{
		"agent": map[string]any{
			"eval-probe": map[string]any{
				"description": "No-model evaluator policy probe",
				"mode":        "all",
				"model":       "openai/gpt-5.6-terra",
				"prompt":      "{file:./agents/eval-probe.md}",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "opencode.json"), base, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeMCP := buildPublicFakeMCP(t, root)
	evaluatorBinary := buildEvaluatorBinary(t, root)
	testCase := contracts.Case{
		Agent: contracts.AgentConfig{Name: "eval-probe", Model: "openai/gpt-5.6-terra"},
		Security: contracts.SecurityConfig{
			ExecutionMode: contracts.ExecutionTrustedLocal,
			Network:       contracts.NetworkHostUnisolated,
		},
		ToolPolicy: contracts.ToolPolicy{
			AllowedTools:   []string{"read", "worker_result"},
			ForbiddenTools: []string{"bash", "edit", "write", "task"},
			FakeMCPs: []contracts.FakeMCP{{
				Name: "candidate_drift", Transport: "stdio", Tools: []string{"worker_result"},
				Command: &contracts.Command{Argv: []string{fakeMCP}},
				Env:     map[string]string{"SKX_FAKE_SCENARIO": "candidate_drift"},
			}},
		},
	}
	effective, err := prepareToolPolicyWithProxy(bundle, testCase, nil, evaluatorBinary)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "opencode.json" {
		t.Fatalf("runtime projection = %v, want only opencode.json", entries)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	binary, err := exec.LookPath("opencode")
	if err != nil {
		t.Fatal(err)
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	runtimeHandle, err := (OpenCodeFactory{
		Binary: filepath.Clean(binary), ExpectedVersion: "1.18.16", StartupTimeout: 30 * time.Second,
		Env: map[string]string{"OPENAI_API_KEY": "skynex-eval-no-model-placeholder"},
		HTTPClient: &http.Client{
			Transport: getOnlyRoundTripper{base: http.DefaultTransport},
			Timeout:   30 * time.Second,
		},
	}).Start(ctx, RuntimeRequest{
		WorkspacePath: workspace, RunPath: runPath, ConfigRoot: configRoot,
		Case:       testCase,
		ToolPolicy: effective,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := runtimeHandle.Close(); err != nil {
			t.Errorf("close no-model runtime: %v", err)
		}
	}()
	tools := runtimeHandle.PromptTools()
	if !tools["read"] || !tools["candidate_drift_worker_result"] {
		t.Fatalf("allowed built-ins are missing from bound runtime catalogue: %#v", tools)
	}
	for _, denied := range []string{"bash", "edit", "write", "task"} {
		if tools[denied] {
			t.Fatalf("denied tool %q was enabled: %#v", denied, tools)
		}
	}
	info := runtimeHandle.Info()
	if info.OpenCodeVersion != "1.18.16" || info.ToolPolicyDigest != effective.Digest {
		t.Fatalf("unexpected runtime provenance: %#v", info)
	}
	toolIDs := make([]string, 0, len(tools)-1)
	for id := range tools {
		if id != "*" {
			toolIDs = append(toolIDs, id)
		}
	}
	sort.Strings(toolIDs)
	wantCatalogDigest, err := contracts.CanonicalDigest(toolIDs)
	if err != nil {
		t.Fatal(err)
	}
	if info.ToolCatalogDigest != wantCatalogDigest {
		t.Fatalf("effective tool catalog digest = %s, want %s for %v", info.ToolCatalogDigest, wantCatalogDigest, toolIDs)
	}
}

// OpenCode 1.18.16 omits disabled MCP entries from GET /mcp. Exercise the
// shipped bundle and its ambient MCP declarations with the public go-run fake,
// while keeping the transport GET-only so this regression cannot call a model.
func TestPinnedOpenCodeAcceptsRealBundleWhenDisabledMCPsAreOmittedWithoutModelCall(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	runPath := filepath.Join(root, "runtime")
	configRoot := filepath.Join(runPath, "control", "xdg-config")
	bundle := filepath.Join(configRoot, "opencode")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	bundleFS, err := assets.OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	if err := assets.ExtractTo(bundleFS, bundle); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(workspace, os.DirFS(publicCoordinationFixtureRoot(t))); err != nil {
		t.Fatal(err)
	}

	evaluatorBinary := buildEvaluatorBinary(t, root)
	testCase := contracts.Case{
		Agent: contracts.AgentConfig{Name: "skynex-orchestrator", Model: "openai/gpt-5.6-terra"},
		Security: contracts.SecurityConfig{
			ExecutionMode: contracts.ExecutionTrustedLocal,
			Network:       contracts.NetworkHostUnisolated,
		},
		ToolPolicy: contracts.ToolPolicy{
			AllowedTools:   []string{"Read", "worker_result"},
			ForbiddenTools: []string{"Edit", "skynex_workflow", "git_commit", "git_push", "github_pr"},
			FakeMCPs: []contracts.FakeMCP{{
				Name: "candidate_drift", Transport: "stdio", Tools: []string{"worker_result"},
				Command: &contracts.Command{Argv: []string{"go", "run", "./fake-mcp"}},
				Env:     map[string]string{"SKX_FAKE_SCENARIO": "candidate_drift"},
			}},
		},
	}
	effective, err := prepareToolPolicyWithProxy(bundle, testCase, nil, evaluatorBinary)
	if err != nil {
		t.Fatal(err)
	}
	if len(effective.DisabledMCPs) == 0 {
		t.Fatal("real bundle did not retain any disabled ambient MCP declarations")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	binary, err := exec.LookPath("opencode")
	if err != nil {
		t.Fatal(err)
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	var rejectedNonGET atomic.Int64
	runtimeHandle, err := (OpenCodeFactory{
		Binary: filepath.Clean(binary), ExpectedVersion: "1.18.16", StartupTimeout: 30 * time.Second,
		Env: map[string]string{"OPENAI_API_KEY": "skynex-eval-no-model-placeholder"},
		HTTPClient: &http.Client{
			Transport: getOnlyRoundTripper{base: http.DefaultTransport, rejectedNonGET: &rejectedNonGET},
			Timeout:   30 * time.Second,
		},
	}).Start(ctx, RuntimeRequest{
		WorkspacePath: workspace, RunPath: runPath, ConfigRoot: configRoot,
		Case:       testCase,
		ToolPolicy: effective,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := runtimeHandle.Close(); err != nil {
			t.Errorf("close real-bundle no-model runtime: %v", err)
		}
	}()

	openCode, ok := runtimeHandle.(*openCodeRuntime)
	if !ok {
		t.Fatalf("runtime type = %T, want *openCodeRuntime", runtimeHandle)
	}
	statuses, err := openCode.client.GetMCPStatusCatalogContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses.Statuses) != 1 || statuses.Statuses["candidate_drift"] != "connected" {
		t.Fatalf("GET /mcp statuses = %#v, want only connected fake", statuses.Statuses)
	}
	for _, name := range effective.DisabledMCPs {
		if _, reported := statuses.Statuses[name]; reported {
			t.Fatalf("OpenCode unexpectedly reported disabled ambient MCP %q", name)
		}
	}
	tools := runtimeHandle.PromptTools()
	if !tools["read"] || !tools["candidate_drift_worker_result"] {
		t.Fatalf("real-bundle allowed tools are missing: %#v", tools)
	}
	if rejectedNonGET.Load() != 0 {
		t.Fatalf("real-bundle preflight attempted %d non-GET requests", rejectedNonGET.Load())
	}
}

// A real MCP that initializes successfully but advertises a name outside the
// declared contract must never have that name synthesized into the OpenCode
// catalogue. The proxy closes the stdio boundary before forwarding the final
// tools/list page, so Start fails during GET-only probing and no session/model
// POST can occur.
func TestPinnedOpenCodeRejectsMismatchedMCPToolAttestationWithoutModelCall(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	runPath := filepath.Join(root, "runtime")
	configRoot := filepath.Join(runPath, "control", "xdg-config")
	bundle := filepath.Join(configRoot, "opencode")
	for _, directory := range []string{workspace, runPath, bundle} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	base, err := json.Marshal(map[string]any{"agent": map[string]any{"eval-probe": map[string]any{
		"description": "No-model evaluator negative probe", "mode": "all",
		"model": "openai/gpt-5.6-terra", "prompt": "No model call.",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "opencode.json"), base, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeMCP, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fakeMCP, err = filepath.EvalSymlinks(fakeMCP)
	if err != nil {
		t.Fatal(err)
	}
	listMarker := filepath.Join(root, "mismatched-tools-list-observed")
	evaluatorBinary := buildEvaluatorBinary(t, root)
	testCase := contracts.Case{
		Agent: contracts.AgentConfig{Name: "eval-probe", Model: "openai/gpt-5.6-terra"},
		Security: contracts.SecurityConfig{
			ExecutionMode: contracts.ExecutionTrustedLocal, Network: contracts.NetworkHostUnisolated,
		},
		ToolPolicy: contracts.ToolPolicy{
			AllowedTools: []string{"read", "declared_result"},
			FakeMCPs: []contracts.FakeMCP{{
				Name: "candidate_drift", Transport: "stdio", Tools: []string{"declared_result"},
				Command: &contracts.Command{Argv: []string{fakeMCP, "-test.run=^TestTaggedMismatchedFakeMCPHelper$"}},
				// This real fixture advertises worker_result, intentionally not
				// the declared_result expected by the evaluator proxy.
				Env: map[string]string{
					"SKX_MISMATCH_HELPER": "1", "SKX_MISMATCH_LIST_MARKER": listMarker,
				},
			}},
		},
	}
	effective, err := prepareToolPolicyWithProxy(bundle, testCase, nil, evaluatorBinary)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	binary, err := exec.LookPath("opencode")
	if err != nil {
		t.Fatal(err)
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	var rejectedNonGET atomic.Int64
	runtimeHandle, err := (OpenCodeFactory{
		Binary: filepath.Clean(binary), ExpectedVersion: "1.18.16", StartupTimeout: 30 * time.Second,
		Env:        map[string]string{"OPENAI_API_KEY": "skynex-eval-no-model-placeholder"},
		HTTPClient: &http.Client{Transport: getOnlyRoundTripper{base: http.DefaultTransport, rejectedNonGET: &rejectedNonGET}, Timeout: 30 * time.Second},
	}).Start(ctx, RuntimeRequest{
		WorkspacePath: workspace, RunPath: runPath, ConfigRoot: configRoot, Case: testCase, ToolPolicy: effective,
	})
	if runtimeHandle != nil {
		_ = runtimeHandle.Close()
		t.Fatal("mismatched real MCP tool set unexpectedly started")
	}
	if err == nil {
		t.Fatal("mismatched real MCP tool set was accepted")
	}
	if !errors.Is(err, ErrRuntimeContractIncompatible) || !strings.Contains(err.Error(), "MCP") || !strings.Contains(err.Error(), "status") {
		t.Fatalf("negative preflight failed for an unrelated reason: %v", err)
	}
	if rejectedNonGET.Load() != 0 {
		t.Fatalf("negative preflight attempted %d non-GET requests", rejectedNonGET.Load())
	}
	marker, markerErr := os.ReadFile(listMarker)
	if markerErr != nil || string(marker) != "mismatched-tools-list-response-prepared\n" {
		t.Fatalf("fake did not reach and answer tools/list mismatch: marker=%q err=%v", marker, markerErr)
	}
	for _, binding := range effective.MCPAttestations {
		if _, statErr := os.Stat(binding.AttestationPath); !os.IsNotExist(statErr) {
			t.Fatalf("invalid final page produced an attestation: %v", statErr)
		}
	}
}

func TestTaggedMismatchedFakeMCPHelper(t *testing.T) {
	if os.Getenv("SKX_MISMATCH_HELPER") != "1" {
		return
	}
	defer os.Exit(0)
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		switch request.Method {
		case "initialize":
			response["result"] = map[string]any{
				"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "mismatch-test", "version": "1"},
			}
		case "tools/list":
			response["result"] = map[string]any{"tools": []any{map[string]any{
				"name": "worker_result", "description": "intentionally outside declared contract",
				"inputSchema": map[string]any{"type": "object"},
			}}}
			if err := os.WriteFile(os.Getenv("SKX_MISMATCH_LIST_MARKER"), []byte("mismatched-tools-list-response-prepared\n"), 0o600); err != nil {
				os.Exit(4)
			}
		default:
			response["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		if err := encoder.Encode(response); err != nil {
			os.Exit(3)
		}
	}
	if scanner.Err() != nil {
		os.Exit(5)
	}
}

type getOnlyRoundTripper struct {
	base           http.RoundTripper
	rejectedNonGET *atomic.Int64
}

func (t getOnlyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet {
		if t.rejectedNonGET != nil {
			t.rejectedNonGET.Add(1)
		}
		return nil, fmt.Errorf("no-model integration probe rejected HTTP method %s", request.Method)
	}
	return t.base.RoundTrip(request)
}

func buildPublicFakeMCP(t *testing.T, root string) string {
	t.Helper()
	sourceDir := filepath.Join(publicCoordinationFixtureRoot(t), "fake-mcp")
	binary := filepath.Join(root, "fake-mcp")
	goHome := filepath.Join(root, "go-home")
	goCache := filepath.Join(root, "go-cache")
	goModCache := filepath.Join(root, "go-mod-cache")
	for _, directory := range []string{goHome, goCache, goModCache} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("go", "build", "-trimpath", "-o", binary, ".")
	command.Dir = sourceDir
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + goHome, "TMPDIR=" + root,
		"GOCACHE=" + goCache, "GOMODCACHE=" + goModCache,
		"GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0",
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build public fake MCP: %v: %s", err, output)
	}
	info, err := os.Lstat(binary)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Fatalf("fake MCP executable has unsafe mode %v", info.Mode())
	}
	return binary
}

func publicCoordinationFixtureRoot(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate integration test source")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
	return filepath.Join(repositoryRoot, "eval", "fixtures", "skynex-orchestrator", "coordination")
}

func buildEvaluatorBinary(t *testing.T, root string) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate integration test source")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
	binary := filepath.Join(root, "skynex-eval")
	goHome := filepath.Join(root, "evaluator-go-home")
	goCache := filepath.Join(root, "evaluator-go-cache")
	for _, directory := range []string{goHome, goCache} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	goEnv := exec.Command("go", "env", "GOMODCACHE")
	goEnv.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local"}
	moduleCacheOutput, err := goEnv.Output()
	if err != nil {
		t.Fatalf("locate offline Go module cache: %v", err)
	}
	goModCache := string(bytes.TrimSpace(moduleCacheOutput))
	if !filepath.IsAbs(goModCache) {
		t.Fatalf("offline Go module cache is not absolute: %q", goModCache)
	}
	command := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/skynex-eval")
	command.Dir = repositoryRoot
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + goHome, "TMPDIR=" + root,
		"GOCACHE=" + goCache, "GOMODCACHE=" + goModCache,
		"GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0",
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build evaluator MCP proxy: %v: %s", err, output)
	}
	return binary
}
