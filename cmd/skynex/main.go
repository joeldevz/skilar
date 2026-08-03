package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/joeldevz/skynex/internal/adapters"
	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/binaryinstall"
	"github.com/joeldevz/skynex/internal/catalog"
	"github.com/joeldevz/skynex/internal/completion"
	"github.com/joeldevz/skynex/internal/config"
	"github.com/joeldevz/skynex/internal/doctor"
	"github.com/joeldevz/skynex/internal/installer"
	"github.com/joeldevz/skynex/internal/models"
	"github.com/joeldevz/skynex/internal/paths"
	"github.com/joeldevz/skynex/internal/preflight"
	"github.com/joeldevz/skynex/internal/profiles"
	"github.com/joeldevz/skynex/internal/prompts"
	"github.com/joeldevz/skynex/internal/runner"
	"github.com/joeldevz/skynex/internal/skillsync"
)

// truncate safely truncates a string to n characters
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// version is set by goreleaser via -ldflags "-X main.version=..."
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) == 4 && os.Args[1] == "internal-install-binary" {
		if err := binaryinstall.Install(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "workflow" {
		if err := runWorkflowCLI(os.Args[2:], "", os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "Workflow error:", err)
			os.Exit(1)
		}
		return
	}
	args := parseArgs()
	if args.ParseError != "" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", args.ParseError)
		os.Exit(2)
	}

	if args.ShowVersion {
		fmt.Printf("skynex %s (%s) built %s\n", version, commit, date)
		os.Exit(0)
	}

	if args.Doctor {
		report := doctor.Run()
		report.Print()
		if report.HasErrors() {
			os.Exit(1)
		}
		os.Exit(0)
	}

	// status
	if args.Status {
		handleStatus()
		os.Exit(0)
	}
	if args.BackupCommand != "" {
		handleBackup(args)
		os.Exit(0)
	}

	if args.Help {
		printUsage()
		os.Exit(0)
	}

	// completion
	if args.Completion != "" {
		handleCompletion(args.Completion)
		os.Exit(0)
	}

	// profile help
	if args.ProfileHelp {
		printProfileUsage()
		os.Exit(0)
	}

	// profiles — list
	if args.ProfileList {
		handleProfileList()
		os.Exit(0)
	}

	// profile create
	if args.ProfileCreate {
		handleProfileCreate("")
		os.Exit(0)
	}

	// profile edit
	if args.ProfileEdit != "" {
		handleProfileEdit(args.ProfileEdit)
		os.Exit(0)
	}

	// profile delete
	if args.ProfileDelete != "" {
		handleProfileDelete(args.ProfileDelete)
		os.Exit(0)
	}

	// profile set default
	if args.ProfileDefault != "" {
		handleProfileSetDefault(args.ProfileDefault)
		os.Exit(0)
	}

	// up
	if args.RunUp {
		handleUp(args.UpProfile, args.UpWeb, args.UpPort)
		os.Exit(0)
	}

	if args.ListPackages {
		cat, err := catalog.Load()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		catalog.Print(cat)
		os.Exit(0)
	}

	// update
	if args.Update {
		handleUpdate(args.UpdatePkg, args.StateDir, args.CleanupDeprecated, args.TrustScripts)
		os.Exit(0)
	}

	// install
	if args.Install {
		if err := runInstall(args, productionInstallDependencies()); err != nil {
			if errors.Is(err, prompts.ErrWizardCancelled) {
				fmt.Println("Installation cancelled. No changes were made.")
			} else {
				fmt.Fprintln(os.Stderr, compactInstallError(err, args.Verbose))
				os.Exit(1)
			}
		}
		os.Exit(0)
	}

	// No recognized command — show help
	printUsage()
	os.Exit(0)
}

