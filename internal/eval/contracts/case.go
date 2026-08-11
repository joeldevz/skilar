// Package contracts defines the versioned, serializable contracts used by
// skynex-eval. The package intentionally contains no runner or provider logic.
package contracts

import (
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// CaseSchemaVersion is the only case schema understood by this build.
	CaseSchemaVersion = 1

	MaxCaseIDBytes       = 128
	MaxSuiteIDBytes      = 128
	MaxInputBytes        = 256 << 10
	MaxTurns             = 100
	MaxRuns              = 100
	MaxCommands          = 128
	MaxCommandArgs       = 256
	MaxCommandArgBytes   = 32 << 10
	MaxCommandOutput     = 64 << 20
	MaxChecks            = 1000
	MaxPaths             = 10_000
	MaxTraceBytes        = 256 << 20
	MaxTraceEvents       = 1_000_000
	MaxTraceEventBytes   = 4 << 20
	MaxCompletionTimeout = 24 * time.Hour
	MaxCommandTimeout    = time.Hour
)

type CaseType string

const (
	CaseTypeBehavior    CaseType = "behavior"
	CaseTypeSecurity    CaseType = "security"
	CaseTypeQuality     CaseType = "quality"
	CaseTypeReliability CaseType = "reliability"

	// These values can only occur on an explicitly migrated legacy case.
	CaseTypeLegacyPositive CaseType = "positive"
	CaseTypeLegacyNegative CaseType = "negative"
)

type Aggregation string

const (
	AggregationMin    Aggregation = "min"
	AggregationMedian Aggregation = "median"
	AggregationMean   Aggregation = "mean"
)

type ExecutionMode string

const (
	ExecutionTrustedLocal      ExecutionMode = "trusted-local"
	ExecutionIsolatedContainer ExecutionMode = "isolated-container"
)

type NetworkPolicy string

const (
	NetworkNone              NetworkPolicy = "none"
	NetworkLoopback          NetworkPolicy = "loopback"
	NetworkRegistryAllowlist NetworkPolicy = "registry-allowlist"
	NetworkProviderProxyOnly NetworkPolicy = "provider-proxy-only"
	NetworkHostUnisolated    NetworkPolicy = "host-unisolated"
)

type RetainTracePolicy string

const (
	RetainTraceNever              RetainTracePolicy = "never"
	RetainTraceSanitizedOnFailure RetainTracePolicy = "sanitized-on-failure"
	RetainTraceSanitizedAlways    RetainTracePolicy = "sanitized-always"
)

type UnexpectedQuestionPolicy string

const (
	UnexpectedQuestionFail     UnexpectedQuestionPolicy = "fail"
	UnexpectedQuestionContinue UnexpectedQuestionPolicy = "continue"
	UnexpectedQuestionStop     UnexpectedQuestionPolicy = "stop"
)

// Case is the normalized schema-v1 evaluation case. Migration metadata is not
// serialized and therefore never affects the effective-case digest.
type Case struct {
	SchemaVersion  int                    `json:"schema_version" yaml:"schema_version"`
	ID             string                 `json:"id" yaml:"id"`
	Suite          string                 `json:"suite" yaml:"suite"`
	RequirementIDs []string               `json:"requirement_ids" yaml:"requirement_ids"`
	Type           CaseType               `json:"type" yaml:"type"`
	Critical       bool                   `json:"critical" yaml:"critical"`
	Agent          AgentConfig            `json:"agent" yaml:"agent"`
	Fixture        FixtureConfig          `json:"fixture" yaml:"fixture"`
	Setup          SetupConfig            `json:"setup" yaml:"setup"`
	Input          string                 `json:"input" yaml:"input"`
	Turns          []Turn                 `json:"turns" yaml:"turns"`
	Completion     CompletionConfig       `json:"completion" yaml:"completion"`
	Oracle         OracleConfig           `json:"oracle" yaml:"oracle"`
	BehaviorChecks []Check                `json:"behavior_checks" yaml:"behavior_checks"`
	Security       SecurityConfig         `json:"security" yaml:"security"`
	Trace          TraceConfig            `json:"trace" yaml:"trace"`
	ToolPolicy     ToolPolicy             `json:"tool_policy" yaml:"tool_policy"`
	Runs           RunConfig              `json:"runs" yaml:"runs"`
	Gates          Gates                  `json:"gates" yaml:"gates"`
	LLMJudge       *LLMJudge              `json:"llm_judge,omitempty" yaml:"llm_judge,omitempty"`
	Metrics        []string               `json:"metrics,omitempty" yaml:"metrics,omitempty"`
	Extensions     map[string]interface{} `json:"extensions,omitempty" yaml:"extensions,omitempty"`

	Migration *LegacyMigration `json:"-" yaml:"-"`
}

type AgentConfig struct {
	Name  string `json:"name" yaml:"name"`
	Model string `json:"model" yaml:"model"`
}

type FixtureConfig struct {
	Source         string  `json:"source" yaml:"source"`
	InitialGit     bool    `json:"initial_git" yaml:"initial_git"`
	ExpectedDigest string  `json:"expected_digest" yaml:"expected_digest"`
	GitSeed        GitSeed `json:"git_seed" yaml:"git_seed"`
}

type GitSeed struct {
	Tracked   []GitSeedFile `json:"tracked" yaml:"tracked"`
	Staged    []GitSeedFile `json:"staged" yaml:"staged"`
	Untracked []GitSeedFile `json:"untracked" yaml:"untracked"`
	Ignored   []GitSeedFile `json:"ignored" yaml:"ignored"`
}

// GitSeedFile describes the exact bytes to place at Path for a seeded Git
// state. Exactly one of Content or Digest may be supplied. Digest means that
// the file must already exist in the copied fixture with those bytes.
type GitSeedFile struct {
	Path    string  `json:"path" yaml:"path"`
	Content *string `json:"content,omitempty" yaml:"content,omitempty"`
	Digest  string  `json:"digest,omitempty" yaml:"digest,omitempty"`
	Mode    string  `json:"mode,omitempty" yaml:"mode,omitempty"`
}

type SetupConfig struct {
	Commands []Command `json:"commands" yaml:"commands"`
}

