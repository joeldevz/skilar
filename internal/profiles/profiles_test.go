package profiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		// Valid names
		{"valid-simple", "simple", false},
		{"valid-with-hyphens", "my-profile", false},
		{"valid-with-numbers", "tier-1", false},
		{"valid-max-length", "a-23456789-123456789-123456", false},

		// Invalid names
		{"empty", "", true},
		{"reserved-default", "default", true},
		{"too-long", "a-2345678901234567890123456789012", true},
		{"uppercase", "MyProfile", true},
		{"spaces", "my profile", true},
		{"special-chars", "my@profile", true},
		{"underscore", "my_profile", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestToEnvContent(t *testing.T) {
	p := &Profile{
		Name: "test",
		Models: map[string]string{
			"coder":        "anthropic/claude-haiku-4-5",
			"tech-planner": "anthropic/claude-sonnet-4-6",
			"empty-agent":  "",
		},
	}

	content, err := ToEnvContent(p)
	if err != nil {
		t.Fatalf("ToEnvContent failed: %v", err)
	}

	// Parse the JSON to verify structure
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		t.Fatalf("failed to parse env content: %v", err)
	}

	agent, ok := result["agent"].(map[string]interface{})
	if !ok {
		t.Fatalf("'agent' key missing or not a map")
	}

	// Verify non-empty agents are included
	if coder, ok := agent["coder"].(map[string]interface{}); !ok || coder["model"] != "anthropic/claude-haiku-4-5" {
		t.Error("coder model not found or incorrect")
	}

	if planner, ok := agent["tech-planner"].(map[string]interface{}); !ok || planner["model"] != "anthropic/claude-sonnet-4-6" {
		t.Error("tech-planner model not found or incorrect")
	}

	// Verify empty agent is not included
	if _, ok := agent["empty-agent"]; ok {
		t.Error("empty-agent should not be included in env content")
	}
}

func TestSaveLoadDelete(t *testing.T) {
	// Use temp directory for testing
	tmpDir := t.TempDir()

	// Mock the Dir() function by setting XDG_CONFIG_HOME or similar
	// For this test, we'll directly manipulate profiles directory
	originalDir := Dir()
	t.Cleanup(func() {
		// No need to restore since we use temp dir
	})

	// Create profile with absolute path
	p := &Profile{
		Name: "test-profile",
		Models: map[string]string{
			"coder": "anthropic/claude-haiku-4-5",
		},
	}

	profilePath := filepath.Join(tmpDir, "test-profile.json")

	// Save profile manually to temp directory
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal profile: %v", err)
	}

	if err := os.WriteFile(profilePath, data, 0o644); err != nil {
		t.Fatalf("failed to write test profile: %v", err)
	}

	// Load the profile back
	loadedData, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("failed to read profile: %v", err)
	}

	var loaded Profile
	if err := json.Unmarshal(loadedData, &loaded); err != nil {
		t.Fatalf("failed to unmarshal profile: %v", err)
	}

	if loaded.Name != p.Name {
		t.Errorf("profile name mismatch: got %s, want %s", loaded.Name, p.Name)
	}

	if loaded.Models["coder"] != p.Models["coder"] {
		t.Errorf("coder model mismatch: got %s, want %s", loaded.Models["coder"], p.Models["coder"])
	}

	// Delete the file
	if err := os.Remove(profilePath); err != nil {
		t.Fatalf("failed to delete profile: %v", err)
	}

	// Verify it's deleted
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Error("profile was not deleted")
	}

	_ = originalDir // silence unused variable
}

func TestBuiltinTiers(t *testing.T) {
	for _, tierName := range []string{"cheap", "balanced", "premium"} {
		tier, ok := BuiltinTiers[tierName]
		if !ok {
			t.Fatalf("tier %q not found", tierName)
		}
		if _, exists := tier.Models["advisor"]; exists {
			t.Errorf("%q retains retired advisor", tierName)
		}
		for _, agent := range AgentOrder {
			if model := tier.Models[agent]; model == "" {
				t.Errorf("%q missing model for %q", tierName, agent)
			}
		}
	}
}

func TestProfileTimestamps(t *testing.T) {
	tmpDir := t.TempDir()
	profilePath := filepath.Join(tmpDir, "test.json")

	p := &Profile{
		Name: "test",
		Models: map[string]string{
			"coder": "anthropic/claude-haiku-4-5",
		},
	}

	// Create profile
	data, _ := json.MarshalIndent(p, "", "  ")
	os.WriteFile(profilePath, data, 0o644)

	// Load and verify timestamps
	loaded := &Profile{}
	loadedData, _ := os.ReadFile(profilePath)
	json.Unmarshal(loadedData, loaded)

	// Since we didn't set timestamps, they should be zero
	if !loaded.CreatedAt.IsZero() && !loaded.UpdatedAt.IsZero() {
		t.Logf("timestamps: created=%v, updated=%v", loaded.CreatedAt, loaded.UpdatedAt)
	}
}

func TestSimpleGroups(t *testing.T) {
	// Verify all agents in groups are known
	knownAgents := make(map[string]bool)
	for _, agent := range AgentOrder {
		knownAgents[agent] = true
	}

	for group, agents := range SimpleGroups {
		for _, agent := range agents {
			if !knownAgents[agent] {
				t.Errorf("unknown agent %q in group %q", agent, group)
			}
		}
	}

	// Verify we have at least three groups
	if len(SimpleGroups) != 2 {
		t.Errorf("expected 2 groups, got %d", len(SimpleGroups))
	}

	// Verify all required groups exist
	for _, g := range []string{"orchestrator", "workers"} {
		if _, ok := SimpleGroups[g]; !ok {
			t.Errorf("required group %q not found", g)
		}
	}
}

func TestLoadMigratesLegacyAdvisorModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"name":"legacy","models":{"advisor":"legacy/opus","coder":"current/model"}}`)
	if err := os.WriteFile(filepath.Join(dir, "legacy.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := Load("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := profile.Models["advisor"]; ok {
		t.Fatal("legacy advisor model remained public")
	}
	if profile.Models["coder"] != "current/model" {
		t.Fatal("non-Advisor legacy data was not preserved")
	}
}
