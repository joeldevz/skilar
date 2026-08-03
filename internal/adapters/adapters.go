package adapters

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/models"
	"github.com/joeldevz/skynex/internal/paths"
)

// InstallOptions contains process-boundary dependencies for an install.
// Empty paths retain the normal user destinations.
type InstallOptions struct {
	ClaudeDir   string
	OpencodeDir string
	Input       io.Reader
	Output      io.Writer
}

// InstallAll installs all packages in the request.
func InstallAll(req *models.InstallRequest, cat *models.Catalog) ([]*models.InstallResult, error) {
	return InstallAllWithReporter(req, cat, discardReporter())
}

// InstallAllWithReporter runs installation without writing to process stdout.
func InstallAllWithReporter(req *models.InstallRequest, cat *models.Catalog, reporter Reporter) ([]*models.InstallResult, error) {
	return InstallAllWithReporterAndOptions(req, cat, reporter, InstallOptions{})
}

func InstallAllWithReporterAndOptions(req *models.InstallRequest, cat *models.Catalog, reporter Reporter, options InstallOptions) ([]*models.InstallResult, error) {
	var results []*models.InstallResult
	if options.ClaudeDir == "" {
		options.ClaudeDir = paths.ClaudeDir()
	}
	if options.OpencodeDir == "" {
		options.OpencodeDir = paths.OpencodeDir()
	}

	for _, pkgID := range req.Packages {
		pkg := cat.Packages[pkgID]
		version := req.Versions[pkgID]

		reporter.Detail("Installing %s (%s)...", pkgID, version)

		var result *models.InstallResult
		var err error

		switch pkg.Adapter {
		case "skills_repo":
			result, err = installSkillsRepo(pkg, req, version, reporter, options)
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

func installSkillsRepo(pkg *models.PackageDefinition, req *models.InstallRequest, version string, reporter Reporter, options InstallOptions) (*models.InstallResult, error) {
	var checkoutDir string
	var commit string

	// Use embedded assets if available (self-contained binary) and version is "latest"
	if assets.Available() && version == "latest" {
		reporter.Detail("Using embedded assets (self-contained mode)...")
		// Extract to temp dir
		tmpDir, err := os.MkdirTemp("", "skynex-assets-*")
		if err != nil {
			return nil, fmt.Errorf("create temp dir: %w", err)
		}
		defer os.RemoveAll(tmpDir)

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
		if skillsFS, err := assets.OpencodeSkillsFS(); err == nil {
			destSkills := filepath.Join(tmpDir, "opencode", "skills")
			if err := assets.ExtractTo(skillsFS, destSkills); err != nil {
				return nil, fmt.Errorf("extract opencode skills assets: %w", err)
			}
		}

		checkoutDir = tmpDir
		commit = "embedded"
	} else {
		// Only an explicitly local workspace is allowed as a source. Remote Git
		// refs cannot be authenticated with the pinned release signing key.
		var err error
		checkoutDir, commit, err = checkoutPackage(pkg, version, reporter)
		if err != nil {
			return nil, err
		}
	}
	if version != "workspace" {
		defer os.RemoveAll(checkoutDir)
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)
	req.SkillsBundleCommit = commit
	req.SkillsBundleVersion = version
	targets := make(map[string]*models.TargetResult)

	for _, target := range req.Targets {
		var artifacts []string
		switch target {
		case "claude":
			reporter.Detail("Installing Claude Code assets...")
			if err := InstallClaudeWithReporter(checkoutDir, req, reporter); err != nil {
				return nil, fmt.Errorf("install claude: %w", err)
			}
			artifacts = []string{
				options.ClaudeDir,
				filepath.Join(options.ClaudeDir, "agents"),
				filepath.Join(options.ClaudeDir, "skills"),
				filepath.Join(options.ClaudeDir, "CLAUDE.md"),
			}
		case "opencode":
			reporter.Detail("Installing OpenCode config...")
			if err := InstallOpencodeWithReporterAndOptions(checkoutDir, req, reporter, options); err != nil {
				return nil, fmt.Errorf("install opencode: %w", err)
			}
			artifactDir := options.OpencodeDir
			if artifactDir == "" {
				artifactDir = paths.OpencodeDir()
			}
			artifacts = []string{artifactDir}
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

func checkoutPackage(pkg *models.PackageDefinition, version string, _ Reporter) (checkoutDir, commit string, err error) {
	if version == "workspace" {
		// Use current directory
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", err
		}
		commit := getCommit(cwd)
		return cwd, commit, nil
	}

	return "", "", fmt.Errorf("remote package version %q is disabled: use embedded latest assets or --package-version %s=workspace from a local checkout; signed source manifests are required for remote refs", version, pkg.ID)
}

// parseLsRemoteCommit is retained for catalog tooling/tests; installation does
// not invoke Git remote resolution because GitHub verification is not a release
// signature and signed source manifests are not yet available.
func parseLsRemoteCommit(output, ref string) string {
	var fallback string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !validCommitSHA(fields[0]) {
			continue
		}
		if fields[1] == ref+"^{}" {
			return fields[0]
		}
		if fallback == "" {
			fallback = fields[0]
		}
	}
	return fallback
}

func validCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
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

	data, err := readExistingFile(configPath)
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
