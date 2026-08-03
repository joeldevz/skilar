package prompts

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------------------
// Styles shared by the legacy profile form.
// ---------------------------------------------------------------------------

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	activeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	groupStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	checkStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	keyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Bold(true)
)

// shortModel strips the provider prefix: "anthropic/claude-haiku-4-5" → "claude-haiku-4-5"
func shortModel(id string) string {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return id
}

// helpBar renders a consistent help bar at the bottom.
// Keys are passed as alternating key/description pairs.
func helpBar(keys ...string) string {
	parts := make([]string, 0, len(keys)/2)
	for i := 0; i < len(keys)-1; i += 2 {
		k := keyStyle.Render(keys[i])
		v := dimStyle.Render(keys[i+1])
		parts = append(parts, k+" "+v)
	}
	return "\n" + strings.Join(parts, dimStyle.Render("   "))
}

// ---------------------------------------------------------------------------
// Domain types preserved from the old implementation
// ---------------------------------------------------------------------------

// ConfigMode distinguishes simple vs advanced model assignment.
type ConfigMode int

const (
	ModeSimple   ConfigMode = iota
	ModeAdvanced ConfigMode = iota
)

type simpleGroup struct {
	key    string   // display name / map key
	desc   string   // what agents it affects
	agents []string // agent names in this group
}

var simpleGroups = []simpleGroup{
	{
		key:    "Orchestrator",
		desc:   "Plans, coordinates and delegates all work",
		agents: []string{"orchestrator"},
	},
	{
		key:    "Workers",
		desc:   "Execute tasks: plan, code, verify, review",
		agents: []string{"tech-planner", "product-planner", "coder", "verifier", "test-reviewer", "security", "skill-validator"},
	},
}

type agentConfig struct {
	name  string
	model string // full ID or ""
}

var agentDescriptions = map[string]string{
	"orchestrator":    "Coordinates all agents, decides strategy",
	"tech-planner":    "Writes PLAN.md with technical steps",
	"product-planner": "Writes SPEC.md with business context",
	"coder":           "Implements code changes",
	"verifier":        "Runs lint, build, tests",
	"test-reviewer":   "Reviews test quality",
	"security":        "Adversarial security judge",
	"skill-validator": "Validates code conventions",
}

var agentList = []string{
	"orchestrator", "tech-planner", "product-planner",
	"test-reviewer", "security", "skill-validator",
}

// ---------------------------------------------------------------------------
// Wizard state enum
// ---------------------------------------------------------------------------

type wizardState int

const (
	stateNameInput         wizardState = iota
	stateModeSelect                    // choose Simple or Advanced
	stateSimpleModelPick               // list of groups, press enter to pick model
	stateAdvancedModelPick             // list of agents, press enter to pick model
	stateProviderSelect                // sub-state: choose provider
	stateModelSelect                   // sub-state: choose model within provider
	stateSummary                       // review and confirm
)

// ---------------------------------------------------------------------------
// profileWizard — the single tea.Model that drives the whole flow
// ---------------------------------------------------------------------------

type profileWizard struct {
	state     wizardState
	prevState wizardState // where to return after provider/model selection

	// --- name input ---
	textInput textinput.Model
	nameErr   string

	// --- mode select ---
	modeCursor int
	mode       ConfigMode

	// --- simple model picker ---
	simpleModels map[string]string // groupKey -> modelID
	simpleCursor int

	// --- advanced model picker ---
	agents    []agentConfig
	advCursor int

	// --- provider/model sub-picker ---
	pickerContext    string // what we're picking for ("Orchestrator", "coder", etc.)
	providers        []string
	provCursor       int
	provOffset       int
	allModels        map[string][]string
	models           []string // models for the selected provider
	modelCursor      int
	modelOffset      int
	selectedProvider string

	// --- summary ---
	confirmed bool
	cancelled bool

	// --- terminal size ---
	width  int
	height int
}

