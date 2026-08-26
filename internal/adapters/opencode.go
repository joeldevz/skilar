package adapters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/models"
	"github.com/joeldevz/skynex/internal/prompts"
	"github.com/joeldevz/skynex/internal/safefs"
	"github.com/joeldevz/skynex/internal/skillsync"
)

// InstallOpencode installs OpenCode config from srcDir, preserving user MCP servers.
// req contains cleanup preference from CLI flags.
func InstallOpencode(srcDir string, req *models.InstallRequest) error {
	return InstallOpencodeWithReporter(srcDir, req, discardReporter())
}

func InstallOpencodeWithReporter(srcDir string, req *models.InstallRequest, reporter Reporter) error {
	return InstallOpencodeWithReporterAndOptions(srcDir, req, reporter, InstallOptions{Input: os.Stdin, Output: io.Discard})
}

func InstallOpencodeWithReporterAndOptions(srcDir string, req *models.InstallRequest, reporter Reporter, options InstallOptions) error {
	sourceDir := filepath.Join(srcDir, "opencode")
	target := options.OpencodeDir
	if target == "" {
		target = opencodeDir()
	}
	if err := archiveInactiveLegacyWorkflowDB(target); err != nil {
		return err
	}
	if _, err := validateInstallDestinationTreeIdentity(target); err != nil {
		return fmt.Errorf("validate opencode install destination: %w", err)
	}

	if _, err := os.Stat(sourceDir); err != nil {
		return fmt.Errorf("opencode source not found: %w", err)
	}

	// Reconcile skills before any other OpenCode mutation. A modified skill is
	// therefore a hard pre-mutation gate, including for update.
	skillsSource := filepath.Join(sourceDir, "skills")
	if _, statErr := os.Stat(skillsSource); statErr == nil {
		stateDir := req.StateDir
		if stateDir == "" {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return homeErr
			}
			stateDir = filepath.Join(home, ".config", "skynex")
		}
		manifestPath := filepath.Join(stateDir, "skills.ownership.json")
		bundle := os.DirFS(skillsSource)
		session, sessionErr := skillsync.NewSession(bundle, filepath.Join(target, "skills"), manifestPath)
		if sessionErr != nil {
			return fmt.Errorf("open OpenCode skills session: %w", sessionErr)
		}
		defer session.Close()
		if req.SkillsBundleVersion == "" {
			req.SkillsBundleVersion = "latest"
		}
		report, inspectErr := session.Inspect()
		if inspectErr != nil {
			return fmt.Errorf("inspect OpenCode skills: %w", inspectErr)
		}
		decisions := make(map[string]skillsync.Decision)
		for path, decision := range req.SkillsDecisions {
			decisions[path] = skillsync.Decision(decision)
		}
		if req.Interactive {
			var decisionErr error
			decisions, decisionErr = prompts.ResolveSkillDecisionsWithIO(report, options.Input, options.Output)
			if decisionErr != nil {
				return decisionErr
			}
		}
		metadata := skillsync.Manifest{Source: "opencode/skills", SourceKind: "bundle", BundleVersion: req.SkillsBundleVersion, BundleCommit: req.SkillsBundleCommit, Package: "skills", Target: "opencode"}
		if applyErr := session.Apply(report, decisions, metadata); applyErr != nil {
			return fmt.Errorf("reconcile OpenCode skills: %w", applyErr)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect OpenCode skills source: %w", statErr)
	}

	// Read backup of existing config before overwrite
	existingConfigPath := filepath.Join(target, "opencode.json")
	var backupConfig map[string]json.RawMessage
	var rawBackup []byte
	if data, err := readExistingFile(existingConfigPath); err == nil {
		rawBackup = data
		if err := json.Unmarshal(data, &backupConfig); err != nil {
			// File exists but can't be parsed — save raw backup before we lose it
			reporter.Warning("existing opencode.json is malformed, preserving as .bak")
			backupPath := existingConfigPath + ".bak"
			if err := writeFile(backupPath, string(rawBackup)); err != nil {
				reporter.Warning("could not save backup: %v", err)
			}
		}
	}

	// Backup existing dir
	if _, err := os.Stat(target); err == nil {
		backupDirIfExistsWithReporter(target, reporter)
	}

	// Copy owned OpenCode config while preserving modified/unknown files.
	reporter.Detail("Copying OpenCode config to %s...", target)
	excluded := map[string]bool{}
	if req == nil || !req.NeuroxEnabled {
		excluded["plugins/neurox.ts"] = true
		plugin := filepath.Join(target, "plugins", "neurox.ts")
		if raw, readErr := os.ReadFile(plugin); readErr == nil {
			owned, ok := loadInventory(target).Files["plugins/neurox.ts"]
			if !ok || owned != fileDigest(raw) {
				reporter.Warning("preserving existing Neurox plugin because it is not an unchanged Skynex-managed file: %s", plugin)
			}
		}
	}
	if err := installOwnedTreeExcluding(sourceDir, target, excluded, reporter); err != nil {
		return fmt.Errorf("copy opencode dir: %w", err)
	}

	// Merge preserved MCP servers
	installedPath := filepath.Join(target, "opencode.json")
	if err := mergeOpencodeConfigForNeurox(installedPath, backupConfig, req != nil && req.NeuroxEnabled, reporter); err != nil {
		reporter.Warning("MCP merge failed: %v", err)
	} else if err := refreshInventoryDigest(target, "opencode.json"); err != nil {
		return fmt.Errorf("refresh managed inventory: %w", err)
	}

	// Deprecated entries are informational only. Never delete them implicitly.
	deprecated, err := FindDeprecatedFiles()
	if err != nil {
		return fmt.Errorf("discover deprecated opencode files: %w", err)
	}
	if len(deprecated) > 0 && deprecated["opencode"] != nil {
		reporter.Detail("Deprecated files detected and preserved (no recursive cleanup is performed):")
		notifyDeprecatedFiles(reporter, "opencode", deprecated["opencode"])
	}

	reporter.Detail("OpenCode installed at %s", target)
	return nil
}