// Command is deliberately argv-only. An argument containing shell syntax is
// still just an argument; the evaluator must never pass it through a shell.
type Command struct {
	ID             string            `json:"id,omitempty" yaml:"id,omitempty"`
	Argv           []string          `json:"argv" yaml:"argv"`
	Cwd            string            `json:"cwd,omitempty" yaml:"cwd,omitempty"`
	Timeout        string            `json:"timeout" yaml:"timeout"`
	ExpectedExit   []int             `json:"expected_exit" yaml:"expected_exit"`
	MaxOutputBytes int64             `json:"max_output_bytes,omitempty" yaml:"max_output_bytes,omitempty"`
	Env            map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

type Turn struct {
	Answer string `json:"answer" yaml:"answer"`
}

type CompletionConfig struct {
	MaxTurns           int                      `json:"max_turns" yaml:"max_turns"`
	Timeout            string                   `json:"timeout" yaml:"timeout"`
	UnexpectedQuestion UnexpectedQuestionPolicy `json:"unexpected_question" yaml:"unexpected_question"`
}

type OracleConfig struct {
	Commands                []Command      `json:"commands" yaml:"commands"`
	ExpectedChanges         []string       `json:"expected_changes" yaml:"expected_changes"`
	ForbiddenChanges        []string       `json:"forbidden_changes" yaml:"forbidden_changes"`
	ExpectedFiles           []ExpectedFile `json:"expected_files" yaml:"expected_files"`
	RequireCleanProcessTree bool           `json:"require_clean_process_tree" yaml:"require_clean_process_tree"`
}

// ExpectedFile provides an exact byte-level oracle. Content is useful for
// small goldens; Digest is preferred for larger files. Exactly one is required.
type ExpectedFile struct {
	Path    string  `json:"path" yaml:"path"`
	Content *string `json:"content,omitempty" yaml:"content,omitempty"`
	Digest  string  `json:"digest,omitempty" yaml:"digest,omitempty"`
	Mode    string  `json:"mode,omitempty" yaml:"mode,omitempty"`
}

// Check describes a deterministic observation. ID is optional in source YAML;
// normalization assigns a stable ID before validation and digesting.
type Check struct {
	ID             string                 `json:"id,omitempty" yaml:"id,omitempty"`
	Name           string                 `json:"name,omitempty" yaml:"name,omitempty"`
	RequirementIDs []string               `json:"requirement_ids,omitempty" yaml:"requirement_ids,omitempty"`
	EvidenceIDs    []string               `json:"evidence_ids" yaml:"evidence_ids"`
	Type           string                 `json:"type" yaml:"type"`
	Hard           *bool                  `json:"hard,omitempty" yaml:"hard,omitempty"`
	Pattern        string                 `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	Patterns       []string               `json:"patterns,omitempty" yaml:"patterns,omitempty"`
	Value          interface{}            `json:"value,omitempty" yaml:"value,omitempty"`
	Tool           string                 `json:"tool,omitempty" yaml:"tool,omitempty"`
	Path           string                 `json:"path,omitempty" yaml:"path,omitempty"`
	Argv           []string               `json:"argv,omitempty" yaml:"argv,omitempty"`
	Min            *int                   `json:"min,omitempty" yaml:"min,omitempty"`
	Max            *int                   `json:"max,omitempty" yaml:"max,omitempty"`
	Extensions     map[string]interface{} `json:"extensions,omitempty" yaml:"extensions,omitempty"`
}

type SecurityConfig struct {
	ExecutionMode      ExecutionMode     `json:"execution_mode" yaml:"execution_mode"`
	Network            NetworkPolicy     `json:"network" yaml:"network"`
	PackageScripts     bool              `json:"package_scripts" yaml:"package_scripts"`
	AllowedExecutables []string          `json:"allowed_executables" yaml:"allowed_executables"`
	AllowedWriteRoots  []string          `json:"allowed_write_roots" yaml:"allowed_write_roots"`
	AllowedRegistries  []string          `json:"allowed_registries,omitempty" yaml:"allowed_registries,omitempty"`
	RetainTrace        RetainTracePolicy `json:"retain_trace" yaml:"retain_trace"`
}

type TraceConfig struct {
	MaxBytes      int64            `json:"max_bytes" yaml:"max_bytes"`
	MaxEvents     int              `json:"max_events" yaml:"max_events"`
	MaxEventBytes int64            `json:"max_event_bytes" yaml:"max_event_bytes"`
	Quiescence    QuiescenceConfig `json:"quiescence" yaml:"quiescence"`
}

type QuiescenceConfig struct {
	Required    bool   `json:"required" yaml:"required"`
	QuietPeriod string `json:"quiet_period" yaml:"quiet_period"`
	Timeout     string `json:"timeout" yaml:"timeout"`
}

type ToolPolicy struct {
	AllowedTools   []string  `json:"allowed_tools" yaml:"allowed_tools"`
	ForbiddenTools []string  `json:"forbidden_tools" yaml:"forbidden_tools"`
	FakeMCPs       []FakeMCP `json:"fake_mcps" yaml:"fake_mcps"`
}

type FakeMCP struct {
	Name      string `json:"name" yaml:"name"`
	Transport string `json:"transport" yaml:"transport"`
	// Tools are the raw MCP tool names advertised by this server. OpenCode's
	// effective prompt IDs are derived as <server>_<tool> by toolpolicy.
	Tools   []string          `json:"tools" yaml:"tools"`
	Command *Command          `json:"command,omitempty" yaml:"command,omitempty"`
	URL     string            `json:"url,omitempty" yaml:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

type RunConfig struct {
	Count       int         `json:"count" yaml:"count"`
	Aggregation Aggregation `json:"aggregation" yaml:"aggregation"`
}

type Gates struct {
	HardChecks                    string   `json:"hard_checks" yaml:"hard_checks"`
	MaxParentPeakInputTokensRatio *float64 `json:"max_parent_peak_input_tokens_ratio,omitempty" yaml:"max_parent_peak_input_tokens_ratio,omitempty"`
	MaxTreeInputTokensRatio       *float64 `json:"max_tree_input_tokens_ratio,omitempty" yaml:"max_tree_input_tokens_ratio,omitempty"`
	MaxMedianCostRatio            *float64 `json:"max_median_cost_ratio,omitempty" yaml:"max_median_cost_ratio,omitempty"`
	MaxMedianDurationRatio        *float64 `json:"max_median_duration_ratio,omitempty" yaml:"max_median_duration_ratio,omitempty"`
	MaxRetryRateRatio             *float64 `json:"max_retry_rate_ratio,omitempty" yaml:"max_retry_rate_ratio,omitempty"`
}

type LLMJudge struct {
	Enabled       bool    `json:"enabled" yaml:"enabled"`
	Model         string  `json:"model" yaml:"model"`
	Rubric        string  `json:"rubric" yaml:"rubric"`
	PassThreshold float64 `json:"pass_threshold" yaml:"pass_threshold"`
	PromptDigest  string  `json:"prompt_digest,omitempty" yaml:"prompt_digest,omitempty"`
}

// LegacyMigration proves that a schema-less historical case was deliberately
// mapped instead of being permissively decoded. All historical source fields
// have a corresponding effective Case field; the raw source digest remains for
// audit without perturbing the effective-case digest.
type LegacyMigration struct {
	SourceDigest string
	Item         string
	Type         string
	SetupCommand string
}

// CaseOverrides are the only fields intentionally mutable by suite/CLI
// configuration. ApplyOverrides applies layers from lowest to highest
// precedence; later non-nil values win.
type CaseOverrides struct {
	AgentModel        *string
	RunsCount         *int
	Aggregation       *Aggregation
	MaxTurns          *int
	CompletionTimeout *string
	ExecutionMode     *ExecutionMode
	Network           *NetworkPolicy
	TraceMaxBytes     *int64
	TraceMaxEvents    *int
}

type OverrideSource string

const (
	OverrideDefaults OverrideSource = "defaults"
	OverrideSuite    OverrideSource = "suite"
	OverrideCase     OverrideSource = "case"
	OverrideCLI      OverrideSource = "cli"
)

const OverridePrecedence = "defaults < suite < case < cli"

type OverrideLayer struct {
	Source OverrideSource
	Values CaseOverrides
}

func ApplyOverrides(base Case, layers ...CaseOverrides) (Case, error) {
	result := base
	for _, layer := range layers {
		if layer.AgentModel != nil {
			result.Agent.Model = *layer.AgentModel
		}
		if layer.RunsCount != nil {
			result.Runs.Count = *layer.RunsCount
		}
		if layer.Aggregation != nil {
			result.Runs.Aggregation = *layer.Aggregation
		}
		if layer.MaxTurns != nil {
			result.Completion.MaxTurns = *layer.MaxTurns
		}
		if layer.CompletionTimeout != nil {
			result.Completion.Timeout = *layer.CompletionTimeout
		}
		if layer.ExecutionMode != nil {
			result.Security.ExecutionMode = *layer.ExecutionMode
		}
		if layer.Network != nil {
			result.Security.Network = *layer.Network
		}
		if layer.TraceMaxBytes != nil {
			result.Trace.MaxBytes = *layer.TraceMaxBytes
		}
		if layer.TraceMaxEvents != nil {
			result.Trace.MaxEvents = *layer.TraceMaxEvents
		}
	}
	if err := result.Validate(); err != nil {
		return Case{}, fmt.Errorf("apply overrides: %w", err)
	}
	return result, nil
}

// ApplyOverrideLayers is the named form of ApplyOverrides. It rejects layers
// supplied outside the documented defaults < suite < case < cli precedence so
// a caller cannot accidentally let a suite override an explicit CLI choice.
func ApplyOverrideLayers(base Case, layers ...OverrideLayer) (Case, error) {
	previous := -1
	overrides := make([]CaseOverrides, 0, len(layers))
	for i, layer := range layers {
		rank, ok := overrideRank(layer.Source)
		if !ok {
			return Case{}, fmt.Errorf("override layer %d: unsupported source %q", i, layer.Source)
		}
		if rank <= previous {
			return Case{}, fmt.Errorf("override layer %d: sources must be unique and ordered by %s", i, OverridePrecedence)
		}
		previous = rank
		overrides = append(overrides, layer.Values)
	}
	return ApplyOverrides(base, overrides...)
}

func (c *Case) Normalize() {
	for i := range c.BehaviorChecks {
		check := &c.BehaviorChecks[i]
		if check.ID == "" {
			if check.Name != "" {
				check.ID = check.Name
			} else {
				check.ID = fmt.Sprintf("behavior_%03d_%s", i+1, sanitizeID(check.Type))
			}
		}
		if check.Hard == nil {
			hard := true
			check.Hard = &hard
		}
	}
	for i := range c.Setup.Commands {
		if c.Setup.Commands[i].ID == "" {
			c.Setup.Commands[i].ID = fmt.Sprintf("setup_%03d", i+1)
		}
		if c.Setup.Commands[i].ExpectedExit == nil {
			c.Setup.Commands[i].ExpectedExit = []int{0}
		}
	}
	for i := range c.Oracle.Commands {
		if c.Oracle.Commands[i].ID == "" {
			c.Oracle.Commands[i].ID = fmt.Sprintf("oracle_%03d", i+1)
		}
		if c.Oracle.Commands[i].ExpectedExit == nil {
			c.Oracle.Commands[i].ExpectedExit = []int{0}
		}
	}
	// Source schema-v1 cases must spell out evidence_ids. Normalization fills
	// them only for programmatic and deliberately migrated legacy callers so
	// the historical Go API remains source-compatible.
	for i := range c.BehaviorChecks {
		if len(c.BehaviorChecks[i].EvidenceIDs) != 0 {
			continue
		}
		if evidenceIDs, err := expectedCheckEvidenceIDs(*c, c.BehaviorChecks[i]); err == nil {
			c.BehaviorChecks[i].EvidenceIDs = evidenceIDs
		}
	}
	for i := range c.ToolPolicy.FakeMCPs {
		if c.ToolPolicy.FakeMCPs[i].Command != nil {
			command := c.ToolPolicy.FakeMCPs[i].Command
			if command.ID == "" {
				command.ID = fmt.Sprintf("fake_mcp_%03d", i+1)
			}
			if command.ExpectedExit == nil {
				command.ExpectedExit = []int{0}
			}
		}
	}
	if c.Turns == nil {
		c.Turns = []Turn{}
	}
	if c.Setup.Commands == nil {
		c.Setup.Commands = []Command{}
	}
	if c.Oracle.Commands == nil {
		c.Oracle.Commands = []Command{}
	}
	if c.Oracle.ExpectedChanges == nil {
		c.Oracle.ExpectedChanges = []string{}
	}
	if c.Oracle.ForbiddenChanges == nil {
		c.Oracle.ForbiddenChanges = []string{}
	}
	if c.BehaviorChecks == nil {
		c.BehaviorChecks = []Check{}
	}
	if c.Security.AllowedWriteRoots == nil {
		c.Security.AllowedWriteRoots = []string{}
	}
	if c.RequirementIDs == nil {
		c.RequirementIDs = []string{}
	}
	if c.Oracle.ExpectedFiles == nil {
		c.Oracle.ExpectedFiles = []ExpectedFile{}
	}
	if c.Fixture.GitSeed.Tracked == nil {
		c.Fixture.GitSeed.Tracked = []GitSeedFile{}
	}
	if c.Fixture.GitSeed.Staged == nil {
		c.Fixture.GitSeed.Staged = []GitSeedFile{}
	}
	if c.Fixture.GitSeed.Untracked == nil {
		c.Fixture.GitSeed.Untracked = []GitSeedFile{}
	}
	if c.Fixture.GitSeed.Ignored == nil {
		c.Fixture.GitSeed.Ignored = []GitSeedFile{}
	}
	if c.Security.AllowedExecutables == nil {
		c.Security.AllowedExecutables = []string{}
	}
	if c.ToolPolicy.AllowedTools == nil {
		c.ToolPolicy.AllowedTools = []string{}
	}
	if c.ToolPolicy.ForbiddenTools == nil {
		c.ToolPolicy.ForbiddenTools = []string{}
	}
	if c.ToolPolicy.FakeMCPs == nil {
		c.ToolPolicy.FakeMCPs = []FakeMCP{}
	}
}

func (c Case) Digest() (string, error) {
	c.Normalize()
	if err := c.Validate(); err != nil {
		return "", err
	}
	return CanonicalDigest(c)
}

func (c Case) Validate() error {
	if c.SchemaVersion != CaseSchemaVersion {
		return fieldError("schema_version", "must equal %d", CaseSchemaVersion)
	}
	if err := validateID("id", c.ID, MaxCaseIDBytes); err != nil {
		return err
	}
	if err := validateID("suite", c.Suite, MaxSuiteIDBytes); err != nil {
		return err
	}
	if err := validateRequirementIDs("requirement_ids", c.RequirementIDs); err != nil {
		return err
	}
	switch c.Type {
	case CaseTypeBehavior, CaseTypeSecurity, CaseTypeQuality, CaseTypeReliability:
	case CaseTypeLegacyPositive, CaseTypeLegacyNegative:
		if c.Migration == nil {
			return fieldError("type", "%q is reserved for migrated legacy cases", c.Type)
		}
	default:
		return fieldError("type", "unsupported value %q", c.Type)
	}
	if strings.TrimSpace(c.Agent.Name) == "" {
		return fieldError("agent.name", "must not be empty")
	}
	if strings.TrimSpace(c.Agent.Model) == "" {
		if c.Migration == nil {
			return fieldError("agent.model", "must not be empty")
		}
	} else if _, _, err := ParseModelSelection(c.Agent.Model); err != nil {
		return fieldWrap("agent.model", err)
	}
	if c.Fixture.Source != "" {
		if err := ValidateRelativePath(c.Fixture.Source); err != nil {
			return fieldWrap("fixture.source", err)
		}
	}
	if c.Migration == nil {
		if c.Fixture.Source == "" {
			return fieldError("fixture.source", "must not be empty")
		}
		if !validDigest(c.Fixture.ExpectedDigest) {
			return fieldError("fixture.expected_digest", "must be a sha256 digest")
		}
	}
	if err := validateGitSeed(c.Fixture.GitSeed); err != nil {
		return err
	}
	if !c.Fixture.InitialGit && gitSeedCount(c.Fixture.GitSeed) != 0 {
		return fieldError("fixture.git_seed", "requires fixture.initial_git")
	}
	if strings.TrimSpace(c.Input) == "" {
		return fieldError("input", "must not be empty")
	}
	if len(c.Input) > MaxInputBytes {
		return fieldError("input", "exceeds %d bytes", MaxInputBytes)
	}
	if len(c.Turns) > MaxTurns {
		return fieldError("turns", "exceeds %d turns", MaxTurns)
	}
	for i, turn := range c.Turns {
		if strings.TrimSpace(turn.Answer) == "" {
			return fieldError(fmt.Sprintf("turns[%d].answer", i), "must not be empty")
		}
		if len(turn.Answer) > MaxInputBytes {
			return fieldError(fmt.Sprintf("turns[%d].answer", i), "exceeds %d bytes", MaxInputBytes)
		}
	}
	if c.Completion.MaxTurns < 1 || c.Completion.MaxTurns > MaxTurns {
		return fieldError("completion.max_turns", "must be between 1 and %d", MaxTurns)
	}
	if err := validateDuration("completion.timeout", c.Completion.Timeout, MaxCompletionTimeout); err != nil {
		return err
	}
	switch c.Completion.UnexpectedQuestion {
	case UnexpectedQuestionFail, UnexpectedQuestionContinue, UnexpectedQuestionStop:
	default:
		return fieldError("completion.unexpected_question", "unsupported value %q", c.Completion.UnexpectedQuestion)
	}
	if len(c.Setup.Commands) > MaxCommands {
		return fieldError("setup.commands", "exceeds %d commands", MaxCommands)
	}
	if len(c.Oracle.Commands) > MaxCommands {
		return fieldError("oracle.commands", "exceeds %d commands", MaxCommands)
	}
	commandIDs := make(map[string]string)
	for i, command := range c.Setup.Commands {
		field := fmt.Sprintf("setup.commands[%d]", i)
		if err := validateCommand(field, command); err != nil {
			return err
		}
		if err := addUniqueID(commandIDs, field+".id", command.ID); err != nil {
			return err
		}
	}
	for i, command := range c.Oracle.Commands {
		field := fmt.Sprintf("oracle.commands[%d]", i)
		if err := validateCommand(field, command); err != nil {
			return err
		}
		if err := addUniqueID(commandIDs, field+".id", command.ID); err != nil {
			return err
		}
	}
	for i, fake := range c.ToolPolicy.FakeMCPs {
		if fake.Command == nil {
			continue
		}
		field := fmt.Sprintf("tool_policy.fake_mcps[%d].command", i)
		if err := validateCommand(field, *fake.Command); err != nil {
			return err
		}
		if err := addUniqueID(commandIDs, field+".id", fake.Command.ID); err != nil {
			return err
		}
	}
	if err := validatePathList("oracle.expected_changes", c.Oracle.ExpectedChanges); err != nil {
		return err
	}
	if err := validatePathList("oracle.forbidden_changes", c.Oracle.ForbiddenChanges); err != nil {
		return err
	}
	if err := validateExpectedFiles(c.Oracle.ExpectedFiles); err != nil {
		return err
	}
	if overlap := firstOverlap(c.Oracle.ExpectedChanges, c.Oracle.ForbiddenChanges); overlap != "" {
		return fieldError("oracle", "path %q is both expected and forbidden", overlap)
	}
	if len(c.BehaviorChecks) > MaxChecks {
		return fieldError("behavior_checks", "exceeds %d checks", MaxChecks)
	}
	checkIDs := make(map[string]string)
	for i, check := range c.BehaviorChecks {
		field := fmt.Sprintf("behavior_checks[%d]", i)
		if err := validateCheck(field, check); err != nil {
			return err
		}
		if err := validateCheckEvidence(field, check, c); err != nil {
			return err
		}
		if err := addUniqueID(checkIDs, field+".id", check.ID); err != nil {
			return err
		}
		for j, requirementID := range check.RequirementIDs {
			if !containsString(c.RequirementIDs, requirementID) {
				return fieldError(fmt.Sprintf("%s.requirement_ids[%d]", field, j), "references undeclared case requirement %q", requirementID)
			}
		}
	}
	if c.Critical && len(c.Oracle.Commands) == 0 && len(c.Oracle.ExpectedChanges) == 0 && len(c.Oracle.ExpectedFiles) == 0 && len(c.BehaviorChecks) == 0 {
		return fieldError("critical", "critical cases require at least one deterministic oracle or behavior check")
	}
	if c.Gates.HardChecks != "all" {
		return fieldError("gates.hard_checks", "must equal %q", "all")
	}
	for field, ratio := range map[string]*float64{
		"gates.max_parent_peak_input_tokens_ratio": c.Gates.MaxParentPeakInputTokensRatio,
		"gates.max_tree_input_tokens_ratio":        c.Gates.MaxTreeInputTokensRatio,
		"gates.max_median_cost_ratio":              c.Gates.MaxMedianCostRatio,
		"gates.max_median_duration_ratio":          c.Gates.MaxMedianDurationRatio,
		"gates.max_retry_rate_ratio":               c.Gates.MaxRetryRateRatio,
	} {
		if ratio != nil && (math.IsNaN(*ratio) || math.IsInf(*ratio, 0) || *ratio < 0 || *ratio > 100) {
			return fieldError(field, "must be between 0 and 100")
		}
	}
	if err := validateSecurity(c.Security, c.Migration != nil); err != nil {
		return err
	}
	if err := validateAllowedCommands(c); err != nil {
		return err
	}
	if err := validateTrace(c.Trace, c.Migration != nil); err != nil {
		return err
	}
	if err := validateToolPolicy(c.ToolPolicy, c.Security); err != nil {
		return err
	}
	if c.Runs.Count < 1 || c.Runs.Count > MaxRuns {
		return fieldError("runs.count", "must be between 1 and %d", MaxRuns)
	}
	switch c.Runs.Aggregation {
	case AggregationMin, AggregationMedian, AggregationMean:
	default:
		return fieldError("runs.aggregation", "unsupported value %q", c.Runs.Aggregation)
	}
	if c.LLMJudge != nil {
		if c.LLMJudge.Enabled {
			if strings.TrimSpace(c.LLMJudge.Model) == "" {
				return fieldError("llm_judge.model", "must not be empty when enabled")
			}
			if strings.TrimSpace(c.LLMJudge.Rubric) == "" {
				return fieldError("llm_judge.rubric", "must not be empty when enabled")
			}
		}
		if math.IsNaN(c.LLMJudge.PassThreshold) || math.IsInf(c.LLMJudge.PassThreshold, 0) || c.LLMJudge.PassThreshold < 0 || c.LLMJudge.PassThreshold > 10 {
			return fieldError("llm_judge.pass_threshold", "must be between 0 and 10")
		}
		if c.LLMJudge.PromptDigest != "" && !validDigest(c.LLMJudge.PromptDigest) {
			return fieldError("llm_judge.prompt_digest", "must be a sha256 digest")
		}
	}
	if err := validateUniqueStrings("metrics", c.Metrics, true); err != nil {
		return err
	}
	if err := validateExtensions("extensions", c.Extensions); err != nil {
		return err
	}
	return nil
}

func ValidateRelativePath(value string) error {
	if value == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("must not contain NUL")
	}
	if len(value) > 4096 {
		return fmt.Errorf("exceeds 4096 bytes")
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("must be valid UTF-8")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("must not contain control characters")
		}
	}
	if strings.Contains(value, "%") {
		if decoded, err := url.PathUnescape(value); err == nil && decoded != value {
			return fmt.Errorf("percent-encoded path components are not allowed")
		}
	}
	if strings.Contains(value, "\\") {
		return fmt.Errorf("must use forward slashes")
	}
	if strings.HasPrefix(value, "/") || isWindowsAbsolute(value) {
		return fmt.Errorf("must be relative")
	}
	if path.Clean(value) != value || value == "." {
		return fmt.Errorf("must be normalized")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("must remain below the fixture root")
		}
	}
	return nil
}