func newProfileWizard(initialModels map[string]string) profileWizard {
	ti := textinput.New()
	ti.Placeholder = "e.g. backend, front-v2"
	ti.Focus()
	ti.CharLimit = 32
	ti.Width = 30

	allModels, _ := LoadOpencodeModels()

	// Build agents list from initialModels.
	agents := make([]agentConfig, len(agentList))
	for i, name := range agentList {
		var model string
		if initialModels != nil {
			model = initialModels[name]
		}
		agents[i] = agentConfig{name: name, model: model}
	}

	simpleModels := make(map[string]string)
	if initialModels != nil {
		// Pre-fill simple groups from initial models using the first agent of each group.
		for _, g := range simpleGroups {
			if len(g.agents) > 0 {
				if m, ok := initialModels[g.agents[0]]; ok {
					simpleModels[g.key] = m
				}
			}
		}
	}

	return profileWizard{
		state:        stateNameInput,
		textInput:    ti,
		allModels:    allModels,
		agents:       agents,
		simpleModels: simpleModels,
	}
}

// ---------------------------------------------------------------------------
// tea.Model interface
// ---------------------------------------------------------------------------

func (m profileWizard) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, tea.WindowSize())
}

func (m profileWizard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Always handle terminal resize.
	if wm, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = wm.Width
		m.height = wm.Height
		return m, nil
	}

	switch m.state {
	case stateNameInput:
		return m.updateNameInput(msg)
	case stateModeSelect:
		return m.updateModeSelect(msg)
	case stateSimpleModelPick:
		return m.updateSimplePick(msg)
	case stateAdvancedModelPick:
		return m.updateAdvancedPick(msg)
	case stateProviderSelect:
		return m.updateProviderSelect(msg)
	case stateModelSelect:
		return m.updateModelSelect(msg)
	case stateSummary:
		return m.updateSummary(msg)
	}
	return m, nil
}

func (m profileWizard) View() string {
	switch m.state {
	case stateNameInput:
		return m.viewNameInput()
	case stateModeSelect:
		return m.viewModeSelect()
	case stateSimpleModelPick:
		return m.viewSimplePick()
	case stateAdvancedModelPick:
		return m.viewAdvancedPick()
	case stateProviderSelect:
		return m.viewProviderSelect()
	case stateModelSelect:
		return m.viewModelSelect()
	case stateSummary:
		return m.viewSummary()
	}
	return ""
}

// ---------------------------------------------------------------------------
// State: stateNameInput
// ---------------------------------------------------------------------------

func (m profileWizard) updateNameInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			name := strings.TrimSpace(m.textInput.Value())
			if err := validateProfileName(name); err != nil {
				m.nameErr = err.Error()
				return m, nil
			}
			m.state = stateModeSelect
			return m, nil
		case "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	// Live validation.
	name := strings.TrimSpace(m.textInput.Value())
	if name != "" {
		if err := validateProfileName(name); err != nil {
			m.nameErr = err.Error()
		} else {
			m.nameErr = ""
		}
	} else {
		m.nameErr = ""
	}
	return m, cmd
}

func (m profileWizard) viewNameInput() string {
	s := "\n"
	s += titleStyle.Render("  New Profile") + "\n"
	s += dimStyle.Render("  ──────────────────────────────") + "\n\n"
	s += fmt.Sprintf("  Profile name: %s\n\n", m.textInput.View())
	if m.nameErr != "" {
		s += "  " + errorStyle.Render("✗ "+m.nameErr) + "\n"
	} else {
		s += "  " + dimStyle.Render("lowercase letters, numbers and hyphens only") + "\n"
	}
	s += helpBar("enter", "next", "esc", "cancel")
	return s
}

// ---------------------------------------------------------------------------
// State: stateModeSelect
// ---------------------------------------------------------------------------

