package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/toolpolicy"
	"github.com/joeldevz/skynex/internal/safefs"
)

const maxAgentPromptBytes int64 = 1 << 20

// freezeAgentBundle copies the content-addressed OpenCode configuration into
// the private run root and removes write bits before the runtime starts. The
// source and frozen copy are both re-digested after execution.
func (e *Engine) freezeAgentBundle(workspace *sandbox.Workspace, testCase contracts.Case) (configRoot, bundleCopy, frozenDigest string, effective toolpolicy.Effective, err error) {
	if e.config.AgentBundleRoot == "" {
		return "", "", "", toolpolicy.Effective{}, fmt.Errorf("agent bundle root is required")
	}
	source, err := sandbox.DigestTree(e.config.AgentBundleRoot, e.config.SnapshotLimits)
	if err != nil {
		return "", "", "", toolpolicy.Effective{}, fmt.Errorf("digest agent bundle: %w", err)
	}
	if source.Digest != e.config.BundleDigest {
		return "", "", "", toolpolicy.Effective{}, fmt.Errorf("agent bundle digest mismatch: got %s, expected %s", source.Digest, e.config.BundleDigest)
	}
	configRoot = filepath.Join(workspace.RunPath(), "control", "xdg-config")
	bundleCopy = filepath.Join(configRoot, "opencode")
	if err := os.MkdirAll(bundleCopy, 0o700); err != nil {
		return "", "", "", toolpolicy.Effective{}, fmt.Errorf("create frozen bundle destination: %w", err)
	}
	copied, err := sandbox.CopyVerifiedTree(e.config.AgentBundleRoot, bundleCopy, e.config.SnapshotLimits)
	if err != nil {
		return "", "", "", toolpolicy.Effective{}, fmt.Errorf("copy agent bundle: %w", err)
	}
	if copied.Digest != source.Digest {
		return "", "", "", toolpolicy.Effective{}, fmt.Errorf("copied agent bundle digest mismatch")
	}
	effective, err = prepareToolPolicy(bundleCopy, testCase, e.config.ExecutableClosure)
	if err != nil {
		return "", "", "", toolpolicy.Effective{}, err
	}
	if err := freezeTree(bundleCopy); err != nil {
		return "", "", "", toolpolicy.Effective{}, err
	}
	frozen, err := sandbox.DigestTree(bundleCopy, e.config.SnapshotLimits)
	if err != nil {
		return "", "", "", toolpolicy.Effective{}, fmt.Errorf("digest frozen agent bundle: %w", err)
	}
	return configRoot, bundleCopy, frozen.Digest, effective, nil
}

func prepareToolPolicy(bundleCopy string, testCase contracts.Case, closures ...*ExecutableClosure) (toolpolicy.Effective, error) {
	var closure *ExecutableClosure
	if len(closures) != 0 {
		closure = closures[0]
	}
	return prepareToolPolicyWithProxy(bundleCopy, testCase, closure, "")
}

func prepareToolPolicyWithProxy(bundleCopy string, testCase contracts.Case, closure *ExecutableClosure, proxyExecutable string) (toolpolicy.Effective, error) {
	configPath := filepath.Join(bundleCopy, "opencode.json")
	base, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return toolpolicy.Effective{}, fmt.Errorf("read copied OpenCode config: %w", err)
	}
	if os.IsNotExist(err) {
		base = []byte(`{}`)
	}
	if len(base) > 8<<20 {
		return toolpolicy.Effective{}, fmt.Errorf("copied OpenCode config exceeds 8 MiB")
	}
	base, err = materializeBundleFilePrompts(bundleCopy, base)
	if err != nil {
		return toolpolicy.Effective{}, err
	}
	base, err = pinCaseProviderConfig(base, testCase)
	if err != nil {
		return toolpolicy.Effective{}, err
	}
	fakes := make([]toolpolicy.FakeMCP, 0, len(testCase.ToolPolicy.FakeMCPs))
	for _, fake := range testCase.ToolPolicy.FakeMCPs {
		if fake.Transport != "stdio" || fake.Command == nil {
			return toolpolicy.Effective{}, fmt.Errorf("fake MCP %q must use evaluator-owned stdio execution", fake.Name)
		}
		argv := append([]string(nil), fake.Command.Argv...)
		var executable string
		if closure != nil {
			executable, err = closure.PathFor(argv[0])
		} else {
			if pathErr := ValidateExecutableSearchPath(os.Getenv("PATH")); pathErr != nil {
				return toolpolicy.Effective{}, fmt.Errorf("validate PATH for fake MCP %q: %w", fake.Name, pathErr)
			}
			executable, err = exec.LookPath(argv[0])
			if err == nil {
				executable, err = filepath.Abs(executable)
			}
		}
		if err != nil {
			return toolpolicy.Effective{}, fmt.Errorf("resolve fake MCP %q executable: %w", fake.Name, err)
		}
		argv[0] = executable
		// OpenCode starts local MCPs from the evaluated workspace. Relative
		// arguments therefore remain rooted in the private fixture copy.
		fakes = append(fakes, toolpolicy.FakeMCP{
			Name: fake.Name, Command: argv, Environment: cloneStrings(fake.Env),
			Tools: append([]string(nil), fake.Tools...),
		})
	}
	fakes, attestations, proxyIdentity, err := prepareMCPProxyPolicy(bundleCopy, proxyExecutable, fakes)
	if err != nil {
		return toolpolicy.Effective{}, err
	}
	effective, err := toolpolicy.Generate(toolpolicy.Input{
		Base: json.RawMessage(base), AllowedTools: testCase.ToolPolicy.AllowedTools,
		ForbiddenTools: testCase.ToolPolicy.ForbiddenTools, FakeMCPs: fakes,
	})
	if err != nil {
		return toolpolicy.Effective{}, fmt.Errorf("generate fail-closed tool policy: %w", err)
	}
	effective.MCPAttestations = attestations
	effective.MCPProxy = proxyIdentity
	if err := writePolicyAtomically(configPath, effective.Config); err != nil {
		return toolpolicy.Effective{}, err
	}
	if err := pruneRuntimeBundle(bundleCopy); err != nil {
		return toolpolicy.Effective{}, err
	}
	return effective, nil
}

