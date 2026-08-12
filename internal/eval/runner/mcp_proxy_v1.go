package runner

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/mcpproxy"
	"github.com/joeldevz/skynex/internal/eval/toolpolicy"
)

func prepareMCPProxyPolicy(bundleCopy, proxyOverride string, fakes []toolpolicy.FakeMCP) ([]toolpolicy.FakeMCP, []toolpolicy.MCPAttestationBinding, toolpolicy.MCPProxyIdentity, error) {
	if len(fakes) == 0 {
		return fakes, nil, toolpolicy.MCPProxyIdentity{}, nil
	}
	if err := toolpolicy.ValidateFakeExecutionBoundary(fakes); err != nil {
		return nil, nil, toolpolicy.MCPProxyIdentity{}, fmt.Errorf("validate original fake MCP execution boundary: %w", err)
	}
	proxy, err := resolveMCPProxyIdentity(proxyOverride)
	if err != nil {
		return nil, nil, toolpolicy.MCPProxyIdentity{}, err
	}
	// The writable attestation directory is a sibling of the immutable
	// opencode bundle. In production both are contained by RunPath/control.
	attestationRoot := filepath.Join(filepath.Dir(filepath.Clean(bundleCopy)), "mcp-attestations")
	if err := os.Mkdir(attestationRoot, 0o700); err != nil {
		return nil, nil, toolpolicy.MCPProxyIdentity{}, fmt.Errorf("create MCP attestation directory: %w", err)
	}
	info, err := os.Lstat(attestationRoot)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, nil, toolpolicy.MCPProxyIdentity{}, fmt.Errorf("MCP attestation directory must be a 0700 directory")
	}

	ordered := append([]toolpolicy.FakeMCP(nil), fakes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	bindings := make([]toolpolicy.MCPAttestationBinding, 0, len(ordered))
	for index := range ordered {
		fake := &ordered[index]
		nonce, err := freshMCPNonce()
		if err != nil {
			return nil, nil, toolpolicy.MCPProxyIdentity{}, err
		}
		attestationPath := filepath.Join(attestationRoot, fmt.Sprintf("tools-%03d.json", index+1))
		if _, err := os.Lstat(attestationPath); err == nil || !os.IsNotExist(err) {
			return nil, nil, toolpolicy.MCPProxyIdentity{}, fmt.Errorf("fresh MCP attestation path is unavailable")
		}

		original := append([]string(nil), fake.Command...)
		arguments := []string{proxy.Path, "__mcp-proxy", "--mcp-name", fake.Name}
		tools := append([]string(nil), fake.Tools...)
		sort.Strings(tools)
		for _, tool := range tools {
			arguments = append(arguments, "--tool", tool)
		}
		environmentKeys := make([]string, 0, len(fake.Environment))
		for key := range fake.Environment {
			environmentKeys = append(environmentKeys, key)
		}
		sort.Strings(environmentKeys)
		for _, key := range environmentKeys {
			arguments = append(arguments, "--env", key+"="+fake.Environment[key])
		}
		arguments = append(arguments, "--")
		arguments = append(arguments, original...)
		fake.Command = arguments
		// OpenCode must not add declared values to the proxy's inherited env;
		// they are explicit argv inputs and only the proxy passes them onward.
		fake.Environment = nil
		bindings = append(bindings, toolpolicy.MCPAttestationBinding{
			MCPName: fake.Name, RawTools: tools, AttestationPath: attestationPath, Nonce: nonce,
		})
	}
	return ordered, bindings, proxy, nil
}

func resolveMCPProxyIdentity(override string) (toolpolicy.MCPProxyIdentity, error) {
	path := override
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return toolpolicy.MCPProxyIdentity{}, fmt.Errorf("resolve evaluator MCP proxy executable: %w", err)
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return toolpolicy.MCPProxyIdentity{}, fmt.Errorf("resolve evaluator MCP proxy executable: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return toolpolicy.MCPProxyIdentity{}, fmt.Errorf("canonicalize evaluator MCP proxy executable: %w", err)
	}
	info, err := os.Lstat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return toolpolicy.MCPProxyIdentity{}, fmt.Errorf("evaluator MCP proxy executable must be an executable regular file")
	}
	digest, err := digestRegularFile(canonical)
	if err != nil {
		return toolpolicy.MCPProxyIdentity{}, fmt.Errorf("digest evaluator MCP proxy executable: %w", err)
	}
	return toolpolicy.MCPProxyIdentity{Path: canonical, ContentDigest: digest}, nil
}

