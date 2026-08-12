package gates

import (
	"fmt"
	"math"
)

// HardThresholds are deterministic authorities evaluated before efficiency.
type HardThresholds struct {
	CriticalCasePassRate float64 `json:"critical_case_pass_rate"`
	PassToFailMaximum    int     `json:"pass_to_fail_regressions"`
	ScopeViolationMax    int     `json:"scope_violations"`
	FalseSuccessMax      int     `json:"false_successes"`
}

// HardEvidence uses pointers so missing evidence cannot be mistaken for zero.
type HardEvidence struct {
	CriticalCasePassRate *float64 `json:"critical_case_pass_rate,omitempty"`
	PassToFail           *int     `json:"pass_to_fail_regressions,omitempty"`
	ScopeViolations      *int     `json:"scope_violations,omitempty"`
	FalseSuccesses       *int     `json:"false_successes,omitempty"`
	TelemetryComplete    *bool    `json:"telemetry_complete,omitempty"`
}

func EvaluateHardGates(thresholds HardThresholds, evidence HardEvidence) []Result {
	if thresholds.CriticalCasePassRate < 0 || thresholds.CriticalCasePassRate > 1 || math.IsNaN(thresholds.CriticalCasePassRate) ||
		thresholds.PassToFailMaximum < 0 || thresholds.ScopeViolationMax < 0 || thresholds.FalseSuccessMax < 0 {
		return []Result{invalidResult("hard_gates", ReasonInvalidRule, "hard-gate thresholds are invalid")}
	}
	results := make([]Result, 0, 5)
	results = append(results, evaluateMinimum("critical_case_pass_rate", evidence.CriticalCasePassRate, thresholds.CriticalCasePassRate))
	results = append(results, evaluateMaximum("pass_to_fail_regressions", evidence.PassToFail, thresholds.PassToFailMaximum))
	results = append(results, evaluateMaximum("scope_violations", evidence.ScopeViolations, thresholds.ScopeViolationMax))
	results = append(results, evaluateMaximum("false_successes", evidence.FalseSuccesses, thresholds.FalseSuccessMax))
	// Quality-only comparisons need no efficiency telemetry. When a caller makes
	// an efficiency claim it opts into this hard precondition by providing the
	// field; EvaluateMetric also checks completeness for every contributing pair.
	if evidence.TelemetryComplete == nil {
		return results
	}
	if !*evidence.TelemetryComplete {
		results = append(results, invalidResult("telemetry_complete", ReasonTelemetryIncomplete, "efficiency telemetry is incomplete"))
	} else {
		results = append(results, Result{Name: "telemetry_complete", Status: StatusPass, Reason: ReasonGateSatisfied})
	}
	return results
}

func evaluateMinimum(name string, observed *float64, minimum float64) Result {
	if observed == nil {
		return invalidResult(name, ReasonMetricMissing, "hard-gate evidence is missing")
	}
	if !finite(*observed) || *observed < 0 || *observed > 1 {
		return invalidResult(name, ReasonInvalidMetric, "hard-gate evidence is outside [0,1]")
	}
	if *observed < minimum {
		return Result{Name: name, Status: StatusRegression, Reason: ReasonHardGateFailed, Detail: fmt.Sprintf("observed %.6g is below minimum %.6g", *observed, minimum)}
	}
	return Result{Name: name, Status: StatusPass, Reason: ReasonGateSatisfied}
}

func evaluateMaximum(name string, observed *int, maximum int) Result {
	if observed == nil {
		return invalidResult(name, ReasonMetricMissing, "hard-gate evidence is missing")
	}
	if *observed < 0 {
		return invalidResult(name, ReasonInvalidMetric, "hard-gate count is negative")
	}
	if *observed > maximum {
		return Result{Name: name, Status: StatusRegression, Reason: ReasonHardGateFailed, Detail: fmt.Sprintf("observed %d exceeds maximum %d", *observed, maximum)}
	}
	return Result{Name: name, Status: StatusPass, Reason: ReasonGateSatisfied}
}
