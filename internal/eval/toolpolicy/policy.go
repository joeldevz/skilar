// Package toolpolicy creates and verifies an evaluator-owned OpenCode tool
// boundary. The generated policy is fail-closed: unknown tools are denied,
// ambient MCPs are disabled, plugins/connectors are removed, and only declared
// local stdio fakes may be enabled.
package toolpolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

const (
	SchemaVersion      = 1
	maxPolicyJSONBytes = 8 << 20
)

var (
	ErrUnsafePolicy = errors.New("unsafe evaluator tool policy")
	namePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	envNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// These external mutation surfaces are denied even if a case accidentally
// places one in AllowedTools. A local fake may expose similarly named read-only
// tools only when it uses a different, explicit fake tool name.
var mutationTools = map[string]struct{}{
	"deploy": {}, "deployment": {}, "email": {}, "github": {},
	"github_create_issue": {}, "github_create_pr": {}, "github_merge": {},
	"github_pr": {}, "github_push": {}, "git_commit": {}, "git_push": {},
	"send_email": {}, "send_message": {}, "skynex_workflow": {},
	"slack": {}, "neurox_save": {}, "neurox_update": {},
	"neurox_session_start": {}, "neurox_session_end": {},
}

var forbiddenFakeExecutables = map[string]struct{}{
	"ash": {}, "bash": {}, "busybox": {}, "cmd": {}, "cmd.exe": {},
	"dash": {}, "env": {}, "fish": {}, "ksh": {}, "powershell": {},
	"powershell.exe": {}, "pwsh": {}, "sh": {}, "zsh": {},
}

type FakeMCP struct {
	Name string `json:"name"`
	// Command is an evaluator-owned, already-resolved stdio command. Generate
	// validates its shape and removes credential-bearing environment, but the
	// caller remains responsible for freezing/pinning the executable content.
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment,omitempty"`
	Tools       []string          `json:"tools"`
}

type Input struct {
	Base            json.RawMessage `json:"-"`
	AllowedTools    []string        `json:"allowed_tools"`
	ForbiddenTools  []string        `json:"forbidden_tools"`
	AmbientMCPNames []string        `json:"ambient_mcp_names,omitempty"`
	FakeMCPs        []FakeMCP       `json:"fake_mcps,omitempty"`
}

type Effective struct {
	SchemaVersion    int               `json:"schema_version"`
	Config           json.RawMessage   `json:"config"`
	Digest           string            `json:"digest"`
	PromptTools      map[string]bool   `json:"prompt_tools"`
	FakeToolBindings []FakeToolBinding `json:"fake_tool_bindings,omitempty"`
	EnabledFakes     []string          `json:"enabled_fakes,omitempty"`
	DisabledMCPs     []string          `json:"disabled_mcps,omitempty"`
}

// FakeToolBinding records the distinction between the raw name returned by an
// MCP server and the effective tool ID exposed by OpenCode. Case contracts use
// RawTool; config and prompt tool maps must use EffectiveID.
type FakeToolBinding struct {
	MCPName     string `json:"mcp_name"`
	RawTool     string `json:"raw_tool"`
	EffectiveID string `json:"effective_id"`
}

// OpenCodeMCPToolID applies OpenCode's documented MCP tool naming rule:
// <server>_<tool>, replacing every character other than ASCII letters,
// numbers, underscore, and hyphen with underscore.
func OpenCodeMCPToolID(serverName, rawTool string) (string, error) {
	if !namePattern.MatchString(serverName) {
		return "", fmt.Errorf("%w: invalid MCP server name %q", ErrUnsafePolicy, serverName)
	}
	if !namePattern.MatchString(rawTool) {
		return "", fmt.Errorf("%w: invalid raw MCP tool name %q", ErrUnsafePolicy, rawTool)
	}
	effective := normalizeOpenCodeToolSegment(serverName) + "_" + normalizeOpenCodeToolSegment(rawTool)
	if !namePattern.MatchString(effective) {
		return "", fmt.Errorf("%w: effective MCP tool ID %q exceeds the supported name bound", ErrUnsafePolicy, effective)
	}
	return effective, nil
}

// ResolveDeclaredToolID translates a case-level raw fake tool name to the
// effective OpenCode ID. Non-fake tools are returned unchanged. Generate
// rejects ambiguous aliases, so this lookup is one-to-one.
func (e Effective) ResolveDeclaredToolID(declared string) string {
	for _, binding := range e.FakeToolBindings {
		if strings.EqualFold(binding.RawTool, declared) {
			return binding.EffectiveID
		}
	}
	return strings.ToLower(declared)
}

// BindPromptTools expands the policy over the runtime's complete tool catalog.
// OpenCode prompt requests use this returned map so a newly installed or
// previously unknown tool is explicitly false rather than relying on defaults.
// Missing allowed tools fail early because such a run cannot exercise the
// declared case contract.
func BindPromptTools(effective Effective, available []string) (map[string]bool, error) {
	policyTools, err := validateEffective(effective)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeNames("runtime tool", available)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(normalized)+1)
	result["*"] = false
	seen := make(map[string]struct{}, len(normalized))
	for _, tool := range normalized {
		seen[tool] = struct{}{}
		result[tool] = policyTools[tool]
	}
	for tool, allowed := range policyTools {
		if tool == "*" || !allowed {
			continue
		}
		if _, ok := seen[tool]; !ok {
			return nil, fmt.Errorf("%w: explicitly allowed tool %q is absent from the runtime catalog", ErrUnsafePolicy, tool)
		}
	}
	return result, nil
}