// SetupOpenCodeDependencies installs only the JavaScript dependencies for an
// already committed, managed OpenCode target.
func SetupOpenCodeDependencies(dir string, trustScripts bool) error {
	return installJSDepsWithReporter(dir, trustScripts, discardReporter())
}

// ValidateManagedOpenCode verifies the minimal canonical inventory marker and
// every owned file before allowing a dependency retry.
func ValidateManagedOpenCode(dir string) error {
	identity, err := validateInstallDestinationTreeIdentity(dir)
	if err != nil {
		return err
	}
	if identity == nil || !identity.IsDir() {
		return fmt.Errorf("not a directory")
	}
	root, err := safefs.Open(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	manifest, err := safefs.ReadFileVerified(root, inventoryName, maxOwnedFileBytes)
	if err != nil {
		return err
	}
	var value struct {
		Version *int              `json:"version"`
		Files   map[string]string `json:"files"`
	}
	if err := json.Unmarshal(manifest, &value); err != nil || (value.Version != nil && *value.Version != 1) || len(value.Files) == 0 {
		return fmt.Errorf("invalid managed inventory")
	}
	if _, ok := value.Files["opencode.json"]; !ok {
		return fmt.Errorf("managed configuration is missing")
	}
	for path, digest := range value.Files {
		clean, pathErr := safefs.Relative(path)
		if path == inventoryName || pathErr != nil || clean != path {
			return fmt.Errorf("invalid managed inventory path")
		}
		info, err := root.Lstat(clean)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("managed file %q is invalid", path)
		}
		raw, err := safefs.ReadFileVerified(root, clean, maxOwnedFileBytes)
		if err != nil || len(raw) == 0 || fileDigest(raw) != digest {
			return fmt.Errorf("managed file %q is invalid", path)
		}
	}
	// Dependency metadata is authenticated by the bundle, not by the target's
	// manifest (which an attacker can rewrite along with the files).
	bundle, err := assets.OpencodeFS()
	if err != nil {
		return err
	}
	for _, path := range []string{"package.json", "bun.lock", "pnpm-lock.yaml", "package-lock.json"} {
		expected, readErr := fs.ReadFile(bundle, path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			return readErr
		}
		actual, readErr := safefs.ReadFileVerified(root, path, maxOwnedFileBytes)
		if readErr != nil || !bytes.Equal(actual, expected) {
			return fmt.Errorf("managed file %q is not the shipped OpenCode bundle", path)
		}
	}
	return nil
}

// mergeOpencodeConfig preserves user MCP servers, forces neurox entry.
func mergeOpencodeConfig(installedPath string, backup map[string]json.RawMessage) error {
	return mergeOpencodeConfigWithReporter(installedPath, backup, discardReporter())
}

func mergeOpencodeConfigWithReporter(installedPath string, backup map[string]json.RawMessage, reporter Reporter) error {
	return mergeOpencodeConfigForNeurox(installedPath, backup, true, reporter)
}

func mergeOpencodeConfigForNeurox(installedPath string, backup map[string]json.RawMessage, enabled bool, reporter Reporter) error {
	data, err := readExistingFile(installedPath)
	if err != nil {
		return err
	}

	var installed map[string]json.RawMessage
	if err := json.Unmarshal(data, &installed); err != nil {
		reporter.Warning("installed opencode.json is malformed: %v", err)
		return err
	}

	// Get backup MCP
	var backupMCP map[string]json.RawMessage
	if raw, ok := backup["mcp"]; ok {
		if err := json.Unmarshal(raw, &backupMCP); err != nil {
			reporter.Warning("could not parse backup MCP config: %v", err)
		}
	}
	if backupMCP == nil {
		backupMCP = make(map[string]json.RawMessage)
	}

	// Installed MCP wins over backup
	var installedMCP map[string]json.RawMessage
	if raw, ok := installed["mcp"]; ok {
		if err := json.Unmarshal(raw, &installedMCP); err != nil {
			reporter.Warning("could not parse installed MCP config: %v", err)
		} else {
			for k, v := range installedMCP {
				if k == "neurox" && !enabled {
					continue
				}
				backupMCP[k] = v
			}
		}
	}

	if enabled {
		neuroxEntry := map[string]interface{}{"command": []string{"neurox", "mcp"}, "enabled": true, "type": "local"}
		neuroxJSON, _ := json.Marshal(neuroxEntry)
		backupMCP["neurox"] = neuroxJSON
	} else if raw, ok := backupMCP["neurox"]; ok {
		var entry struct {
			Command []string `json:"command"`
			Type    string   `json:"type"`
		}
		if json.Unmarshal(raw, &entry) == nil && len(entry.Command) == 2 && entry.Command[0] == "neurox" && entry.Command[1] == "mcp" && entry.Type == "local" {
			delete(backupMCP, "neurox")
		} else {
			reporter.Warning("preserving custom Neurox MCP entry while the Skynex Neurox integration is disabled")
		}
	}

	mergedMCP, _ := json.Marshal(backupMCP)
	installed["mcp"] = mergedMCP

	out, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(installedPath, string(out)+"\n")
}