// materializeBundleFilePrompts turns OpenCode's file-prompt shorthand into an
// evaluator-owned inline value before the runtime sees the config. This keeps
// the generated policy and OpenCode's resolved /config representation equal,
// while the rooted read binds every prompt to the already verified bundle
// copy. Inline prompts remain unchanged; any attempted or malformed file
// expansion fails closed.
func materializeBundleFilePrompts(bundleCopy string, base []byte) ([]byte, error) {
	var config map[string]any
	decoder := json.NewDecoder(bytes.NewReader(base))
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode copied OpenCode config prompts: %w", err)
	}
	if trailingErr := decoder.Decode(new(any)); !errors.Is(trailingErr, io.EOF) {
		return nil, fmt.Errorf("decode copied OpenCode config prompts: multiple JSON values")
	}
	if config == nil {
		return nil, fmt.Errorf("copied OpenCode config prompts must be an object")
	}

	root, err := safefs.Open(filepath.Clean(bundleCopy))
	if err != nil {
		return nil, fmt.Errorf("open copied OpenCode bundle for prompt resolution: %w", err)
	}
	defer root.Close()

	for _, field := range []string{"agent", "mode"} {
		entries, ok := config[field].(map[string]any)
		if !ok {
			continue
		}
		for name, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				continue
			}
			rawPrompt, exists := entry["prompt"]
			if !exists {
				continue
			}
			prompt, ok := rawPrompt.(string)
			if !ok {
				continue
			}
			materialized, changed, promptErr := materializeFilePrompt(root, prompt)
			if promptErr != nil {
				return nil, fmt.Errorf("resolve %s %q prompt: %w", field, name, promptErr)
			}
			if changed {
				entry["prompt"] = materialized
			}
		}
	}

	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode copied OpenCode config prompts: %w", err)
	}
	return encoded, nil
}

func materializeFilePrompt(root *safefs.Root, prompt string) (string, bool, error) {
	const prefix = "{file:"
	if !strings.Contains(prompt, prefix) {
		if strings.Contains(prompt, "{file") {
			return "", false, fmt.Errorf("file prompt must use exact {file:REL} syntax")
		}
		return prompt, false, nil
	}
	if !strings.HasPrefix(prompt, prefix) || !strings.HasSuffix(prompt, "}") || strings.Count(prompt, prefix) != 1 {
		return "", false, fmt.Errorf("file prompt must use exact {file:REL} syntax")
	}
	relative := strings.TrimSuffix(strings.TrimPrefix(prompt, prefix), "}")
	if relative == "" || strings.TrimSpace(relative) != relative || strings.ContainsAny(relative, "{}\\") {
		return "", false, fmt.Errorf("file prompt contains an invalid relative path")
	}
	if strings.HasPrefix(relative, "/") || strings.HasPrefix(relative, "//") || filepath.IsAbs(filepath.FromSlash(relative)) || hasWindowsVolumePrefix(relative) {
		return "", false, fmt.Errorf("file prompt path must be relative")
	}
	for strings.HasPrefix(relative, "./") {
		relative = strings.TrimPrefix(relative, "./")
	}
	if relative == "" || relative == "." || path.Clean(relative) != relative {
		return "", false, fmt.Errorf("file prompt contains a non-canonical relative path")
	}
	for _, component := range strings.Split(relative, "/") {
		if component == "" || component == "." || component == ".." {
			return "", false, fmt.Errorf("file prompt path contains traversal")
		}
	}

	content, err := safefs.ReadFileVerified(root, relative, maxAgentPromptBytes)
	if err != nil {
		return "", false, fmt.Errorf("read file prompt %q: %w", relative, err)
	}
	if !utf8.Valid(content) {
		return "", false, fmt.Errorf("file prompt %q is not valid UTF-8", relative)
	}
	materialized := strings.TrimSpace(string(content))
	if materialized == "" {
		return "", false, fmt.Errorf("file prompt %q is empty", relative)
	}
	return materialized, true, nil
}