func validateEffective(effective Effective) (map[string]bool, error) {
	if effective.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: unsupported effective policy schema version %d", ErrUnsafePolicy, effective.SchemaVersion)
	}
	if len(effective.Config) == 0 || len(effective.Config) > maxPolicyJSONBytes {
		return nil, fmt.Errorf("%w: effective config has invalid size", ErrUnsafePolicy)
	}
	sum := sha256.Sum256(effective.Config)
	if effective.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("%w: effective config digest mismatch", ErrUnsafePolicy)
	}
	var config map[string]any
	decoder := json.NewDecoder(bytes.NewReader(effective.Config))
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil || config == nil {
		return nil, fmt.Errorf("%w: decode effective config", ErrUnsafePolicy)
	}
	if trailingErr := decoder.Decode(new(any)); !errors.Is(trailingErr, io.EOF) {
		return nil, fmt.Errorf("%w: effective config contains trailing JSON", ErrUnsafePolicy)
	}
	canonical, err := json.Marshal(config)
	if err != nil || !bytes.Equal(canonical, effective.Config) {
		return nil, fmt.Errorf("%w: effective config is not canonical", ErrUnsafePolicy)
	}
	command, ok := config["command"].(map[string]any)
	if !ok || len(command) != 0 {
		return nil, fmt.Errorf("%w: effective command catalogue must be an empty object", ErrUnsafePolicy)
	}
	plugins, ok := config["plugin"].([]any)
	if !ok || len(plugins) != 0 {
		return nil, fmt.Errorf("%w: effective plugins/connectors must be empty", ErrUnsafePolicy)
	}
	for key, want := range map[string]any{
		"formatter": false, "lsp": false, "share": "disabled", "autoshare": false, "autoupdate": false,
	} {
		if config[key] != want {
			return nil, fmt.Errorf("%w: effective %s is not pinned offline", ErrUnsafePolicy, key)
		}
	}
	if _, exists := config["experimental"]; exists {
		return nil, fmt.Errorf("%w: effective experimental configuration is forbidden", ErrUnsafePolicy)
	}
	if err := rejectInlineProviderCredentials(config["provider"]); err != nil {
		return nil, err
	}
	rawTools, ok := config["tools"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: effective config tools are missing", ErrUnsafePolicy)
	}
	tools := make(map[string]bool, len(rawTools))
	for name, raw := range rawTools {
		enabled, ok := raw.(bool)
		if !ok || !namePattern.MatchString(name) && name != "*" {
			return nil, fmt.Errorf("%w: invalid effective tool entry %q", ErrUnsafePolicy, name)
		}
		tools[name] = enabled
	}
	if wildcard, exists := tools["*"]; !exists || wildcard {
		return nil, fmt.Errorf("%w: effective tool wildcard must deny", ErrUnsafePolicy)
	}
	if len(tools) != len(effective.PromptTools) {
		return nil, fmt.Errorf("%w: effective prompt tools differ from config", ErrUnsafePolicy)
	}
	for name, enabled := range tools {
		if promptEnabled, exists := effective.PromptTools[name]; !exists || promptEnabled != enabled {
			return nil, fmt.Errorf("%w: effective prompt tool %q differs from config", ErrUnsafePolicy, name)
		}
	}
	rawPermission, ok := config["permission"].(map[string]any)
	if !ok || len(rawPermission) != len(tools) {
		return nil, fmt.Errorf("%w: effective permissions differ from tools", ErrUnsafePolicy)
	}
	for name, enabled := range tools {
		if enabled {
			if _, mutating := mutationTools[strings.ToLower(name)]; mutating {
				return nil, fmt.Errorf("%w: mutating external tool %q is enabled", ErrUnsafePolicy, name)
			}
		}
		want := "deny"
		if enabled {
			want = "allow"
		}
		if rawPermission[name] != want {
			return nil, fmt.Errorf("%w: effective permission for %q differs from tools", ErrUnsafePolicy, name)
		}
	}
	rawMCP, ok := config["mcp"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: effective MCP config is missing", ErrUnsafePolicy)
	}
	enabledMCPs := make(map[string]struct{})
	enabledNames := make([]string, 0)
	disabledNames := make([]string, 0)
	for name, rawEntry := range rawMCP {
		entry, ok := rawEntry.(map[string]any)
		if !ok || !namePattern.MatchString(name) {
			return nil, fmt.Errorf("%w: invalid effective MCP entry %q", ErrUnsafePolicy, name)
		}
		enabled, ok := entry["enabled"].(bool)
		if !ok {
			return nil, fmt.Errorf("%w: effective MCP %q has no explicit enabled boolean", ErrUnsafePolicy, name)
		}
		if !enabled {
			if len(entry) != 1 {
				return nil, fmt.Errorf("%w: disabled MCP %q retains ambient configuration", ErrUnsafePolicy, name)
			}
			disabledNames = append(disabledNames, name)
			continue
		}
		command, commandOK := entry["command"].([]any)
		executable := ""
		if commandOK && len(command) != 0 {
			executable, _ = command[0].(string)
		}
		if entry["type"] != "local" || executable == "" || !filepath.IsAbs(executable) {
			return nil, fmt.Errorf("%w: enabled MCP %q is not a declared absolute local command", ErrUnsafePolicy, name)
		}
		enabledMCPs[name] = struct{}{}
		enabledNames = append(enabledNames, name)
	}
	sort.Strings(enabledNames)
	sort.Strings(disabledNames)
	if !slices.Equal(enabledNames, effective.EnabledFakes) || !slices.Equal(disabledNames, effective.DisabledMCPs) {
		return nil, fmt.Errorf("%w: effective MCP metadata differs from config", ErrUnsafePolicy)
	}
	seenRaw := make(map[string]struct{}, len(effective.FakeToolBindings))
	seenEffective := make(map[string]struct{}, len(effective.FakeToolBindings))
	boundMCPs := make(map[string]struct{}, len(enabledMCPs))
	for _, binding := range effective.FakeToolBindings {
		want, err := OpenCodeMCPToolID(binding.MCPName, binding.RawTool)
		_, mcpEnabled := enabledMCPs[binding.MCPName]
		if err != nil || want != binding.EffectiveID || !tools[binding.EffectiveID] || !mcpEnabled {
			return nil, fmt.Errorf("%w: invalid fake tool binding for %q/%q", ErrUnsafePolicy, binding.MCPName, binding.RawTool)
		}
		rawKey := strings.ToLower(binding.RawTool)
		if _, duplicate := seenRaw[rawKey]; duplicate {
			return nil, fmt.Errorf("%w: duplicate raw fake tool binding %q", ErrUnsafePolicy, binding.RawTool)
		}
		if _, duplicate := seenEffective[binding.EffectiveID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate effective fake tool binding %q", ErrUnsafePolicy, binding.EffectiveID)
		}
		seenRaw[rawKey] = struct{}{}
		seenEffective[binding.EffectiveID] = struct{}{}
		boundMCPs[binding.MCPName] = struct{}{}
	}
	if len(boundMCPs) != len(enabledMCPs) {
		return nil, fmt.Errorf("%w: an enabled fake MCP has no declared tool binding", ErrUnsafePolicy)
	}
	return tools, nil
}