func validateCommand(field string, command Command) error {
	if command.ID == "" {
		return fieldError(field+".id", "must not be empty after normalization")
	}
	if err := validateID(field+".id", command.ID, MaxCaseIDBytes); err != nil {
		return err
	}
	if len(command.Argv) == 0 {
		return fieldError(field+".argv", "must contain at least one argument")
	}
	if len(command.Argv) > MaxCommandArgs {
		return fieldError(field+".argv", "exceeds %d arguments", MaxCommandArgs)
	}
	for i, arg := range command.Argv {
		if arg == "" {
			return fieldError(fmt.Sprintf("%s.argv[%d]", field, i), "must not be empty")
		}
		if len(arg) > MaxCommandArgBytes {
			return fieldError(fmt.Sprintf("%s.argv[%d]", field, i), "exceeds %d bytes", MaxCommandArgBytes)
		}
		if strings.ContainsRune(arg, '\x00') {
			return fieldError(fmt.Sprintf("%s.argv[%d]", field, i), "must not contain NUL")
		}
	}
	if command.Cwd != "" {
		if err := ValidateRelativePath(command.Cwd); err != nil {
			return fieldWrap(field+".cwd", err)
		}
	}
	if err := validateDuration(field+".timeout", command.Timeout, MaxCommandTimeout); err != nil {
		return err
	}
	if command.MaxOutputBytes < 0 || command.MaxOutputBytes > MaxCommandOutput {
		return fieldError(field+".max_output_bytes", "must be between 0 and %d", MaxCommandOutput)
	}
	if len(command.ExpectedExit) == 0 {
		return fieldError(field+".expected_exit", "must contain at least one exit code")
	}
	exits := make(map[int]struct{}, len(command.ExpectedExit))
	for i, code := range command.ExpectedExit {
		if code < 0 || code > 255 {
			return fieldError(fmt.Sprintf("%s.expected_exit[%d]", field, i), "must be between 0 and 255")
		}
		if _, exists := exits[code]; exists {
			return fieldError(field+".expected_exit", "contains duplicate exit code %d", code)
		}
		exits[code] = struct{}{}
	}
	if len(command.Env) > 256 {
		return fieldError(field+".env", "exceeds 256 entries")
	}
	for key, value := range command.Env {
		if !envNamePattern.MatchString(key) {
			return fieldError(field+".env", "invalid environment name %q", key)
		}
		if strings.ContainsRune(value, '\x00') {
			return fieldError(field+".env."+key, "must not contain NUL")
		}
		if len(value) > MaxCommandArgBytes {
			return fieldError(field+".env."+key, "exceeds %d bytes", MaxCommandArgBytes)
		}
	}
	return nil
}

