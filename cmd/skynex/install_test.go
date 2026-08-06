package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/installer"
	"github.com/joeldevz/skynex/internal/models"
	"github.com/joeldevz/skynex/internal/preflight"
	"github.com/joeldevz/skynex/internal/prompts"
)

func installTestDeps(events *[]string) installDependencies {
	return installDependencies{
		loadCatalog: func() (*models.Catalog, error) {
			*events = append(*events, "catalog")
			return &models.Catalog{Packages: map[string]*models.PackageDefinition{
				"pkg": {ID: "pkg", DefaultVersion: "latest", SupportedTargets: []string{"opencode"}},
			}}, nil
		},
		loadConfig: func(string) (map[string]interface{}, error) {
			*events = append(*events, "config")
			return map[string]interface{}{}, nil
		},
		wizard: func(prompts.WizardOptions) (*models.InstallRequest, error) {
			*events = append(*events, "wizard")
			return &models.InstallRequest{Interactive: true}, nil
		},
		preflight: func(*models.InstallRequest, *models.Catalog, preflight.Options) []*models.ValidationIssue {
			*events = append(*events, "preflight")
			return nil
		},
		apply: func(_ *installer.Plan, callback func() error) error {
			*events = append(*events, "apply")
			*events = append(*events, "apply-callback")
			return callback()
		},
		output: &strings.Builder{}, errorOutput: &strings.Builder{},
	}
}

func TestRunInstallManageBackupsPrunesToThreeAndContinues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	events := []string{}
	deps := installTestDeps(&events)
	deps.listSnapshots = func(string) ([]installer.Snapshot, error) {
		snapshots := make([]installer.Snapshot, 5)
		for i := range snapshots {
			snapshots[i] = installer.Snapshot{ID: "retained", CreatedAt: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)}
		}
		return snapshots, nil
	}
	deps.chooseBackupCapacity = func(string, bool) prompts.BackupCapacityChoice {
		return prompts.BackupManage
	}
	pruned := 0
	deps.pruneSnapshots = func(_ string, count int) (int, error) {
		pruned = count
		return count, nil
	}

	if err := runInstall(&cliArgs{StateDir: t.TempDir()}, deps); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"catalog", "config", "wizard", "preflight", "apply", "apply-callback"}) {
		t.Fatalf("events = %v", events)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2 (keep 3)", pruned)
	}
}

func TestRunInstallInteractiveSuccessAppliesInOrder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	events := []string{}
	deps := installTestDeps(&events)
	var appliedPlan *installer.Plan
	mutationCalled := false
	baseApply := deps.apply
	deps.apply = func(plan *installer.Plan, callback func() error) error {
		appliedPlan = plan
		return baseApply(plan, func() error { mutationCalled = true; return callback() })
	}
	if err := runInstall(&cliArgs{StateDir: t.TempDir(), Verbose: true}, deps); err != nil {
		t.Fatal(err)
	}
	want := []string{"catalog", "config", "wizard", "preflight", "apply", "apply-callback"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if appliedPlan == nil || len(appliedPlan.Operations) != 2 || appliedPlan.Operations[0].Kind != installer.WriteState || appliedPlan.Operations[1].Kind != installer.WriteState {
		t.Fatalf("unexpected applied plan: %#v", appliedPlan)
	}
	if !mutationCalled {
		t.Fatal("successful install did not execute its mutation callback")
	}
}

func TestRunInstallSuccessSummarySharedByInteractiveAndNonInteractive(t *testing.T) {
	for _, nonInteractive := range []bool{false, true} {
		t.Run(map[bool]string{false: "interactive", true: "noninteractive"}[nonInteractive], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			events := []string{}
			deps := installTestDeps(&events)
			args := &cliArgs{StateDir: t.TempDir(), NonInteractive: nonInteractive}
			if nonInteractive {
				args.Packages, args.Targets, args.Yes = []string{"pkg"}, []string{"opencode"}, true
				deps.apply = func(*installer.Plan, func() error) error { events = append(events, "apply"); return nil }
			}
			var output strings.Builder
			deps.output, deps.errorOutput = &output, &output
			if err := runInstall(args, deps); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "✓ Installation complete.") || !strings.Contains(output.String(), "Neurox:") || !strings.Contains(output.String(), "State files:") {
				t.Fatalf("summary=%q", output.String())
			}
		})
	}
}