// Generate overlays a restrictive boundary on an evaluator-owned base config.
// It does not merge with user config; callers must start OpenCode with this
// config as the complete effective layer and then call VerifyRuntimeConfig on
// the server's resolved /config response.
func Generate(input Input) (Effective, error) {
	normalized, err := normalizeInput(input)
	if err != nil {
		return Effective{}, err
	}
	base := make(map[string]any)
	if len(normalized.Base) > maxPolicyJSONBytes {
		return Effective{}, fmt.Errorf("%w: base config exceeds %d bytes", ErrUnsafePolicy, maxPolicyJSONBytes)
	}
	if len(bytes.TrimSpace(normalized.Base)) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(normalized.Base))
		decoder.UseNumber()
		if err := decoder.Decode(&base); err != nil {
			return Effective{}, fmt.Errorf("%w: decode base config: %v", ErrUnsafePolicy, err)
		}
		if trailingErr := decoder.Decode(new(any)); !errors.Is(trailingErr, io.EOF) {
			return Effective{}, fmt.Errorf("%w: base config contains multiple JSON values", ErrUnsafePolicy)
		}
		if base == nil {
			return Effective{}, fmt.Errorf("%w: base config must be a JSON object, not null", ErrUnsafePolicy)
		}
	}

	ambient := make(map[string]struct{}, len(normalized.AmbientMCPNames))
	for _, name := range normalized.AmbientMCPNames {
		ambient[name] = struct{}{}
	}
	if rawMCP, ok := base["mcp"].(map[string]any); ok {
		for name := range rawMCP {
			if !namePattern.MatchString(name) {
				return Effective{}, fmt.Errorf("%w: invalid ambient MCP name %q", ErrUnsafePolicy, name)
			}
			ambient[name] = struct{}{}
		}
	} else if value, exists := base["mcp"]; exists && value != nil {
		return Effective{}, fmt.Errorf("%w: base mcp field must be an object", ErrUnsafePolicy)
	}
	if err := rejectInlineProviderCredentials(base["provider"]); err != nil {
		return Effective{}, err
	}

	allowed := make(map[string]struct{}, len(normalized.AllowedTools))
	for _, tool := range normalized.AllowedTools {
		allowed[tool] = struct{}{}
	}
	bindings, effectiveAllowed, err := bindFakeTools(normalized.FakeMCPs, allowed)
	if err != nil {
		return Effective{}, err
	}
	forbiddenFolded := make(map[string]string, len(normalized.ForbiddenTools))
	for _, forbidden := range normalized.ForbiddenTools {
		forbiddenFolded[strings.ToLower(forbidden)] = forbidden
	}
	for effectiveTool := range effectiveAllowed {
		if forbidden, conflict := forbiddenFolded[strings.ToLower(effectiveTool)]; conflict {
			return Effective{}, fmt.Errorf("%w: effective tool %q conflicts with forbidden tool %q", ErrUnsafePolicy, effectiveTool, forbidden)
		}
	}
	tools := map[string]bool{"*": false}
	permission := map[string]string{"*": "deny"}
	effectiveAllowedNames := make([]string, 0, len(effectiveAllowed))
	for tool := range effectiveAllowed {
		effectiveAllowedNames = append(effectiveAllowedNames, tool)
	}
	sort.Strings(effectiveAllowedNames)
	for _, tool := range effectiveAllowedNames {
		tools[tool] = true
		permission[tool] = "allow"
	}
	for _, tool := range normalized.ForbiddenTools {
		tools[tool] = false
		permission[tool] = "deny"
	}

	mcp := make(map[string]any, len(ambient)+len(normalized.FakeMCPs))
	for name := range ambient {
		mcp[name] = map[string]any{"enabled": false}
	}
	enabledFakes := make([]string, 0, len(normalized.FakeMCPs))
	for _, fake := range normalized.FakeMCPs {
		entry := map[string]any{"type": "local", "enabled": true, "command": fake.Command}
		if len(fake.Environment) != 0 {
			entry["environment"] = fake.Environment
		}
		mcp[fake.Name] = entry
		enabledFakes = append(enabledFakes, fake.Name)
		delete(ambient, fake.Name)
	}
	base["mcp"] = mcp
	base["plugin"] = []string{}
	// OpenCode otherwise injects built-in command templates into the resolved
	// config. Evaluation prompts do not need slash commands, so pin an empty
	// evaluator-owned map and make any layered addition observable.
	base["command"] = map[string]any{}
	base["formatter"] = false
	base["lsp"] = false
	base["share"] = "disabled"
	base["autoshare"] = false
	base["autoupdate"] = false
	delete(base, "experimental")
	base["tools"] = tools
	base["permission"] = permission
	for _, field := range []string{"agent", "mode"} {
		if err := applyAgentBoundary(base, field, tools, permission); err != nil {
			return Effective{}, err
		}
	}

	raw, err := json.Marshal(base)
	if err != nil {
		return Effective{}, fmt.Errorf("marshal effective config: %w", err)
	}
	sum := sha256.Sum256(raw)
	disabled := make([]string, 0, len(ambient))
	for name := range ambient {
		disabled = append(disabled, name)
	}
	sort.Strings(disabled)
	return Effective{
		SchemaVersion:    SchemaVersion,
		Config:           raw,
		Digest:           "sha256:" + hex.EncodeToString(sum[:]),
		PromptTools:      tools,
		FakeToolBindings: bindings,
		EnabledFakes:     enabledFakes,
		DisabledMCPs:     disabled,
	}, nil
}

