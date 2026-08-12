package prompts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	huhspinner "charm.land/huh/v2/spinner"
	huhlipgloss "charm.land/lipgloss/v2"
	"github.com/joeldevz/skynex/internal/models"
)

// ErrWizardCancelled means the user left before any install operation ran.
var ErrWizardCancelled = errors.New("installation cancelled")

const (
	setupRecommended = "recommended"
	setupCustom      = "custom"
)

var optionalComponents = []string{
	"official-skills",
	"skynex-plugins",
	"recommended-model-profile",
	"neurox-memory",
	"context7-documentation",
}

var componentLabels = map[string]string{
	"official-skills":           "Official skills",
	"skynex-plugins":            "Skynex plugins",
	"recommended-model-profile": "Recommended model profile",
	"neurox-memory":             "Neurox memory",
	"context7-documentation":    "Context7 documentation",
}

// WizardOptions controls the presentation without changing the install contract.
type WizardOptions struct {
	Existing bool
	Verbose  bool
	Input    io.Reader
	Output   io.Writer
}

// WizardSelection is the structured result collected by the Huh form.
// Components are currently recorded as bundle metadata: the installer backend
// only has granular support for the skills package and OpenCode target.
type WizardSelection struct {
	Environment string
	Setup       string
	Components  []string
}

func selectionEnablesNeurox(selection WizardSelection) bool {
	if selection.Setup == setupRecommended {
		return true
	}
	for _, component := range selection.Components {
		if component == "neurox-memory" {
			return true
		}
	}
	return false
}

func NewWizardSelection() WizardSelection {
	return WizardSelection{
		Environment: "opencode",
		Setup:       setupRecommended,
		Components:  append([]string(nil), optionalComponents...),
	}
}

func environmentStatus() string {
	return "✓ OpenCode\n  Claude Code  Coming soon"
}

func validateWizardSelection(selection WizardSelection) error {
	if selection.Environment != "opencode" {
		return fmt.Errorf("unsupported environment: %q", selection.Environment)
	}
	if selection.Setup != setupRecommended && selection.Setup != setupCustom {
		return fmt.Errorf("unsupported setup: %q", selection.Setup)
	}
	valid := make(map[string]struct{}, len(optionalComponents))
	for _, component := range optionalComponents {
		valid[component] = struct{}{}
	}
	for _, component := range selection.Components {
		if _, ok := valid[component]; !ok {
			return fmt.Errorf("unsupported component: %q", component)
		}
	}
	return nil
}

func componentSummary(selection WizardSelection) string {
	selected := make(map[string]struct{}, len(selection.Components))
	for _, component := range selection.Components {
		selected[component] = struct{}{}
	}

	lines := []string{"  ✓ Agents and orchestration  Required"}
	for _, component := range optionalComponents {
		if _, ok := selected[component]; ok {
			lines = append(lines, "  ✓ "+componentLabels[component])
		}
	}
	return strings.Join(lines, "\n")
}

func reviewSummary(selection WizardSelection) string {
	setup := "Recommended"
	if selection.Setup == setupCustom {
		setup = "Custom"
	}
	return fmt.Sprintf("Environment\n  OpenCode\n\nSetup\n  %s\n\nComponents\n%s\n\nBackup\n  Enabled\n\nConfiguration\n  Existing settings preserved", setup, componentSummary(selection))
}

func accessibleMode() bool {
	value, ok := os.LookupEnv("ACCESSIBLE")
	if !ok {
		return false
	}
	enabled, err := strconv.ParseBool(value)
	return err == nil && enabled
}

func noColor() bool {
	_, ok := os.LookupEnv("NO_COLOR")
	return ok
}

const customSelectionHelpText = "↑/↓ move · space toggle · enter continue · esc/back"

func customSelectionHelp() string {
	return customSelectionHelpText
}

