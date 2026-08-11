package gates

import (
	"fmt"
	"math"

	"github.com/joeldevz/skynex/internal/eval/stats"
)

type Direction string

const (
	LowerOrEqual  Direction = "lower_or_equal"
	HigherOrEqual Direction = "higher_or_equal"
)

type Observation struct {
	Value             *float64     `json:"value,omitempty"`
	Status            stats.Status `json:"status"`
	TelemetryComplete bool         `json:"telemetry_complete"`
}

type Pair struct {
	BlockID   string      `json:"block_id"`
	CaseID    string      `json:"case_id"`
	Control   Observation `json:"control"`
	Candidate Observation `json:"candidate"`
}

// MetricRule is evaluated on paired non-inferiority violations. For a lower-is-
// better ratio r, violation = candidate-r*control. For higher-is-better,
// violation = r*control-candidate. A gate passes only when the CI upper bound is
// <= 0, regresses when the lower bound is > 0, otherwise it is inconclusive.
type MetricRule struct {
	Name      string                `json:"name"`
	Direction Direction             `json:"direction"`
	Ratio     float64               `json:"ratio"`
	Scope     stats.Scope           `json:"scope"`
	Estimator stats.PairedEstimator `json:"estimator,omitempty"`
	Bootstrap stats.BootstrapConfig `json:"bootstrap"`
}

func EvaluateMetric(rule MetricRule, pairs []Pair) Result {
	if err := validateMetricRule(rule); err != nil {
		return invalidResult(rule.Name, ReasonInvalidRule, err.Error())
	}
	seen := make(map[string]struct{}, len(pairs))
	numeric := make([]stats.PairedValue, 0, len(pairs))
	for _, pair := range pairs {
		if pair.BlockID == "" {
			return invalidResult(rule.Name, ReasonEvidenceInvalid, "pair block_id is required")
		}
		if pair.CaseID == "" {
			return invalidResult(rule.Name, ReasonEvidenceInvalid, fmt.Sprintf("block %s has no case_id", pair.BlockID))
		}
		if _, duplicate := seen[pair.BlockID]; duplicate {
			return invalidResult(rule.Name, ReasonEvidenceInvalid, fmt.Sprintf("duplicate block_id %q", pair.BlockID))
		}
		seen[pair.BlockID] = struct{}{}
		if result, terminal := classifyPair(rule, pair); terminal {
			return result
		}
		if !pairEligible(rule.Scope, pair) {
			continue
		}
		if !pair.Control.TelemetryComplete || !pair.Candidate.TelemetryComplete {
			return invalidResult(rule.Name, ReasonTelemetryIncomplete, fmt.Sprintf("block %s has incomplete telemetry", pair.BlockID))
		}
		if pair.Control.Value == nil || pair.Candidate.Value == nil {
			return invalidResult(rule.Name, ReasonMetricMissing, fmt.Sprintf("block %s has missing metric", pair.BlockID))
		}
		if !finite(*pair.Control.Value) || !finite(*pair.Candidate.Value) {
			return invalidResult(rule.Name, ReasonInvalidMetric, fmt.Sprintf("block %s has non-finite metric", pair.BlockID))
		}
		numeric = append(numeric, stats.PairedValue{
			BlockID: pair.BlockID, CaseID: pair.CaseID,
			Control: *pair.Control.Value, Candidate: *pair.Candidate.Value,
		})
	}
	transform := func(control, candidate float64) float64 {
		if rule.Direction == LowerOrEqual {
			return candidate - rule.Ratio*control
		}
		return rule.Ratio*control - candidate
	}
	estimator := rule.Estimator
	if estimator == "" {
		estimator = stats.EstimatorMedian
	}
	var summary stats.PairedSummary
	if estimator == stats.EstimatorMean {
		summary = stats.SummarizeTransformedMean(numeric, rule.Bootstrap, transform)
	} else {
		summary = stats.SummarizeTransformed(numeric, rule.Bootstrap, transform)
	}
	if !summary.CI.Available {
		reason := ReasonEvidenceInvalid
		status := StatusInvalid
		switch summary.CI.Reason {
		case "insufficient_pairs":
			reason = ReasonInsufficientPairs
			status = StatusInconclusive
		case "insufficient_clusters":
			reason = ReasonInsufficientClusters
			status = StatusInconclusive
		}
		return Result{Name: rule.Name, Status: status, Reason: reason, Detail: summary.CI.Reason, Paired: summary}
	}
	if summary.CI.Upper <= 0 {
		return Result{Name: rule.Name, Status: StatusPass, Reason: ReasonGateSatisfied, Paired: summary}
	}
	if summary.CI.Lower > 0 {
		return Result{Name: rule.Name, Status: StatusRegression, Reason: ReasonCIExceedsThreshold, Paired: summary}
	}
	return Result{Name: rule.Name, Status: StatusInconclusive, Reason: ReasonCICrossesThreshold, Paired: summary}
}

