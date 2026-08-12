package contracts

import (
	"fmt"
	"math"
	"regexp"
	"unicode/utf8"
)

const ResultSchemaVersion = 1

const (
	maxResultChecks        = 10_000
	maxResultEvidenceItems = 100_000
	maxResultKindLength    = 256
	maxResultSummaryLength = 65_536
	maxResultTreeLength    = 65_536
	maxResultPathLength    = 4_096
	maxResultHostLength    = 128
)

// Process exit codes are stable machine-facing classifications. A run status
// maps one-to-one except that a successful run is zero.
const (
	ExitSuccess         = 0
	ExitFailed          = 1
	ExitInvalid         = 2
	ExitInconclusive    = 3
	ExitAborted         = 4
	ExitInfrastructure  = 5
	ExitBudgetExhausted = 6
)

type RunStatus string

const (
	RunStatusPass            RunStatus = "pass"
	RunStatusFail            RunStatus = "fail"
	RunStatusInvalid         RunStatus = "invalid"
	RunStatusInconclusive    RunStatus = "inconclusive"
	RunStatusAborted         RunStatus = "aborted"
	RunStatusInfraError      RunStatus = "infra_error"
	RunStatusBudgetExhausted RunStatus = "budget_exhausted"
)

func (s RunStatus) Valid() bool {
	switch s {
	case RunStatusPass, RunStatusFail, RunStatusInvalid, RunStatusInconclusive, RunStatusAborted, RunStatusInfraError, RunStatusBudgetExhausted:
		return true
	default:
		return false
	}
}

func (s RunStatus) ExitCode() int {
	switch s {
	case RunStatusPass:
		return ExitSuccess
	case RunStatusFail:
		return ExitFailed
	case RunStatusInvalid:
		return ExitInvalid
	case RunStatusInconclusive:
		return ExitInconclusive
	case RunStatusAborted:
		return ExitAborted
	case RunStatusInfraError:
		return ExitInfrastructure
	case RunStatusBudgetExhausted:
		return ExitBudgetExhausted
	default:
		return ExitInvalid
	}
}

type CheckStatus string

const (
	CheckStatusPass    CheckStatus = "pass"
	CheckStatusFail    CheckStatus = "fail"
	CheckStatusInvalid CheckStatus = "invalid"
	CheckStatusSkipped CheckStatus = "skipped"
)

type EvidenceSource string

const (
	EvidenceEvaluator EvidenceSource = "evaluator"
	EvidenceProvider  EvidenceSource = "provider"
	EvidenceLLMJudge  EvidenceSource = "llm_judge"
)

// RunResult is one immutable repetition. Aggregates must refer to RunResult
// values rather than replacing them with a summary sample.
type RunResult struct {
	SchemaVersion     int           `json:"schema_version"`
	RunID             string        `json:"run_id"`
	CaseID            string        `json:"case_id"`
	Variant           string        `json:"variant"`
	Repetition        int           `json:"repetition"`
	Status            RunStatus     `json:"status"`
	Provenance        Provenance    `json:"provenance"`
	Checks            []CheckResult `json:"checks"`
	Usage             Usage         `json:"usage"`
	Coordination      Coordination  `json:"coordination"`
	Timing            Timing        `json:"timing"`
	Evidence          Evidence      `json:"evidence"`
	TelemetryComplete bool          `json:"telemetry_complete"`
	Error             *RunError     `json:"error"`
}

type Provenance struct {
	GitSHA             string            `json:"git_sha"`
	CaseDigest         string            `json:"case_digest"`
	PromptDigest       string            `json:"prompt_digest"`
	ConfigDigest       string            `json:"config_digest"`
	FixtureDigest      string            `json:"fixture_digest"`
	OpenCodeVersion    string            `json:"opencode_version"`
	OpenCodeAPIDigest  string            `json:"opencode_api_digest,omitempty"`
	Model              string            `json:"model"`
	Provider           string            `json:"provider"`
	ToolsetDigest      string            `json:"toolset_digest"`
	JudgeDigest        string            `json:"judge_digest,omitempty"`
	PricingTableDigest string            `json:"pricing_table_digest"`
	ExecutionMode      ExecutionMode     `json:"execution_mode"`
	Network            NetworkPolicy     `json:"network"`
	Host               HostProvenance    `json:"host"`
	Extensions         map[string]string `json:"extensions,omitempty"`
}

