package toolpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
)

type Verification struct {
	Valid      bool     `json:"valid"`
	Violations []string `json:"violations,omitempty"`
}

// VerifyRuntimeConfig checks the security-sensitive fields of OpenCode's
// resolved runtime config. This catches layered user/global configuration that
// re-enables an MCP, plugin, connector, or tool after Generate.
func VerifyRuntimeConfig(raw json.RawMessage, expected Effective) Verification {
	violations := make([]string, 0)
	if _, err := validateEffective(expected); err != nil {
		return Verification{Violations: []string{"invalid expected effective policy: " + err.Error()}}
	}
	if len(raw) > maxPolicyJSONBytes {
		return Verification{Violations: []string{fmt.Sprintf("runtime config exceeds %d bytes", maxPolicyJSONBytes)}}
	}
	var runtime map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&runtime); err != nil {
		return Verification{Violations: []string{"decode runtime config: " + err.Error()}}
	}
	if runtime == nil {
		return Verification{Violations: []string{"runtime config must be a JSON object, not null"}}
	}
	if trailingErr := decoder.Decode(new(any)); !errors.Is(trailingErr, io.EOF) {
		return Verification{Violations: []string{"runtime config contains multiple JSON values"}}
	}
	var wanted map[string]any
	wantedDecoder := json.NewDecoder(bytes.NewReader(expected.Config))
	wantedDecoder.UseNumber()
	if err := wantedDecoder.Decode(&wanted); err != nil {
		return Verification{Violations: []string{"decode expected config: " + err.Error()}}
	}

	if !reflect.DeepEqual(runtime["plugin"], wanted["plugin"]) {
		violations = append(violations, "runtime plugin configuration differs from the evaluator-owned policy")
	}
	if err := rejectInlineProviderCredentials(runtime["provider"]); err != nil {
		violations = append(violations, "runtime provider authority is unsafe: "+err.Error())
	}
	if !reflect.DeepEqual(runtime["provider"], wanted["provider"]) {
		violations = append(violations, "runtime provider configuration differs from the evaluator-owned policy")
	}
	for key, expectedValue := range map[string]any{
		"formatter":  false,
		"lsp":        false,
		"share":      "disabled",
		"autoshare":  false,
		"autoupdate": false,
	} {
		if !reflect.DeepEqual(runtime[key], expectedValue) {
			violations = append(violations, fmt.Sprintf("runtime %s differs from the generated offline policy", key))
		}
	}
	if _, exists := runtime["experimental"]; exists {
		violations = append(violations, "runtime experimental configuration is not permitted")
	}
	// These fields can import prompt text or executable command templates from
	// project/user layers (including remote URLs and Git references). Preserve
	// evaluator-owned values, but reject every runtime addition or override.
	for _, key := range []string{
		"instructions", "skills", "references", "reference", "command", "commands",
		"shell", "server", "model", "small_model", "default_agent",
		"enabled_providers", "disabled_providers", "enterprise",
	} {
		if !reflect.DeepEqual(runtime[key], wanted[key]) {
			violations = append(violations, fmt.Sprintf("runtime %s differs from the evaluator-owned policy", key))
		}
	}
	wantedMCP, _ := wanted["mcp"].(map[string]any)
	runtimeMCP, ok := runtime["mcp"].(map[string]any)
	if !ok {
		violations = append(violations, "runtime mcp field is missing or not an object")
	} else {
		for name, rawEntry := range runtimeMCP {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				violations = append(violations, fmt.Sprintf("runtime MCP %q is not an object", name))
				continue
			}
			wantedEntry, declared := wantedMCP[name]
			if !declared {
				if enabledValue, exists := entry["enabled"]; !exists || enabledValue != false {
					violations = append(violations, fmt.Sprintf("undeclared runtime MCP %q is not explicitly disabled", name))
				}
				continue
			}
			if !reflect.DeepEqual(entry, wantedEntry) {
				violations = append(violations, fmt.Sprintf("runtime MCP %q differs from the generated policy", name))
			}
		}
		for name := range wantedMCP {
			if _, exists := runtimeMCP[name]; !exists {
				violations = append(violations, fmt.Sprintf("declared MCP %q is missing at runtime", name))
			}
		}
	}

	verifyToolBoundary("runtime", runtime, wanted, &violations)
	for _, field := range []string{"agent", "mode"} {
		verifyAgentBoundary(field, runtime, wanted, &violations)
	}
	sort.Strings(violations)
	return Verification{Valid: len(violations) == 0, Violations: violations}
}

func verifyAgentBoundary(field string, runtime map[string]any, wanted map[string]any, violations *[]string) {
	wantedAgents, _ := wanted[field].(map[string]any)
	runtimeAgents, _ := runtime[field].(map[string]any)
	for name, wantedAgentValue := range wantedAgents {
		wantedAgent, _ := wantedAgentValue.(map[string]any)
		runtimeAgent, ok := runtimeAgents[name].(map[string]any)
		if !ok {
			*violations = append(*violations, fmt.Sprintf("runtime %s %q is missing or not an object", field, name))
			continue
		}
		verifyToolBoundary("runtime "+field+" "+name, runtimeAgent, wantedAgent, violations)
		for _, key := range []string{"prompt", "description", "model", "mode", "steps", "temperature", "top_p", "disable", "hidden"} {
			if !reflect.DeepEqual(runtimeAgent[key], wantedAgent[key]) {
				*violations = append(*violations, fmt.Sprintf("runtime %s %q %s differs from the evaluator-owned policy", field, name, key))
			}
		}
	}
	for name := range runtimeAgents {
		if _, declared := wantedAgents[name]; !declared {
			*violations = append(*violations, fmt.Sprintf("undeclared runtime %s %q could carry additional tool authority", field, name))
		}
	}
}

func verifyToolBoundary(label string, runtime map[string]any, wanted map[string]any, violations *[]string) {
	if !reflect.DeepEqual(runtime["tools"], wanted["tools"]) {
		*violations = append(*violations, label+" tools differ from the generated policy")
	}
	if !reflect.DeepEqual(runtime["permission"], wanted["permission"]) {
		*violations = append(*violations, label+" permissions differ from the generated policy")
	}
}