func compactInstallError(err error, verbose bool) string {
	message := strings.ReplaceAll(err.Error(), "^[[", "\x1b[")
	message = strings.Join(strings.Fields(ansi.Strip(message)), " ")
	if verbose {
		return "Installation failed: " + message
	}
	return "Installation failed: " + message + " (rerun with --verbose for details)"
}

func handleBackup(args *cliArgs) {
	stateDir := args.StateDir
	if stateDir == "" {
		stateDir = paths.StateDir()
	}
	snapshots, err := installer.ListSnapshots(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Backup error: %v\n", err)
		return
	}
	if args.BackupCommand == "list" {
		if len(snapshots) == 0 {
			fmt.Println("No retained backups.")
			return
		}
		for _, snapshot := range snapshots {
			status := snapshot.Status
			if status == "" {
				status = "unknown"
			}
			eligible := "no"
			if snapshot.EligibleToPrune {
				eligible = "yes"
			}
			fmt.Printf("%s  %s  %s  %s  files=%d  prune=%s\n", snapshot.ID, status, snapshot.CreatedAt.Format("2006-01-02 15:04:05Z07:00"), formatSnapshotSize(snapshot.Size), snapshot.FileCount, eligible)
		}
		fmt.Printf("Retained: %d\n", len(snapshots))
		return
	}
	keep := args.BackupKeep
	if keep == 0 {
		keep = 3
	}
	eligible := 0
	for _, snapshot := range snapshots {
		if snapshot.EligibleToPrune {
			eligible++
		}
	}
	remove := eligible - keep
	if remove <= 0 {
		fmt.Printf("Pruned: 0; retained: %d\n", len(snapshots))
		return
	}
	if !args.BackupYes && !prompts.ConfirmBackupPrune(os.Stdin, os.Stdout, remove) {
		fmt.Println("Backup prune cancelled.")
		return
	}
	removed, err := installer.PruneSnapshots(stateDir, remove)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Backup prune failed: %v\n", err)
		return
	}
	remaining, _ := installer.ListSnapshots(stateDir)
	fmt.Printf("Pruned: %d; retained: %d\n", removed, len(remaining))
}

type cliArgs struct {
	Packages          []string
	Targets           []string
	Versions          map[string]string
	NonInteractive    bool
	Yes               bool
	TrustScripts      bool
	StateDir          string
	Help              bool
	ListPackages      bool
	ListVersions      string
	ShowVersion       bool
	Doctor            bool
	Install           bool
	ProfileHelp       bool
	ProfileList       bool
	ProfileCreate     bool
	ProfileEdit       string
	ProfileDelete     string
	ProfileDefault    string
	UpProfile         string
	UpWeb             bool
	UpPort            int
	RunUp             bool
	Update            bool
	UpdatePkg         string
	Completion        string // bash, zsh, or fish
	Status            bool
	CleanupDeprecated bool
	DryRun            bool
	Verbose           bool
	Force             bool
	BackupCommand     string
	BackupYes         bool
	BackupKeep        int
	LegacyVersion     bool
	ParseError        string
	WithNeurox        bool
	WithoutNeurox     bool
}

func parseArgs() *cliArgs {
	return parseArgsFrom(os.Args[1:])
}