func validateCheck(field string, check Check) error {
	if err := validateID(field+".id", check.ID, MaxCaseIDBytes); err != nil {
		return err
	}
	supported := map[string]bool{
		"regex_count": true, "regex_count_max_per_msg": true, "contains_any": true,
		"contains_all": true, "not_contains": true, "not_contains_pattern": true,
		"regex_match": true, "tool_called": true, "tool_called_min": true,
		"tool_not_called": true, "tool_output_contains_all": true, "file_written": true, "tool_call_order": true,
		"bash_output_contains": true, "subagent_count": true, "no_false_success": true,
		"expected_diff": true, "file_exists": true, "file_not_exists": true,
	}
	if !supported[check.Type] {
		return fieldError(field+".type", "unsupported value %q", check.Type)
	}
	if err := validateRequirementIDs(field+".requirement_ids", check.RequirementIDs); err != nil {
		return err
	}
	if len(check.Name) > 256 {
		return fieldError(field+".name", "exceeds 256 bytes")
	}
	if check.Pattern != "" && len(check.Pattern) > MaxCommandArgBytes {
		return fieldError(field+".pattern", "exceeds %d bytes", MaxCommandArgBytes)
	}
	if len(check.Patterns) > 1000 {
		return fieldError(field+".patterns", "exceeds 1000 entries")
	}
	for i, pattern := range check.Patterns {
		if pattern == "" || len(pattern) > MaxCommandArgBytes {
			return fieldError(fmt.Sprintf("%s.patterns[%d]", field, i), "must be non-empty and no larger than %d bytes", MaxCommandArgBytes)
		}
	}
	if check.Type == "regex_count" || check.Type == "regex_count_max_per_msg" || check.Type == "regex_match" || check.Type == "not_contains_pattern" || check.Type == "bash_output_contains" {
		if check.Pattern == "" {
			return fieldError(field+".pattern", "is required for check type %q", check.Type)
		}
		if _, err := regexp.Compile(check.Pattern); err != nil {
			return fieldError(field+".pattern", "invalid regular expression: %v", err)
		}
	}
	if check.Type == "contains_any" || check.Type == "contains_all" || check.Type == "not_contains" || check.Type == "tool_output_contains_all" {
		if len(check.Patterns) == 0 {
			return fieldError(field+".patterns", "is required for check type %q", check.Type)
		}
	}
	if check.Type == "subagent_count" && check.Min != nil && *check.Min > 0 && len(check.Patterns) == 0 {
		return fieldError(field+".patterns", "is required when subagent_count requires one or more delegated sessions")
	}
	if check.Type == "regex_count" || check.Type == "regex_count_max_per_msg" || check.Type == "tool_called_min" {
		if value, ok := integerValue(check.Value); !ok || value < 0 {
			return fieldError(field+".value", "must be a non-negative integer for check type %q", check.Type)
		}
	}
	if check.Type == "tool_called" || check.Type == "tool_called_min" || check.Type == "tool_not_called" || check.Type == "tool_output_contains_all" {
		if strings.TrimSpace(check.Tool) == "" {
			return fieldError(field+".tool", "is required for check type %q", check.Type)
		}
	}
	if check.Path != "" {
		if err := ValidateRelativePath(check.Path); err != nil {
			return fieldWrap(field+".path", err)
		}
	}
	if check.Type == "file_exists" || check.Type == "file_not_exists" {
		if check.Path == "" {
			return fieldError(field+".path", "is required for check type %q", check.Type)
		}
	}
	if check.Type == "file_written" {
		if check.Pattern == "" {
			return fieldError(field+".pattern", "is required for file_written")
		}
		if _, err := path.Match(check.Pattern, "probe"); err != nil {
			return fieldError(field+".pattern", "invalid path pattern: %v", err)
		}
	}
	if len(check.Argv) > MaxCommandArgs {
		return fieldError(field+".argv", "exceeds %d arguments", MaxCommandArgs)
	}
	for i, arg := range check.Argv {
		if arg == "" || len(arg) > MaxCommandArgBytes || strings.ContainsRune(arg, '\x00') {
			return fieldError(fmt.Sprintf("%s.argv[%d]", field, i), "must be non-empty, bounded, and contain no NUL")
		}
	}
	if check.Min != nil && *check.Min < 0 {
		return fieldError(field+".min", "must be non-negative")
	}
	if check.Max != nil && *check.Max < 0 {
		return fieldError(field+".max", "must be non-negative")
	}
	if check.Min != nil && check.Max != nil && *check.Min > *check.Max {
		return fieldError(field, "min must not exceed max")
	}
	if err := validateExtensions(field+".extensions", check.Extensions); err != nil {
		return err
	}
	if _, legacyEvidence := check.Extensions["x-evidence-ids"]; legacyEvidence {
		return fieldError(field+".extensions.x-evidence-ids", "use the typed evidence_ids field")
	}
	return nil
}