func bindFakeTools(fakes []FakeMCP, declaredAllowed map[string]struct{}) ([]FakeToolBinding, map[string]struct{}, error) {
	effectiveAllowed := make(map[string]struct{}, len(declaredAllowed))
	for tool := range declaredAllowed {
		effectiveAllowed[tool] = struct{}{}
	}
	bindings := make([]FakeToolBinding, 0)
	rawOwners := make(map[string]string)
	effectiveOwners := make(map[string]FakeToolBinding)
	for _, fake := range fakes {
		for _, rawTool := range fake.Tools {
			if _, ok := declaredAllowed[rawTool]; !ok {
				return nil, nil, fmt.Errorf("%w: fake MCP %q exposes raw tool %q which is not explicitly allowed", ErrUnsafePolicy, fake.Name, rawTool)
			}
			if owner, duplicate := rawOwners[rawTool]; duplicate {
				return nil, nil, fmt.Errorf("%w: raw fake tool %q is ambiguous between MCPs %q and %q", ErrUnsafePolicy, rawTool, owner, fake.Name)
			}
			effectiveID, err := OpenCodeMCPToolID(fake.Name, rawTool)
			if err != nil {
				return nil, nil, err
			}
			binding := FakeToolBinding{MCPName: fake.Name, RawTool: rawTool, EffectiveID: effectiveID}
			if owner, collision := effectiveOwners[effectiveID]; collision {
				return nil, nil, fmt.Errorf("%w: fake MCP tools %q/%q and %q/%q normalize to the same OpenCode ID %q", ErrUnsafePolicy, owner.MCPName, owner.RawTool, fake.Name, rawTool, effectiveID)
			}
			if _, declared := declaredAllowed[effectiveID]; declared && effectiveID != rawTool {
				return nil, nil, fmt.Errorf("%w: effective fake tool ID %q collides with a separately declared allowed tool", ErrUnsafePolicy, effectiveID)
			}
			rawOwners[rawTool] = fake.Name
			effectiveOwners[effectiveID] = binding
			delete(effectiveAllowed, rawTool)
			effectiveAllowed[effectiveID] = struct{}{}
			bindings = append(bindings, binding)
		}
	}
	return bindings, effectiveAllowed, nil
}