func parseArgsFrom(osArgs []string) *cliArgs {
	args := &cliArgs{Versions: make(map[string]string)}
	value := func(i *int, flag string) (string, bool) {
		if *i+1 >= len(osArgs) || isFlag(osArgs[*i+1]) {
			args.ParseError = fmt.Sprintf("%s requires a value", flag)
			return "", false
		}
		*i = *i + 1
		return osArgs[*i], true
	}

	for i := 0; i < len(osArgs); i++ {
		switch osArgs[i] {
		case "--help", "-h":
			args.Help = true
		case "version":
			args.ShowVersion = true
		case "doctor":
			args.Doctor = true
		case "install":
			args.Install = true
		case "backup":
			if i+1 >= len(osArgs) {
				args.ParseError = "backup requires list or prune"
				break
			}
			sub := osArgs[i+1]
			if sub != "list" && sub != "prune" {
				args.ParseError = "backup requires list or prune"
				break
			}
			args.BackupCommand = sub
			i++
		case "profiles":
			// alias for `profile list`
			args.ProfileList = true
		case "profile":
			if i+1 >= len(osArgs) {
				// skynex profile (no subcommand)
				args.ProfileHelp = true
				break
			}
			sub := osArgs[i+1]
			switch sub {
			case "list":
				args.ProfileList = true
				i++
			case "create":
				args.ProfileCreate = true
				i++
			default:
				// sub is a profile name; expect an action verb next
				profileName := sub
				i++
				if i+1 < len(osArgs) {
					verb := osArgs[i+1]
					i++
					switch verb {
					case "edit":
						args.ProfileEdit = profileName
					case "delete":
						args.ProfileDelete = profileName
					case "default":
						args.ProfileDefault = profileName
					default:
						// unknown verb — treat as profile help
						args.ProfileHelp = true
					}
				} else {
					// name with no verb — treat as profile help
					args.ProfileHelp = true
				}
			}
		case "completion":
			if v, ok := value(&i, "completion"); ok {
				args.Completion = v
			} else {
				args.Completion = "help"
			}
		case "up":
			// skynex up [profile] [--web] [--port N]
			for i+1 < len(osArgs) {
				next := osArgs[i+1]
				if next == "--web" {
					args.UpWeb = true
					i++
				} else if next == "--port" {
					if i+2 >= len(osArgs) || isFlag(osArgs[i+2]) {
						args.ParseError = "--port requires a value"
						break
					}
					if _, err := fmt.Sscanf(osArgs[i+2], "%d", &args.UpPort); err != nil {
						args.ParseError = "--port requires an integer value"
						break
					}
					i += 2
				} else if !strings.HasPrefix(next, "-") && args.UpProfile == "" {
					args.UpProfile = next
					i++
				} else {
					break
				}
			}
			args.RunUp = true
		case "--list-packages":
			args.ListPackages = true
		case "--list-versions":
			if v, ok := value(&i, "--list-versions"); ok {
				args.ListVersions = v
			}
		case "--package":
			if v, ok := value(&i, "--package"); ok {
				args.Packages = append(args.Packages, v)
			}
		case "--target":
			if v, ok := value(&i, "--target"); ok {
				if v == "both" {
					args.Targets = append(args.Targets, "claude", "opencode")
				} else {
					args.Targets = append(args.Targets, v)
				}
			}
		case "--package-version":
			if i+1 >= len(osArgs) || isFlag(osArgs[i+1]) {
				args.ParseError = "--package-version requires PKG=VER"
				break
			}
			i++
			parts := splitOnce(osArgs[i], "=")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				args.ParseError = "--package-version requires PKG=VER"
				break
			}
			args.Versions[parts[0]] = parts[1]
		case "--version":
			if args.Install && i+1 < len(osArgs) && !isFlag(osArgs[i+1]) {
				i++
				parts := splitOnce(osArgs[i], "=")
				if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
					args.ParseError = "--version requires PKG=VER"
					break
				}
				args.Versions[parts[0]] = parts[1]
				args.LegacyVersion = true
			} else {
				args.ShowVersion = true
			}
		case "--non-interactive":
			args.NonInteractive = true
		case "--yes", "-y":
			args.Yes = true
			args.BackupYes = true
		case "--keep":
			if v, ok := value(&i, "--keep"); ok {
				if _, err := fmt.Sscanf(v, "%d", &args.BackupKeep); err != nil || args.BackupKeep < 0 {
					args.ParseError = "--keep requires a non-negative integer"
				}
			}
		case "--trust-setup-scripts":
			args.TrustScripts = true
		case "--with-neurox":
			args.WithNeurox = true
		case "--without-neurox":
			args.WithoutNeurox = true
		case "--state-dir":
			if v, ok := value(&i, "--state-dir"); ok {
				args.StateDir = v
			}
		case "--cleanup-deprecated":
			args.CleanupDeprecated = true
		case "--dry-run":
			args.DryRun = true
		case "--verbose":
			args.Verbose = true
		case "--force":
			args.Force = true
		case "update":
			args.Update = true
			// optional package name
			if i+1 < len(osArgs) && !isFlag(osArgs[i+1]) {
				i++
				args.UpdatePkg = osArgs[i]
			}
		case "status":
			args.Status = true
		default:
			if isFlag(osArgs[i]) && args.ParseError == "" {
				args.ParseError = fmt.Sprintf("unknown option: %s", osArgs[i])
			}
		}
	}
	if args.WithNeurox && args.WithoutNeurox {
		args.ParseError = "--with-neurox and --without-neurox are mutually exclusive"
	}
	return args
}

