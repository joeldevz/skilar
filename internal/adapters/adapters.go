package adapters

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/models"
	"github.com/joeldevz/skynex/internal/paths"
)

// InstallAll installs all packages in the request.
func InstallAll(req *models.InstallRequest, cat *models.Catalog) ([]*models.InstallResult, error) {
	var results []*models.InstallResult

	for _, pkgID := range req.Packages {
		pkg := cat.Packages[pkgID]
		version := req.Versions[pkgID]

		fmt.Printf("\n  Installing %s (%s)...\n", pkgID, version)

		var result *models.InstallResult
		var err error

		switch pkg.Adapter {
		case "skills_repo":
			result, err = installSkillsRepo(pkg, req, version)
		default:
			return nil, fmt.Errorf("unknown adapter: %s", pkg.Adapter)
		}

		if err != nil {
			return nil, fmt.Errorf("install %s: %w", pkgID, err)
		}
		results = append(results, result)
	}

	return results, nil
}

func installSkillsRepo(pkg *models.PackageDefinition, req *models.InstallRequest, version string) (*models.InstallResult, error) {
	var checkoutDir string
	var commit string

	// Use embedded assets if available (self-contained binary) and version is "latest"
	if assets.Available() && version == "latest" {
		fmt.Println("    Using embedded assets (self-contained mode)...")
		// Extract to temp dir
		tmpDir, err := os.MkdirTemp("", "skynex-assets-*")
		if err != nil {
			return nil, fmt.Errorf("create temp dir: %w", err)
		}
		// Don't defer removal — adapters need the dir during install
		// It will be cleaned by OS on next boot

		// Extract claude-code assets
		if claudeCodeFS, err := assets.ClaudeCodeFS(); err == nil {
			destClaude := filepath.Join(tmpDir, "claude-code")
			if err := assets.ExtractTo(claudeCodeFS, destClaude); err != nil {
				return nil, fmt.Errorf("extract claude-code assets: %w", err)
			}
		}

		// Extract opencode assets
		if opencodeFS, err := assets.OpencodeFS(); err == nil {
			destOpencode := filepath.Join(tmpDir, "opencode")
			if err := assets.ExtractTo(opencodeFS, destOpencode); err != nil {
				return nil, fmt.Errorf("extract opencode assets: %w", err)
			}
		}

		checkoutDir = tmpDir
		commit = "embedded"
	} else {
		// Clone or use workspace (fallback)
		var err error
		checkoutDir, commit, err = checkoutPackage(pkg, version)
		if err != nil {
			return nil, err
		}
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)
	targets := make(map[string]*models.TargetResult)

	for _, target := range req.Targets {
		var artifacts []string
		switch target {
		case "claude":
			fmt.Println("    Installing Claude Code assets...")
			if err := InstallClaude(checkoutDir, req); err != nil {
				return nil, fmt.Errorf("install claude: %w", err)
			}
			artifacts = []string{
				paths.ClaudeDir(),
				filepath.Join(paths.ClaudeDir(), "agents"),
				filepath.Join(paths.ClaudeDir(), "skills"),
				filepath.Join(paths.ClaudeDir(), "CLAUDE.md"),
			}
		case "opencode":
			fmt.Println("    Installing OpenCode config...")
			if err := InstallOpencode(checkoutDir, req); err != nil {
				return nil, fmt.Errorf("install opencode: %w", err)
			}
			artifacts = []string{paths.OpencodeDir()}
		}

		targets[target] = &models.TargetResult{
			Status:      "installed",
			InstalledAt: timestamp,
			Artifacts:   artifacts,
		}
	}

	// Inject advisor model into opencode.json if configured
	if req.Advisor != nil && req.Advisor.Enabled {
		injectAdvisorModel(req.Advisor.Model)
	}

	return &models.InstallResult{
		PackageID:        pkg.ID,
		RequestedVersion: version,
		ResolvedVersion:  version,
		Commit:           commit,
		Targets:          targets,
	}, nil
}

func checkoutPackage(pkg *models.PackageDefinition, version string) (string, string, error) {
	if version == "workspace" {
		// Use current directory
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", err
		}
		commit := getCommit(cwd)
		return cwd, commit, nil
	}

	// Clone to temp dir
	tmpDir, err := os.MkdirTemp("", "skynex-*")
	if err != nil {
		return "", "", err
	}

	fmt.Printf("    Cloning %s...\n", pkg.RepoURL)
	cmd := exec.Command("git", "clone", "--depth", "1", pkg.RepoURL, tmpDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("git clone failed: %w", err)
	}

	// If version is a specific tag, checkout
	if version != "latest" {
		checkoutCmd := exec.Command("git", "checkout", version)
		checkoutCmd.Dir = tmpDir
		if err := checkoutCmd.Run(); err != nil {
			return "", "", fmt.Errorf("git checkout %s failed: %w", version, err)
		}
	}

	commit := getCommit(tmpDir)
	return tmpDir, commit, nil
}

func getCommit(dir string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "unknown-"
	}
	return trimNewline(string(out))
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// injectAdvisorModel updates the installed opencode.json with the chosen advisor model.
func injectAdvisorModel(model string) {
	configPath := filepath.Join(paths.OpencodeDir(), "opencode.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		return // Silently skip if not installed
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return
	}

	agents, ok := config["agent"].(map[string]interface{})
	if !ok {
		return
	}

	advisorAgent, ok := agents["advisor"].(map[string]interface{})
	if !ok {
		return
	}

	advisorAgent["model"] = model
	agents["advisor"] = advisorAgent
	config["agent"] = agents

	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return
	}
	_ = writeFile(configPath, string(append(out, '\n')))
}
