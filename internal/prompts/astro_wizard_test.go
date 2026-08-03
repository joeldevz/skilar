package prompts

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccessibleWizardCustomSessionCanCancelOrFinish(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	t.Setenv("NO_COLOR", "1")

	for _, test := range []struct {
		name        string
		input       string
		wantErr     error
		wantRequest bool
	}{
		{name: "cancel", input: "2\ny\ny\ny\ny\ny\nn\n", wantErr: ErrWizardCancelled},
		{name: "finish", input: "2\ny\ny\ny\ny\ny\ny\n", wantRequest: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			request, err := runWizardWithIO(WizardOptions{}, newAccessibleLineReader(test.input), &output)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v, want %v", err, test.wantErr)
			}
			if test.wantRequest {
				if err != nil || request == nil {
					t.Fatalf("request=%#v, error=%v; want completed request", request, err)
				}
				if len(request.Targets) != 1 || request.Targets[0] != "opencode" {
					t.Fatalf("targets=%v, want only OpenCode", request.Targets)
				}
			}
			for _, want := range []string{"↑/↓ move", "space toggle", "enter continue", "esc/back"} {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("rendered accessible session output missing %q: %q", want, output.String())
				}
			}
			if !strings.Contains(output.String(), environmentStatus()) {
				t.Fatalf("rendered accessible session output missing environment status: %q", output.String())
			}
			for _, unwanted := range []string{"Environments", "Choose where", "not selectable"} {
				if strings.Contains(output.String(), unwanted) {
					t.Fatalf("rendered accessible session output contains unwanted text %q: %q", unwanted, output.String())
				}
			}
		})
	}
}

func TestWizardShowsCompactEnvironmentStatusBeforeSetup(t *testing.T) {
	status := environmentStatus()
	if status != "✓ OpenCode\n  Claude Code  Coming soon" {
		t.Fatalf("status=%q, want compact environment status", status)
	}
	for _, unwanted := range []string{"Environments", "Choose where", "not selectable", "Claude Code — Coming soon"} {
		if strings.Contains(status, unwanted) {
			t.Fatalf("status contains unwanted text %q: %q", unwanted, status)
		}
	}
}

func TestWizardGroupHeadingsDoNotDuplicateSetupOrIntro(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	t.Setenv("NO_COLOR", "1")
	var output bytes.Buffer
	_, err := runWizardWithIO(WizardOptions{}, newAccessibleLineReader("1\ny\ny\ny\ny\ny\ny\n"), &output)
	if err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	if strings.Count(rendered, "Setup") != 1 {
		t.Fatalf("Setup heading count = %d, output=%q", strings.Count(rendered, "Setup"), rendered)
	}
	if strings.Count(rendered, "Launch sequence initiated.") != 1 || strings.Count(rendered, "Let's assemble your AI crew.") != 1 {
		t.Fatalf("intro was not limited to the first group: %q", rendered)
	}
}

func TestInstallerPromptsHaveNoLegacyBubbleTeaPath(t *testing.T) {
	source, err := os.ReadFile("prompts.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, legacy := range []string{"bubbletea", "tea.NewProgram", "multiSelectModel", "singleSelectModel"} {
		if strings.Contains(text, legacy) {
			t.Fatalf("legacy interactive renderer %q remains in prompts.go", legacy)
		}
	}
}

type accessibleLineReader struct {
	lines []string
	index int
}

func newAccessibleLineReader(input string) *accessibleLineReader {
	return &accessibleLineReader{lines: strings.Split(strings.TrimSuffix(input, "\n"), "\n")}
}

func (r *accessibleLineReader) Read(p []byte) (int, error) {
	if r.index >= len(r.lines) {
		return 0, io.EOF
	}
	line := r.lines[r.index] + "\n"
	r.index++
	return copy(p, line), nil
}

func TestWizardDefaultsAndSelection(t *testing.T) {
	defaults := NewWizardSelection()
	if defaults.Environment != "opencode" || defaults.Setup != "recommended" {
		t.Fatalf("unexpected defaults: %#v", defaults)
	}
	if len(defaults.Components) != len(optionalComponents) {
		t.Fatalf("components=%d, want %d", len(defaults.Components), len(optionalComponents))
	}
	if !selectionEnablesNeurox(defaults) {
		t.Fatal("recommended setup must enable Neurox")
	}
	if selectionEnablesNeurox(WizardSelection{Environment: "opencode", Setup: setupCustom, Components: []string{"official-skills"}}) {
		t.Fatal("custom setup without neurox-memory must disable Neurox")
	}
	if !selectionEnablesNeurox(WizardSelection{Environment: "opencode", Setup: setupCustom, Components: []string{"neurox-memory"}}) {
		t.Fatal("custom neurox-memory selection must enable Neurox")
	}

}