func TestRunInstallWizardCancelDoesNotApply(t *testing.T) {
	events := []string{}
	deps := installTestDeps(&events)
	deps.wizard = func(prompts.WizardOptions) (*models.InstallRequest, error) {
		events = append(events, "wizard")
		return nil, prompts.ErrWizardCancelled
	}
	if err := runInstall(&cliArgs{StateDir: t.TempDir()}, deps); !errors.Is(err, prompts.ErrWizardCancelled) {
		t.Fatalf("error = %v", err)
	}
	if want := []string{"catalog", "config", "wizard"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunInstallNonInteractivePreflightErrorPreventsApply(t *testing.T) {
	events := []string{}
	deps := installTestDeps(&events)
	deps.preflight = func(*models.InstallRequest, *models.Catalog, preflight.Options) []*models.ValidationIssue {
		events = append(events, "preflight")
		return []*models.ValidationIssue{{Level: "error", Message: "blocked"}}
	}
	args := &cliArgs{Packages: []string{"pkg"}, Targets: []string{"opencode"}, NonInteractive: true, StateDir: t.TempDir()}
	if err := runInstall(args, deps); err == nil {
		t.Fatal("expected preflight error")
	}
	if want := []string{"catalog", "config", "preflight"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRunInstallApplyCallbackErrorPropagates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	events := []string{}
	deps := installTestDeps(&events)
	deps.wizard = func(prompts.WizardOptions) (*models.InstallRequest, error) {
		return &models.InstallRequest{Packages: []string{"pkg"}, Targets: []string{"opencode"}, Versions: map[string]string{"pkg": "latest"}, Interactive: true}, nil
	}
	deps.loadCatalog = func() (*models.Catalog, error) {
		return &models.Catalog{Packages: map[string]*models.PackageDefinition{
			"pkg": {ID: "pkg", Adapter: "injected-error", DefaultVersion: "latest", SupportedTargets: []string{"opencode"}},
		}}, nil
	}
	err := runInstall(&cliArgs{StateDir: t.TempDir(), Verbose: true}, deps)
	if err == nil || !strings.Contains(err.Error(), "unknown adapter: injected-error") {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(events, []string{"config", "preflight", "apply", "apply-callback"}) {
		t.Fatalf("events = %v", events)
	}
	if output, ok := deps.output.(*strings.Builder); ok && strings.Contains(output.String(), "Installation complete") {
		t.Fatalf("failure printed success: %q", output.String())
	}
}

func TestInstallSummaryStableSuccessNoopNeuroxAndSnapshot(t *testing.T) {
	plan := &installer.Plan{Version: 1, Operations: []installer.Operation{
		{Kind: installer.WriteState, Destination: "/state/z.lock"},
		{Kind: installer.InstallTarget, PackageID: "zeta", Target: "opencode", Destination: "/open"},
		{Kind: installer.InstallTarget, PackageID: "alpha", Target: "claude", Destination: "/claude"},
		{Kind: installer.WriteState, Destination: "/state/a.json"},
		{Kind: installer.CleanupDeprecated, Destination: "/state"},
	}}
	results := []*models.InstallResult{
		{PackageID: "zeta", ResolvedVersion: "2", Targets: map[string]*models.TargetResult{"opencode": {Status: "installed", Artifacts: []string{"/open/z", "/open/a"}}}},
		{PackageID: "alpha", ResolvedVersion: "1", Targets: map[string]*models.TargetResult{"claude": {Status: "installed", Artifacts: []string{"/claude"}}}},
	}
	request := &models.InstallRequest{NeuroxSelectionSet: true, NeuroxEnabled: true}
	issues := []*models.ValidationIssue{{Level: "warning", Message: "z warning"}, {Level: "warning", Message: "a warning"}}
	var first, second strings.Builder
	snapshot := &installer.Snapshot{ID: "snap-123"}
	if err := renderInstallSummary(&first, request, plan, results, issues, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := renderInstallSummary(&second, request, plan, results, issues, snapshot); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatal("summary is nondeterministic")
	}
	got := first.String()
	for _, want := range []string{"✓ Installation complete.", "alpha @ 1", "zeta @ 2", "claude -> /claude", "opencode -> /open", "artifact: /open/a", "Neurox: enabled", "/state/a.json", "Cleanup: applied", "a warning", "Recovery snapshot: snap-123"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: %s", want, got)
		}
	}
	if strings.Index(got, "alpha @ 1") > strings.Index(got, "zeta @ 2") || strings.Index(got, "a warning") > strings.Index(got, "z warning") {
		t.Fatalf("summary not sorted: %s", got)
	}

	unchanged := []*models.InstallResult{{PackageID: "alpha", ResolvedVersion: "1", Targets: map[string]*models.TargetResult{"claude": {Status: "unchanged"}}}}
	var noop strings.Builder
	if err := renderInstallSummary(&noop, &models.InstallRequest{}, plan, unchanged, nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(noop.String(), "Installation complete") || !strings.Contains(noop.String(), "Nothing changed") || !strings.Contains(noop.String(), "Neurox: preserved") {
		t.Fatalf("bad no-op summary: %s", noop.String())
	}
}

func TestRunInstallExactCurrentSkipsWizardAndApply(t *testing.T) {
	events := []string{}
	deps := installTestDeps(&events)
	deps.exactCurrent = func(string, string) bool {
		events = append(events, "current")
		return true
	}
	deps.wizard = func(prompts.WizardOptions) (*models.InstallRequest, error) {
		events = append(events, "wizard")
		return nil, errors.New("wizard must not open")
	}
	deps.apply = func(*installer.Plan, func() error) error {
		events = append(events, "apply")
		return nil
	}
	var output strings.Builder
	deps.output, deps.errorOutput = &output, &output
	if err := runInstall(&cliArgs{StateDir: t.TempDir()}, deps); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"catalog", "config", "current"}) {
		t.Fatalf("events = %v", events)
	}
	if got := output.String(); got != "✓ Skynex is up to date.\n  Nothing to install.\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestRunInstallNonInteractiveExactCurrentSkipsSnapshotAndApply(t *testing.T) {
	events := []string{}
	deps := installTestDeps(&events)
	deps.exactRequestCurrent = func(string, string, *models.InstallRequest) bool {
		events = append(events, "current")
		return true
	}
	deps.preflight = func(*models.InstallRequest, *models.Catalog, preflight.Options) []*models.ValidationIssue {
		events = append(events, "preflight")
		return nil
	}
	deps.listSnapshots = func(string) ([]installer.Snapshot, error) {
		events = append(events, "snapshots")
		return nil, nil
	}
	deps.apply = func(*installer.Plan, func() error) error {
		events = append(events, "apply")
		return nil
	}
	var output strings.Builder
	deps.output, deps.errorOutput = &output, &output
	args := &cliArgs{
		StateDir:       t.TempDir(),
		NonInteractive: true,
		Packages:       []string{"pkg"},
		Targets:        []string{"opencode"},
		Versions:       map[string]string{"pkg": "latest"},
		Yes:            true,
	}
	if err := runInstall(args, deps); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"catalog", "config", "current"}) {
		t.Fatalf("events = %v", events)
	}
	if got := output.String(); got != "✓ Skynex is up to date.\n  Nothing changed.\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestRunInstallForceSkipsExactCurrentGate(t *testing.T) {
	events := []string{}
	deps := installTestDeps(&events)
	deps.exactCurrent = func(string, string) bool {
		events = append(events, "current")
		return true
	}
	if err := runInstall(&cliArgs{StateDir: t.TempDir(), Force: true}, deps); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"catalog", "config", "wizard", "preflight", "apply", "apply-callback"}) {
		t.Fatalf("events = %v", events)
	}
}