func isFlag(s string) bool {
	return len(s) > 0 && s[0] == '-'
}

func splitOnce(s, sep string) []string {
	for i := 0; i < len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return []string{s[:i], s[i+len(sep):]}
		}
	}
	return []string{s}
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
}

func resolveNonInteractive(args *cliArgs, cat *models.Catalog, cfg map[string]interface{}) (*models.InstallRequest, error) {
	if len(args.Packages) == 0 {
		return nil, fmt.Errorf("--package is required in non-interactive mode")
	}
	if len(args.Targets) == 0 {
		return nil, fmt.Errorf("--target is required in non-interactive mode")
	}

	// Validate packages exist in catalog
	for _, pkg := range args.Packages {
		if _, ok := cat.Packages[pkg]; !ok {
			return nil, fmt.Errorf("unknown package: %s", pkg)
		}
	}
	for pkg, version := range args.Versions {
		if _, ok := cat.Packages[pkg]; !ok {
			return nil, fmt.Errorf("unknown package in --version: %s", pkg)
		}
		if !validVersionRef(version) {
			return nil, fmt.Errorf("unsafe version for %s", pkg)
		}
	}

	// Resolve versions
	versions := make(map[string]string)
	for _, pkg := range args.Packages {
		if v, ok := args.Versions[pkg]; ok {
			versions[pkg] = v
		} else {
			versions[pkg] = cat.Packages[pkg].DefaultVersion
		}
	}

	req := &models.InstallRequest{
		Packages:           args.Packages,
		Targets:            args.Targets,
		Versions:           versions,
		Interactive:        false,
		CleanupDeprecated:  args.CleanupDeprecated,
		TrustSetupScripts:  args.TrustScripts,
		NeuroxEnabled:      !args.WithoutNeurox,
		NeuroxSelectionSet: true,
	}

	return req, nil
}

func validVersionRef(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.Contains(value, "..") || strings.HasPrefix(value, "-") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._+-", r) {
			continue
		}
		return false
	}
	return true
}

func handleProfileList() {
	defaultName := profiles.GetDefault()

	fmt.Println("\n  Built-in tiers:")
	tiers := []struct{ name, desc string }{
		{"cheap", "Haiku everywhere — fast & cheap"},
		{"balanced", "Sonnet for planning, Haiku for execution"},
		{"premium", "Opus for planning, Sonnet for execution"},
	}
	for _, t := range tiers {
		marker := ""
		if t.name == defaultName {
			marker = " ★"
		}
		fmt.Printf("  %-16s %s%s\n", t.name, t.desc, marker)
	}
	fmt.Println()

	saved, err := profiles.List()
	if err != nil || len(saved) == 0 {
		fmt.Println("  No custom profiles saved.")
		fmt.Println("  Create one: skynex profile create")
		return
	}

	fmt.Println("  Custom profiles:")
	for _, p := range saved {
		marker := ""
		if p.Name == defaultName {
			marker = " ★"
		}
		fmt.Printf("  %-16s %d agents configured%s\n", p.Name, len(p.Models), marker)
	}
	fmt.Println()
	fmt.Printf("  Default: %s\n", defaultName)
	fmt.Println("  Usage: skynex up")
}

