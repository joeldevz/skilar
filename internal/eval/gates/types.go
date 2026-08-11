// Package gates turns compatibility, correctness, and paired efficiency
// evidence into stable process decisions.
package gates

import (
	"fmt"

	"github.com/joeldevz/skynex/internal/eval/stats"
)

type Status string

const (
	StatusPass          Status = "pass"
	StatusNotApplicable Status = "not_applicable"
	StatusRegression    Status = "regression"
	StatusInconclusive  Status = "inconclusive"
	StatusInvalid       Status = "invalid"
	StatusInfraError    Status = "infra_error"
)

// Stable process exit codes. These are deliberately small and disjoint so CI
// can distinguish product regression from untrustworthy evidence.
const (
	ExitPass         = 0
	ExitRegression   = 1
	ExitInvalid      = 2
	ExitInconclusive = 3
	ExitAborted      = 4 // reserved to match run-status exit codes
	ExitInfraError   = 5
)

type ReasonCode string

const (
	ReasonGateSatisfied        ReasonCode = "gate_satisfied"
	ReasonNotApplicable        ReasonCode = "not_applicable"
	ReasonHardGateFailed       ReasonCode = "hard_gate_failed"
	ReasonPassToFail           ReasonCode = "pass_to_fail_regression"
	ReasonCompatibility        ReasonCode = "compatibility_mismatch"
	ReasonTelemetryIncomplete  ReasonCode = "telemetry_incomplete"
	ReasonMetricMissing        ReasonCode = "metric_missing"
	ReasonInvalidMetric        ReasonCode = "invalid_metric"
	ReasonInvalidRule          ReasonCode = "invalid_gate_rule"
	ReasonInsufficientPairs    ReasonCode = "insufficient_pairs"
	ReasonInsufficientClusters ReasonCode = "insufficient_clusters"
	ReasonCICrossesThreshold   ReasonCode = "confidence_interval_crosses_threshold"
	ReasonCIExceedsThreshold   ReasonCode = "confidence_interval_exceeds_threshold"
	ReasonEvidenceInvalid      ReasonCode = "evidence_invalid"
	ReasonEvidenceInconclusive ReasonCode = "evidence_inconclusive"
	ReasonInfrastructure       ReasonCode = "infrastructure_error"
)

type Result struct {
	Name   string              `json:"name"`
	Status Status              `json:"status"`
	Reason ReasonCode          `json:"reason"`
	Detail string              `json:"detail,omitempty"`
	Paired stats.PairedSummary `json:"paired,omitempty"`
}

type Decision struct {
	Status   Status   `json:"status"`
	ExitCode int      `json:"exit_code"`
	Reasons  []Reason `json:"reasons"`
	Results  []Result `json:"results"`
}

type Reason struct {
	Gate   string     `json:"gate"`
	Code   ReasonCode `json:"code"`
	Detail string     `json:"detail,omitempty"`
}

// Combine applies stable precedence: infrastructure and invalid evidence outrank
// a measured regression; regression outranks statistical uncertainty.
func Combine(results ...Result) Decision {
	decision := Decision{Status: StatusPass, ExitCode: ExitPass, Results: append([]Result(nil), results...)}
	for _, result := range results {
		if result.Status != StatusPass && result.Status != StatusNotApplicable {
			decision.Reasons = append(decision.Reasons, Reason{Gate: result.Name, Code: result.Reason, Detail: result.Detail})
		}
		if statusPrecedence(result.Status) > statusPrecedence(decision.Status) {
			decision.Status = result.Status
		}
	}
	decision.ExitCode = ExitCode(decision.Status)
	return decision
}

func ExitCode(status Status) int {
	switch status {
	case StatusPass:
		return ExitPass
	case StatusNotApplicable:
		return ExitPass
	case StatusRegression:
		return ExitRegression
	case StatusInvalid:
		return ExitInvalid
	case StatusInconclusive:
		return ExitInconclusive
	case StatusInfraError:
		return ExitInfraError
	default:
		return ExitInvalid
	}
}

func statusPrecedence(status Status) int {
	switch status {
	case StatusInfraError:
		return 5
	case StatusInvalid:
		return 4
	case StatusRegression:
		return 3
	case StatusInconclusive:
		return 2
	case StatusPass:
		return 1
	case StatusNotApplicable:
		return 0
	default:
		return 4
	}
}

func invalidResult(name string, reason ReasonCode, detail string) Result {
	return Result{Name: name, Status: StatusInvalid, Reason: reason, Detail: detail}
}

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("gate name is required")
	}
	return nil
}
