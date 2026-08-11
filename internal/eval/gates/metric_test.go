package gates

import (
	"fmt"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/stats"
)

func TestEvaluateMetricUsesConfidenceInterval(t *testing.T) {
	tests := []struct {
		name       string
		candidates []float64
		want       Status
		wantReason ReasonCode
	}{
		{"pass", []float64{8, 8, 8, 8, 8}, StatusPass, ReasonGateSatisfied},
		{"regression", []float64{12, 12, 12, 12, 12}, StatusRegression, ReasonCIExceedsThreshold},
		{"crosses", []float64{5, 5, 10, 15, 15}, StatusInconclusive, ReasonCICrossesThreshold},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pairs := makePairs(10, test.candidates, stats.StatusPass, stats.StatusPass)
			result := EvaluateMetric(testRule(stats.ScopeAllCompleted), pairs)
			if result.Status != test.want || result.Reason != test.wantReason {
				t.Fatalf("result = %+v", result)
			}
			if !result.Paired.CI.Available {
				t.Fatalf("CI unavailable: %+v", result.Paired)
			}
		})
	}
}

func TestEvaluateMetricHandlesZeroBaselineWithoutDivision(t *testing.T) {
	zeros := []float64{0, 0, 0, 0, 0}
	result := EvaluateMetric(testRule(stats.ScopeAllCompleted), makePairs(0, zeros, stats.StatusPass, stats.StatusPass))
	if result.Status != StatusPass || result.Paired.CI.Lower != 0 || result.Paired.CI.Upper != 0 {
		t.Fatalf("zero/zero should satisfy algebraic ratio gate: %+v", result)
	}
	ones := []float64{1, 1, 1, 1, 1}
	result = EvaluateMetric(testRule(stats.ScopeAllCompleted), makePairs(0, ones, stats.StatusPass, stats.StatusPass))
	if result.Status != StatusRegression {
		t.Fatalf("positive candidate over zero lower-is-better baseline should regress: %+v", result)
	}
}

func TestEvaluateMetricMeanDoesNotPassRareRetryRegression(t *testing.T) {
	candidates := make([]float64, 20)
	candidates[0] = 1
	rule := testRule(stats.ScopeAllCompleted)
	rule.Name = "retry_rate"
	rule.Estimator = stats.EstimatorMean
	result := EvaluateMetric(rule, makePairs(0, candidates, stats.StatusPass, stats.StatusPass))
	if result.Status == StatusPass || result.Paired.Estimator != stats.EstimatorMean || result.Paired.CI.Upper <= 0 {
		t.Fatalf("rare retry regression was hidden: %+v", result)
	}
}

func TestPerPassMetricCannotRewardPassToFail(t *testing.T) {
	zero := 0.0
	control := 100.0
	pairs := makePairs(100, []float64{90, 90, 90, 90, 90}, stats.StatusPass, stats.StatusPass)
	pairs[0].Control.Value = &control
	pairs[0].Candidate.Value = &zero
	pairs[0].Candidate.Status = stats.StatusFail
	result := EvaluateMetric(testRule(stats.ScopeSuccessful), pairs)
	if result.Status != StatusRegression || result.Reason != ReasonPassToFail {
		t.Fatalf("cheap failure was rewarded/excluded silently: %+v", result)
	}
}

func TestEvaluateMetricFailsClosedForMissingAndInvalidEvidence(t *testing.T) {
	pairs := makePairs(10, []float64{9, 9, 9, 9, 9}, stats.StatusPass, stats.StatusPass)
	pairs[0].Candidate.Value = nil
	if result := EvaluateMetric(testRule(stats.ScopeAllCompleted), pairs); result.Status != StatusInvalid || result.Reason != ReasonMetricMissing {
		t.Fatalf("missing metric result = %+v", result)
	}
	pairs = makePairs(10, []float64{9, 9, 9, 9, 9}, stats.StatusPass, stats.StatusPass)
	pairs[0].Candidate.TelemetryComplete = false
	if result := EvaluateMetric(testRule(stats.ScopeAllCompleted), pairs); result.Status != StatusInvalid || result.Reason != ReasonTelemetryIncomplete {
		t.Fatalf("incomplete telemetry result = %+v", result)
	}
	pairs = makePairs(10, []float64{9, 9, 9, 9, 9}, stats.StatusPass, stats.StatusPass)
	pairs[0].Candidate.Status = stats.StatusInfraError
	if result := EvaluateMetric(testRule(stats.ScopeAllCompleted), pairs); result.Status != StatusInvalid || result.Reason != ReasonEvidenceInvalid {
		t.Fatalf("infra sample result = %+v", result)
	}
}

func TestEvaluateMetricRequiresEnoughPairsAndValidRule(t *testing.T) {
	pairs := makePairs(10, []float64{9, 9}, stats.StatusPass, stats.StatusPass)
	if result := EvaluateMetric(testRule(stats.ScopeAllCompleted), pairs); result.Status != StatusInconclusive || result.Reason != ReasonInsufficientPairs {
		t.Fatalf("small sample result = %+v", result)
	}
	rule := testRule(stats.ScopeAllCompleted)
	rule.Ratio = 0
	if result := EvaluateMetric(rule, pairs); result.Status != StatusInvalid || result.Reason != ReasonInvalidRule {
		t.Fatalf("invalid rule result = %+v", result)
	}
}

func TestEvaluateMetricMapsInsufficientCaseClustersToInconclusive(t *testing.T) {
	pairs := makePairs(10, []float64{9, 9, 9, 9, 9}, stats.StatusPass, stats.StatusPass)
	for index := range pairs {
		pairs[index].CaseID = "only-case"
	}
	result := EvaluateMetric(testRule(stats.ScopeAllCompleted), pairs)
	if result.Status != StatusInconclusive || result.Reason != ReasonInsufficientClusters || result.Detail != "insufficient_clusters" {
		t.Fatalf("single case cluster result = %+v", result)
	}
}

func TestEvaluateMetricRejectsMissingCaseID(t *testing.T) {
	pairs := makePairs(10, []float64{9, 9, 9, 9, 9}, stats.StatusPass, stats.StatusPass)
	pairs[0].CaseID = ""
	result := EvaluateMetric(testRule(stats.ScopeAllCompleted), pairs)
	if result.Status != StatusInvalid || result.Reason != ReasonEvidenceInvalid {
		t.Fatalf("missing case identity result = %+v", result)
	}
}

func testRule(scope stats.Scope) MetricRule {
	return MetricRule{
		Name: "cost", Direction: LowerOrEqual, Ratio: 1, Scope: scope,
		Bootstrap: stats.BootstrapConfig{Confidence: 0.95, Iterations: 2_000, MinimumPairs: 2, Seed: 42},
	}
}

func makePairs(control float64, candidates []float64, controlStatus, candidateStatus stats.Status) []Pair {
	pairs := make([]Pair, len(candidates))
	split := len(candidates) / 2
	for i, candidate := range candidates {
		controlValue := control
		candidateValue := candidate
		caseID := "case-a"
		if i >= split {
			caseID = "case-b"
		}
		pairs[i] = Pair{
			BlockID: fmt.Sprintf("block-%02d", i), CaseID: caseID,
			Control:   Observation{Value: &controlValue, Status: controlStatus, TelemetryComplete: true},
			Candidate: Observation{Value: &candidateValue, Status: candidateStatus, TelemetryComplete: true},
		}
	}
	return pairs
}
