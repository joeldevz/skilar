package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/adapters"
	"github.com/joeldevz/skynex/internal/catalog"
	"github.com/joeldevz/skynex/internal/completion"
	"github.com/joeldevz/skynex/internal/config"
	"github.com/joeldevz/skynex/internal/doctor"
	"github.com/joeldevz/skynex/internal/models"
	"github.com/joeldevz/skynex/internal/paths"
	"github.com/joeldevz/skynex/internal/preflight"
	"github.com/joeldevz/skynex/internal/profiles"
	"github.com/joeldevz/skynex/internal/prompts"
	"github.com/joeldevz/skynex/internal/runner"
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
	if len(os.Args) > 1 && os.Args[1] == "workflow" {
		if err := runWorkflowCLI(os.Args[2:], "", os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "Workflow error:", err)
			os.Exit(1)
		}
		return
	}
	args := parseArgs()

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
		handleUpdate(args.UpdatePkg, args.StateDir, args.CleanupDeprecated)
		os.Exit(0)
	}

	// install
	if args.Install {
		// Load catalog
		cat, err := catalog.Load()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading catalog: %v\n", err)
			os.Exit(1)
		}

		// Load existing config
		stateDir := args.StateDir
		if stateDir == "" {
			stateDir = paths.StateDir()
		}
		cfg := config.LoadOrDefault(stateDir + "/skills.config.json")

		// Resolve request
		var request *models.InstallRequest
		if args.NonInteractive {
			request, err = resolveNonInteractive(args, cat, cfg)
		} else {
			request, err = prompts.ResolveInteractive(cat, cfg, args)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(2)
		}
		request.StateDir = stateDir

		// Preflight
		issues := preflight.Run(request, cat)
		if preflight.HasErrors(issues) {
			preflight.PrintIssues(issues)
			fmt.Fprintln(os.Stderr, "\nInstallation aborted due to validation errors.")
			os.Exit(2)
		}

		// Confirm
		if !args.NonInteractive && !args.Yes {
			if !prompts.ConfirmPlan(request, cat) {
				fmt.Println("Installation cancelled.")
				os.Exit(0)
			}
		}

		// Install
		fmt.Println("\nInstalling packages...")
		results, err := adapters.InstallAll(request, cat)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nInstallation failed: %v\n", err)
			os.Exit(1)
		}

		// Save state
		config.SaveConfig(stateDir+"/skills.config.json", request, cfg)
		config.SaveLock(stateDir+"/skills.lock.json", results, request)

		// Print results
		fmt.Println("\nInstallation complete!")
		for _, r := range results {
			fmt.Printf("\n  %s @ %s (%s)\n", r.PackageID, r.ResolvedVersion, truncate(r.Commit, 8))
			for target, tr := range r.Targets {
				fmt.Printf("    [%s] %s: %s\n", target, tr.Status, joinStrings(tr.Artifacts))
			}
		}
		fmt.Printf("\nState files written to %s\n", stateDir)
		os.Exit(0)
	}

	// No recognized command — show help
	printUsage()
	os.Exit(0)
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
	AdvisorModel      string
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
}

