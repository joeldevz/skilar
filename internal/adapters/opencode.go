package adapters

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/joeldevz/skynex/internal/models"
)

// InstallOpencode installs OpenCode config from srcDir, preserving user MCP servers.
// req contains cleanup preference from CLI flags.
func InstallOpencode(srcDir string, req *models.InstallRequest) error {
	sourceDir := filepath.Join(srcDir, "opencode")
	target := opencodeDir()
	if err := validateInstallDestinationTree(target); err != nil {
		return fmt.Errorf("validate opencode install destination: %w", err)
	}

	if _, err := os.Stat(sourceDir); err != nil {
		return fmt.Errorf("opencode source not found: %w", err)
	}

	// Read backup of existing config before overwrite
	existingConfigPath := filepath.Join(target, "opencode.json")
	var backupConfig map[string]json.RawMessage
	var rawBackup []byte
	if data, err := os.ReadFile(existingConfigPath); err == nil {
		rawBackup = data
		if err := json.Unmarshal(data, &backupConfig); err != nil {
			// File exists but can't be parsed — save raw backup before we lose it
			fmt.Fprintf(os.Stderr, "Warning: existing opencode.json is malformed, preserving as .bak\n")
			backupPath := existingConfigPath + ".bak"
			if err := writeFile(backupPath, string(rawBackup)); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not save backup: %v\n", err)
			}
		}
	}

	// Backup existing dir
	if _, err := os.Stat(target); err == nil {
		backupDirIfExists(target)
	}

	// Copy opencode/ → target using Go (no rsync)
	fmt.Printf("    Copying OpenCode config to %s...\n", target)
	if err := copyDirExcluding(sourceDir, target, []string{"node_modules"}); err != nil {
		return fmt.Errorf("copy opencode dir: %w", err)
	}

	// Merge preserved MCP servers
	if backupConfig != nil {
		installedPath := filepath.Join(target, "opencode.json")
		if err := mergeOpencodeConfig(installedPath, backupConfig); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: MCP merge failed: %v\n", err)
		}
	}

	// Install JS dependencies (bun or npm)
	fmt.Println("    Installing OpenCode dependencies...")
	if err := validateInstallDestinationTree(target); err != nil {
		return fmt.Errorf("revalidate opencode install destination before dependencies: %w", err)
	}
	if err := installJSDeps(target); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: dependency install failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "Run manually: cd %s && bun install\n", target)
	}

	// Handle deprecated file cleanup only with explicit consent.
	deprecated, err := FindDeprecatedFiles()
	if err != nil {
		return fmt.Errorf("discover deprecated opencode files: %w", err)
	}
	if len(deprecated) > 0 && deprecated["opencode"] != nil {
		files := deprecated["opencode"]
		shouldRemove := req != nil && req.CleanupDeprecated
		if req != nil && req.Interactive {
			shouldRemove = PromptCleanupDeprecated(map[string][]DeprecatedFile{"opencode": files})
		} else if !shouldRemove {
			fmt.Println("    Deprecated files retained; rerun with --cleanup-deprecated to remove them.")
		}
		if shouldRemove {
			if req == nil || !req.Interactive {
				NotifyDeprecatedFiles("opencode", files)
			}
			if _, err := RemoveDeprecatedFiles(files); err != nil {
				return fmt.Errorf("cleanup deprecated opencode files: %w", err)
			}
		}
	}

	fmt.Printf("    OpenCode installed at %s\n", target)
	return nil
}

// mergeOpencodeConfig preserves user MCP servers, forces neurox entry.
func mergeOpencodeConfig(installedPath string, backup map[string]json.RawMessage) error {
	data, err := os.ReadFile(installedPath)
	if err != nil {
		return err
	}

	var installed map[string]json.RawMessage
	if err := json.Unmarshal(data, &installed); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: installed opencode.json is malformed: %v\n", err)
		return err
	}

	// Get backup MCP
	var backupMCP map[string]json.RawMessage
	if raw, ok := backup["mcp"]; ok {
		if err := json.Unmarshal(raw, &backupMCP); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not parse backup MCP config: %v\n", err)
		}
	}
	if backupMCP == nil {
		backupMCP = make(map[string]json.RawMessage)
	}

	// Installed MCP wins over backup
	var installedMCP map[string]json.RawMessage
	if raw, ok := installed["mcp"]; ok {
		if err := json.Unmarshal(raw, &installedMCP); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not parse installed MCP config: %v\n", err)
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

func installJSDeps(dir string) error {
	var cmd *exec.Cmd
	if _, err := exec.LookPath("bun"); err == nil {
		cmd = exec.Command("bun", "install", "--silent")
	} else if _, err := exec.LookPath("npm"); err == nil {
		cmd = exec.Command("npm", "install", "--silent")
	} else {
		return fmt.Errorf("neither bun nor npm found")
	}
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func backupDirIfExists(dir string) {
	// No-op if doesn't exist
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return
	}

	// Save opencode.json as backup before overwrite
	configPath := filepath.Join(dir, "opencode.json")
	if data, err := os.ReadFile(configPath); err == nil {
		backupPath := configPath + ".bak"
		if err := writeFile(backupPath, string(data)); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not save opencode.json backup: %v\n", err)
		} else {
			fmt.Printf("    Backed up existing config to %s.bak\n", configPath)
		}
	}
}

func opencodeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot determine home directory: %v\n", err)
		// Return a fallback path that will likely fail gracefully downstream
		return "~/.config/opencode"
	}
	return filepath.Join(home, ".config", "opencode")
}
