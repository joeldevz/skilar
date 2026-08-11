package toolpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestGenerateFailClosedEffectiveConfigAndCanonicalDigest(t *testing.T) {
	input := Input{
		Base: json.RawMessage(`{
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
	if effective.SchemaVersion != SchemaVersion || !strings.HasPrefix(effective.Digest, "sha256:") || len(effective.Digest) != 71 {
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
}
