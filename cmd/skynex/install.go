package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
	loadCatalog          func() (*models.Catalog, error)
	loadConfig           func(string) (map[string]interface{}, error)
	loadConfigHash       func(string) (string, error)
	wizard               func(prompts.WizardOptions) (*models.InstallRequest, error)
	preflight            func(*models.InstallRequest, *models.Catalog, preflight.Options) []*models.ValidationIssue
	apply                func(*installer.Plan, func() error) error
	output               io.Writer
	input                io.Reader
	errorOutput          io.Writer
	claudeDir            func() string
	opencodeDir          func() string
	listSnapshots        func(string) ([]installer.Snapshot, error)
	pruneSnapshots       func(string, int) (int, error)
	chooseBackupCapacity func(string, bool) prompts.BackupCapacityChoice
	exactCurrent         func(string, string) bool
	exactRequestCurrent  func(string, string, *models.InstallRequest) bool
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
		chooseBackupCapacity: func(summary string, canPrune bool) prompts.BackupCapacityChoice {
			return prompts.ChooseBackupCapacity(os.Stdin, os.Stderr, summary, canPrune)
		},
		exactCurrent:        isExactEmbeddedInstall,
		exactRequestCurrent: isExactRequestedEmbeddedInstall,
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
	if deps.chooseBackupCapacity == nil {
		deps.chooseBackupCapacity = func(summary string, canPrune bool) prompts.BackupCapacityChoice {
			return prompts.ChooseBackupCapacity(deps.input, deps.errorOutput, summary, canPrune)
		}
	}
	if deps.exactCurrent == nil {
		deps.exactCurrent = isExactEmbeddedInstall
	}
	if deps.exactRequestCurrent == nil {
		deps.exactRequestCurrent = isExactRequestedEmbeddedInstall
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
	if !args.Force && !interactive && deps.exactRequestCurrent(stateDir, deps.opencodeDir(), request) {
		fmt.Fprintln(deps.output, "✓ Skynex is up to date.")
		fmt.Fprintln(deps.output, "  Nothing changed.")
		return nil
	}
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
	beforeSnapshots, _ := deps.listSnapshots(stateDir)
	if snapshots := beforeSnapshots; len(snapshots) >= 5 {
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
		choice := deps.chooseBackupCapacity(fmt.Sprintf("%d recovery snapshots retained (oldest %s, %s)", len(snapshots), oldest.CreatedAt.Format("2006-01-02"), formatSnapshotSize(oldest.Size)), eligible > 0)
		switch choice {
		case prompts.BackupCancel:
			fmt.Fprintln(deps.output, "Backups retained. Manage them with: skynex backup list")
			return nil
		case prompts.BackupManage:
			remove := len(snapshots) - 3
			removed, pruneErr := deps.pruneSnapshots(stateDir, remove)
			if pruneErr != nil {
				return fmt.Errorf("manage retained backups: %w", pruneErr)
			}
			if removed < remove {
				return fmt.Errorf("snapshot capacity remains blocked: removed %d of %d required backups; run `skynex backup list` to inspect non-prunable recovery data", removed, remove)
			}
		case prompts.BackupRemoveOldest:
			if eligible == 0 {
				return errors.New("cannot remove oldest backup: no retained backup is eligible for pruning")
			}
			if _, pruneErr := deps.pruneSnapshots(stateDir, 1); pruneErr != nil {
				return fmt.Errorf("prune oldest backup: %w", pruneErr)
			}
		default:
			return fmt.Errorf("unsupported backup capacity choice %q", choice)
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
	afterSnapshots, _ := deps.listSnapshots(stateDir)
	return renderInstallSummary(deps.output, request, plan, results, issues, newSnapshot(beforeSnapshots, afterSnapshots))
}

func newSnapshot(before, after []installer.Snapshot) *installer.Snapshot {
	known := map[string]bool{}
	for _, item := range before {
		known[item.ID] = true
	}
	for i := range after {
		if !known[after[i].ID] {
			return &after[i]
		}
	}
	return nil
}

func renderInstallSummary(out io.Writer, request *models.InstallRequest, plan *installer.Plan, results []*models.InstallResult, issues []*models.ValidationIssue, snapshot *installer.Snapshot) error {
	unchanged := len(results) > 0
	for _, result := range results {
		for _, target := range result.Targets {
			if target.Status != "unchanged" {
				unchanged = false
			}
		}
	}
	if unchanged {
		fmt.Fprintln(out, "✓ Skynex is up to date.\n  Nothing changed.")
	} else {
		fmt.Fprintln(out, "✓ Installation complete.")
	}
	packages := append([]*models.InstallResult(nil), results...)
	sort.Slice(packages, func(i, j int) bool { return packages[i].PackageID < packages[j].PackageID })
	if len(packages) > 0 {
		fmt.Fprintln(out, "Packages:")
		for _, item := range packages {
			version := item.ResolvedVersion
			if version == "" {
				version = item.RequestedVersion
			}
			fmt.Fprintf(out, "  - %s @ %s\n", item.PackageID, version)
		}
	}
	type targetLine struct {
		target, destination string
		artifacts           []string
	}
	var targets []targetLine
	for _, op := range plan.Operations {
		if op.Kind == installer.InstallTarget {
			var artifacts []string
			for _, result := range packages {
				if result.PackageID == op.PackageID {
					if tr := result.Targets[op.Target]; tr != nil {
						artifacts = append(artifacts, tr.Artifacts...)
					}
				}
			}
			sort.Strings(artifacts)
			targets = append(targets, targetLine{op.Target, op.Destination, artifacts})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].target == targets[j].target {
			return targets[i].destination < targets[j].destination
		}
		return targets[i].target < targets[j].target
	})
	if len(targets) > 0 {
		fmt.Fprintln(out, "Targets:")
		for _, item := range targets {
			fmt.Fprintf(out, "  - %s -> %s\n", item.target, item.destination)
			for _, artifact := range item.artifacts {
				fmt.Fprintf(out, "      artifact: %s\n", artifact)
			}
		}
	}
	neurox := "preserved"
	if request.NeuroxSelectionSet {
		if request.NeuroxEnabled {
			neurox = "enabled"
		} else {
			neurox = "disabled"
		}
	}
	fmt.Fprintf(out, "Integrations:\n  - Neurox: %s\n", neurox)
	var stateFiles []string
	cleanup := false
	for _, op := range plan.Operations {
		if op.Kind == installer.WriteState {
			stateFiles = append(stateFiles, op.Destination)
		}
		if op.Kind == installer.CleanupDeprecated {
			cleanup = true
		}
	}
	sort.Strings(stateFiles)
	if len(stateFiles) > 0 {
		fmt.Fprintln(out, "State files:")
		for _, path := range stateFiles {
			fmt.Fprintf(out, "  - %s\n", path)
		}
	}
	if cleanup {
		fmt.Fprintln(out, "Cleanup: applied")
	} else {
		fmt.Fprintln(out, "Cleanup: not requested")
	}
	var warnings []string
	for _, issue := range issues {
		if issue.Level == "warning" {
			warnings = append(warnings, issue.Message)
		}
	}
	sort.Strings(warnings)
	if len(warnings) > 0 {
		fmt.Fprintln(out, "Warnings:")
		for _, warning := range warnings {
			fmt.Fprintf(out, "  - %s\n", warning)
		}
	}
	if snapshot != nil {
		fmt.Fprintf(out, "Recovery snapshot: %s\n", snapshot.ID)
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

func isExactRequestedEmbeddedInstall(stateDir, opencodeDir string, request *models.InstallRequest) bool {
	if request == nil || request.CleanupDeprecated || request.TrustSetupScripts ||
		len(request.Packages) != 1 || request.Packages[0] != "skills" ||
		len(request.Targets) != 1 || request.Targets[0] != "opencode" ||
		request.Versions["skills"] != "latest" || !isExactEmbeddedInstall(stateDir, opencodeDir) {
		return false
	}
	configData, err := os.ReadFile(filepath.Join(stateDir, "skills.config.json"))
	if err != nil {
		return false
	}
	var cfg map[string]interface{}
	if json.Unmarshal(configData, &cfg) != nil {
		return false
	}
	defaults, ok := cfg["defaults"].(map[string]interface{})
	if !ok || defaults["interactive"] != request.Interactive || !targetsContain(defaults["targets"], "opencode") {
		return false
	}
	if request.NeuroxSelectionSet && defaults["neuroxEnabled"] != request.NeuroxEnabled {
		return false
	}
	return opencodeManagedAssetsCurrent(opencodeDir, request.NeuroxEnabled) && neuroxConfigCurrent(opencodeDir, request.NeuroxEnabled)
}

func opencodeManagedAssetsCurrent(opencodeDir string, neuroxEnabled bool) bool {
	bundle, err := assets.OpencodeFS()
	if err != nil {
		return false
	}
	expected := map[string]string{}
	if err := fs.WalkDir(bundle, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		path = filepath.ToSlash(path)
		if entry.IsDir() && (path == "skills" || path == "node_modules") {
			return fs.SkipDir
		}
		if entry.IsDir() || (!neuroxEnabled && path == "plugins/neurox.ts") {
			return nil
		}
		raw, readErr := fs.ReadFile(bundle, path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(raw)
		expected[path] = fmt.Sprintf("%x", sum[:])
		return nil
	}); err != nil {
		return false
	}
	manifestData, err := os.ReadFile(filepath.Join(opencodeDir, ".skynex-manifest.json"))
	if err != nil {
		return false
	}
	var manifest struct {
		Files map[string]string `json:"files"`
	}
	if json.Unmarshal(manifestData, &manifest) != nil || len(manifest.Files) != len(expected) {
		return false
	}
	for path, digest := range expected {
		if manifest.Files[path] != digest {
			return false
		}
		// opencode.json intentionally differs from the embedded source after
		// provider connections are merged. Its managed Neurox shape is checked
		// separately without treating user providers as drift.
		if path == "opencode.json" {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(opencodeDir, filepath.FromSlash(path)))
		if readErr != nil {
			return false
		}
		sum := sha256.Sum256(raw)
		if fmt.Sprintf("%x", sum[:]) != digest {
			return false
		}
	}
	return true
}

func neuroxConfigCurrent(opencodeDir string, enabled bool) bool {
	raw, err := os.ReadFile(filepath.Join(opencodeDir, "opencode.json"))
	if err != nil {
		return false
	}
	var config struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return false
	}
	entry, exists := config.MCP["neurox"]
	if !enabled {
		return !exists
	}
	var neurox struct {
		Command []string `json:"command"`
		Enabled bool     `json:"enabled"`
		Type    string   `json:"type"`
	}
	return exists && json.Unmarshal(entry, &neurox) == nil && neurox.Enabled && neurox.Type == "local" &&
		len(neurox.Command) == 2 && neurox.Command[0] == "neurox" && neurox.Command[1] == "mcp"
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
