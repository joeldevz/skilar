// Package stats provides deterministic descriptive and paired statistics for
// evaluation evidence. It never discards the raw samples used for an estimate.
package stats

// Status mirrors the stable run-result vocabulary without importing runner or
// contracts, keeping the statistical layer usable by both.
type Status string

const (
	StatusPass            Status = "pass"
	StatusFail            Status = "fail"
	StatusInvalid         Status = "invalid"
	StatusInconclusive    Status = "inconclusive"
	StatusAborted         Status = "aborted"
	StatusInfraError      Status = "infra_error"
	StatusBudgetExhausted Status = "budget_exhausted"
)

// Scope declares which outcomes may contribute numeric measurements.
type Scope string

const (
	ScopeAllCompleted Scope = "all_completed"
	ScopeSuccessful   Scope = "successful_only"
)

// Sample retains a single repetition. Nil Value represents unavailable
// telemetry and is distinct from a measured zero.
type Sample struct {
	ID                string   `json:"id"`
	CaseID            string   `json:"case_id"`
	Variant           string   `json:"variant"`
	Repetition        int      `json:"repetition"`
	Status            Status   `json:"status"`
	Value             *float64 `json:"value,omitempty"`
	TelemetryComplete bool     `json:"telemetry_complete"`
}

// Estimate avoids encoding an unavailable value as zero.
type Estimate struct {
	Available bool    `json:"available"`
	Value     float64 `json:"value,omitempty"`
	N         int     `json:"n"`
	Reason    string  `json:"reason,omitempty"`
}

// Interval is a two-sided confidence interval.
type Interval struct {
	Available  bool    `json:"available"`
	Lower      float64 `json:"lower,omitempty"`
	Upper      float64 `json:"upper,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Method     string  `json:"method,omitempty"`
	N          int     `json:"n"`
	Reason     string  `json:"reason,omitempty"`
}

// SummaryConfig controls when descriptive estimates are allowed. Defaults are
// deliberately conservative: median needs 3 samples and p95 needs 20.
type SummaryConfig struct {
	Scope              Scope
	MinimumForMedian   int
	MinimumForQuantile int
	Quantiles          []float64
}

// Summary includes every source sample plus the eligible values and explicit
// unavailable counts. Quantile keys use a stable label such as p95.
type Summary struct {
	Samples          []Sample            `json:"samples"`
	EligibleValues   []float64           `json:"eligible_values"`
	Total            int                 `json:"total"`
	Eligible         int                 `json:"eligible"`
	Unavailable      int                 `json:"unavailable"`
	ExcludedByStatus int                 `json:"excluded_by_status"`
	Median           Estimate            `json:"median"`
	Quantiles        map[string]Estimate `json:"quantiles"`
}

// Outcome is the non-numeric input for reliability rates.
type Outcome struct {
	ID         string `json:"id"`
	CaseID     string `json:"case_id"`
	Variant    string `json:"variant"`
	Repetition int    `json:"repetition"`
	Status     Status `json:"status"`
}

// Rate carries its numerator and denominator so empty populations cannot look
// like a measured zero rate.
type Rate struct {
	Available   bool    `json:"available"`
	Value       float64 `json:"value,omitempty"`
	Numerator   int     `json:"numerator"`
	Denominator int     `json:"denominator"`
	Reason      string  `json:"reason,omitempty"`
}

// ReliabilitySummary reports run outcomes and case-level flakiness.
type ReliabilitySummary struct {
	Outcomes           []Outcome      `json:"outcomes"`
	Counts             map[Status]int `json:"counts"`
	PassRate           Rate           `json:"pass_rate"`
	FailureRate        Rate           `json:"failure_rate"`
	InvalidRate        Rate           `json:"invalid_rate"`
	InconclusiveRate   Rate           `json:"inconclusive_rate"`
	InfrastructureRate Rate           `json:"infrastructure_rate"`
	FlakyRate          Rate           `json:"flaky_rate"`
	FlakyCaseVariants  []string       `json:"flaky_case_variants"`
}