func (m profileWizard) updateModeSelect(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.modeCursor > 0 {
				m.modeCursor--
			}
		case "down", "j":
			if m.modeCursor < 1 {
				m.modeCursor++
			}
		case "enter":
			if m.modeCursor == 1 {
				m.mode = ModeAdvanced
				m.state = stateAdvancedModelPick
			} else {
				m.mode = ModeSimple
				m.state = stateSimpleModelPick
			}
			return m, nil
		case "esc":
			m.state = stateNameInput
			return m, nil
		case "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m profileWizard) viewModeSelect() string {
	s := "\n"
	s += titleStyle.Render("  Configure Models") + "\n"
	s += dimStyle.Render("  ──────────────────────────────") + "\n\n"

	options := []struct {
		label string
		desc  string
		hint  string
	}{
		{"Simple", "Set one model per role", "Orchestrator · Workers"},
		{"Advanced", "Set model per agent individually", "10 agents: orchestrator, coder, tech-planner..."},
	}

	for i, opt := range options {
		indicator := "  ○"
		labelStyle := dimStyle
		if i == m.modeCursor {
			indicator = "  ●"
			labelStyle = activeStyle
		}
		s += labelStyle.Render(fmt.Sprintf("%s  %s", indicator, opt.label)) + "\n"
		s += dimStyle.Render(fmt.Sprintf("     %s", opt.desc)) + "\n"
		s += dimStyle.Render(fmt.Sprintf("     %s", opt.hint)) + "\n\n"
	}

	s += helpBar("↑↓", "navigate", "enter", "select", "esc", "back")
	return s
}

// ---------------------------------------------------------------------------
// State: stateSimpleModelPick
// ---------------------------------------------------------------------------

func (m profileWizard) updateSimplePick(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.simpleCursor > 0 {
				m.simpleCursor--
			}
		case "down", "j":
			if m.simpleCursor < len(simpleGroups)-1 {
				m.simpleCursor++
			}
		case "enter":
			g := simpleGroups[m.simpleCursor]
			m.pickerContext = g.key
			m.prevState = stateSimpleModelPick
			m = m.initProviderSelect()
			return m, nil
		case "s":
			// Set all groups to the same model.
			m.pickerContext = "all groups"
			m.prevState = stateSimpleModelPick
			m = m.initProviderSelectForAll()
			return m, nil
		case "c":
			m.state = stateSummary
			return m, nil
		case "esc":
			m.state = stateModeSelect
			return m, nil
		case "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m profileWizard) viewSimplePick() string {
	configured := 0
	for _, g := range simpleGroups {
		if m.simpleModels[g.key] != "" {
			configured++
		}
	}
	total := len(simpleGroups)

	s := "\n"
	s += titleStyle.Render("  Model Setup — Simple") + "\n"
	if configured == total {
		s += checkStyle.Render(fmt.Sprintf("  ✓ All %d roles configured — press c to save", total)) + "\n"
	} else {
		s += warnStyle.Render(fmt.Sprintf("  %d/%d roles configured", configured, total)) + "\n"
	}
	s += dimStyle.Render("  ──────────────────────────────") + "\n\n"

	for i, g := range simpleGroups {
		isSelected := i == m.simpleCursor
		modelID := m.simpleModels[g.key]

		cursor := "  "
		labelStyle := dimStyle
		if isSelected {
			cursor = "▶ "
			labelStyle = activeStyle
		}
		s += labelStyle.Render(fmt.Sprintf("%s%s", cursor, g.key)) + "\n"
		s += dimStyle.Render(fmt.Sprintf("     %s", g.desc)) + "\n"
		s += dimStyle.Render(fmt.Sprintf("     Agents: %s", strings.Join(g.agents, ", "))) + "\n"

		if modelID == "" {
			s += "     " + warnStyle.Render("⚠  not set — press enter to pick a model") + "\n"
		} else {
			s += "     " + checkStyle.Render("✓  "+shortModel(modelID)) + "  " + dimStyle.Render("("+modelID+")") + "\n"
		}
		s += "\n"
	}

	s += helpBar(
		"enter", "pick model for selected role",
		"s", "set all to same model",
		"c", "save profile",
		"esc", "back",
	)
	return s
}

// ---------------------------------------------------------------------------
// State: stateAdvancedModelPick
// ---------------------------------------------------------------------------