func normalizeOpenCodeToolSegment(value string) string {
	return strings.Map(func(char rune) rune {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' {
			return char
		}
		return '_'
	}, value)
}

func rejectInlineProviderCredentials(value any) error {
	providers, ok := value.(map[string]any)
	if !ok {
		if value == nil {
			return nil
		}
		return fmt.Errorf("%w: base provider field must be an object", ErrUnsafePolicy)
	}
	for providerName, rawProvider := range providers {
		provider, ok := rawProvider.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: provider %q must be an object", ErrUnsafePolicy, providerName)
		}
		nodes := 0
		if err := scanProviderCredentials(providerName, provider, "provider."+providerName, 0, &nodes); err != nil {
			return err
		}
	}
	return nil
}

func scanProviderCredentials(providerName string, value any, path string, depth int, nodes *int) error {
	(*nodes)++
	if depth > 32 || *nodes > 100_000 {
		return fmt.Errorf("%w: provider %q configuration is too deeply nested or complex", ErrUnsafePolicy, providerName)
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if credentialFieldName(key) && credentialValuePresent(child) {
				return fmt.Errorf("%w: provider %q contains inline credential field %q at %s; use an evaluator-owned provider proxy", ErrUnsafePolicy, providerName, key, path)
			}
			if text, ok := child.(string); ok && urlContainsCredentials(text) {
				return fmt.Errorf("%w: provider %q contains credentials in a URL at %s.%s", ErrUnsafePolicy, providerName, path, key)
			}
			if err := scanProviderCredentials(providerName, child, path+"."+key, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := scanProviderCredentials(providerName, child, fmt.Sprintf("%s[%d]", path, index), depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

func credentialFieldName(key string) bool {
	normalized := strings.Map(func(char rune) rune {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			return char
		}
		return -1
	}, strings.ToLower(key))
	for _, exact := range []string{
		"apikey", "token", "accesstoken", "bearertoken", "secret", "clientsecret",
		"password", "authorization", "proxyauthorization", "cookie", "privatekey", "credential",
		"header", "headers",
	} {
		if normalized == exact || strings.HasSuffix(normalized, exact) {
			return true
		}
	}
	return false
}

func credentialValuePresent(value any) bool {
	if value == nil {
		return false
	}
	if text, ok := value.(string); ok {
		return text != ""
	}
	if object, ok := value.(map[string]any); ok {
		return len(object) != 0
	}
	if array, ok := value.([]any); ok {
		return len(array) != 0
	}
	return true
}

func urlContainsCredentials(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs() && parsed.User != nil
}

func applyAgentBoundary(base map[string]any, field string, tools map[string]bool, permission map[string]string) error {
	agents, ok := base[field].(map[string]any)
	if !ok {
		if value, exists := base[field]; exists && value != nil {
			return fmt.Errorf("%w: base %s field must be an object", ErrUnsafePolicy, field)
		}
		return nil
	}
	for name, rawAgent := range agents {
		agent, ok := rawAgent.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: %s %q must be an object", ErrUnsafePolicy, field, name)
		}
		agent["tools"] = cloneBoolMap(tools)
		agent["permission"] = cloneStringMap(permission)
	}
	return nil
}

func normalizeInput(input Input) (Input, error) {
	fakes := make([]FakeMCP, len(input.FakeMCPs))
	for i, fake := range input.FakeMCPs {
		fakes[i] = fake
		fakes[i].Command = append([]string(nil), fake.Command...)
		fakes[i].Tools = append([]string(nil), fake.Tools...)
		if fake.Environment != nil {
			fakes[i].Environment = make(map[string]string, len(fake.Environment))
			for key, value := range fake.Environment {
				fakes[i].Environment[key] = value
			}
		}
	}
	input.FakeMCPs = fakes
	rawFakeTools := make(map[string]string)
	for _, fake := range input.FakeMCPs {
		for _, rawTool := range fake.Tools {
			if !namePattern.MatchString(rawTool) {
				return Input{}, fmt.Errorf("%w: invalid fake MCP tool name %q", ErrUnsafePolicy, rawTool)
			}
			folded := strings.ToLower(rawTool)
			if existing, collision := rawFakeTools[folded]; collision && existing != rawTool {
				return Input{}, fmt.Errorf("%w: raw fake tools %q and %q differ only by case", ErrUnsafePolicy, existing, rawTool)
			}
			rawFakeTools[folded] = rawTool
		}
	}
	var err error
	input.AllowedTools, err = normalizeToolDeclarations("allowed tool", input.AllowedTools, rawFakeTools)
	if err != nil {
		return Input{}, err
	}
	input.ForbiddenTools, err = normalizeToolDeclarations("forbidden tool", input.ForbiddenTools, rawFakeTools)
	if err != nil {
		return Input{}, err
	}
	input.AmbientMCPNames, err = normalizeNames("ambient MCP", input.AmbientMCPNames)
	if err != nil {
		return Input{}, err
	}
	allowedFolded := make(map[string]string, len(input.AllowedTools))
	for _, tool := range input.AllowedTools {
		if _, mutating := mutationTools[strings.ToLower(tool)]; mutating {
			return Input{}, fmt.Errorf("%w: external mutation tool %q cannot be allowed", ErrUnsafePolicy, tool)
		}
		allowedFolded[strings.ToLower(tool)] = tool
	}
	for _, tool := range input.ForbiddenTools {
		if allowedTool, overlap := allowedFolded[strings.ToLower(tool)]; overlap {
			return Input{}, fmt.Errorf("%w: tool %q is both allowed and forbidden as %q", ErrUnsafePolicy, allowedTool, tool)
		}
	}
	seenFakes := make(map[string]struct{}, len(input.FakeMCPs))
	for i := range input.FakeMCPs {
		fake := &input.FakeMCPs[i]
		if !namePattern.MatchString(fake.Name) {
			return Input{}, fmt.Errorf("%w: invalid fake MCP name %q", ErrUnsafePolicy, fake.Name)
		}
		if _, duplicate := seenFakes[fake.Name]; duplicate {
			return Input{}, fmt.Errorf("%w: duplicate fake MCP %q", ErrUnsafePolicy, fake.Name)
		}
		seenFakes[fake.Name] = struct{}{}
		if len(fake.Command) == 0 || !filepath.IsAbs(fake.Command[0]) {
			return Input{}, fmt.Errorf("%w: fake MCP %q command must start with an absolute executable", ErrUnsafePolicy, fake.Name)
		}
		if _, shell := forbiddenFakeExecutables[strings.ToLower(filepath.Base(fake.Command[0]))]; shell {
			return Input{}, fmt.Errorf("%w: fake MCP %q uses forbidden shell executable", ErrUnsafePolicy, fake.Name)
		}
		if len(fake.Command) > 256 {
			return Input{}, fmt.Errorf("%w: fake MCP %q command exceeds 256 arguments", ErrUnsafePolicy, fake.Name)
		}
		commandBytes := 0
		for _, arg := range fake.Command {
			if strings.IndexByte(arg, 0) >= 0 {
				return Input{}, fmt.Errorf("%w: fake MCP %q command contains NUL", ErrUnsafePolicy, fake.Name)
			}
			commandBytes += len(arg)
			if commandBytes > 64<<10 {
				return Input{}, fmt.Errorf("%w: fake MCP %q command exceeds 64KiB", ErrUnsafePolicy, fake.Name)
			}
		}
		fake.Tools, err = normalizeNames("fake MCP tool", fake.Tools)
		if err != nil {
			return Input{}, err
		}
		if len(fake.Tools) == 0 {
			return Input{}, fmt.Errorf("%w: fake MCP %q must declare its tools", ErrUnsafePolicy, fake.Name)
		}
		if len(fake.Environment) > 128 {
			return Input{}, fmt.Errorf("%w: fake MCP %q environment exceeds 128 entries", ErrUnsafePolicy, fake.Name)
		}
		environmentBytes := 0
		for key, value := range fake.Environment {
			if !envNamePattern.MatchString(key) || strings.IndexByte(value, 0) >= 0 {
				return Input{}, fmt.Errorf("%w: fake MCP %q has invalid environment entry", ErrUnsafePolicy, fake.Name)
			}
			if secretEnvironmentKey(key) {
				return Input{}, fmt.Errorf("%w: fake MCP %q has credential-like environment key %q", ErrUnsafePolicy, fake.Name, key)
			}
			environmentBytes += len(key) + len(value) + 1
			if environmentBytes > 64<<10 {
				return Input{}, fmt.Errorf("%w: fake MCP %q environment exceeds 64KiB", ErrUnsafePolicy, fake.Name)
			}
		}
	}
	sort.Slice(input.FakeMCPs, func(i, j int) bool { return input.FakeMCPs[i].Name < input.FakeMCPs[j].Name })
	return input, nil
}

func normalizeNames(label string, values []string) ([]string, error) {
	result := append([]string(nil), values...)
	sort.Strings(result)
	for i, value := range result {
		if !namePattern.MatchString(value) {
			return nil, fmt.Errorf("%w: invalid %s name %q", ErrUnsafePolicy, label, value)
		}
		if i > 0 && result[i-1] == value {
			return nil, fmt.Errorf("%w: duplicate %s %q", ErrUnsafePolicy, label, value)
		}
	}
	return result, nil
}

func normalizeToolDeclarations(label string, values []string, rawFakeTools map[string]string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !namePattern.MatchString(value) {
			return nil, fmt.Errorf("%w: invalid %s name %q", ErrUnsafePolicy, label, value)
		}
		folded := strings.ToLower(value)
		canonical := folded
		if rawTool, fake := rawFakeTools[folded]; fake {
			canonical = rawTool
		}
		if _, duplicate := seen[folded]; duplicate {
			return nil, fmt.Errorf("%w: duplicate %s %q (case-insensitive)", ErrUnsafePolicy, label, value)
		}
		seen[folded] = struct{}{}
		result = append(result, canonical)
	}
	sort.Strings(result)
	return result, nil
}

func cloneBoolMap(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func secretEnvironmentKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, exact := range []string{
		"SSH_AUTH_SOCK", "SSH_AGENT_PID", "DOCKER_CONFIG", "REGISTRY_AUTH_FILE",
		"KUBECONFIG", "NETRC", "NPM_CONFIG_USERCONFIG", "PIP_CONFIG_FILE",
		"GNUPGHOME", "AZURE_CONFIG_DIR", "GOOGLE_APPLICATION_CREDENTIALS",
	} {
		if upper == exact {
			return true
		}
	}
	for _, prefix := range []string{"AWS_", "AZURE_", "GCP_", "GOOGLE_", "OCI_", "GH_", "GITHUB_", "GITLAB_"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	for _, fragment := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "PRIVATE_KEY", "AUTHORIZATION", "COOKIE"} {
		if strings.Contains(upper, fragment) {
			return true
		}
	}
	return false
}