func verifyMCPProxyIdentity(identity toolpolicy.MCPProxyIdentity) error {
	if identity.Path == "" || identity.ContentDigest == "" {
		return fmt.Errorf("MCP proxy identity is missing")
	}
	current, err := resolveMCPProxyIdentity(identity.Path)
	if err != nil || current != identity {
		return fmt.Errorf("MCP proxy executable identity drifted")
	}
	return nil
}

func digestRegularFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func freshMCPNonce() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate fresh MCP attestation nonce: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func materializeMCPRuntimeManifest(runPath string, effective toolpolicy.Effective) (string, error) {
	if len(effective.EnabledFakes) == 0 {
		if len(effective.MCPAttestations) != 0 || effective.MCPProxy != (toolpolicy.MCPProxyIdentity{}) {
			return "", fmt.Errorf("unexpected MCP proxy runtime authority")
		}
		return "", nil
	}
	if err := verifyMCPProxyIdentity(effective.MCPProxy); err != nil {
		return "", err
	}
	if err := verifyEffectiveMCPProxyCommands(effective); err != nil {
		return "", err
	}
	controlRoot := filepath.Join(filepath.Clean(runPath), "control")

	expectedTools := make(map[string][]string, len(effective.EnabledFakes))
	for _, binding := range effective.FakeToolBindings {
		expectedTools[binding.MCPName] = append(expectedTools[binding.MCPName], binding.RawTool)
	}
	for name := range expectedTools {
		sort.Strings(expectedTools[name])
	}
	bindings := append([]toolpolicy.MCPAttestationBinding(nil), effective.MCPAttestations...)
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].MCPName < bindings[j].MCPName })
	if len(bindings) != len(effective.EnabledFakes) {
		return "", fmt.Errorf("MCP attestation binding count differs from enabled policy")
	}
	runtimeBindings := make([]mcpproxy.RuntimeBinding, 0, len(bindings))
	for index, binding := range bindings {
		if index > 0 && bindings[index-1].MCPName == binding.MCPName {
			return "", fmt.Errorf("duplicate MCP attestation binding")
		}
		if index >= len(effective.EnabledFakes) || binding.MCPName != effective.EnabledFakes[index] {
			return "", fmt.Errorf("MCP attestation identity differs from enabled policy")
		}
		rawTools := append([]string(nil), binding.RawTools...)
		sort.Strings(rawTools)
		if !equalStringSlices(rawTools, expectedTools[binding.MCPName]) {
			return "", fmt.Errorf("MCP attestation tool contract differs from effective policy")
		}
		relative, err := filepath.Rel(controlRoot, filepath.Clean(binding.AttestationPath))
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return "", fmt.Errorf("MCP attestation path escapes the run control root")
		}
		rootInfo, err := os.Lstat(filepath.Dir(binding.AttestationPath))
		if err != nil || !rootInfo.IsDir() || rootInfo.Mode().Perm() != 0o700 {
			return "", fmt.Errorf("MCP attestation directory is unavailable")
		}
		if _, err := os.Lstat(binding.AttestationPath); err == nil || !os.IsNotExist(err) {
			return "", fmt.Errorf("MCP attestation path is not fresh")
		}
		runtimeBindings = append(runtimeBindings, mcpproxy.RuntimeBinding{
			MCPName: binding.MCPName, AttestationPath: binding.AttestationPath, Nonce: binding.Nonce,
		})
	}
	manifestPath := filepath.Join(controlRoot, "mcp-proxy-manifest.json")
	if _, err := os.Lstat(manifestPath); err == nil || !os.IsNotExist(err) {
		return "", fmt.Errorf("fresh MCP proxy manifest path is unavailable")
	}
	if err := mcpproxy.WriteRuntimeManifest(manifestPath, runtimeBindings); err != nil {
		return "", fmt.Errorf("materialize MCP proxy runtime manifest")
	}
	return manifestPath, nil
}