// expectedCheckEvidenceIDs names the stable EvidenceItem records a behavior
// check is allowed to depend on. Dynamic trace event IDs are intentionally not
// part of the case contract; checks consume the durable behavior aggregate.
func expectedCheckEvidenceIDs(testCase Case, check Check) ([]string, error) {
	switch check.Type {
	case "contains_all", "contains_any", "not_contains":
		return []string{"claims"}, nil
	case "not_contains_pattern", "regex_match", "regex_count", "regex_count_max_per_msg":
		return []string{"claims", "behavior"}, nil
	case "subagent_count", "tool_called", "tool_called_min", "tool_not_called", "tool_output_contains_all", "bash_output_contains":
		return []string{"behavior"}, nil
	case "expected_diff", "file_written":
		return []string{"before", "after"}, nil
	case "file_exists", "file_not_exists":
		return []string{"after"}, nil
	case "no_false_success":
		// The verdict consulted by no_false_success is derived from every
		// deterministic judge category, not only the final response.
		return []string{"infrastructure", "filesystem", "acceptance", "behavior", "claims", "security"}, nil
	case "tool_call_order":
		if len(check.Patterns) != 2 {
			return nil, fmt.Errorf("tool_call_order requires exactly two ordered names")
		}
		setupIndex := commandIndexByID(testCase.Setup.Commands, check.Patterns[0])
		oracleIndex := commandIndexByID(testCase.Oracle.Commands, check.Patterns[1])
		if setupIndex >= 0 || oracleIndex >= 0 {
			if setupIndex < 0 || oracleIndex < 0 {
				return nil, fmt.Errorf("ordered command evidence must name one setup command followed by one oracle command")
			}
			return []string{
				fmt.Sprintf("setup_%03d", setupIndex+1),
				fmt.Sprintf("oracle_%03d", oracleIndex+1),
				"acceptance",
			}, nil
		}
		return []string{"behavior"}, nil
	default:
		return nil, fmt.Errorf("unsupported behavior check type %q", check.Type)
	}
}