func (m profileWizard) updateAdvancedPick(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.advCursor > 0 {
				m.advCursor--
			}
		case "down", "j":
			if m.advCursor < len(m.agents)-1 {
				m.advCursor++
			}
		case "enter":
			agent := m.agents[m.advCursor]
			m.pickerContext = agent.name
			m.prevState = stateAdvancedModelPick
			m = m.initProviderSelect()
			return m, nil
		case "s":
			m.pickerContext = "all agents"
			m.prevState = stateAdvancedModelPick
			m = m.initProviderSelectForAll()
			return m, nil
		case "c":
			m.state = stateSummary
			return m, nil
		case "esc":
			m.state = stateModeSelect
			return m, nil
		case "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m profileWizard) viewAdvancedPick() string {
	configured := 0
	for _, a := range m.agents {
		if a.model != "" {
			configured++
		}
	}
	total := len(m.agents)

	s := "\n"
	s += titleStyle.Render("  Model Setup — Advanced") + "\n"
	if configured == total {
		s += checkStyle.Render(fmt.Sprintf("  ✓ All %d agents configured — press c to save", total)) + "\n"
	} else {
		s += warnStyle.Render(fmt.Sprintf("  %d/%d agents configured", configured, total)) + "\n"
	}
	s += dimStyle.Render("  ──────────────────────────────") + "\n\n"

	for i, ac := range m.agents {
		isSelected := i == m.advCursor
		cursor := "  "
		nameStyle := dimStyle
		if isSelected {
			cursor = "▶ "
			nameStyle = activeStyle
		}
		desc := agentDescriptions[ac.name]
		s += nameStyle.Render(fmt.Sprintf("%s%-18s", cursor, ac.name))
		s += dimStyle.Render(desc) + "\n"
		if ac.model == "" {
			s += "     " + warnStyle.Render("⚠  not set") + "\n"
		} else {
			s += "     " + checkStyle.Render("✓  "+shortModel(ac.model)) + "\n"
		}
	}

	s += helpBar(
		"enter", "pick model",
		"s", "set all",
		"c", "save",
		"esc", "back",
	)
	return s
}

// ---------------------------------------------------------------------------
// State: stateProviderSelect
// ---------------------------------------------------------------------------