func verifyEffectiveMCPProxyCommands(effective toolpolicy.Effective) error {
	verification := toolpolicy.VerifyRuntimeConfig(effective.Config, effective)
	if !verification.Valid {
		return fmt.Errorf("effective MCP proxy policy is invalid")
	}
	var document map[string]any
	if err := json.Unmarshal(effective.Config, &document); err != nil {
		return fmt.Errorf("decode effective MCP proxy policy")
	}
	rawMCPs, ok := document["mcp"].(map[string]any)
	if !ok {
		return fmt.Errorf("effective MCP proxy policy lacks MCP configuration")
	}
	expectedTools := make(map[string][]string, len(effective.MCPAttestations))
	for _, binding := range effective.MCPAttestations {
		expectedTools[binding.MCPName] = append([]string(nil), binding.RawTools...)
		sort.Strings(expectedTools[binding.MCPName])
	}
	proxied := 0
	for mcpName, rawEntry := range rawMCPs {
		entry, ok := rawEntry.(map[string]any)
		if !ok || entry["enabled"] != true {
			continue
		}
		if _, inherited := entry["environment"]; inherited {
			return fmt.Errorf("effective MCP proxy retains an OpenCode child environment")
		}
		rawCommand, ok := entry["command"].([]any)
		if !ok {
			return fmt.Errorf("effective MCP proxy command is invalid")
		}
		command := make([]string, len(rawCommand))
		for index, rawArgument := range rawCommand {
			argument, ok := rawArgument.(string)
			if !ok {
				return fmt.Errorf("effective MCP proxy command is invalid")
			}
			command[index] = argument
		}
		if len(command) < 4 || command[0] != effective.MCPProxy.Path || command[1] != "__mcp-proxy" {
			return fmt.Errorf("effective MCP command bypasses evaluator proxy")
		}
		declared, err := mcpproxy.ParseArgs(command[2:])
		if err != nil {
			return fmt.Errorf("effective MCP proxy command is invalid")
		}
		tools, exists := expectedTools[declared.MCPName]
		if !exists || declared.MCPName != mcpName || !equalStringSlices(declared.ExpectedTools, tools) {
			return fmt.Errorf("effective MCP proxy command differs from attestation contract")
		}
		proxied++
	}
	if proxied != len(effective.EnabledFakes) {
		return fmt.Errorf("effective MCP proxy command count differs from enabled policy")
	}
	return nil
}

func attestedMCPToolIDs(effective toolpolicy.Effective) ([]string, error) {
	if len(effective.EnabledFakes) == 0 {
		if len(effective.MCPAttestations) != 0 {
			return nil, fmt.Errorf("unexpected MCP tool attestation metadata")
		}
		return []string{}, nil
	}
	if err := verifyMCPProxyIdentity(effective.MCPProxy); err != nil {
		return nil, err
	}
	expectedBindings := make(map[string]map[string]string, len(effective.EnabledFakes))
	for _, binding := range effective.FakeToolBindings {
		if expectedBindings[binding.MCPName] == nil {
			expectedBindings[binding.MCPName] = make(map[string]string)
		}
		expectedBindings[binding.MCPName][binding.RawTool] = binding.EffectiveID
	}
	metadata := append([]toolpolicy.MCPAttestationBinding(nil), effective.MCPAttestations...)
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].MCPName < metadata[j].MCPName })
	if len(metadata) != len(effective.EnabledFakes) {
		return nil, fmt.Errorf("runtime MCP tool attestations are incomplete")
	}
	ids := make([]string, 0, len(effective.FakeToolBindings))
	for index, binding := range metadata {
		if index > 0 && metadata[index-1].MCPName == binding.MCPName || index >= len(effective.EnabledFakes) || binding.MCPName != effective.EnabledFakes[index] {
			return nil, fmt.Errorf("runtime MCP tool attestation identity mismatch")
		}
		attestation, err := mcpproxy.VerifyAttestation(binding.AttestationPath, binding.MCPName, binding.Nonce, binding.RawTools)
		if err != nil {
			return nil, fmt.Errorf("runtime MCP tool attestation is invalid")
		}
		declared := expectedBindings[binding.MCPName]
		if len(attestation.RawTools) != len(declared) {
			return nil, fmt.Errorf("runtime MCP tool attestation differs from policy")
		}
		for _, rawTool := range attestation.RawTools {
			effectiveID, exists := declared[rawTool]
			if !exists {
				return nil, fmt.Errorf("runtime MCP tool attestation differs from policy")
			}
			observedID, err := toolpolicy.OpenCodeMCPToolID(binding.MCPName, rawTool)
			if err != nil || observedID != effectiveID {
				return nil, fmt.Errorf("runtime MCP tool ID derivation differs from policy")
			}
			ids = append(ids, observedID)
		}
	}
	return ids, nil
}