func customSelectionDescription(accessible bool) string {
	if accessible {
		return customSelectionHelp()
	}
	return customSelectionHelp() + " · " + strconv.Itoa(len(optionalComponents)) + " options"
}

func wizardKeyMap() *huh.KeyMap {
	keyMap := huh.NewDefaultKeyMap()
	keyMap.MultiSelect.Toggle.SetHelp("space", "toggle")
	return keyMap
}

func wizardTheme(noColorEnabled bool) huh.ThemeFunc {
	return func(isDark bool) *huh.Styles {
		styles := huh.ThemeBase(isDark)
		styles.Focused.SelectSelector = huhlipgloss.NewStyle().SetString("◉ ")
		styles.Blurred.SelectSelector = huhlipgloss.NewStyle().SetString("○ ")
		if noColorEnabled {
			return styles
		}
		purple := huhlipgloss.Color("#B983FF")
		cyan := huhlipgloss.Color("#67E8F9")
		green := huhlipgloss.Color("#86EFAC")
		styles.Group.Title = huhlipgloss.NewStyle().Foreground(purple).Bold(true)
		styles.Focused.SelectSelector = styles.Focused.SelectSelector.Foreground(cyan)
		styles.Blurred.SelectSelector = styles.Blurred.SelectSelector.Foreground(cyan)
		styles.Focused.MultiSelectSelector = huhlipgloss.NewStyle().Foreground(cyan).SetString("> ")
		styles.Focused.SelectedOption = huhlipgloss.NewStyle().Foreground(green)
		styles.Focused.SelectedPrefix = huhlipgloss.NewStyle().Foreground(green).SetString("[•] ")
		return styles
	}
}

func spinnerTheme(noColorEnabled bool) huhspinner.ThemeFunc {
	return func(bool) *huhspinner.Styles {
		styles := &huhspinner.Styles{}
		if !noColorEnabled {
			styles.Spinner = huhlipgloss.NewStyle().Foreground(huhlipgloss.Color("#86EFAC"))
			styles.Title = huhlipgloss.NewStyle().Foreground(huhlipgloss.Color("#67E8F9"))
		}
		return styles
	}
}

func brandHeader(existing bool) string {
	if noColor() {
		if existing {
			return "skynex  Update & repair\nLaunch sequence initiated.\nLet's assemble your AI crew."
		}
		return "skynex\nLaunch sequence initiated.\nLet's assemble your AI crew."
	}
	name := huhlipgloss.NewStyle().Bold(true).Foreground(huhlipgloss.Color("#B983FF")).Render("skynex")
	if existing {
		name += "  " + huhlipgloss.NewStyle().Foreground(huhlipgloss.Color("#FDE68A")).Render("Update & repair")
	}
	return name + "\n" + huhlipgloss.NewStyle().Foreground(huhlipgloss.Color("#67E8F9")).Render("Launch sequence initiated.") + "\nLet's assemble your AI crew."
}

// RunWizard collects the install decision and performs no installation itself.
func RunWizard(options WizardOptions) (*models.InstallRequest, error) {
	input, output := options.Input, options.Output
	if input == nil {
		input = os.Stdin
	}
	if output == nil {
		output = os.Stderr
	}
	return runWizardWithIO(options, input, output)
}

func RunWizardWithIO(options WizardOptions, input io.Reader, output io.Writer) (*models.InstallRequest, error) {
	options.Input, options.Output = input, output
	return RunWizard(options)
}