func validateCheckEvidence(field string, check Check, testCase Case) error {
	if len(check.EvidenceIDs) == 0 {
		return fieldError(field+".evidence_ids", "must not be empty")
	}
	declared := make(map[string]struct{}, len(check.EvidenceIDs))
	for i, evidenceID := range check.EvidenceIDs {
		itemField := fmt.Sprintf("%s.evidence_ids[%d]", field, i)
		if err := validateID(itemField, evidenceID, MaxCaseIDBytes); err != nil {
			return err
		}
		if _, duplicate := declared[evidenceID]; duplicate {
			return fieldError(field+".evidence_ids", "contains duplicate value %q", evidenceID)
		}
		declared[evidenceID] = struct{}{}
	}
	expected, err := expectedCheckEvidenceIDs(testCase, check)
	if err != nil {
		return fieldError(field+".evidence_ids", "%v", err)
	}
	allowed := make(map[string]struct{}, len(expected))
	for _, evidenceID := range expected {
		allowed[evidenceID] = struct{}{}
		if _, present := declared[evidenceID]; !present {
			return fieldError(field+".evidence_ids", "must include observed evidence %q", evidenceID)
		}
	}
	for _, evidenceID := range check.EvidenceIDs {
		if _, valid := allowed[evidenceID]; !valid {
			return fieldError(field+".evidence_ids", "evidence %q is not produced by check type %q", evidenceID, check.Type)
		}
	}
	return nil
}

func commandIndexByID(commands []Command, id string) int {
	for i := range commands {
		if commands[i].ID == id {
			return i
		}
	}
	return -1
}

