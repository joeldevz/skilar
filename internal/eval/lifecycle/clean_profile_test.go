package lifecycle

import (
	"strings"
	"testing"
)

func TestBuildEnvironmentRejectsAmbientIdentityAndOpenCodeOverrides(t *testing.T) {
	reserved := []string{
		"HOME",
		"TMPDIR",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"XDG_STATE_HOME",
		"XDG_CACHE_HOME",
		"XDG_RUNTIME_DIR",
		"OPENCODE_CONFIG",
		"OPENCODE_CONFIG_CONTENT",
		"OPENCODE_CONFIG_DIR",
		"OPENCODE_DISABLE_PROJECT_CONFIG",
	}
	for _, key := range reserved {
		key := key
		t.Run("allowlist_"+strings.ToLower(key), func(t *testing.T) {
			runDir := t.TempDir()
			t.Setenv(key, "/ambient/must-not-be-inherited")
			if _, err := buildEnvironment([]string{key}, nil, runDir); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("reserved allowlist name %q was accepted: %v", key, err)
			}
		})
		t.Run("override_"+strings.ToLower(key), func(t *testing.T) {
			if _, err := buildEnvironment(nil, map[string]string{key: "/ambient/must-not-be-injected"}, t.TempDir()); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("reserved override name %q was accepted: %v", key, err)
			}
		})
	}
}

func TestBuildEnvironmentDoesNotInheritAmbientProviderOrOpenCodeVariables(t *testing.T) {
	ambient := map[string]string{
		"OPENAI_API_KEY":                  "ambient-openai-secret",
		"ANTHROPIC_API_KEY":               "ambient-anthropic-secret",
		"OPENCODE_CONFIG":                 "/ambient/opencode.json",
		"OPENCODE_CONFIG_CONTENT":         `{"plugin":["ambient"]}`,
		"OPENCODE_DISABLE_PROJECT_CONFIG": "0",
		"CODEX_HOME":                      "/ambient/codex",
		"NODE_OPTIONS":                    "--require=/ambient/plugin.js",
	}
	for key, value := range ambient {
		t.Setenv(key, value)
	}
	env, err := buildEnvironment(nil, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	effective := envMap(env)
	for key := range ambient {
		if value, exists := effective[key]; exists {
			t.Fatalf("ambient variable %q leaked with value %q", key, value)
		}
	}
}
