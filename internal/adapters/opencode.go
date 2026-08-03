package adapters

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

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

	// Copy opencode/ → target using Go (no rsync)
	reporter.Detail("Copying OpenCode config to %s...", target)
	if err := copyDirExcluding(sourceDir, target, []string{"node_modules", "skills"}); err != nil {
		return fmt.Errorf("copy opencode dir: %w", err)
	}

	// Merge preserved MCP servers
	if backupConfig != nil {
		installedPath := filepath.Join(target, "opencode.json")
		if err := mergeOpencodeConfigWithReporter(installedPath, backupConfig, reporter); err != nil {
			reporter.Warning("MCP merge failed: %v", err)
		}
	}

	// Install JS dependencies (bun or npm)
	reporter.Detail("Installing OpenCode dependencies...")
	identity, err := validateInstallDestinationTreeIdentity(target)
	if err != nil {
		return fmt.Errorf("revalidate opencode install destination before dependencies: %w", err)
	}
	if identity == nil {
		return fmt.Errorf("revalidate opencode install destination before dependencies: destination disappeared")
	}
	current, statErr := os.Lstat(target)
	if statErr != nil || !os.SameFile(identity, current) || identity.Mode().Type() != current.Mode().Type() {
		if statErr == nil {
			statErr = fmt.Errorf("directory identity changed")
		}
		return fmt.Errorf("revalidate opencode install destination before dependencies: %w", statErr)
	}
	if err := installJSDepsWithReporter(target, req != nil && req.TrustSetupScripts, reporter); err != nil {
		reporter.Warning("dependency install failed: %v", err)
		return fmt.Errorf("OpenCode dependency install failed: %w (managed config/skills rolled back; dependencies may require rerun: cd %s && bun install)", err, target)
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

// mergeOpencodeConfig preserves user MCP servers, forces neurox entry.
func mergeOpencodeConfig(installedPath string, backup map[string]json.RawMessage) error {
	return mergeOpencodeConfigWithReporter(installedPath, backup, discardReporter())
}

func mergeOpencodeConfigWithReporter(installedPath string, backup map[string]json.RawMessage, reporter Reporter) error {
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
				backupMCP[k] = v
			}
		}
	}

	// Force neurox entry
	neuroxEntry := map[string]interface{}{
		"command": []string{"neurox", "mcp"},
		"enabled": true,
		"type":    "local",
	}
	neuroxJSON, _ := json.Marshal(neuroxEntry)
	backupMCP["neurox"] = neuroxJSON

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
	if _, err := exec.LookPath("bun"); err == nil {
		cmd = exec.Command("bun", "install", "--silent", "--ignore-scripts")
	} else if _, err := exec.LookPath("npm"); err == nil {
		cmd = exec.Command("npm", "install", "--silent", "--ignore-scripts")
	} else {
		return fmt.Errorf("neither bun nor npm found")
	}
	if beforeInstallCwdOpen != nil {
		if err := beforeInstallCwdOpen(); err != nil {
			return fmt.Errorf("prepare dependency install directory: %w", err)
		}
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