func TestWizardValidationAndSummary(t *testing.T) {
	if err := validateWizardSelection(WizardSelection{}); err == nil {
		t.Fatal("empty environment must be rejected")
	}
	if err := validateWizardSelection(WizardSelection{Environment: "claude", Setup: setupRecommended}); err == nil {
		t.Fatal("Claude must not be selectable")
	}
	selection := WizardSelection{
		Environment: "opencode",
		Setup:       "custom",
		Components:  []string{"official-skills", "neurox-memory"},
	}
	if err := validateWizardSelection(selection); err != nil {
		t.Fatal(err)
	}
	summary := reviewSummary(selection)
	for _, want := range []string{
		"Environment\n  OpenCode",
		"Setup\n  Custom",
		"Components\n  ✓ Agents and orchestration  Required\n  ✓ Official skills\n  ✓ Neurox memory",
		"Backup\n  Enabled",
		"Configuration\n  Existing settings preserved",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q: %s", want, summary)
		}
	}
	for _, unwanted := range []string{"official-skills", "neurox-memory", "Skynex plugins", "Context7 documentation"} {
		if strings.Contains(summary, unwanted) {
			t.Fatalf("summary contains unwanted component %q: %s", unwanted, summary)
		}
	}

	recommended := reviewSummary(NewWizardSelection())
	for _, want := range []string{"Setup\n  Recommended", "  ✓ Skynex plugins", "  ✓ Recommended model profile", "  ✓ Context7 documentation"} {
		if !strings.Contains(recommended, want) {
			t.Fatalf("recommended summary missing %q: %s", want, recommended)
		}
	}
	if strings.Contains(recommended, "[") || strings.Contains(recommended, "]") {
		t.Fatalf("recommended summary contains internal component IDs: %s", recommended)
	}
}

func TestWizardThemeAccessibilityAndNoColor(t *testing.T) {
	if !accessibleMode() {
		t.Log("ACCESSIBLE is unset by default")
	}
	t.Setenv("ACCESSIBLE", "1")
	if !accessibleMode() {
		t.Fatal("ACCESSIBLE=1 must enable accessible mode")
	}
	t.Setenv("NO_COLOR", "1")
	theme := wizardTheme(true).Theme(false)
	if strings.Contains(theme.Focused.SelectSelector.Render("x"), "\x1b[") {
		t.Fatal("NO_COLOR theme emitted ANSI")
	}
}

func TestWizardThemeUsesRadioGlyphsForSelectAndCheckboxesForMultiSelect(t *testing.T) {
	theme := wizardTheme(true).Theme(false)
	if got := theme.Focused.SelectSelector.String(); got != "◉ " {
		t.Fatalf("selected Select glyph = %q, want %q", got, "◉ ")
	}
	if got := theme.Blurred.SelectSelector.String(); got != "○ " {
		t.Fatalf("unselected Select glyph = %q, want %q", got, "○ ")
	}
	if got := theme.Focused.SelectedPrefix.String(); got == "◉ " || got == "○ " || !strings.Contains(got, "[") {
		t.Fatalf("MultiSelect selected prefix = %q, want checkbox marker", got)
	}
}

func TestWizardSelectionHelpIsVisibleAndAccessible(t *testing.T) {
	for _, want := range []string{"↑/↓ move", "space toggle", "enter continue", "esc/back"} {
		if !strings.Contains(customSelectionHelp(), want) {
			t.Fatalf("selection help missing %q: %q", want, customSelectionHelp())
		}
	}
	if got := customSelectionDescription(true); got != customSelectionHelp() {
		t.Fatalf("accessible description=%q, want %q", got, customSelectionHelp())
	}
}

func TestRunProgressAccessibleRunsActionBeforeReturningErrorWithoutTerminalControl(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	var output bytes.Buffer
	called := false
	errWant := errors.New("install failed")
	err := runProgressWithIO(false, &output, func() error {
		called = true
		return errWant
	})
	if !called {
		t.Fatal("install action was not run")
	}
	if !errors.Is(err, errWant) {
		t.Fatalf("error=%v, want %v", err, errWant)
	}
	if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), "^[[") {
		t.Fatalf("accessible progress emitted terminal control bytes: %q", output.String())
	}
}

func TestExistingInstall(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if ExistingInstall(state, filepath.Join(root, "opencode")) {
		t.Fatal("empty install reported as existing")
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "skills.lock.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ExistingInstall(state, filepath.Join(root, "opencode")) {
		t.Fatal("lock file not detected")
	}
}
