package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/joeldevz/skynex/internal/adapters"
	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/catalog"
	"github.com/joeldevz/skynex/internal/config"
	"github.com/joeldevz/skynex/internal/installer"
	"github.com/joeldevz/skynex/internal/models"
	"github.com/joeldevz/skynex/internal/paths"
	"github.com/joeldevz/skynex/internal/preflight"
	"github.com/joeldevz/skynex/internal/prompts"
	"github.com/joeldevz/skynex/internal/skillsync"
)

type installDependencies struct {
	loadCatalog    func() (*models.Catalog, error)
	loadConfig     func(string) (map[string]interface{}, error)
	loadConfigHash func(string) (string, error)
	wizard         func(prompts.WizardOptions) (*models.InstallRequest, error)
	preflight      func(*models.InstallRequest, *models.Catalog, preflight.Options) []*models.ValidationIssue
	apply          func(*installer.Plan, func() error) error
	output         io.Writer
	input          io.Reader
	errorOutput    io.Writer
	claudeDir      func() string
	opencodeDir    func() string
	listSnapshots  func(string) ([]installer.Snapshot, error)
	pruneSnapshots func(string, int) (int, error)
	exactCurrent   func(string, string) bool
}

func formatSnapshotSize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f KiB", float64(size)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(size)/(1024*1024))
}

func productionInstallDependencies() installDependencies {
	return installDependencies{
		loadCatalog: catalog.Load,
		loadConfig:  func(path string) (map[string]interface{}, error) { return config.LoadOrDefault(path) },
		loadConfigHash: func(path string) (string, error) {
			_, hash, err := config.LoadOrDefaultWithHash(path)
			return hash, err
		},
		wizard:         prompts.RunWizard,
		preflight:      preflight.RunWithOptions,
		apply:          installer.Apply,
		output:         os.Stdout,
		input:          os.Stdin,
		errorOutput:    os.Stderr,
		claudeDir:      paths.ClaudeDir,
		opencodeDir:    paths.OpencodeDir,
		listSnapshots:  installer.ListSnapshots,
		pruneSnapshots: installer.PruneSnapshots,
		exactCurrent:   isExactEmbeddedInstall,
	}
}

