package toolpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestGenerateFailClosedEffectiveConfigAndCanonicalDigest(t *testing.T) {
	input := Input{
		Base: json.RawMessage(`{
			"$schema":"https://opencode.ai/config.json",
			"provider":{"openai":{"name":"test"}},
			"plugin":["ambient-mutator"],
			"formatter":{"prettier":{"command":["npx","prettier"]}},
			"lsp":{"typescript":{"command":["typescript-language-server"]}},
			"share":"auto","autoshare":true,"autoupdate":true,
			"experimental":{"openTelemetry":true,"primary_tools":["github_push"]},
			"mcp":{
				"context7":{"type":"remote","url":"https://example.invalid"},
				"neurox":{"type":"local","command":["neurox","mcp"],"enabled":true}
			},
			"agent":{"skynex-orchestrator":{"model":"openai/test","tools":{"github_push":true}}},
			"mode":{"legacy":{"tools":{"deploy":true}}}
		}`),
		AllowedTools:    []string{"read", "edit", "neurox_recall", "neurox_context"},
		ForbiddenTools:  []string{"github_push", "deploy"},
		AmbientMCPNames: []string{"exa"},
		FakeMCPs: []FakeMCP{{
			Name: "neurox", Command: []string{"/opt/skynex/fake-mcp", "--scenario", "neurox"},
			Environment: map[string]string{"SKYNEX_SCENARIO": "neurox"},
			Tools:       []string{"neurox_recall", "neurox_context"},
		}},
	}
	effective, err := Generate(input)
	if err != nil {
		t.Fatal(err)
	}
	if effective.SchemaVersion != SchemaVersion || !strings.HasPrefix(effective.Digest, "sha256:") || len(effective.Digest) != 71 ||
		!strings.HasPrefix(effective.AuthorizationDigest, "sha256:") || len(effective.AuthorizationDigest) != 71 {
		t.Fatalf("effective metadata = %#v", effective)
	}
	if !reflect.DeepEqual(effective.EnabledFakes, []string{"neurox"}) || !reflect.DeepEqual(effective.DisabledMCPs, []string{"context7", "exa"}) {
		t.Fatalf("fake/MCP sets = enabled %#v disabled %#v", effective.EnabledFakes, effective.DisabledMCPs)
	}
	wantBindings := []FakeToolBinding{
		{MCPName: "neurox", RawTool: "neurox_context", EffectiveID: "neurox_neurox_context"},
		{MCPName: "neurox", RawTool: "neurox_recall", EffectiveID: "neurox_neurox_recall"},
	}
	if !reflect.DeepEqual(effective.FakeToolBindings, wantBindings) {
		t.Fatalf("fake tool bindings = %#v, want %#v", effective.FakeToolBindings, wantBindings)
	}
	var config map[string]any
	if err := json.Unmarshal(effective.Config, &config); err != nil {
		t.Fatal(err)
	}
	if plugins, ok := config["plugin"].([]any); !ok || len(plugins) != 0 {
		t.Fatalf("plugins = %#v", config["plugin"])
	}
	if config["formatter"] != false || config["lsp"] != false || config["share"] != "disabled" || config["autoshare"] != false || config["autoupdate"] != false {
		t.Fatalf("offline boundary not applied: %#v", config)
	}
	if _, exists := config["experimental"]; exists {
		t.Fatalf("experimental authority survived: %#v", config["experimental"])
	}
	if _, exists := config["$schema"]; exists {
		t.Fatalf("schema metadata survived runtime projection: %#v", config["$schema"])
	}
	if provider, ok := config["provider"].(map[string]any); !ok || len(provider) != 0 {
		t.Fatalf("provider authority survived runtime projection: %#v", config["provider"])
	}
	mcp := config["mcp"].(map[string]any)
	for _, name := range []string{"context7", "exa"} {
		entry := mcp[name].(map[string]any)
		if entry["enabled"] != false || len(entry) != 1 {
			t.Fatalf("ambient MCP %s = %#v", name, entry)
		}
	}
	neurox := mcp["neurox"].(map[string]any)
	if neurox["type"] != "local" || neurox["enabled"] != true {
		t.Fatalf("fake neurox = %#v", neurox)
	}
	tools := config["tools"].(map[string]any)
	permission := config["permission"].(map[string]any)
	if tools["*"] != false || permission["*"] != "deny" || tools["read"] != true || permission["github_push"] != "deny" {
		t.Fatalf("tool boundary = tools %#v permissions %#v", tools, permission)
	}
	if tools["neurox_context"] != nil || tools["neurox_recall"] != nil || tools["neurox_neurox_context"] != true || tools["neurox_neurox_recall"] != true {
		t.Fatalf("raw MCP tool names were not replaced by effective OpenCode IDs: %#v", tools)
	}
	agent := config["agent"].(map[string]any)["skynex-orchestrator"].(map[string]any)
	if !reflect.DeepEqual(agent["tools"], config["tools"]) || !reflect.DeepEqual(agent["permission"], config["permission"]) {
		t.Fatalf("agent did not receive exact tool boundary: %#v", agent)
	}
	legacy := config["mode"].(map[string]any)["legacy"].(map[string]any)
	if !reflect.DeepEqual(legacy["tools"], config["tools"]) || !reflect.DeepEqual(legacy["permission"], config["permission"]) {
		t.Fatalf("legacy mode did not receive exact tool boundary: %#v", legacy)
	}

	reordered := input
	reordered.Base = json.RawMessage(`{"mode":{"legacy":{"tools":{"deploy":true}}},"experimental":{"primary_tools":["github_push"],"openTelemetry":true},"autoupdate":true,"autoshare":true,"share":"auto","lsp":{"typescript":{"command":["typescript-language-server"]}},"formatter":{"prettier":{"command":["npx","prettier"]}},"agent":{"skynex-orchestrator":{"tools":{"github_push":true},"model":"openai/test"}},"mcp":{"neurox":{"enabled":true,"command":["neurox","mcp"],"type":"local"},"context7":{"url":"https://example.invalid","type":"remote"}},"plugin":["ambient-mutator"],"provider":{"openai":{"name":"test"}}}`)
	reordered.AllowedTools = []string{"neurox_context", "read", "neurox_recall", "edit"}
	reordered.ForbiddenTools = []string{"deploy", "github_push"}
	reordered.FakeMCPs = []FakeMCP{{
		Name: "neurox", Command: []string{"/opt/skynex/fake-mcp", "--scenario", "neurox"},
		Environment: map[string]string{"SKYNEX_SCENARIO": "neurox"},
		Tools:       []string{"neurox_context", "neurox_recall"},
	}}
	second, err := Generate(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if second.Digest != effective.Digest || string(second.Config) != string(effective.Config) {
		t.Fatalf("canonical output changed with input order\nfirst:  %s %s\nsecond: %s %s", effective.Digest, effective.Config, second.Digest, second.Config)
	}
}

func TestAuthorizationDigestExcludesPromptAndModelButTracksAuthority(t *testing.T) {
	first, err := Generate(Input{
		Base:         json.RawMessage(`{"model":"openai/model-a","agent":{"orchestrator":{"prompt":"candidate A","description":"A","model":"openai/model-a"}}}`),
		AllowedTools: []string{"read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate(Input{
		Base:         json.RawMessage(`{"model":"openai/model-b","agent":{"orchestrator":{"prompt":"candidate B","description":"B","model":"openai/model-b"}}}`),
		AllowedTools: []string{"read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("full config digest did not observe prompt/model change")
	}
	if first.AuthorizationDigest != second.AuthorizationDigest {
		t.Fatalf("authorization digest changed with prompt/model only: %s != %s", first.AuthorizationDigest, second.AuthorizationDigest)
	}

	widened, err := Generate(Input{
		Base:         json.RawMessage(`{"model":"openai/model-b","agent":{"orchestrator":{"prompt":"candidate B"}}}`),
		AllowedTools: []string{"read", "edit"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if widened.AuthorizationDigest == second.AuthorizationDigest {
		t.Fatal("authorization digest ignored a tool-authority change")
	}
}

func TestControlledPluginIsExactPinnedAuthorityAndRevalidated(t *testing.T) {
	pluginPath := filepath.Join(t.TempDir(), "skynex-workflow.ts")
	content := []byte("export default async function workflow() {}\n")
	if err := os.WriteFile(pluginPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	identity := ControlledPluginIdentity{
		Path:          pluginPath,
		ContentDigest: "sha256:" + hex.EncodeToString(sum[:]),
	}
	effective, err := Generate(Input{AllowedTools: []string{"read"}, Plugin: &identity})
	if err != nil {
		t.Fatal(err)
	}
	if effective.Plugin == nil || effective.Plugin == &identity || *effective.Plugin != identity {
		t.Fatalf("effective plugin identity = %#v", effective.Plugin)
	}
	serialized, err := json.Marshal(effective)
	if err != nil {
		t.Fatal(err)
	}
	var serializedFields map[string]any
	if err := json.Unmarshal(serialized, &serializedFields); err != nil {
		t.Fatal(err)
	}
	if _, exposed := serializedFields["plugin"]; exposed {
		t.Fatal("live plugin identity was serialized as effective-policy metadata")
	}
	var config map[string]any
	if err := json.Unmarshal(effective.Config, &config); err != nil {
		t.Fatal(err)
	}
	wantURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(pluginPath)}).String()
	if plugins, ok := config["plugin"].([]any); !ok || len(plugins) != 1 || plugins[0] != wantURL {
		t.Fatalf("controlled plugin config = %#v, want [%q]", config["plugin"], wantURL)
	}
	if verification := VerifyRuntimeConfig(effective.Config, effective); !verification.Valid {
		t.Fatalf("controlled plugin config did not verify: %#v", verification)
	}

	var widened map[string]any
	if err := json.Unmarshal(effective.Config, &widened); err != nil {
		t.Fatal(err)
	}
	widened["plugin"] = []any{wantURL, "file:///ambient.ts"}
	raw, err := json.Marshal(widened)
	if err != nil {
		t.Fatal(err)
	}
	if verification := VerifyRuntimeConfig(raw, effective); verification.Valid {
		t.Fatal("runtime config with an additional plugin verified")
	}

	if err := os.WriteFile(pluginPath, []byte("export default async function changed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BindPromptTools(effective, []string{"read"}); !errors.Is(err, ErrUnsafePolicy) || !strings.Contains(err.Error(), "content digest mismatch") {
		t.Fatalf("mutated controlled plugin error = %v", err)
	}
	if verification := VerifyRuntimeConfig(effective.Config, effective); verification.Valid {
		t.Fatal("expected policy remained valid after controlled plugin mutation")
	}
}

func TestControlledPluginRejectsRelativeSymlinkAndWrongDigest(t *testing.T) {
	pluginPath := filepath.Join(t.TempDir(), "plugin.ts")
	content := []byte("export default {}\n")
	if err := os.WriteFile(pluginPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	validDigest := "sha256:" + hex.EncodeToString(sum[:])

	tests := []struct {
		name     string
		identity ControlledPluginIdentity
	}{
		{name: "relative", identity: ControlledPluginIdentity{Path: "plugin.ts", ContentDigest: validDigest}},
		{name: "wrong digest", identity: ControlledPluginIdentity{Path: pluginPath, ContentDigest: "sha256:" + strings.Repeat("0", 64)}},
		{name: "uppercase digest", identity: ControlledPluginIdentity{Path: pluginPath, ContentDigest: "sha256:" + strings.Repeat("A", 64)}},
	}
	if runtime.GOOS != "windows" {
		symlinkPath := filepath.Join(t.TempDir(), "plugin-link.ts")
		if err := os.Symlink(pluginPath, symlinkPath); err != nil {
			t.Fatal(err)
		}
		tests = append(tests, struct {
			name     string
			identity ControlledPluginIdentity
		}{name: "symlink", identity: ControlledPluginIdentity{Path: symlinkPath, ContentDigest: validDigest}})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Generate(Input{AllowedTools: []string{"read"}, Plugin: &test.identity}); !errors.Is(err, ErrUnsafePolicy) {
				t.Fatalf("error = %v, want ErrUnsafePolicy", err)
			}
		})
	}
}

func TestAuthorizationDigestUsesControlledPluginContentNotHostPath(t *testing.T) {
	content := []byte("export default async function workflow() {}\n")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	identities := make([]ControlledPluginIdentity, 2)
	for index := range identities {
		path := filepath.Join(t.TempDir(), "skynex-workflow.ts")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		identities[index] = ControlledPluginIdentity{Path: path, ContentDigest: digest}
	}
	first, err := Generate(Input{AllowedTools: []string{"read"}, Plugin: &identities[0]})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate(Input{AllowedTools: []string{"read"}, Plugin: &identities[1]})
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("config digest ignored the different absolute plugin file URL")
	}
	if first.AuthorizationDigest != second.AuthorizationDigest {
		t.Fatalf("authorization digest depended on host plugin path: %s != %s", first.AuthorizationDigest, second.AuthorizationDigest)
	}
}

func TestFakeMCPRawToolsMapToEffectiveOpenCodeIDs(t *testing.T) {
	effective, err := Generate(Input{
		AllowedTools: []string{"Read", "worker_result"},
		FakeMCPs: []FakeMCP{{
			Name: "worker_failure", Command: []string{"/opt/skynex/fake-worker"}, Tools: []string{"worker_result"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantBinding := []FakeToolBinding{{
		MCPName: "worker_failure", RawTool: "worker_result", EffectiveID: "worker_failure_worker_result",
	}}
	if !reflect.DeepEqual(effective.FakeToolBindings, wantBinding) {
		t.Fatalf("bindings = %#v, want %#v", effective.FakeToolBindings, wantBinding)
	}
	if effective.ResolveDeclaredToolID("worker_result") != "worker_failure_worker_result" || effective.ResolveDeclaredToolID("Read") != "read" {
		t.Fatalf("unexpected declared tool resolution: %#v", effective.FakeToolBindings)
	}
	if effective.PromptTools["worker_result"] || !effective.PromptTools["worker_failure_worker_result"] {
		t.Fatalf("prompt tools did not use effective MCP ID: %#v", effective.PromptTools)
	}
	bound, err := BindPromptTools(effective, []string{"read", "bash", "worker_failure_worker_result"})
	if err != nil {
		t.Fatal(err)
	}
	wantBound := map[string]bool{"*": false, "read": true, "bash": false, "worker_failure_worker_result": true}
	if !reflect.DeepEqual(bound, wantBound) {
		t.Fatalf("bound tools = %#v, want %#v", bound, wantBound)
	}
	if _, err := BindPromptTools(effective, []string{"read", "worker_result"}); !errors.Is(err, ErrUnsafePolicy) {
		t.Fatalf("raw runtime tool ID should not satisfy effective policy: %v", err)
	}

	normalized, err := OpenCodeMCPToolID("worker.failure", "result:read")
	if err != nil || normalized != "worker_failure_result_read" {
		t.Fatalf("normalized ID = %q, err = %v", normalized, err)
	}
	if _, err := OpenCodeMCPToolID(strings.Repeat("a", 100), strings.Repeat("b", 100)); !errors.Is(err, ErrUnsafePolicy) {
		t.Fatalf("oversized effective ID error = %v", err)
	}
}

func TestForgedEffectivePolicyCannotReenableCommands(t *testing.T) {
	effective, err := Generate(Input{AllowedTools: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(effective.Config, &config); err != nil {
		t.Fatal(err)
	}
	config["command"] = map[string]any{"escape": map[string]any{"template": "bash -lc env"}}
	effective.Config, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(effective.Config)
	effective.Digest = "sha256:" + hex.EncodeToString(sum[:])
	if _, err := BindPromptTools(effective, []string{"read"}); err == nil || !strings.Contains(err.Error(), "command catalogue") {
		t.Fatalf("forged effective command policy was accepted: %v", err)
	}
}

func TestGenerateRejectsUnsafeAuthority(t *testing.T) {
	valid := Input{
		AllowedTools: []string{"read", "neurox_context"},
		FakeMCPs: []FakeMCP{{
			Name: "neurox", Command: []string{"/opt/fake-mcp"}, Tools: []string{"neurox_context"},
		}},
	}
	tests := []struct {
		name   string
		mutate func(*Input)
	}{
		{"mutating external tool", func(input *Input) { input.AllowedTools = append(input.AllowedTools, "github_push") }},
		{"relative fake executable", func(input *Input) { input.FakeMCPs[0].Command[0] = "fake-mcp" }},
		{"shell fake executable", func(input *Input) { input.FakeMCPs[0].Command[0] = "/bin/sh" }},
		{"environment trampoline", func(input *Input) { input.FakeMCPs[0].Command = []string{"/usr/bin/env", "sh", "-c", "echo unsafe"} }},
		{"fake credential", func(input *Input) { input.FakeMCPs[0].Environment = map[string]string{"API_TOKEN": "secret"} }},
		{"fake HOME override", func(input *Input) { input.FakeMCPs[0].Environment = map[string]string{"HOME": "/tmp/escape"} }},
		{"fake loader override", func(input *Input) { input.FakeMCPs[0].Environment = map[string]string{"LD_PRELOAD": "/tmp/escape.so"} }},
		{"fake manifest override", func(input *Input) {
			input.FakeMCPs[0].Environment = map[string]string{"SKYNEX_EVAL_MCP_PROXY_MANIFEST": "/tmp/escape"}
		}},
		{"undeclared fake tool", func(input *Input) { input.FakeMCPs[0].Tools = append(input.FakeMCPs[0].Tools, "neurox_recall") }},
		{"case-insensitive allow forbid overlap", func(input *Input) { input.ForbiddenTools = []string{"READ"} }},
		{"trailing JSON", func(input *Input) { input.Base = json.RawMessage(`{} {}`) }},
		{"null base", func(input *Input) { input.Base = json.RawMessage(`null`) }},
		{"inline provider credential", func(input *Input) {
			input.Base = json.RawMessage(`{"provider":{"openai":{"options":{"apiKey":"raw-secret"}}}}`)
		}},
		{"nested provider authorization header", func(input *Input) {
			input.Base = json.RawMessage(`{"provider":{"openai":{"options":{"headers":{"Authorization":"Bearer raw-secret"}}}}}`)
		}},
		{"opaque provider header", func(input *Input) {
			input.Base = json.RawMessage(`{"provider":{"openai":{"options":{"headers":{"X-Session":"raw-secret"}}}}}`)
		}},
		{"credentialed provider URL", func(input *Input) {
			input.Base = json.RawMessage(`{"provider":{"openai":{"options":{"baseURL":"https://user:pass@proxy.invalid/v1"}}}}`)
		}},
		{"instructions file", func(input *Input) {
			input.Base = json.RawMessage(`{"instructions":["/tmp/ambient.md"]}`)
		}},
		{"instructions URL", func(input *Input) {
			input.Base = json.RawMessage(`{"instructions":["https://attacker.invalid/prompt.md"]}`)
		}},
		{"skills", func(input *Input) { input.Base = json.RawMessage(`{"skills":{"paths":["/tmp/skills"]}}`) }},
		{"references", func(input *Input) { input.Base = json.RawMessage(`{"references":["https://attacker.invalid"]}`) }},
		{"reference", func(input *Input) { input.Base = json.RawMessage(`{"reference":"/tmp/reference.md"}`) }},
		{"shell", func(input *Input) { input.Base = json.RawMessage(`{"shell":{"command":["/bin/sh"]}}`) }},
		{"server", func(input *Input) { input.Base = json.RawMessage(`{"server":{"hostname":"0.0.0.0"}}`) }},
		{"enterprise", func(input *Input) { input.Base = json.RawMessage(`{"enterprise":{"url":"https://attacker.invalid"}}`) }},
		{"default agent", func(input *Input) { input.Base = json.RawMessage(`{"default_agent":"ambient"}`) }},
		{"disabled providers", func(input *Input) { input.Base = json.RawMessage(`{"disabled_providers":["openai"]}`) }},
		{"unknown root field", func(input *Input) {
			input.Base = json.RawMessage(`{"future_authority":{"command":["/bin/sh"]}}`)
		}},
		{"unknown agent field", func(input *Input) {
			input.Base = json.RawMessage(`{"agent":{"orchestrator":{"options":{"headers":{"X-Authority":"ambient"}}}}}`)
		}},
		{"residual file substitution", func(input *Input) {
			input.Base = json.RawMessage(`{"agent":{"orchestrator":{"prompt":"materialized {file:/tmp/ambient.md}"}}}`)
		}},
		{"residual environment substitution", func(input *Input) {
			input.Base = json.RawMessage(`{"agent":{"orchestrator":{"description":"{env:OPENAI_API_KEY}"}}}`)
		}},
		{"fake command substitution", func(input *Input) {
			input.FakeMCPs[0].Command = append(input.FakeMCPs[0].Command, "{file:/tmp/argument}")
		}},
		{"fake environment substitution", func(input *Input) {
			input.FakeMCPs[0].Environment = map[string]string{"SCENARIO": "{env:OPENAI_API_KEY}"}
		}},
		{"ambiguous raw fake alias", func(input *Input) {
			input.FakeMCPs = append(input.FakeMCPs, FakeMCP{Name: "second", Command: []string{"/opt/second"}, Tools: []string{"neurox_context"}})
		}},
		{"normalized effective ID collision", func(input *Input) {
			input.AllowedTools = append(input.AllowedTools, "neurox:context")
			input.FakeMCPs = []FakeMCP{
				{Name: "neurox.one", Command: []string{"/opt/one"}, Tools: []string{"neurox_context"}},
				{Name: "neurox:one", Command: []string{"/opt/two"}, Tools: []string{"neurox:context"}},
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			input.AllowedTools = append([]string(nil), valid.AllowedTools...)
			input.FakeMCPs = append([]FakeMCP(nil), valid.FakeMCPs...)
			input.FakeMCPs[0].Command = append([]string(nil), valid.FakeMCPs[0].Command...)
			input.FakeMCPs[0].Tools = append([]string(nil), valid.FakeMCPs[0].Tools...)
			test.mutate(&input)
			if _, err := Generate(input); !errors.Is(err, ErrUnsafePolicy) {
				t.Fatalf("error = %v, want ErrUnsafePolicy", err)
			}
		})
	}
}

func TestGenerateAcceptsMaterializedPublicBundleProjection(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate policy test source")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
	bundleRoot := filepath.Join(repositoryRoot, "internal", "assets", "data", "opencode")
	raw, err := os.ReadFile(filepath.Join(bundleRoot, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	agents, ok := config["agent"].(map[string]any)
	if !ok || len(agents) == 0 {
		t.Fatalf("public bundle agents = %#v", config["agent"])
	}
	for name, rawAgent := range agents {
		agent, ok := rawAgent.(map[string]any)
		if !ok {
			t.Fatalf("public bundle agent %q = %#v", name, rawAgent)
		}
		prompt, ok := agent["prompt"].(string)
		if !ok || !strings.HasPrefix(prompt, "{file:./") || !strings.HasSuffix(prompt, "}") {
			t.Fatalf("public bundle agent %q prompt = %#v", name, agent["prompt"])
		}
		relative := strings.TrimSuffix(strings.TrimPrefix(prompt, "{file:./"), "}")
		if filepath.Clean(relative) != filepath.FromSlash(relative) || strings.HasPrefix(relative, "..") {
			t.Fatalf("public bundle agent %q has unsafe prompt path %q", name, relative)
		}
		content, err := os.ReadFile(filepath.Join(bundleRoot, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read public bundle agent %q prompt: %v", name, err)
		}
		agent["prompt"] = strings.TrimSpace(string(content))
	}
	config["model"] = "openai/gpt-5.6-terra"
	config["small_model"] = "openai/gpt-5.6-terra"
	config["enabled_providers"] = []any{"openai"}
	config["provider"] = map[string]any{}
	materialized, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := Generate(Input{Base: materialized, AllowedTools: []string{"read"}})
	if err != nil {
		t.Fatalf("materialized public bundle projection failed: %v", err)
	}
	var projected map[string]any
	if err := json.Unmarshal(effective.Config, &projected); err != nil {
		t.Fatal(err)
	}
	projectedAgents, ok := projected["agent"].(map[string]any)
	if !ok || len(projectedAgents) != len(agents) {
		t.Fatalf("projected public agents = %d, want %d", len(projectedAgents), len(agents))
	}
	for _, forbidden := range []string{"$schema", "instructions", "skills", "references", "shell", "server", "enterprise"} {
		if _, exists := projected[forbidden]; exists {
			t.Fatalf("public runtime projection retained %q", forbidden)
		}
	}
}

func TestVerifyRuntimeConfigRejectsLayeredAuthority(t *testing.T) {
	effective, err := Generate(Input{
		Base:         json.RawMessage(`{"agent":{"orchestrator":{}},"mcp":{"remote":{"type":"remote","enabled":true}},"provider":{"test":{"options":{"timeout":30}}}}`),
		AllowedTools: []string{"read"}, ForbiddenTools: []string{"github_push"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if verification := VerifyRuntimeConfig(effective.Config, effective); !verification.Valid {
		t.Fatalf("generated config did not verify: %#v", verification)
	}

	mutations := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"extra MCP", func(config map[string]any) {
			config["mcp"].(map[string]any)["github"] = map[string]any{"type": "remote", "enabled": true}
		}},
		{"plugin", func(config map[string]any) { config["plugin"] = []any{"ambient-plugin"} }},
		{"tool enabled", func(config map[string]any) { config["tools"].(map[string]any)["github_push"] = true }},
		{"extra agent", func(config map[string]any) { config["agent"].(map[string]any)["ambient"] = map[string]any{} }},
		{"remote instructions", func(config map[string]any) { config["instructions"] = []any{"https://attacker.invalid/prompt.md"} }},
		{"agent prompt override", func(config map[string]any) {
			config["agent"].(map[string]any)["orchestrator"].(map[string]any)["prompt"] = "{file:/tmp/ambient.md}"
		}},
		{"missing plugin boundary", func(config map[string]any) { delete(config, "plugin") }},
		{"provider credential", func(config map[string]any) {
			config["provider"] = map[string]any{"openai": map[string]any{"headers": map[string]any{"X-API-Key": "secret"}}}
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			var runtime map[string]any
			if err := json.Unmarshal(effective.Config, &runtime); err != nil {
				t.Fatal(err)
			}
			mutation.mutate(runtime)
			raw, _ := json.Marshal(runtime)
			verification := VerifyRuntimeConfig(raw, effective)
			if verification.Valid || len(verification.Violations) == 0 {
				t.Fatalf("unsafe runtime config verified: %s", raw)
			}
		})
	}
	trailing := append(append([]byte(nil), effective.Config...), []byte(` {}`)...)
	if verification := VerifyRuntimeConfig(trailing, effective); verification.Valid || len(verification.Violations) == 0 {
		t.Fatal("runtime config with trailing JSON verified")
	}
}

func TestBindPromptToolsDeniesEntireRuntimeCatalog(t *testing.T) {
	effective, err := Generate(Input{AllowedTools: []string{"read", "neurox_context"}, ForbiddenTools: []string{"github_push"}})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := BindPromptTools(effective, []string{"github_push", "bash", "neurox_context", "read"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"*": false, "bash": false, "github_push": false, "neurox_context": true, "read": true}
	if !reflect.DeepEqual(bound, want) {
		t.Fatalf("bound tools = %#v, want %#v", bound, want)
	}
	if _, err := BindPromptTools(effective, []string{"read"}); !errors.Is(err, ErrUnsafePolicy) {
		t.Fatalf("missing allowed tool error = %v", err)
	}
	mutated := effective
	mutated.PromptTools = make(map[string]bool, len(effective.PromptTools)+1)
	for name, enabled := range effective.PromptTools {
		mutated.PromptTools[name] = enabled
	}
	mutated.PromptTools["github_push"] = true
	if _, err := BindPromptTools(mutated, []string{"github_push", "read"}); !errors.Is(err, ErrUnsafePolicy) {
		t.Fatalf("mutated effective metadata error = %v", err)
	}
	mutated = effective
	mutated.Config = append(append(json.RawMessage(nil), effective.Config...), ' ')
	if verification := VerifyRuntimeConfig(effective.Config, mutated); verification.Valid || len(verification.Violations) == 0 {
		t.Fatal("mutated expected config verified")
	}
	mutated = effective
	mutated.AuthorizationDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err := BindPromptTools(mutated, []string{"github_push", "neurox_context", "read"}); !errors.Is(err, ErrUnsafePolicy) || !strings.Contains(err.Error(), "authorization digest mismatch") {
		t.Fatalf("mutated authorization digest error = %v", err)
	}
}