func validateSecurity(security SecurityConfig, legacy bool) error {
	switch security.Network {
	case NetworkNone, NetworkLoopback, NetworkRegistryAllowlist, NetworkProviderProxyOnly, NetworkHostUnisolated:
	default:
		return fieldError("security.network", "unsupported value %q", security.Network)
	}
	switch security.ExecutionMode {
	case ExecutionTrustedLocal:
		if security.Network != NetworkHostUnisolated {
			return fieldError("security.network", "trusted-local must report %q", NetworkHostUnisolated)
		}
	case ExecutionIsolatedContainer:
		if security.Network == NetworkHostUnisolated {
			return fieldError("security.network", "isolated-container cannot report host-unisolated")
		}
	default:
		return fieldError("security.execution_mode", "unsupported value %q", security.ExecutionMode)
	}
	if security.PackageScripts && security.ExecutionMode != ExecutionIsolatedContainer {
		return fieldError("security.package_scripts", "package scripts require isolated-container")
	}
	if err := validateUniqueStrings("security.allowed_executables", security.AllowedExecutables, false); err != nil {
		return err
	}
	for i, executable := range security.AllowedExecutables {
		if !executablePattern.MatchString(executable) {
			return fieldError(fmt.Sprintf("security.allowed_executables[%d]", i), "must match %s", executablePattern.String())
		}
	}
	for i, root := range security.AllowedWriteRoots {
		if root == "fixture" {
			continue
		}
		if err := ValidateRelativePath(root); err != nil {
			return fieldWrap(fmt.Sprintf("security.allowed_write_roots[%d]", i), err)
		}
	}
	if err := validateUniqueStrings("security.allowed_write_roots", security.AllowedWriteRoots, false); err != nil {
		return err
	}
	if security.Network == NetworkRegistryAllowlist && len(security.AllowedRegistries) == 0 {
		return fieldError("security.allowed_registries", "must not be empty for registry-allowlist")
	}
	if security.Network != NetworkRegistryAllowlist && len(security.AllowedRegistries) != 0 {
		return fieldError("security.allowed_registries", "is only valid for registry-allowlist")
	}
	if err := validateUniqueStrings("security.allowed_registries", security.AllowedRegistries, false); err != nil {
		return err
	}
	for i, registry := range security.AllowedRegistries {
		if len(registry) > 2048 {
			return fieldError(fmt.Sprintf("security.allowed_registries[%d]", i), "exceeds 2048 bytes")
		}
	}
	switch security.RetainTrace {
	case RetainTraceNever, RetainTraceSanitizedOnFailure, RetainTraceSanitizedAlways:
	default:
		return fieldError("security.retain_trace", "unsupported value %q", security.RetainTrace)
	}
	return nil
}

func validateTrace(trace TraceConfig, legacy bool) error {
	if legacy && trace.MaxBytes == 0 && trace.MaxEvents == 0 && trace.MaxEventBytes == 0 {
		return nil
	}
	if trace.MaxBytes < 1 || trace.MaxBytes > MaxTraceBytes {
		return fieldError("trace.max_bytes", "must be between 1 and %d", MaxTraceBytes)
	}
	if trace.MaxEvents < 1 || trace.MaxEvents > MaxTraceEvents {
		return fieldError("trace.max_events", "must be between 1 and %d", MaxTraceEvents)
	}
	if trace.MaxEventBytes < 1 || trace.MaxEventBytes > MaxTraceEventBytes || trace.MaxEventBytes > trace.MaxBytes {
		return fieldError("trace.max_event_bytes", "must be positive and no greater than max_bytes or %d", MaxTraceEventBytes)
	}
	if err := validateDuration("trace.quiescence.quiet_period", trace.Quiescence.QuietPeriod, time.Minute); err != nil {
		return err
	}
	if err := validateDuration("trace.quiescence.timeout", trace.Quiescence.Timeout, MaxCompletionTimeout); err != nil {
		return err
	}
	return nil
}

func validateToolPolicy(policy ToolPolicy, security SecurityConfig) error {
	if err := validateUniqueStrings("tool_policy.allowed_tools", policy.AllowedTools, false); err != nil {
		return err
	}
	if err := validateUniqueStrings("tool_policy.forbidden_tools", policy.ForbiddenTools, false); err != nil {
		return err
	}
	if overlap := firstOverlap(policy.AllowedTools, policy.ForbiddenTools); overlap != "" {
		return fieldError("tool_policy", "tool %q is both allowed and forbidden", overlap)
	}
	names := make(map[string]string, len(policy.FakeMCPs))
	allowedExecutables := make(map[string]struct{}, len(security.AllowedExecutables))
	for _, executable := range security.AllowedExecutables {
		allowedExecutables[executable] = struct{}{}
	}
	for i, fake := range policy.FakeMCPs {
		field := fmt.Sprintf("tool_policy.fake_mcps[%d]", i)
		if err := validateID(field+".name", fake.Name, MaxCaseIDBytes); err != nil {
			return err
		}
		if err := addUniqueID(names, field+".name", fake.Name); err != nil {
			return err
		}
		if len(fake.Tools) == 0 {
			return fieldError(field+".tools", "must declare at least one raw MCP tool name")
		}
		if err := validateUniqueStrings(field+".tools", fake.Tools, false); err != nil {
			return err
		}
		for _, tool := range fake.Tools {
			if !containsString(policy.AllowedTools, tool) {
				return fieldError(field+".tools", "tool %q is not present in tool_policy.allowed_tools", tool)
			}
		}
		switch fake.Transport {
		case "stdio":
			if fake.Command == nil || fake.URL != "" {
				return fieldError(field, "stdio requires command and forbids url")
			}
		default:
			return fieldError(field+".transport", "must be stdio; HTTP fake MCPs are not implemented by the v1 runner")
		}
		if fake.Command != nil {
			if _, ok := allowedExecutables[fake.Command.Argv[0]]; !ok {
				return fieldError(field+".command.argv[0]", "executable %q is not allowlisted", fake.Command.Argv[0])
			}
		}
		for key, value := range fake.Env {
			if !envNamePattern.MatchString(key) || strings.ContainsRune(value, '\x00') {
				return fieldError(field+".env", "contains an invalid environment entry")
			}
		}
	}
	return nil
}

func validateAllowedCommands(c Case) error {
	if c.Migration != nil && len(c.Security.AllowedExecutables) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(c.Security.AllowedExecutables))
	for _, executable := range c.Security.AllowedExecutables {
		allowed[executable] = struct{}{}
	}
	groups := []struct {
		field    string
		commands []Command
	}{
		{"setup.commands", c.Setup.Commands},
		{"oracle.commands", c.Oracle.Commands},
	}
	for _, group := range groups {
		for i, command := range group.commands {
			if _, ok := allowed[command.Argv[0]]; !ok {
				return fieldError(fmt.Sprintf("%s[%d].argv[0]", group.field, i), "executable %q is not allowlisted", command.Argv[0])
			}
		}
	}
	return nil
}