func TestNeuroxFlagsResolveExplicitAndRecommendedDefaults(t *testing.T) {
	cat := &models.Catalog{Packages: map[string]*models.PackageDefinition{"skills": {ID: "skills", DefaultVersion: "latest"}}}
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{"recommended default", []string{"install", "--package", "skills", "--target", "opencode", "--non-interactive"}, true},
		{"explicit on", []string{"install", "--package", "skills", "--target", "opencode", "--non-interactive", "--with-neurox"}, true},
		{"explicit off", []string{"install", "--package", "skills", "--target", "opencode", "--non-interactive", "--without-neurox"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := parseArgsFrom(tc.argv)
			req, err := resolveNonInteractive(args, cat, map[string]interface{}{})
			if err != nil {
				t.Fatal(err)
			}
			if req.NeuroxEnabled != tc.want {
				t.Fatalf("NeuroxEnabled=%v want %v", req.NeuroxEnabled, tc.want)
			}
		})
	}
	if args := parseArgsFrom([]string{"install", "--with-neurox", "--without-neurox"}); args.ParseError == "" {
		t.Fatal("conflicting Neurox flags must fail")
	}
}

func TestUpdateRequestDefaultsToRecommendedNeurox(t *testing.T) {
	req := newUpdateInstallRequest([]string{"skills"}, []string{"opencode"}, map[string]string{"skills": "latest"}, t.TempDir(), false)
	if !req.NeuroxEnabled || !req.NeuroxSelectionSet {
		t.Fatalf("update request lost recommended Neurox default: %#v", req)
	}
}