func handleProfileCreate(initialName string) {
	// Call the TUI flow
	result, err := prompts.RunProfileCreationFlow(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cancelled.\n")
		return
	}

	p := &profiles.Profile{
		Name:      result.Name,
		Models:    result.Models,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := profiles.Save(p); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving profile: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n  ✓ Profile %q saved.\n", p.Name)
	fmt.Printf("  Usage: skynex up %s\n\n", p.Name)
}

func handleProfileEdit(name string) {
	p, err := profiles.Load(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Profile %q not found.\n", name)
		os.Exit(1)
	}

	result, err := prompts.RunProfileCreationFlow(p.Models)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cancelled.\n")
		return
	}

	p.Models = result.Models
	p.UpdatedAt = time.Now()

	if err := profiles.Save(p); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n  ✓ Profile %q updated.\n\n", p.Name)
}

func handleProfileDelete(name string) {
	if err := profiles.Delete(name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  ✓ Profile %q deleted.\n", name)
}

func handleProfileSetDefault(name string) {
	if err := profiles.SetDefault(name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  ✓ %q is now the default profile.\n", name)
	fmt.Printf("  Run skynex up to launch with this profile.\n")
}

func handleUp(profileName string, web bool, port int) {
	if profileName == "" {
		profileName = profiles.GetDefault()
	}

	fmt.Printf("\n  Launching OpenCode")
	if profileName != "" {
		fmt.Printf(" with profile: %s", profileName)
	}
	if web {
		fmt.Printf(" (web UI)")
	}
	if port > 0 {
		fmt.Printf(" on port %d", port)
	}
	fmt.Println()

	if err := runner.Run(runner.Options{
		Profile: profileName,
		Web:     web,
		Port:    port,
	}); err != nil {
		// If the process terminated normally (exit 0 or ctrl+c), don't treat as error
		fmt.Fprintf(os.Stderr, "opencode exited: %v\n", err)
	}
}

func handleCompletion(shell string) {
	switch shell {
	case "bash":
		fmt.Print(completion.Bash())
	case "zsh":
		fmt.Print(completion.Zsh())
	case "fish":
		fmt.Print(completion.Fish())
	default:
		fmt.Fprintf(os.Stderr, "Unknown shell: %s\nSupported: bash, zsh, fish\n\nUsage:\n  skynex completion bash  > /etc/bash_completion.d/skynex\n  skynex completion zsh   > ~/.zfunc/_skynex\n  skynex completion fish  > ~/.config/fish/completions/skynex.fish\n", shell)
		os.Exit(1)
	}
}

func handleUpdate(pkg string, stateDir string, cleanupDeprecated bool, trustScripts ...bool) {
	if stateDir == "" {
		stateDir = paths.StateDir()
	}
	// Update is deliberately non-interactive: never overwrite local skill
	// edits, and perform this gate before the binary self-upgrade can mutate.
	if pkg == "" || pkg == "skills" {
		if bundle, bundleErr := assets.OpencodeSkillsFS(); bundleErr == nil {
			report, inspectErr := skillsync.Inspect(bundle, filepath.Join(paths.OpencodeDir(), "skills"), filepath.Join(stateDir, "skills.ownership.json"))
			if inspectErr != nil {
				fmt.Fprintf(os.Stderr, "Update aborted: cannot inspect OpenCode skills: %v\n", inspectErr)
				os.Exit(2)
			}
			for _, entry := range report.Entries {
				if entry.Status == skillsync.Modified {
					fmt.Fprintf(os.Stderr, "Update aborted: modified OpenCode skill %q would require a decision.\nRun: skynex install\n", entry.Path)
					os.Exit(2)
				}
			}
			_ = report.Close()
		}
	}

	// Self-upgrade binary when updating all packages
	if pkg == "" {
		fmt.Println("\n  Checking for binary updates...")
		if err := selfUpgrade(); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: binary upgrade failed: %v\n", err)
			fmt.Fprintln(os.Stderr, "  Continuing with package update...")
		}
	}

	// Load existing config to know what was installed
	cfg, err := config.LoadOrDefault(stateDir + "/skills.config.json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}
	pkgsMap, ok := cfg["packages"].(map[string]interface{})
	if !ok || len(pkgsMap) == 0 {
		fmt.Fprintln(os.Stderr, "No packages installed yet. Run: skynex install")
		os.Exit(1)
	}

	// Load catalog
	cat, err := catalog.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading catalog: %v\n", err)
		os.Exit(1)
	}

	// Determine which packages to update
	var packagesToUpdate []string
	if pkg != "" {
		// Update specific package
		if _, exists := pkgsMap[pkg]; !exists {
			fmt.Fprintf(os.Stderr, "Package %q is not installed. Installed: %s\n", pkg, installedPkgNames(pkgsMap))
			os.Exit(1)
		}
		packagesToUpdate = []string{pkg}
	} else {
		// Update all
		for p := range pkgsMap {
			packagesToUpdate = append(packagesToUpdate, p)
		}
	}

	// Resolve targets from config defaults
	var targets []string
	if defaults, ok := cfg["defaults"].(map[string]interface{}); ok {
		if t, ok := defaults["targets"].([]interface{}); ok {
			for _, v := range t {
				if s, ok := v.(string); ok {
					targets = append(targets, s)
				}
			}
		}
	}
	if len(targets) == 0 {
		targets = []string{"claude", "opencode"}
	}

	// Resolve versions — always use "latest" for updates
	versions := make(map[string]string)
	for _, p := range packagesToUpdate {
		versions[p] = "latest"
	}

	request := newUpdateInstallRequest(packagesToUpdate, targets, versions, stateDir, cleanupDeprecated, trustScripts...)
	if defaults, ok := cfg["defaults"].(map[string]interface{}); ok {
		if enabled, ok := defaults["neuroxEnabled"].(bool); ok {
			request.NeuroxEnabled, request.NeuroxSelectionSet = enabled, true
		}
	}

	// Preflight
	issues := preflight.Run(request, cat)
	if preflight.HasErrors(issues) {
		preflight.PrintIssues(issues)
		fmt.Fprintln(os.Stderr, "\nUpdate aborted due to validation errors.")
		os.Exit(2)
	}

	// Install, skill ownership, config, and lock are one snapshot transaction.
	fmt.Printf("\n  Updating %s...\n", strings.Join(packagesToUpdate, ", "))
	plan, err := installer.Build(request, cat, installer.Destinations{
		ClaudeDir: paths.ClaudeDir(), OpencodeDir: paths.OpencodeDir(), StateDir: stateDir,
		StateConfigFile: filepath.Join(stateDir, "skills.config.json"), StateLockFile: filepath.Join(stateDir, "skills.lock.json"),
		OwnershipManifest: filepath.Join(stateDir, "skills.ownership.json"),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nUpdate failed while building plan: %v\n", err)
		os.Exit(1)
	}
	var results []*models.InstallResult
	if err := installer.Apply(plan, func() error {
		var installErr error
		results, installErr = adapters.InstallAll(request, cat)
		if installErr != nil {
			return installErr
		}
		if request.CleanupDeprecated {
			deprecated, discoveryErr := adapters.FindDeprecatedFiles()
			if discoveryErr != nil {
				return discoveryErr
			}
			var allFiles []adapters.DeprecatedFile
			for _, files := range deprecated {
				allFiles = append(allFiles, files...)
			}
			if len(allFiles) > 0 {
				adapters.NotifyDeprecatedFiles("", allFiles)
				if _, removeErr := adapters.RemoveDeprecatedFiles(allFiles); removeErr != nil {
					return removeErr
				}
			}
		}
		if saveErr := config.SaveConfig(stateDir+"/skills.config.json", request, cfg); saveErr != nil {
			return saveErr
		}
		return config.SaveLock(stateDir+"/skills.lock.json", results, request)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "\nUpdate failed: %v\n", err)
		os.Exit(1)
	}

	// Print results
	fmt.Println("\n  Update complete!")
	for _, r := range results {
		fmt.Printf("    %s @ %s (%s)\n", r.PackageID, r.ResolvedVersion, truncate(r.Commit, 8))
		for target, tr := range r.Targets {
			fmt.Printf("      [%s] %s\n", target, tr.Status)
		}
	}
	fmt.Println()
}

func newUpdateInstallRequest(packages, targets []string, versions map[string]string, stateDir string, cleanupDeprecated bool, trustScripts ...bool) *models.InstallRequest {
	trust := len(trustScripts) > 0 && trustScripts[0]
	return &models.InstallRequest{
		Packages:           packages,
		Targets:            targets,
		Versions:           versions,
		Interactive:        false,
		StateDir:           stateDir,
		CleanupDeprecated:  cleanupDeprecated,
		TrustSetupScripts:  trust,
		NeuroxEnabled:      true,
		NeuroxSelectionSet: true,
	}
}

func installedPkgNames(pkgs map[string]interface{}) string {
	names := make([]string, 0, len(pkgs))
	for k := range pkgs {
		names = append(names, k)
	}
	return strings.Join(names, ", ")
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func colorFn(active bool, code string) string {
	if active {
		return code
	}
	return ""
}

func handleStatus() {
	useColors := isTerminal()
	green := colorFn(useColors, "\033[0;32m")
	yellow := colorFn(useColors, "\033[1;33m")
	dim := colorFn(useColors, "\033[2m")
	bold := colorFn(useColors, "\033[1m")
	reset := colorFn(useColors, "\033[0m")

	// Version
	fmt.Printf("\n  %sskynex%s %s %s(%s)%s\n", bold, reset, version, dim, commit, reset)
	fmt.Println()

	// Installed packages
	stateDir := paths.StateDir()
	lockPath := stateDir + "/skills.lock.json"
	lockData, err := os.ReadFile(lockPath)

	fmt.Printf("  %sInstalled packages:%s\n", bold, reset)
	if err != nil {
		fmt.Printf("    %sNone — run: skynex install%s\n", dim, reset)
	} else {
		var lock map[string]interface{}
		if err := json.Unmarshal(lockData, &lock); err == nil {
			if pkgs, ok := lock["packages"].(map[string]interface{}); ok {
				for pkgID, v := range pkgs {
					pkg, ok := v.(map[string]interface{})
					if !ok {
						continue
					}
					ver := "unknown"
					if rv, ok := pkg["resolvedVersion"].(string); ok {
						ver = rv
					}
					commitStr := ""
					if c, ok := pkg["commit"].(string); ok && len(c) >= 8 {
						commitStr = c[:8]
					}

					// Get targets
					targetList := []string{}
					if targets, ok := pkg["targets"].(map[string]interface{}); ok {
						for t := range targets {
							targetList = append(targetList, t)
						}
					}
					targetsStr := strings.Join(targetList, ", ")
					if targetsStr == "" {
						targetsStr = "-"
					}

					fmt.Printf("    %s%-12s%s %s%s%s  → %s", green, pkgID, reset, dim, ver, reset, targetsStr)
					if commitStr != "" {
						fmt.Printf("  %s(%s)%s", dim, commitStr, reset)
					}
					fmt.Println()
				}
			}
		}
	}
	fmt.Println()

	// Profiles
	defaultProfile := profiles.GetDefault()
	fmt.Printf("  %sDefault profile:%s %s ★\n", bold, reset, defaultProfile)

	saved, _ := profiles.List()
	if len(saved) > 0 {
		names := make([]string, len(saved))
		for i, p := range saved {
			names[i] = p.Name
		}
		fmt.Printf("  %sCustom profiles:%s  %d (%s)\n", bold, reset, len(saved), strings.Join(names, ", "))
	} else {
		fmt.Printf("  %sCustom profiles:%s  none\n", bold, reset)
	}
	fmt.Println()

	// Tools
	fmt.Printf("  %sTools:%s\n", bold, reset)
	tools := []struct{ name, binary string }{
		{"opencode", "opencode"},
		{"claude", "claude"},
		{"git", "git"},
	}
	for _, t := range tools {
		path, err := exec.LookPath(t.binary)
		if err != nil {
			fmt.Printf("    %s✗%s  %-12s %snot found%s\n", yellow, reset, t.name, dim, reset)
		} else {
			fmt.Printf("    %s✓%s  %-12s %s%s%s\n", green, reset, t.name, dim, path, reset)
		}
	}
	fmt.Println()
}

func printUsage() {
	fmt.Println(`Usage: skynex [command] [options]

Commands:
  workflow <command>      Inspect or control managed workflows
  install                 Interactive installer (TUI)
  backup list              List retained recovery backups
  backup prune             Remove eligible backups (interactive)
  backup prune --yes --keep N  Prune eligible backups for automation
  update [package]        Update installed packages to latest version
  status                  Show installed packages, profiles, and tools
  doctor                  Check environment and dependencies
  version                 Show version
  profile                 Manage profiles (list, create, edit, delete)
  profile list            List all profiles (builtin + custom)
  profile create          Create a new profile (TUI)
  profile <name> edit     Edit an existing profile
  profile <name> delete   Delete a custom profile
  profile <name> default  Set default profile for skynex up
  up [profile]            Launch OpenCode with a profile
                          Builtin: cheap, balanced, premium
                          Custom: any profile you created
  up [profile] --web      Launch web UI instead of TUI
  up [profile] --port N   Use specific port (with --web)

Examples:
  skynex install
  skynex update                    Update all installed packages
  skynex update skills             Update only skills
  skynex up                        Launch with balanced profile
  skynex up cheap                  Haiku everywhere
  skynex up frontend               Your custom frontend profile
  skynex up frontend --web --port 3001
  skynex profile list
  skynex profile create
  skynex profile backend edit
  skynex profile backend delete

Options:
   --package PACKAGE          Package to install (skills). Repeatable.
   --target TARGET            Target: claude, opencode, or both. Repeatable.
   --version PKG=VER          Version for a package (e.g., skills=latest). Repeatable.
    --cleanup-deprecated       Remove deprecated skynex-managed files (interactive install: prompts; update: flag only).
    --dry-run                  Print the deterministic install plan and run read-only preflight.
   --verbose                 Show detailed install progress.
	   --force                   Bypass the exact-current no-op check.
   --non-interactive          Skip prompts, require all inputs via flags.
   --yes, -y                  Skip confirmation prompt.
   --trust-setup-scripts      Trust external setup scripts.
	--with-neurox              Install the Neurox MCP and OpenCode plugin (recommended default).
	--without-neurox           Do not install Neurox integration.
   --state-dir DIR            State directory (default: ~/.config/skynex).
   --list-packages            List available packages.
   --list-versions PKG        List versions for a package.
   --version                  Show version and exit.
   -h, --help                 Show this help.`)
}

func printProfileUsage() {
	fmt.Println(`Usage: skynex profile <command>

Commands:
  list                    List all profiles (builtin + custom)
  create                  Create a new profile (TUI)
  <name> edit             Edit an existing profile
  <name> delete           Delete a custom profile
  <name> default          Set a profile as the default for skynex up

Examples:
  skynex profile list
  skynex profile create
  skynex profile backend edit
  skynex profile backend delete
  skynex profile backend default`)
}