func validateMetricRule(rule MetricRule) error {
	if err := validateName(rule.Name); err != nil {
		return err
	}
	if rule.Direction != LowerOrEqual && rule.Direction != HigherOrEqual {
		return fmt.Errorf("unsupported direction %q", rule.Direction)
	}
	if rule.Ratio <= 0 || !finite(rule.Ratio) {
		return fmt.Errorf("ratio must be finite and greater than zero")
	}
	if rule.Scope != stats.ScopeAllCompleted && rule.Scope != stats.ScopeSuccessful {
		return fmt.Errorf("unsupported metric scope %q", rule.Scope)
	}
	if rule.Estimator != "" && rule.Estimator != stats.EstimatorMedian && rule.Estimator != stats.EstimatorMean {
		return fmt.Errorf("unsupported paired estimator %q", rule.Estimator)
	}
	if err := stats.ValidateBootstrapConfig(rule.Bootstrap); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return nil
}

func classifyPair(rule MetricRule, pair Pair) (Result, bool) {
	statuses := []stats.Status{pair.Control.Status, pair.Candidate.Status}
	for _, status := range statuses {
		switch status {
		case stats.StatusInvalid, stats.StatusAborted, stats.StatusInfraError, stats.StatusBudgetExhausted:
			return invalidResult(rule.Name, ReasonEvidenceInvalid, fmt.Sprintf("block %s has status %s", pair.BlockID, status)), true
		case stats.StatusInconclusive:
			return Result{Name: rule.Name, Status: StatusInconclusive, Reason: ReasonEvidenceInconclusive, Detail: fmt.Sprintf("block %s is inconclusive", pair.BlockID)}, true
		case stats.StatusPass, stats.StatusFail:
		default:
			return invalidResult(rule.Name, ReasonEvidenceInvalid, fmt.Sprintf("block %s has unknown status %q", pair.BlockID, status)), true
		}
	}
	// Successful-only efficiency metrics cannot make a cheap failure look like an
	// improvement: every control pass that becomes a candidate fail is an immediate
	// regression before failed samples are excluded.
	if rule.Scope == stats.ScopeSuccessful && pair.Control.Status == stats.StatusPass && pair.Candidate.Status == stats.StatusFail {
		return Result{Name: rule.Name, Status: StatusRegression, Reason: ReasonPassToFail, Detail: fmt.Sprintf("block %s changed pass to fail", pair.BlockID)}, true
	}
	return Result{}, false
}

func pairEligible(scope stats.Scope, pair Pair) bool {
	if scope == stats.ScopeSuccessful {
		return pair.Control.Status == stats.StatusPass && pair.Candidate.Status == stats.StatusPass
	}
	return (pair.Control.Status == stats.StatusPass || pair.Control.Status == stats.StatusFail) &&
		(pair.Candidate.Status == stats.StatusPass || pair.Candidate.Status == stats.StatusFail)
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