func parseArgs() *cliArgs {
	args := &cliArgs{Versions: make(map[string]string)}
	osArgs := os.Args[1:]

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
			if i+1 < len(osArgs) {
				i++
				args.Completion = osArgs[i]
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
				} else if next == "--port" && i+2 < len(osArgs) {
					fmt.Sscanf(osArgs[i+2], "%d", &args.UpPort)
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
			if i+1 < len(osArgs) {
				i++
				args.ListVersions = osArgs[i]
			}
		case "--package":
			if i+1 < len(osArgs) {
				i++
				args.Packages = append(args.Packages, osArgs[i])
			}
		case "--target":
			if i+1 < len(osArgs) {
				i++
				if osArgs[i] == "both" {
					args.Targets = append(args.Targets, "claude", "opencode")
				} else {
					args.Targets = append(args.Targets, osArgs[i])
				}
			}
		case "--version":
			// Check if this is the info flag or package version flag
			if i+1 < len(osArgs) && !isFlag(osArgs[i+1]) {
				i++
				parts := splitOnce(osArgs[i], "=")
				if len(parts) == 2 {
					args.Versions[parts[0]] = parts[1]
				}
			} else {
				// --version with no arg or followed by flag = show version
				args.ShowVersion = true
			}
		case "--non-interactive":
			args.NonInteractive = true
		case "--yes", "-y":
			args.Yes = true
		case "--trust-setup-scripts":
			args.TrustScripts = true
		case "--state-dir":
			if i+1 < len(osArgs) {
				i++
				args.StateDir = osArgs[i]
			}
		case "--advisor-model":
			if i+1 < len(osArgs) {
				i++
				args.AdvisorModel = osArgs[i]
			}
		case "--cleanup-deprecated":
			args.CleanupDeprecated = true
		case "update":
			args.Update = true
			// optional package name
			if i+1 < len(osArgs) && !isFlag(osArgs[i+1]) {
				i++
				args.UpdatePkg = osArgs[i]
			}
		case "status":
			args.Status = true
		}
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
		Packages:          args.Packages,
		Targets:           args.Targets,
		Versions:          versions,
		Interactive:       false,
		CleanupDeprecated: args.CleanupDeprecated,
	}

	// Advisor config from flag
	if args.AdvisorModel != "" {
		req.Advisor = &models.AdvisorConfig{
			Enabled: true,
			Model:   args.AdvisorModel,
			MaxUses: 3,
		}
	}

	return req, nil
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

func handleUpdate(pkg string, stateDir string, cleanupDeprecated bool) {
	if stateDir == "" {
		stateDir = paths.StateDir()
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
	cfg := config.LoadOrDefault(stateDir + "/skills.config.json")
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

	request := newUpdateInstallRequest(packagesToUpdate, targets, versions, stateDir, cleanupDeprecated)

	// Preflight
	issues := preflight.Run(request, cat)
	if preflight.HasErrors(issues) {
		preflight.PrintIssues(issues)
		fmt.Fprintln(os.Stderr, "\nUpdate aborted due to validation errors.")
		os.Exit(2)
	}

	// Install
	fmt.Printf("\n  Updating %s...\n", strings.Join(packagesToUpdate, ", "))
	results, err := adapters.InstallAll(request, cat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nUpdate failed: %v\n", err)
		os.Exit(1)
	}

	// Handle deprecated file cleanup (update: only if --cleanup-deprecated is set)
	if request.CleanupDeprecated {
		deprecated, err := adapters.FindDeprecatedFiles()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: deprecated file discovery failed: %v\n", err)
			os.Exit(1)
		}
		if len(deprecated) > 0 {
			var allFiles []adapters.DeprecatedFile
			for _, files := range deprecated {
				allFiles = append(allFiles, files...)
			}
			adapters.NotifyDeprecatedFiles("", allFiles)
			removed, err := adapters.RemoveDeprecatedFiles(allFiles)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: cleanup failed: %v\n", err)
				os.Exit(1)
			} else if removed > 0 {
				fmt.Printf("  Removed %d deprecated files.\n", removed)
			}
		}
	}

	// Save state
	config.SaveConfig(stateDir+"/skills.config.json", request, cfg)
	config.SaveLock(stateDir+"/skills.lock.json", results, request)

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

func newUpdateInstallRequest(packages, targets []string, versions map[string]string, stateDir string, cleanupDeprecated bool) *models.InstallRequest {
	return &models.InstallRequest{
		Packages:          packages,
		Targets:           targets,
		Versions:          versions,
		Interactive:       false,
		StateDir:          stateDir,
		CleanupDeprecated: cleanupDeprecated,
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
   --advisor-model MODEL      Advisor model (e.g., anthropic/claude-opus-4-6).
   --cleanup-deprecated       Remove deprecated skynex-managed files (interactive install: prompts; update: flag only).
   --non-interactive          Skip prompts, require all inputs via flags.
   --yes, -y                  Skip confirmation prompt.
   --trust-setup-scripts      Trust external setup scripts.
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