// runInstall is the production install orchestration seam. It owns request
// resolution and the ordering gate: a successful wizard and preflight must
// both complete before apply is invoked.
func runInstall(args *cliArgs, deps installDependencies) error {
	if args == nil {
		return errors.New("install arguments are required")
	}
	if deps.loadCatalog == nil {
		return errors.New("catalog dependency is required")
	}
	if deps.loadConfig == nil {
		return errors.New("config dependency is required")
	}
	if deps.wizard == nil {
		return errors.New("wizard dependency is required")
	}
	if deps.preflight == nil {
		return errors.New("preflight dependency is required")
	}
	if deps.apply == nil {
		return errors.New("apply dependency is required")
	}
	if deps.output == nil {
		deps.output = io.Discard
	}
	if deps.input == nil {
		deps.input = strings.NewReader("")
	}
	if deps.errorOutput == nil {
		deps.errorOutput = deps.output
	}
	if deps.claudeDir == nil {
		deps.claudeDir = paths.ClaudeDir
	}
	if deps.opencodeDir == nil {
		deps.opencodeDir = paths.OpencodeDir
	}
	if deps.listSnapshots == nil {
		deps.listSnapshots = installer.ListSnapshots
	}
	if deps.pruneSnapshots == nil {
		deps.pruneSnapshots = installer.PruneSnapshots
	}
	if deps.exactCurrent == nil {
		deps.exactCurrent = isExactEmbeddedInstall
	}

	cat, err := deps.loadCatalog()
	if err != nil {
		return fmt.Errorf("load catalog: %w", err)
	}
	stateDir := args.StateDir
	if stateDir == "" {
		stateDir = paths.StateDir()
	}
	cfg, err := deps.loadConfig(filepath.Join(stateDir, "skills.config.json"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	configPath := filepath.Join(stateDir, "skills.config.json")
	var configHash string
	if deps.loadConfigHash != nil {
		configHash, err = deps.loadConfigHash(configPath)
		if err != nil {
			return fmt.Errorf("load config identity: %w", err)
		}
	}
	var request *models.InstallRequest
	interactive := !args.NonInteractive && !args.DryRun && len(args.Packages) == 0 && len(args.Targets) == 0 && len(args.Versions) == 0
	if interactive {
		if !args.Force && deps.exactCurrent(stateDir, deps.opencodeDir()) {
			fmt.Fprintln(deps.output, "✓ Skynex is up to date.")
			fmt.Fprintln(deps.output, "  Nothing to install.")
			return nil
		}
		existing := prompts.ExistingInstall(stateDir, deps.opencodeDir())
		request, err = deps.wizard(prompts.WizardOptions{Existing: existing, Input: deps.input, Output: deps.errorOutput})
	} else {
		request, err = resolveNonInteractive(args, cat, cfg)
	}
	if err != nil {
		if errors.Is(err, prompts.ErrWizardCancelled) {
			return prompts.ErrWizardCancelled
		}
		return fmt.Errorf("resolve install request: %w", err)
	}
	if request == nil {
		return errors.New("resolve install request: wizard returned no request")
	}
	request.StateDir, request.TrustSetupScripts = stateDir, args.TrustScripts
	if args.LegacyVersion {
		fmt.Fprintln(deps.output, "Warning: install --version PKG=VER is deprecated; use --package-version PKG=VER")
	}
	if args.TrustScripts && interactive && !prompts.ConfirmTrustSetupScriptsWithIO(deps.input, deps.errorOutput) {
		return prompts.ErrWizardCancelled
	}
	issues := deps.preflight(request, cat, preflight.Options{ReadOnly: args.DryRun})
	if preflight.HasErrors(issues) {
		return fmt.Errorf("install preflight failed")
	}
	plan, err := installer.Build(request, cat, installer.Destinations{ClaudeDir: deps.claudeDir(), OpencodeDir: deps.opencodeDir(), StateDir: stateDir, StateConfigFile: filepath.Join(stateDir, "skills.config.json"), StateLockFile: filepath.Join(stateDir, "skills.lock.json"), OwnershipManifest: filepath.Join(stateDir, "skills.ownership.json")})
	if err != nil {
		return fmt.Errorf("build install plan: %w", err)
	}
	if args.DryRun {
		preflight.PrintIssues(issues, deps.output)
		return plan.RenderText(deps.output)
	}
	if !interactive && !args.NonInteractive && !args.Yes && !prompts.ConfirmPlanWithIO(request, deps.input, deps.output) {
		return nil
	}
	if snapshots, inventoryErr := deps.listSnapshots(stateDir); inventoryErr == nil && len(snapshots) >= 5 {
		eligible := 0
		for _, snapshot := range snapshots {
			if snapshot.EligibleToPrune {
				eligible++
			}
		}
		if !interactive {
			return fmt.Errorf("snapshot capacity reached: %d snapshots retained; run `skynex backup prune --yes --keep 3`", len(snapshots))
		}
		oldest := snapshots[0]
		choice := prompts.ChooseBackupCapacity(deps.input, deps.errorOutput, fmt.Sprintf("%d recovery snapshots retained (oldest %s, %s)", len(snapshots), oldest.CreatedAt.Format("2006-01-02"), formatSnapshotSize(oldest.Size)), eligible > 0)
		if choice != prompts.BackupRemoveOldest || eligible == 0 {
			fmt.Fprintln(deps.output, "Backups retained. Manage them with: skynex backup list")
			return nil
		}
		if _, pruneErr := deps.pruneSnapshots(stateDir, 1); pruneErr != nil {
			return fmt.Errorf("prune oldest backup: %w", pruneErr)
		}
	}
	var results []*models.InstallResult
	if err := deps.apply(plan, func() error {
		reporter := adapters.NewReporter(deps.output, args.Verbose)
		// Do not run a Huh/Bubble Tea renderer across the transaction. Its
		// asynchronous terminal capability queries can arrive after the form
		// returns and corrupt rollback/error output. The wizard is complete and
		// owns no terminal state by this point; Apply must run plainly.
		var err error
		results, err = adapters.InstallAllWithReporterAndOptions(request, cat, reporter, adapters.InstallOptions{
			ClaudeDir: deps.claudeDir(), OpencodeDir: deps.opencodeDir(), Input: deps.input, Output: deps.errorOutput,
		})
		if err != nil {
			return err
		}
		if err := config.SaveConfig(configPath, request, cfg, configHash); err != nil {
			return err
		}
		return config.SaveLock(filepath.Join(stateDir, "skills.lock.json"), results, request)
	}); err != nil {
		return err
	}
	return nil
}

func isExactEmbeddedInstall(stateDir, opencodeDir string) bool {
	bundle, err := assets.OpencodeSkillsFS()
	if err != nil {
		return false
	}
	report, err := skillsync.Inspect(bundle, filepath.Join(opencodeDir, "skills"), filepath.Join(stateDir, "skills.ownership.json"))
	if err != nil {
		return false
	}
	defer report.Close()
	if report.Manifest == nil || report.Legacy || report.Manifest.Source != "opencode/skills" || report.Manifest.SourceKind != "bundle" || report.Manifest.BundleVersion != "latest" || report.Manifest.BundleCommit != "embedded" || report.Manifest.Package != "skills" || report.Manifest.Target != "opencode" || skillsync.TreeHash(report.Manifest.Files) != report.BundleTreeSHA256 {
		return false
	}
	for _, entry := range report.Entries {
		switch entry.Status {
		case skillsync.Outdated, skillsync.Missing, skillsync.Modified, skillsync.Retired:
			return false
		}
	}
	for _, file := range report.Manifest.Files {
		entry := findSkillEntry(report.Entries, file.Path)
		if entry.Status != skillsync.Current || !entry.Owned {
			return false
		}
	}

	configData, err := os.ReadFile(filepath.Join(stateDir, "skills.config.json"))
	if err != nil {
		return false
	}
	var cfg map[string]interface{}
	if json.Unmarshal(configData, &cfg) != nil || !configHasEmbeddedSkills(cfg) {
		return false
	}
	lockData, err := os.ReadFile(filepath.Join(stateDir, "skills.lock.json"))
	if err != nil {
		return false
	}
	var lock map[string]interface{}
	if json.Unmarshal(lockData, &lock) != nil {
		return false
	}
	return lockHasEmbeddedSkills(lock)
}

func findSkillEntry(entries []skillsync.Entry, path string) skillsync.Entry {
	for _, entry := range entries {
		if entry.Path == path {
			return entry
		}
	}
	return skillsync.Entry{}
}

func configHasEmbeddedSkills(cfg map[string]interface{}) bool {
	packages, ok := cfg["packages"].(map[string]interface{})
	if !ok {
		return false
	}
	skills, ok := packages["skills"].(map[string]interface{})
	if !ok || skills["version"] != "latest" {
		return false
	}
	return targetsContain(skills["targets"], "opencode")
}

func lockHasEmbeddedSkills(lock map[string]interface{}) bool {
	packages, ok := lock["packages"].(map[string]interface{})
	if !ok {
		return false
	}
	skills, ok := packages["skills"].(map[string]interface{})
	if !ok || skills["resolvedVersion"] != "latest" || skills["commit"] != "embedded" {
		return false
	}
	targets, ok := skills["targets"].(map[string]interface{})
	if !ok {
		return false
	}
	opencode, ok := targets["opencode"].(map[string]interface{})
	return ok && opencode["status"] == "installed"
}

func targetsContain(value interface{}, target string) bool {
	items, ok := value.([]interface{})
	if !ok {
		return false
	}
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