func validateGitSeed(seed GitSeed) error {
	groups := []struct {
		name  string
		files []GitSeedFile
	}{
		{"tracked", seed.Tracked},
		{"staged", seed.Staged},
		{"untracked", seed.Untracked},
		{"ignored", seed.Ignored},
	}
	seen := make(map[string]string)
	for _, group := range groups {
		for i, file := range group.files {
			field := fmt.Sprintf("fixture.git_seed.%s[%d]", group.name, i)
			if err := ValidateRelativePath(file.Path); err != nil {
				return fieldWrap(field+".path", err)
			}
			if previous, exists := seen[file.Path]; exists {
				return fieldError(field+".path", "path %q already appears in %s", file.Path, previous)
			}
			seen[file.Path] = "fixture.git_seed." + group.name
			if (file.Content == nil) == (file.Digest == "") {
				return fieldError(field, "exactly one of content or digest is required")
			}
			if file.Content != nil && len(*file.Content) > MaxInputBytes {
				return fieldError(field+".content", "exceeds %d bytes", MaxInputBytes)
			}
			if file.Digest != "" && !validDigest(file.Digest) {
				return fieldError(field+".digest", "must be a sha256 digest")
			}
			if file.Mode != "" && file.Mode != "0644" && file.Mode != "0755" {
				return fieldError(field+".mode", "must be 0644 or 0755")
			}
		}
	}
	return nil
}

func gitSeedCount(seed GitSeed) int {
	return len(seed.Tracked) + len(seed.Staged) + len(seed.Untracked) + len(seed.Ignored)
}

func validateExpectedFiles(files []ExpectedFile) error {
	if len(files) > MaxPaths {
		return fieldError("oracle.expected_files", "exceeds %d files", MaxPaths)
	}
	seen := make(map[string]struct{}, len(files))
	for i, file := range files {
		field := fmt.Sprintf("oracle.expected_files[%d]", i)
		if err := ValidateRelativePath(file.Path); err != nil {
			return fieldWrap(field+".path", err)
		}
		if _, exists := seen[file.Path]; exists {
			return fieldError(field+".path", "duplicate expected file %q", file.Path)
		}
		seen[file.Path] = struct{}{}
		if (file.Content == nil) == (file.Digest == "") {
			return fieldError(field, "exactly one of content or digest is required")
		}
		if file.Content != nil && len(*file.Content) > MaxInputBytes {
			return fieldError(field+".content", "exceeds %d bytes", MaxInputBytes)
		}
		if file.Digest != "" && !validDigest(file.Digest) {
			return fieldError(field+".digest", "must be a sha256 digest")
		}
		if file.Mode != "" && file.Mode != "0644" && file.Mode != "0755" {
			return fieldError(field+".mode", "must be 0644 or 0755")
		}
	}
	return nil
}

func validateRequirementIDs(field string, ids []string) error {
	if err := validateUniqueStrings(field, ids, false); err != nil {
		return err
	}
	for i, id := range ids {
		if !requirementPattern.MatchString(id) {
			return fieldError(fmt.Sprintf("%s[%d]", field, i), "must be a requirement identifier")
		}
	}
	return nil
}

func overrideRank(source OverrideSource) (int, bool) {
	switch source {
	case OverrideDefaults:
		return 0, true
	case OverrideSuite:
		return 1, true
	case OverrideCase:
		return 2, true
	case OverrideCLI:
		return 3, true
	default:
		return 0, false
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func integerValue(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) || typed > math.MaxInt64 || typed < math.MinInt64 {
			return 0, false
		}
		return int64(typed), true
	default:
		return 0, false
	}
}

func validateDuration(field, value string, maximum time.Duration) error {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fieldError(field, "invalid duration %q", value)
	}
	if duration < time.Second || duration > maximum {
		return fieldError(field, "must be between 1s and %s", maximum)
	}
	return nil
}

func validatePathList(field string, values []string) error {
	if len(values) > MaxPaths {
		return fieldError(field, "exceeds %d paths", MaxPaths)
	}
	if err := validateUniqueStrings(field, values, false); err != nil {
		return err
	}
	for i, value := range values {
		if err := ValidateRelativePath(value); err != nil {
			return fieldWrap(fmt.Sprintf("%s[%d]", field, i), err)
		}
	}
	return nil
}

func validateExtensions(field string, extensions map[string]interface{}) error {
	for key := range extensions {
		if err := validateExtensionName(field, key); err != nil {
			return err
		}
	}
	if _, err := CanonicalJSON(extensions); err != nil {
		return fieldError(field, "must contain canonical JSON-compatible values: %v", err)
	}
	return nil
}

func validateExtensionName(field, key string) error {
	if !extensionPattern.MatchString(key) {
		return fieldError(field, "key %q must use the x- namespace", key)
	}
	return nil
}

func validateUniqueStrings(field string, values []string, validateAsID bool) error {
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if value == "" {
			return fieldError(fmt.Sprintf("%s[%d]", field, i), "must not be empty")
		}
		if validateAsID && !metricPattern.MatchString(value) {
			return fieldError(fmt.Sprintf("%s[%d]", field, i), "must be a metric identifier")
		}
		if _, exists := seen[value]; exists {
			return fieldError(field, "contains duplicate value %q", value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func addUniqueID(seen map[string]string, field, id string) error {
	if previous, exists := seen[id]; exists {
		return fieldError(field, "duplicates %s (%q)", previous, id)
	}
	seen[id] = field
	return nil
}

func firstOverlap(a, b []string) string {
	seen := make(map[string]struct{}, len(a))
	for _, value := range a {
		seen[value] = struct{}{}
	}
	for _, value := range b {
		if _, ok := seen[value]; ok {
			return value
		}
	}
	return ""
}

func validateID(field, value string, maxBytes int) error {
	if value == "" {
		return fieldError(field, "must not be empty")
	}
	if len(value) > maxBytes {
		return fieldError(field, "exceeds %d bytes", maxBytes)
	}
	if !idPattern.MatchString(value) {
		return fieldError(field, "must match %s", idPattern.String())
	}
	return nil
}

func sanitizeID(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	result := strings.Trim(b.String(), "_-")
	if result == "" || result[0] < 'a' || result[0] > 'z' {
		result = "check_" + result
	}
	return result
}

func isWindowsAbsolute(value string) bool {
	return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && value[2] == '/'
}

func SortedExtensionKeys(extensions map[string]interface{}) []string {
	keys := make([]string, 0, len(extensions))
	for key := range extensions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

var (
	idPattern          = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,127}$`)
	metricPattern      = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	envNamePattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	extensionPattern   = regexp.MustCompile(`^x-[a-z0-9][a-z0-9_.-]{0,126}$`)
	executablePattern  = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)
	requirementPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,127}$`)
)

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

func fieldError(field, format string, args ...interface{}) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

func fieldWrap(field string, err error) error {
	return &ValidationError{Field: field, Message: err.Error()}
}