func hasWindowsVolumePrefix(relative string) bool {
	return len(relative) >= 2 && relative[1] == ':' &&
		(relative[0] >= 'a' && relative[0] <= 'z' || relative[0] >= 'A' && relative[0] <= 'Z')
}

// pruneRuntimeBundle removes every convention-based OpenCode discovery
// surface after the complete effective config has been written. The immutable
// source bundle remains untouched; the subsequently frozen digest covers this
// one-file runtime projection.
func pruneRuntimeBundle(bundleCopy string) error {
	root, err := safefs.Open(filepath.Clean(bundleCopy))
	if err != nil {
		return fmt.Errorf("open copied OpenCode bundle for runtime projection: %w", err)
	}
	defer root.Close()

	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("read copied OpenCode bundle for runtime projection: %w", err)
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return fmt.Errorf("read copied OpenCode bundle for runtime projection: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close copied OpenCode bundle during runtime projection: %w", closeErr)
	}
	for _, entry := range entries {
		if entry.Name() == "opencode.json" {
			continue
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			return fmt.Errorf("remove auto-discovered OpenCode bundle entry %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func pinCaseProviderConfig(base []byte, testCase contracts.Case) ([]byte, error) {
	provider, _, err := contracts.ParseModelSelection(testCase.Agent.Model)
	if err != nil {
		return nil, fmt.Errorf("pin case provider config: %w", err)
	}
	config := make(map[string]any)
	decoder := json.NewDecoder(bytes.NewReader(base))
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode case provider config: %w", err)
	}
	if config == nil {
		return nil, fmt.Errorf("pin case provider config: config must be an object")
	}
	config["model"] = testCase.Agent.Model
	config["small_model"] = testCase.Agent.Model
	config["enabled_providers"] = []string{provider}
	// Provider implementations and endpoints are runtime authority, not an
	// experiment variable. The pinned OpenCode binary supplies the built-in
	// OpenAI provider; bundles may not redirect OAuth or load provider packages.
	config["provider"] = map[string]any{}
	// OpenCode can select delegated agents from either the current agent map or
	// the legacy mode map. Pin every declared entry so the complete session tree
	// uses the case model enforced by validateObservedRootModel.
	for _, field := range []string{"agent", "mode"} {
		rawEntries, exists := config[field]
		if !exists {
			continue
		}
		entries, ok := rawEntries.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pin case provider config: %s configuration must be an object", field)
		}
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("pin case provider config: %s entries must be objects", field)
			}
			entry["model"] = testCase.Agent.Model
		}
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode case provider config: %w", err)
	}
	return encoded, nil
}

func rejectAmbientOpenCodeInputs(workspace string) error {
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return fmt.Errorf("inspect fixture runtime authority: %w", err)
	}
	reserved := map[string]struct{}{
		"opencode.json": {}, "opencode.jsonc": {}, ".opencode": {}, ".claude": {},
	}
	for _, entry := range entries {
		name := entry.Name()
		_, exact := reserved[name]
		if strings.HasPrefix(name, ".env") || exact {
			return fmt.Errorf("fixture contains undeclared OpenCode/provider authority %q", name)
		}
	}
	return nil
}

func writePolicyAtomically(path string, content []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".opencode-policy-")
	if err != nil {
		return fmt.Errorf("create tool-policy file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() { _ = os.Remove(temporaryPath) }
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		cleanup()
		return fmt.Errorf("restrict tool-policy file: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		cleanup()
		return fmt.Errorf("write tool-policy file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		cleanup()
		return fmt.Errorf("sync tool-policy file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close tool-policy file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		cleanup()
		return fmt.Errorf("install tool-policy file: %w", err)
	}
	return nil
}

func (e *Engine) verifyFrozenBundle(bundleCopy, frozenDigest string) (bool, error) {
	if e.config.AgentBundleRoot == "" {
		return true, nil
	}
	source, err := sandbox.DigestTree(e.config.AgentBundleRoot, e.config.SnapshotLimits)
	if err != nil {
		return false, fmt.Errorf("recheck source agent bundle: %w", err)
	}
	if source.Digest != e.config.BundleDigest {
		return false, fmt.Errorf("source agent bundle drifted: got %s, expected %s", source.Digest, e.config.BundleDigest)
	}
	frozen, err := sandbox.DigestTree(bundleCopy, e.config.SnapshotLimits)
	if err != nil {
		return false, fmt.Errorf("recheck frozen agent bundle: %w", err)
	}
	if frozen.Digest != frozenDigest {
		return false, fmt.Errorf("frozen agent bundle drifted: got %s, expected %s", frozen.Digest, frozenDigest)
	}
	return true, nil
}

func freezeTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refuse symlink while freezing bundle: %s", path)
		}
		mode := info.Mode().Perm() &^ 0o222
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("freeze bundle path %q: %w", path, err)
		}
		return nil
	})
}

func thawTree(root string) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
}