type HostProvenance struct {
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Runtime string `json:"runtime,omitempty"`
}

type CheckResult struct {
	ID             string      `json:"id"`
	Type           string      `json:"type"`
	Status         CheckStatus `json:"status"`
	Hard           bool        `json:"hard"`
	Summary        string      `json:"summary"`
	RequirementIDs []string    `json:"requirement_ids"`
	EvidenceIDs    []string    `json:"evidence_ids"`
	Error          *RunError   `json:"error,omitempty"`
}

type TokenUsage struct {
	FirstInputTokens  int64    `json:"first_input_tokens"`
	PeakInputTokens   int64    `json:"peak_input_tokens"`
	SumInputTokens    int64    `json:"sum_input_tokens"`
	OutputTokens      int64    `json:"output_tokens"`
	CacheReadTokens   int64    `json:"cache_read_tokens"`
	CacheWriteTokens  int64    `json:"cache_write_tokens"`
	ReasoningTokens   int64    `json:"reasoning_tokens,omitempty"`
	ProviderCostUSD   *float64 `json:"cost_usd,omitempty"`
	CalculatedCostUSD *float64 `json:"calculated_cost_usd,omitempty"`
}

type TreeUsage struct {
	SumInputTokens    int64    `json:"sum_input_tokens"`
	OutputTokens      int64    `json:"output_tokens"`
	CacheReadTokens   int64    `json:"cache_read_tokens"`
	CacheWriteTokens  int64    `json:"cache_write_tokens"`
	ReasoningTokens   int64    `json:"reasoning_tokens,omitempty"`
	ProviderCostUSD   *float64 `json:"cost_usd,omitempty"`
	CalculatedCostUSD *float64 `json:"calculated_cost_usd,omitempty"`
	Sessions          int      `json:"sessions"`
}

type Usage struct {
	Parent TokenUsage `json:"parent"`
	Tree   TreeUsage  `json:"tree"`
}

type Coordination struct {
	ToolCalls        int `json:"tool_calls"`
	SubagentCalls    int `json:"subagent_calls"`
	Retries          int `json:"retries"`
	RepeatedCommands int `json:"repeated_commands"`
	RepeatedReads    int `json:"repeated_reads"`
}

type Timing struct {
	WallMS  int64 `json:"wall_ms"`
	ModelMS int64 `json:"model_ms"`
}

type Evidence struct {
	BeforeTree  string         `json:"before_tree"`
	AfterTree   string         `json:"after_tree"`
	DiffDigest  string         `json:"diff_digest"`
	TraceDigest string         `json:"trace_digest"`
	TracePath   string         `json:"trace_path"`
	Items       []EvidenceItem `json:"items"`
}

type EvidenceItem struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Source   EvidenceSource `json:"source"`
	Digest   string         `json:"digest"`
	Path     string         `json:"path,omitempty"`
	Summary  string         `json:"summary,omitempty"`
	Complete bool           `json:"complete"`
}