func readExistingFile(path string) ([]byte, error) {
	root, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return safefs.ReadFileVerified(root, filepath.Base(path), 4<<20)
}

func installJSDeps(dir string) error { return installJSDepsWithReporter(dir, false, discardReporter()) }

func installJSDepsWithReporter(dir string, trustScripts bool, reporter Reporter) error {
	identity, err := validateInstallDestinationTreeIdentity(dir)
	if err != nil {
		return fmt.Errorf("validate dependency install destination: %w", err)
	}
	if identity == nil {
		return fmt.Errorf("validate dependency install destination: destination disappeared")
	}
	var cmd *exec.Cmd
	manager := ""
	if _, err := exec.LookPath("bun"); err == nil {
		manager = "bun"
		cmd = exec.Command("bun", "install", "--frozen-lockfile", "--silent", "--ignore-scripts")
	} else if _, err := exec.LookPath("pnpm"); err == nil {
		manager = "pnpm"
		cmd = exec.Command("pnpm", "install", "--frozen-lockfile", "--silent", "--ignore-scripts")
	} else if _, err := exec.LookPath("npm"); err == nil {
		manager = "npm"
		cmd = exec.Command("npm", "ci", "--silent", "--ignore-scripts")
	} else {
		return fmt.Errorf("no supported JavaScript package manager found (bun, pnpm, or npm); install bun, pnpm, or npm and try again")
	}
	if beforeInstallCwdOpen != nil {
		if err := beforeInstallCwdOpen(); err != nil {
			return fmt.Errorf("prepare dependency install directory: %w", err)
		}
	}
	if err := validateCommittedDependencyMetadata(dir, manager); err != nil {
		return err
	}
	cwd, err := openInstallCwd(dir, identity)
	if err != nil {
		return err
	}
	defer cwd.Close()
	if err := cwd.verify(dir); err != nil {
		return fmt.Errorf("revalidate dependency install destination before subprocess: %w", err)
	}
	current, statErr := os.Lstat(dir)
	if statErr != nil || !os.SameFile(identity, current) || identity.Mode().Type() != current.Mode().Type() {
		if statErr == nil {
			statErr = fmt.Errorf("directory identity changed")
		}
		return fmt.Errorf("revalidate dependency install destination before subprocess: %w", statErr)
	}
	if trustScripts {
		args := append([]string(nil), cmd.Args[1:]...)
		filtered := args[:0]
		for _, arg := range args {
			if arg != "--ignore-scripts" {
				filtered = append(filtered, arg)
			}
		}
		cmd.Args = append([]string{cmd.Path}, filtered...)
	}
	if r, ok := reporter.(*installReporter); ok && r.verbose {
		cmd.Stdout = sanitizedWriter{r.out}
		cmd.Stderr = sanitizedWriter{r.out}
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = cmd.Stdout
	}
	if err := validateExistingNodeModules(dir); err != nil {
		return fmt.Errorf("revalidate node_modules before dependency install: %w", err)
	}
	if err := runWithInstallCwd(cwd, cmd); err != nil {
		return err
	}
	if err := cwd.verify(dir); err != nil {
		return fmt.Errorf("revalidate dependency install destination after subprocess: %w", err)
	}
	if err := validateExistingNodeModules(dir); err != nil {
		return fmt.Errorf("revalidate node_modules after dependency install: %w", err)
	}
	return nil
}

func backupDirIfExists(dir string) { backupDirIfExistsWithReporter(dir, discardReporter()) }

func backupDirIfExistsWithReporter(dir string, reporter Reporter) {
	// No-op if doesn't exist
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return
	}

	// Save opencode.json as backup before overwrite
	configPath := filepath.Join(dir, "opencode.json")
	if data, err := readExistingFile(configPath); err == nil {
		backupPath := configPath + ".bak"
		if err := writeFile(backupPath, string(data)); err != nil {
			reporter.Warning("could not save opencode.json backup: %v", err)
		} else {
			reporter.Detail("Backed up existing config to %s.bak", configPath)
		}
	}
}

func opencodeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Return a fallback path that will likely fail gracefully downstream.
		return "~/.config/opencode"
	}
	return filepath.Join(home, ".config", "opencode")
}
