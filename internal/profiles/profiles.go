package profiles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Profile represents a configuration of models by agent.
type Profile struct {
	Name      string            `json:"name"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Models    map[string]string `json:"models"` // agentName -> modelID
}

// AgentOrder defines the display order for known agents.
var AgentOrder = []string{
	"orchestrator",
	"tech-planner",
	"product-planner",
	"coder",
	"verifier",
	"test-reviewer",
	"security",
	"skill-validator",
}

// SimpleGroups organizes agents into logical groups for simple mode.
var SimpleGroups = map[string][]string{
	"orchestrator": {"orchestrator"},
	"workers": {
		"tech-planner",
		"product-planner",
		"coder",
		"verifier",
		"test-reviewer",
		"security",
		"skill-validator",
	},
}

// BuiltinTiers defines predefined tier profiles.
var BuiltinTiers = map[string]*Profile{
	"cheap": {
		Name: "cheap",
		Models: map[string]string{
			"orchestrator":    "anthropic/claude-haiku-4-5",
			"tech-planner":    "anthropic/claude-haiku-4-5",
			"product-planner": "anthropic/claude-haiku-4-5",
			"coder":           "anthropic/claude-haiku-4-5",
			"verifier":        "anthropic/claude-haiku-4-5",
			"test-reviewer":   "anthropic/claude-haiku-4-5",
			"security":        "anthropic/claude-haiku-4-5",
			"skill-validator": "anthropic/claude-haiku-4-5",
		},
	},
	"balanced": {
		Name: "balanced",
		Models: map[string]string{
			"orchestrator":    "anthropic/claude-sonnet-4-6",
			"tech-planner":    "anthropic/claude-sonnet-4-6",
			"product-planner": "anthropic/claude-haiku-4-5",
			"coder":           "anthropic/claude-haiku-4-5",
			"verifier":        "anthropic/claude-haiku-4-5",
			"test-reviewer":   "anthropic/claude-haiku-4-5",
			"security":        "anthropic/claude-haiku-4-5",
			"skill-validator": "anthropic/claude-haiku-4-5",
		},
	},
	"premium": {
		Name: "premium",
		Models: map[string]string{
			"orchestrator":    "anthropic/claude-opus-4-6",
			"tech-planner":    "anthropic/claude-sonnet-4-6",
			"product-planner": "anthropic/claude-sonnet-4-6",
			"coder":           "anthropic/claude-sonnet-4-6",
			"verifier":        "anthropic/claude-haiku-4-5",
			"test-reviewer":   "anthropic/claude-haiku-4-5",
			"security":        "anthropic/claude-sonnet-4-6",
			"skill-validator": "anthropic/claude-haiku-4-5",
		},
	},
}

// Dir returns the directory where profiles are stored.
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "skynex", "profiles")
}

// List returns all saved profiles sorted by name.
func List() ([]*Profile, error) {
	dir := Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []*Profile{}, nil
		}
		return nil, fmt.Errorf("failed to read profiles directory: %w", err)
	}

	var profiles []*Profile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".json")
		p, err := Load(name)
		if err != nil {
			// Skip profiles that fail to load
			continue
		}
		profiles = append(profiles, p)
	}

	// Sort by name
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].Name < profiles[j].Name
	})

	return profiles, nil
}

// Load loads a profile by name.
func Load(name string) (*Profile, error) {
	path := filepath.Join(Dir(), name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load profile %q: %w", name, err)
	}

	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("failed to parse profile %q: %w", name, err)
	}
	// Accept legacy profiles but never expose the retired Advisor role.
	delete(p.Models, "advisor")

	return &p, nil
}

// Save saves a profile (creates or overwrites).
func Save(p *Profile) error {
	if err := ValidateName(p.Name); err != nil {
		return err
	}

	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create profiles directory: %w", err)
	}

	// Set timestamps
	now := time.Now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now

	// Initialize models map if nil
	if p.Models == nil {
		p.Models = make(map[string]string)
	}
	delete(p.Models, "advisor")

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal profile: %w", err)
	}

	path := filepath.Join(dir, p.Name+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("failed to write profile: %w", err)
	}

	return nil
}

// Delete deletes a profile by name.
func Delete(name string) error {
	path := filepath.Join(Dir(), name+".json")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("profile %q not found", name)
		}
		return fmt.Errorf("failed to delete profile: %w", err)
	}
	return nil
}

// Exists checks if a profile exists.
func Exists(name string) bool {
	path := filepath.Join(Dir(), name+".json")
	_, err := os.Stat(path)
	return err == nil
}

// ValidateName returns error if name is invalid.
// Rules: lowercase, hyphens ok, no spaces, not "default", not empty, max 32 chars
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("profile name cannot be empty")
	}

	if name == "default" {
		return fmt.Errorf("profile name cannot be 'default'")
	}

	if len(name) > 32 {
		return fmt.Errorf("profile name cannot exceed 32 characters")
	}

	if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(name) {
		return fmt.Errorf("profile name must be lowercase letters, numbers, and hyphens only")
	}

	return nil
}

// DefaultProfilePath returns the path to the default profile file.
func DefaultProfilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "skynex", "default-profile")
}

// SetDefault sets the default profile name. Validates that the profile exists
// (either as a builtin tier or a saved custom profile).
func SetDefault(name string) error {
	// Check builtin tiers first
	if _, ok := BuiltinTiers[name]; !ok {
		// Check custom profiles
		if !Exists(name) {
			return fmt.Errorf("profile %q not found (not a builtin tier or custom profile)", name)
		}
	}

	path := DefaultProfilePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
		return fmt.Errorf("failed to save default profile: %w", err)
	}
	return nil
}

// GetDefault returns the default profile name, or "balanced" if none is set.
func GetDefault() string {
	data, err := os.ReadFile(DefaultProfilePath())
	if err != nil {
		return "balanced"
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return "balanced"
	}
	return name
}

// ToEnvContent converts a profile to the JSON string for OPENCODE_CONFIG_CONTENT.
// Only includes agents that have a model assigned (non-empty).
func ToEnvContent(p *Profile) (string, error) {
	// Build minimal JSON with only non-empty models
	content := map[string]interface{}{
		"agent": make(map[string]interface{}),
	}

	agentMap := content["agent"].(map[string]interface{})
	for agent, model := range p.Models {
		if model != "" {
			agentMap[agent] = map[string]string{"model": model}
		}
	}

	data, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("failed to marshal env content: %w", err)
	}

	return string(data), nil
}

// DefaultProfile returns a profile with the current models from opencode.json.
// Used as the base when creating a new profile.
func DefaultProfile() (*Profile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}

	configPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("opencode.json not found at %s", configPath)
		}
		return nil, fmt.Errorf("failed to read opencode.json: %w", err)
	}

	var config struct {
		Agent map[string]struct {
			Model string `json:"model"`
		} `json:"agent"`
	}

	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse opencode.json: %w", err)
	}

	models := make(map[string]string)
	for _, agent := range AgentOrder {
		if cfg, ok := config.Agent[agent]; ok && cfg.Model != "" {
			models[agent] = cfg.Model
		}
	}

	return &Profile{
		Name:   "default",
		Models: models,
	}, nil
}
