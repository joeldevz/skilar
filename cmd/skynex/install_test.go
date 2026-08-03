package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

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