func runWizardWithIO(options WizardOptions, input io.Reader, output io.Writer) (*models.InstallRequest, error) {
	selection := NewWizardSelection()
	components := append([]string(nil), selection.Components...)

	setup := selection.Setup
	setupField := huh.NewSelect[string]().
		Title("Setup").
		Options(
			huh.NewOption("Recommended default", setupRecommended),
			huh.NewOption("Custom", setupCustom),
		).
		Value(&setup)

	customFields := make([]huh.Field, 0, len(optionalComponents))
	optionalChoices := make(map[string]*bool, len(optionalComponents))
	if accessibleMode() {
		for _, component := range optionalComponents {
			selected := true
			optionalChoices[component] = &selected
			customFields = append(customFields, huh.NewConfirm().
				Title(componentLabels[component]).
				Affirmative("Include").
				Negative("Skip").
				Value(&selected))
		}
	} else {
		customOptions := make([]huh.Option[string], 0, len(optionalComponents))
		for _, component := range optionalComponents {
			customOptions = append(customOptions, huh.NewOption(componentLabels[component], component).Selected(true))
		}
		customFields = append(customFields, huh.NewMultiSelect[string]().
			Title("Optional components").
			Description(customSelectionDescription(false)).
			Options(customOptions...).
			Value(&components))
	}

	confirmed := false
	review := huh.NewNote().
		TitleFunc(func() string {
			return reviewSummary(WizardSelection{Environment: "opencode", Setup: setup, Components: components})
		}, nil)
	confirm := huh.NewConfirm().
		Title("Continue with this plan?").
		Affirmative("Continue").
		Negative("Cancel").
		Value(&confirmed)

	customGroupFields := append([]huh.Field{huh.NewNote().
		Title("Agents and orchestration — Required").
		Description(customSelectionDescription(accessibleMode()))}, customFields...)
	groups := []*huh.Group{
		huh.NewGroup(huh.NewNote().Title(brandHeader(options.Existing)), huh.NewNote().Title(environmentStatus())),
		huh.NewGroup(setupField),
		huh.NewGroup(customGroupFields...).Title("Custom").WithHideFunc(func() bool { return setup != setupCustom }),
		huh.NewGroup(review, confirm).Title("Review"),
	}

	form := huh.NewForm(groups...).
		WithTheme(wizardTheme(noColor())).
		WithKeyMap(wizardKeyMap()).
		WithAccessible(accessibleMode()).
		WithInput(input).
		WithOutput(output).
		WithShowHelp(true)
	if err := form.Run(); err != nil {
		return nil, fmt.Errorf("wizard: %w", err)
	}
	if !confirmed {
		return nil, ErrWizardCancelled
	}

	if accessibleMode() {
		components = components[:0]
		for _, component := range optionalComponents {
			if *optionalChoices[component] {
				components = append(components, component)
			}
		}
	}
	selection = WizardSelection{Environment: "opencode", Setup: setup, Components: components}
	if err := validateWizardSelection(selection); err != nil {
		return nil, fmt.Errorf("wizard selection: %w", err)
	}
	return &models.InstallRequest{
		Packages: []string{"skills"}, Targets: []string{"opencode"},
		Versions: map[string]string{"skills": "latest"}, Interactive: true,
		NeuroxEnabled: selectionEnablesNeurox(selection), NeuroxSelectionSet: true,
	}, nil
}

// RunProgress uses Huh's official spinner for real installation work.
func RunProgress(verbose bool, action func() error) error {
	return runProgressWithIO(verbose, os.Stderr, action)
}

// RunProgressWithIO binds progress output to the caller's stream.
func RunProgressWithIO(verbose bool, output io.Writer, action func() error) error {
	return runProgressWithIO(verbose, output, action)
}

func runProgressWithIO(verbose bool, output io.Writer, action func() error) error {
	if verbose || accessibleMode() {
		return action()
	}
	return huhspinner.New().
		Title("Installing OpenCode crew...").
		WithAccessible(accessibleMode()).
		WithTheme(spinnerTheme(noColor())).
		WithOutput(output).
		ActionWithErr(func(context.Context) error { return action() }).
		Run()
}

func ExistingInstall(stateDir, opencodeDir string) bool {
	for _, path := range []string{
		filepath.Join(stateDir, "skills.config.json"),
		filepath.Join(stateDir, "skills.lock.json"),
		filepath.Join(opencodeDir, "opencode.json"),
	} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}