type RunError struct {
	Kind        string   `json:"kind"`
	Message     string   `json:"message"`
	Retryable   bool     `json:"retryable"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
}

func (r RunResult) Digest() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	return CanonicalDigest(r)
}

func (r RunResult) Validate() error {
	if r.SchemaVersion != ResultSchemaVersion {
		return fieldError("schema_version", "must equal %d", ResultSchemaVersion)
	}
	if err := validateID("run_id", r.RunID, MaxCaseIDBytes); err != nil {
		return err
	}
	if err := validateID("case_id", r.CaseID, MaxCaseIDBytes); err != nil {
		return err
	}
	if err := validateID("variant", r.Variant, MaxCaseIDBytes); err != nil {
		return err
	}
	if r.Repetition < 1 || r.Repetition > MaxRuns {
		return fieldError("repetition", "must be between 1 and %d", MaxRuns)
	}
	if !r.Status.Valid() {
		return fieldError("status", "unsupported value %q", r.Status)
	}
	if err := r.Provenance.validate(); err != nil {
		return err
	}
	if err := validateSchemaString("evidence.before_tree", r.Evidence.BeforeTree, 0, maxResultTreeLength); err != nil {
		return err
	}
	if err := validateSchemaString("evidence.after_tree", r.Evidence.AfterTree, 0, maxResultTreeLength); err != nil {
		return err
	}
	if err := validateSchemaString("evidence.trace_path", r.Evidence.TracePath, 0, maxResultPathLength); err != nil {
		return err
	}
	if r.Evidence.Items == nil {
		return fieldError("evidence.items", "must be an array")
	}
	if len(r.Evidence.Items) > maxResultEvidenceItems {
		return fieldError("evidence.items", "exceeds %d items", maxResultEvidenceItems)
	}
	evidenceIDs := make(map[string]string, len(r.Evidence.Items))
	evidenceByID := make(map[string]EvidenceItem, len(r.Evidence.Items))
	for i, item := range r.Evidence.Items {
		field := fmt.Sprintf("evidence.items[%d]", i)
		if err := validateID(field+".id", item.ID, MaxCaseIDBytes); err != nil {
			return err
		}
		if err := addUniqueID(evidenceIDs, field+".id", item.ID); err != nil {
			return err
		}
		evidenceByID[item.ID] = item
		if err := validateSchemaString(field+".kind", item.Kind, 1, maxResultKindLength); err != nil {
			return err
		}
		switch item.Source {
		case EvidenceEvaluator, EvidenceProvider, EvidenceLLMJudge:
		default:
			return fieldError(field+".source", "unsupported value %q", item.Source)
		}
		if !validDigest(item.Digest) {
			return fieldError(field+".digest", "must be a sha256 digest")
		}
		if err := validateSchemaString(field+".path", item.Path, 0, maxResultPathLength); err != nil {
			return err
		}
		if err := validateSchemaString(field+".summary", item.Summary, 0, maxResultSummaryLength); err != nil {
			return err
		}
	}
	if r.Checks == nil {
		return fieldError("checks", "must be an array")
	}
	if len(r.Checks) > maxResultChecks {
		return fieldError("checks", "exceeds %d items", maxResultChecks)
	}
	checkIDs := make(map[string]string, len(r.Checks))
	allHardPassed := true
	hardChecks := 0
	for i, check := range r.Checks {
		field := fmt.Sprintf("checks[%d]", i)
		if err := validateID(field+".id", check.ID, MaxCaseIDBytes); err != nil {
			return err
		}
		if err := addUniqueID(checkIDs, field+".id", check.ID); err != nil {
			return err
		}
		if err := validateSchemaString(field+".type", check.Type, 1, maxResultKindLength); err != nil {
			return err
		}
		if err := validateSchemaString(field+".summary", check.Summary, 0, maxResultSummaryLength); err != nil {
			return err
		}
		if len(check.RequirementIDs) == 0 {
			return fieldError(field+".requirement_ids", "must not be empty")
		}
		if err := validateRequirementIDs(field+".requirement_ids", check.RequirementIDs); err != nil {
			return err
		}
		switch check.Status {
		case CheckStatusPass, CheckStatusFail, CheckStatusInvalid, CheckStatusSkipped:
		default:
			return fieldError(field+".status", "unsupported value %q", check.Status)
		}
		if check.Hard {
			hardChecks++
			if check.Status != CheckStatusPass {
				allHardPassed = false
			}
		}
		if err := validateEvidenceReferences(field+".evidence_ids", check.EvidenceIDs, true, evidenceIDs); err != nil {
			return err
		}
		if r.Status == RunStatusPass && check.Hard && check.Status == CheckStatusPass {
			if len(check.EvidenceIDs) == 0 {
				return fieldError(field+".evidence_ids", "must not be empty for a passing hard check")
			}
			for j, evidenceID := range check.EvidenceIDs {
				if !evidenceByID[evidenceID].Complete {
					return fieldError(fmt.Sprintf("%s.evidence_ids[%d]", field, j), "references incomplete evidence %q", evidenceID)
				}
			}
		}
		if err := validateRunError(field+".error", check.Error, evidenceIDs); err != nil {
			return err
		}
		if check.Status == CheckStatusPass && check.Error != nil {
			return fieldError(field+".error", "must be null or omitted for a passing check")
		}
	}
	if r.Status == RunStatusPass {
		if hardChecks == 0 {
			return fieldError("checks", "a passing run requires at least one hard check")
		}
		if !allHardPassed {
			return fieldError("status", "pass is impossible while a hard check is not passing")
		}
		if r.Error != nil {
			return fieldError("error", "must be null for a passing run")
		}
	}
	if err := validateRunError("error", r.Error, evidenceIDs); err != nil {
		return err
	}
	if err := validateUsage(r.Usage); err != nil {
		return err
	}
	if !r.Provenance.ProviderCostUSDAuthoritative() {
		if r.Usage.Parent.ProviderCostUSD != nil {
			return fieldError("usage.parent.cost_usd", "must be omitted for %s billing", BillingModeChatGPTSubscription)
		}
		if r.Usage.Tree.ProviderCostUSD != nil {
			return fieldError("usage.tree.cost_usd", "must be omitted for %s billing", BillingModeChatGPTSubscription)
		}
	}
	if r.Coordination.ToolCalls < 0 || r.Coordination.SubagentCalls < 0 || r.Coordination.Retries < 0 || r.Coordination.RepeatedCommands < 0 || r.Coordination.RepeatedReads < 0 {
		return fieldError("coordination", "counts must be non-negative")
	}
	if r.Timing.WallMS < 0 || r.Timing.ModelMS < 0 || r.Timing.ModelMS > r.Timing.WallMS {
		return fieldError("timing", "durations must be non-negative and model_ms must not exceed wall_ms")
	}
	for field, digest := range map[string]string{
		"evidence.diff_digest":  r.Evidence.DiffDigest,
		"evidence.trace_digest": r.Evidence.TraceDigest,
	} {
		if digest != "" && !validDigest(digest) {
			return fieldError(field, "must be a sha256 digest")
		}
	}
	return nil
}

func (p Provenance) validate() error {
	for field, digest := range map[string]string{
		"provenance.case_digest":          p.CaseDigest,
		"provenance.prompt_digest":        p.PromptDigest,
		"provenance.config_digest":        p.ConfigDigest,
		"provenance.fixture_digest":       p.FixtureDigest,
		"provenance.toolset_digest":       p.ToolsetDigest,
		"provenance.pricing_table_digest": p.PricingTableDigest,
	} {
		if !validDigest(digest) {
			return fieldError(field, "must be a sha256 digest")
		}
	}
	for field, digest := range map[string]string{
		"provenance.opencode_api_digest": p.OpenCodeAPIDigest,
		"provenance.judge_digest":        p.JudgeDigest,
	} {
		if digest != "" && !validDigest(digest) {
			return fieldError(field, "must be a sha256 digest")
		}
	}
	if !resultGitSHAPattern.MatchString(p.GitSHA) {
		return fieldError("provenance.git_sha", "must contain 40 to 64 lowercase hexadecimal characters")
	}
	for field, value := range map[string]string{
		"provenance.opencode_version": p.OpenCodeVersion,
		"provenance.model":            p.Model,
		"provenance.provider":         p.Provider,
	} {
		if err := validateSchemaString(field, value, 1, maxResultKindLength); err != nil {
			return err
		}
	}
	parsedProvider, _, err := ParseModelSelection(p.Model)
	if err != nil {
		return fieldError("provenance.model", "must be a valid provider/model selection: %v", err)
	}
	if parsedProvider != p.Provider {
		return fieldError("provenance.provider", "must equal the provider parsed from provenance.model")
	}
	switch p.Network {
	case NetworkNone, NetworkLoopback, NetworkRegistryAllowlist, NetworkProviderProxyOnly, NetworkHostUnisolated:
	default:
		return fieldError("provenance.network", "unsupported value %q", p.Network)
	}
	switch p.ExecutionMode {
	case ExecutionTrustedLocal:
		if p.Network != NetworkHostUnisolated {
			return fieldError("provenance.network", "trusted-local must report host-unisolated")
		}
	case ExecutionIsolatedContainer:
		if p.Network == NetworkHostUnisolated {
			return fieldError("provenance.network", "isolated-container cannot report host-unisolated")
		}
	default:
		return fieldError("provenance.execution_mode", "unsupported value %q", p.ExecutionMode)
	}
	if err := validateSchemaString("provenance.host.os", p.Host.OS, 1, maxResultHostLength); err != nil {
		return err
	}
	if err := validateSchemaString("provenance.host.arch", p.Host.Arch, 1, maxResultHostLength); err != nil {
		return err
	}
	if err := validateSchemaString("provenance.host.runtime", p.Host.Runtime, 0, maxResultKindLength); err != nil {
		return err
	}
	for key := range p.Extensions {
		if err := validateExtensionName("provenance.extensions", key); err != nil {
			return err
		}
	}
	if err := validateProviderBillingProvenance(p); err != nil {
		return err
	}
	return nil
}

func validateUsage(usage Usage) error {
	values := []int64{
		usage.Parent.FirstInputTokens, usage.Parent.PeakInputTokens, usage.Parent.SumInputTokens, usage.Parent.OutputTokens,
		usage.Parent.CacheReadTokens, usage.Parent.CacheWriteTokens, usage.Parent.ReasoningTokens,
		usage.Tree.SumInputTokens, usage.Tree.OutputTokens, usage.Tree.CacheReadTokens,
		usage.Tree.CacheWriteTokens, usage.Tree.ReasoningTokens,
	}
	for _, value := range values {
		if value < 0 {
			return fieldError("usage", "token counts must be non-negative")
		}
	}
	if usage.Parent.FirstInputTokens > usage.Parent.PeakInputTokens {
		return fieldError("usage.parent.first_input_tokens", "must not exceed peak_input_tokens")
	}
	if usage.Parent.ProviderCostUSD != nil && !finiteNonNegative(*usage.Parent.ProviderCostUSD) {
		return fieldError("usage.parent.cost_usd", "must be non-negative")
	}
	if usage.Tree.ProviderCostUSD != nil && !finiteNonNegative(*usage.Tree.ProviderCostUSD) {
		return fieldError("usage.tree.cost_usd", "must be non-negative")
	}
	if usage.Parent.CalculatedCostUSD != nil && !finiteNonNegative(*usage.Parent.CalculatedCostUSD) {
		return fieldError("usage.parent.calculated_cost_usd", "must be non-negative")
	}
	if usage.Tree.CalculatedCostUSD != nil && !finiteNonNegative(*usage.Tree.CalculatedCostUSD) {
		return fieldError("usage.tree.calculated_cost_usd", "must be non-negative")
	}
	if usage.Tree.Sessions < 0 {
		return fieldError("usage.tree.sessions", "must be non-negative")
	}
	return nil
}

func finiteNonNegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func validateRunError(field string, value *RunError, evidenceIDs map[string]string) error {
	if value == nil {
		return nil
	}
	if err := validateSchemaString(field+".kind", value.Kind, 1, maxResultKindLength); err != nil {
		return err
	}
	if err := validateSchemaString(field+".message", value.Message, 1, maxResultSummaryLength); err != nil {
		return err
	}
	return validateEvidenceReferences(field+".evidence_ids", value.EvidenceIDs, false, evidenceIDs)
}

func validateEvidenceReferences(field string, ids []string, requiredArray bool, evidenceIDs map[string]string) error {
	if requiredArray && ids == nil {
		return fieldError(field, "must be an array")
	}
	seen := make(map[string]string, len(ids))
	for i, id := range ids {
		itemField := fmt.Sprintf("%s[%d]", field, i)
		if err := validateID(itemField, id, MaxCaseIDBytes); err != nil {
			return err
		}
		if err := addUniqueID(seen, itemField, id); err != nil {
			return err
		}
		if _, exists := evidenceIDs[id]; !exists {
			return fieldError(itemField, "references unknown evidence %q", id)
		}
	}
	return nil
}

func validateSchemaString(field, value string, minimum, maximum int) error {
	if !utf8.ValidString(value) {
		return fieldError(field, "must contain valid UTF-8")
	}
	length := utf8.RuneCountInString(value)
	if length < minimum || length > maximum {
		if minimum == 0 {
			return fieldError(field, "exceeds %d characters", maximum)
		}
		return fieldError(field, "must contain between %d and %d characters", minimum, maximum)
	}
	return nil
}

var resultGitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