// initProviderSelect transitions to provider selection for the currently
// set pickerContext (single group/agent).
func (m profileWizard) initProviderSelect() profileWizard {
	providers := make([]string, 0, len(m.allModels))
	for p := range m.allModels {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	m.providers = providers
	m.provCursor = 0
	m.provOffset = 0
	m.state = stateProviderSelect
	return m
}

// initProviderSelectForAll is the same but signals "all" via pickerContext.
func (m profileWizard) initProviderSelectForAll() profileWizard {
	return m.initProviderSelect()
}

func (m profileWizard) updateProviderSelect(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.provCursor > 0 {
				m.provCursor--
				if m.provCursor < m.provOffset {
					m.provOffset = m.provCursor
				}
			}
		case "down", "j":
			if m.provCursor < len(m.providers)-1 {
				m.provCursor++
				if m.provCursor >= m.provOffset+maxVisible {
					m.provOffset = m.provCursor - maxVisible + 1
				}
			}
		case "enter":
			m.selectedProvider = m.providers[m.provCursor]
			m.models = m.allModels[m.selectedProvider]
			m.modelCursor = 0
			m.modelOffset = 0
			m.state = stateModelSelect
			return m, nil
		case "esc", "q":
			m.state = m.prevState
			return m, nil
		case "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m profileWizard) viewProviderSelect() string {
	s := "\n"
	s += titleStyle.Render("  Select Provider") + "\n"
	s += groupStyle.Render("  for: "+m.pickerContext) + "\n"
	s += dimStyle.Render("  ──────────────────────────────") + "\n\n"

	start := m.provOffset
	end := m.provOffset + maxVisible
	if end > len(m.providers) {
		end = len(m.providers)
	}

	if start > 0 {
		s += dimStyle.Render(fmt.Sprintf("  ↑ %d more\n", start))
	}

	for i := start; i < end; i++ {
		p := m.providers[i]
		count := len(m.allModels[p])
		cursor := "  "
		style := dimStyle
		if i == m.provCursor {
			cursor = "▶ "
			style = activeStyle
		}
		s += style.Render(fmt.Sprintf("%s%-20s %s", cursor, p, dimStyle.Render(fmt.Sprintf("(%d models)", count)))) + "\n"
	}

	if end < len(m.providers) {
		s += dimStyle.Render(fmt.Sprintf("  ↓ %d more\n", len(m.providers)-end))
	}

	s += helpBar("↑↓", "navigate", "enter", "select", "esc", "back")
	return s
}

// ---------------------------------------------------------------------------
// State: stateModelSelect
// ---------------------------------------------------------------------------

func (m profileWizard) updateModelSelect(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.modelCursor > 0 {
				m.modelCursor--
				if m.modelCursor < m.modelOffset {
					m.modelOffset = m.modelCursor
				}
			}
		case "down", "j":
			if m.modelCursor < len(m.models)-1 {
				m.modelCursor++
				if m.modelCursor >= m.modelOffset+maxVisible {
					m.modelOffset = m.modelCursor - maxVisible + 1
				}
			}
		case "enter":
			chosen := m.models[m.modelCursor]
			m = m.applyModelSelection(chosen)
			m.state = m.prevState
			return m, nil
		case "esc", "q":
			// Back to provider select.
			m.state = stateProviderSelect
			return m, nil
		case "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

// applyModelSelection writes the chosen model ID back to the appropriate slot(s).
func (m profileWizard) applyModelSelection(modelID string) profileWizard {
	ctx := m.pickerContext
	if m.prevState == stateSimpleModelPick {
		if ctx == "all groups" {
			for _, g := range simpleGroups {
				m.simpleModels[g.key] = modelID
			}
		} else {
			m.simpleModels[ctx] = modelID
		}
	} else { // stateAdvancedModelPick
		if ctx == "all agents" {
			for i := range m.agents {
				m.agents[i].model = modelID
			}
		} else {
			for i, a := range m.agents {
				if a.name == ctx {
					m.agents[i].model = modelID
					break
				}
			}
		}
	}
	return m
}

func (m profileWizard) viewModelSelect() string {
	s := "\n"
	s += titleStyle.Render("  Select Model") + "\n"
	s += groupStyle.Render("  for: "+m.pickerContext) + "  " + dimStyle.Render("provider: "+m.selectedProvider) + "\n"
	s += dimStyle.Render("  ──────────────────────────────") + "\n\n"

	start := m.modelOffset
	end := m.modelOffset + maxVisible
	if end > len(m.models) {
		end = len(m.models)
	}

	if start > 0 {
		s += dimStyle.Render(fmt.Sprintf("  ↑ %d more\n", start))
	}

	for i := start; i < end; i++ {
		model := m.models[i]
		cursor := "  "
		style := dimStyle
		if i == m.modelCursor {
			cursor = "▶ "
			style = activeStyle
		}
		s += style.Render(fmt.Sprintf("%s%s", cursor, shortModel(model))) + "\n"
	}

	if end < len(m.models) {
		s += dimStyle.Render(fmt.Sprintf("  ↓ %d more\n", len(m.models)-end))
	}

	s += helpBar("↑↓", "navigate", "enter", "select", "esc", "back to providers")
	return s
}

// ---------------------------------------------------------------------------
// State: stateSummary
// ---------------------------------------------------------------------------

func (m profileWizard) updateSummary(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter", "y", "c":
			m.confirmed = true
			return m, tea.Quit
		case "esc", "n", "q":
			// Back to the model picker that was active.
			if m.mode == ModeSimple {
				m.state = stateSimpleModelPick
			} else {
				m.state = stateAdvancedModelPick
			}
			return m, nil
		case "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m profileWizard) viewSummary() string {
	name := strings.TrimSpace(m.textInput.Value())
	modeLabel := map[ConfigMode]string{ModeSimple: "Simple", ModeAdvanced: "Advanced"}[m.mode]

	s := "\n"
	s += titleStyle.Render("  Profile Summary") + "\n"
	s += dimStyle.Render("  ──────────────────────────────") + "\n\n"
	s += fmt.Sprintf("  Name:  %s\n", activeStyle.Render(name))
	s += fmt.Sprintf("  Mode:  %s\n\n", dimStyle.Render(modeLabel))

	if m.mode == ModeSimple {
		for _, g := range simpleGroups {
			modelID := m.simpleModels[g.key]
			if modelID == "" {
				s += fmt.Sprintf("  %-14s %s\n", g.key, warnStyle.Render("not set"))
			} else {
				s += fmt.Sprintf("  %-14s %s\n", g.key, checkStyle.Render(shortModel(modelID)))
				s += fmt.Sprintf("  %-14s %s\n", "", dimStyle.Render("→ "+strings.Join(g.agents, ", ")))
			}
		}
	} else {
		for _, a := range m.agents {
			if a.model == "" {
				s += fmt.Sprintf("  %-18s %s\n", a.name, warnStyle.Render("not set"))
			} else {
				s += fmt.Sprintf("  %-18s %s\n", a.name, checkStyle.Render(shortModel(a.model)))
			}
		}
	}

	s += "\n"
	s += helpBar("enter", "save profile", "esc", "go back and edit")
	return s
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func validateProfileName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	if len(name) > 32 {
		return fmt.Errorf("max 32 characters")
	}
	if name == "default" {
		return fmt.Errorf("\"default\" is reserved")
	}
	if !regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`).MatchString(name) {
		return fmt.Errorf("use lowercase letters, numbers and hyphens (e.g. backend, front-v2)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// ProfileResult is the output of the creation flow.
type ProfileResult struct {
	Name   string
	Models map[string]string
}

// RunProfileCreationFlow runs the complete wizard as a single fullscreen program.
// Returns *ProfileResult or an error if cancelled.
func RunProfileCreationFlow(initialModels map[string]string) (*ProfileResult, error) {
	wizard := newProfileWizard(initialModels)
	p := tea.NewProgram(wizard, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return nil, err
	}
	result := final.(profileWizard)
	if result.cancelled || !result.confirmed {
		return nil, fmt.Errorf("cancelled")
	}

	name := strings.TrimSpace(result.textInput.Value())
	models := make(map[string]string)

	if result.mode == ModeSimple {
		// Expand group models to individual agent keys.
		for _, g := range simpleGroups {
			modelID := result.simpleModels[g.key]
			if modelID != "" {
				for _, agentName := range g.agents {
					models[agentName] = modelID
				}
			}
		}
	} else {
		for _, a := range result.agents {
			if a.model != "" {
				models[a.name] = a.model
			}
		}
	}

	return &ProfileResult{Name: name, Models: models}, nil
}

// RunProfileNameForm runs just the name-input screen as a standalone program.
// Useful in contexts where only a name is needed (rename, etc.).
func RunProfileNameForm(initial string) (string, error) {
	ti := textinput.New()
	ti.Placeholder = "e.g. backend, front-v2"
	ti.SetValue(initial)
	ti.Focus()
	ti.CharLimit = 32
	ti.Width = 30

	m := profileNameForm{textInput: ti}
	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return "", err
	}
	result := final.(profileNameForm)
	if result.cancelled {
		return "", fmt.Errorf("cancelled")
	}
	name := strings.TrimSpace(result.textInput.Value())
	if name == "" {
		return "", fmt.Errorf("cancelled")
	}
	return name, nil
}

// ---------------------------------------------------------------------------
// profileNameForm — tiny standalone form kept for RunProfileNameForm
// ---------------------------------------------------------------------------

type profileNameForm struct {
	textInput textinput.Model
	err       string
	cancelled bool
}

func (m profileNameForm) Init() tea.Cmd { return textinput.Blink }

func (m profileNameForm) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			name := strings.TrimSpace(m.textInput.Value())
			if err := validateProfileName(name); err != nil {
				m.err = err.Error()
				return m, nil
			}
			return m, tea.Quit
		case "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	name := strings.TrimSpace(m.textInput.Value())
	if name != "" {
		if err := validateProfileName(name); err != nil {
			m.err = err.Error()
		} else {
			m.err = ""
		}
	} else {
		m.err = ""
	}
	return m, cmd
}

func (m profileNameForm) View() string {
	s := "\n"
	s += titleStyle.Render("  Profile Name") + "\n"
	s += dimStyle.Render("  ──────────────────────────────") + "\n\n"
	s += fmt.Sprintf("  Profile name: %s\n\n", m.textInput.View())
	if m.err != "" {
		s += "  " + errorStyle.Render("✗ "+m.err) + "\n"
	} else {
		s += "  " + dimStyle.Render("lowercase letters, numbers and hyphens only") + "\n"
	}
	s += helpBar("enter", "confirm", "esc", "cancel")
	return s
}
