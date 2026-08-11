package metrics

import "fmt"

// TokenUsage keeps billing and context token classes separate. Callers must not
// infer input tokens by subtracting cache tokens from another field: providers
// disagree about whether their input total includes cached tokens.
type TokenUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Validate rejects provider data which cannot represent real token usage.
func (u TokenUsage) Validate() error {
	if u.Input < 0 || u.Output < 0 || u.Reasoning < 0 || u.CacheRead < 0 || u.CacheWrite < 0 {
		return fmt.Errorf("token counts must be non-negative")
	}
	return nil
}

// CostSource records which authority supplied a cost.
type CostSource string

const (
	CostSourceProvider   CostSource = "provider"
	CostSourceCalculated CostSource = "calculated"
)

// CostValue represents a cost without using zero as a sentinel for missing
// pricing. A free request is Available with USD == 0; an unknown price is not
// Available and carries a stable Reason.
type CostValue struct {
	Available bool       `json:"available"`
	USD       float64    `json:"usd,omitempty"`
	Source    CostSource `json:"source"`
	Reason    string     `json:"reason,omitempty"`
}

// MessageUsage is the smallest deduplication unit. MessageID must be stable
// within its session. Sequence is used only to identify first/peak context; it
// is not part of identity.
type MessageUsage struct {
	SessionID      string     `json:"session_id"`
	MessageID      string     `json:"message_id"`
	Sequence       int        `json:"sequence"`
	Provider       string     `json:"provider,omitempty"`
	Model          string     `json:"model,omitempty"`
	Tokens         TokenUsage `json:"tokens"`
	ProviderCost   CostValue  `json:"provider_cost"`
	CalculatedCost CostValue  `json:"calculated_cost"`
	DurationMS     int64      `json:"duration_ms,omitempty"`
}

// SessionUsage contains direct messages for exactly one session. Descendants
// are represented as other SessionUsage values linked by ParentID.
type SessionUsage struct {
	SessionID string         `json:"session_id"`
	ParentID  string         `json:"parent_id,omitempty"`
	Messages  []MessageUsage `json:"messages"`
}

// Issue identifies why telemetry could not be treated as authoritative.
type Issue struct {
	Code      string `json:"code"`
	SessionID string `json:"session_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Fatal     bool   `json:"fatal"`
}

// CostTotal retains the sum of known values even when the complete total is
// unavailable. Consumers may display KnownUSD as partial evidence, but gates
// must use USD only when Available is true.
type CostTotal struct {
	Available          bool     `json:"available"`
	USD                float64  `json:"usd,omitempty"`
	KnownUSD           float64  `json:"known_usd"`
	UnavailableValues  int      `json:"unavailable_values"`
	UnavailableReasons []string `json:"unavailable_reasons,omitempty"`
}

// Aggregate is a deduplicated usage summary for one session or a reachable
// session tree.
type Aggregate struct {
	Tokens           TokenUsage `json:"tokens"`
	FirstInputTokens int64      `json:"first_input_tokens"`
	PeakInputTokens  int64      `json:"peak_input_tokens"`
	Messages         int        `json:"messages"`
	Sessions         int        `json:"sessions"`
	DurationMS       int64      `json:"duration_ms"`
	ProviderCost     CostTotal  `json:"provider_cost"`
	CalculatedCost   CostTotal  `json:"calculated_cost"`
	Complete         bool       `json:"complete"`
	Issues           []Issue    `json:"issues,omitempty"`
}

// UsageSummary keeps parent-only and full-tree evidence separate.
type UsageSummary struct {
	Parent Aggregate `json:"parent"`
	Tree   Aggregate `json:"tree"`
}
