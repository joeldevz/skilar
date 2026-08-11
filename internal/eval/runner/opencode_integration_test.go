//go:build opencode_integration

package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/toolpolicy"
)

// This integration test performs the strongest useful preflight that does not
// invoke a provider: it starts the pinned OpenCode binary with an evaluator-
// owned config, probes the resolved config and API, and binds the complete
// runtime tool catalogue to the fail-closed per-prompt map.
func TestPinnedOpenCodeAcceptsAndEnforcesGeneratedPolicyWithoutModelCall(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	runPath := filepath.Join(root, "runtime")
	configRoot := filepath.Join(root, "config")
	bundle := filepath.Join(configRoot, "opencode")
	for _, directory := range []string{workspace, runPath, bundle} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	base, err := json.Marshal(map[string]any{
		"agent": map[string]any{
			"eval-probe": map[string]any{
				"description": "No-model evaluator policy probe",
				"mode":        "all",
				"model":       "openai/gpt-5.6-terra",
				"prompt":      "Do not execute: this agent only proves configuration loading.",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	effective, err := toolpolicy.Generate(toolpolicy.Input{
		Base: base, AllowedTools: []string{"read", "glob", "grep"},
		ForbiddenTools: []string{"bash", "edit", "write", "task"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "opencode.json"), effective.Config, 0o600); err != nil {
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
	runtime, err := (OpenCodeFactory{
		Binary: filepath.Clean(binary), ExpectedVersion: "1.18.16", StartupTimeout: 30 * time.Second,
		Env: map[string]string{"OPENAI_API_KEY": "skynex-eval-no-model-placeholder"},
	}).Start(ctx, RuntimeRequest{
		WorkspacePath: workspace, RunPath: runPath, ConfigRoot: configRoot,
		Case: contracts.Case{
			Agent: contracts.AgentConfig{Name: "eval-probe", Model: "openai/gpt-5.6-terra"},
			Security: contracts.SecurityConfig{
				ExecutionMode: contracts.ExecutionTrustedLocal,
				Network:       contracts.NetworkHostUnisolated,
			},
		},
		ToolPolicy: effective,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	tools := runtime.PromptTools()
	if !tools["read"] || !tools["glob"] || !tools["grep"] {
		t.Fatalf("allowed built-ins are missing from bound runtime catalogue: %#v", tools)
	}
	for _, denied := range []string{"bash", "edit", "write", "task"} {
		if tools[denied] {
			t.Fatalf("denied tool %q was enabled: %#v", denied, tools)
		}
	}
	if runtime.Info().OpenCodeVersion != "1.18.16" || runtime.Info().ToolPolicyDigest != effective.Digest {
		t.Fatalf("unexpected runtime provenance: %#v", runtime.Info())
	}
}
