package stats

import (
	"math"
	"testing"
)

func TestSummarizePreservesRawSamplesAndEnforcesSufficiency(t *testing.T) {
	one, two := 1.0, 2.0
	samples := []Sample{
		{ID: "1", Status: StatusPass, Value: &one, TelemetryComplete: true},
		{ID: "2", Status: StatusFail, Value: &two, TelemetryComplete: true},
		{ID: "3", Status: StatusInvalid, Value: &two, TelemetryComplete: true},
		{ID: "4", Status: StatusPass, TelemetryComplete: true},
		{ID: "5", Status: StatusPass, Value: &two, TelemetryComplete: false},
	}
	summary := Summarize(samples, SummaryConfig{})
	if summary.Total != 5 || summary.Eligible != 2 || summary.Unavailable != 2 || summary.ExcludedByStatus != 1 {
		t.Fatalf("unexpected counts: %+v", summary)
	}
	if summary.Median.Available || summary.Median.Reason != "insufficient_samples" {
		t.Fatalf("small median should be unavailable: %+v", summary.Median)
	}
	if summary.Quantiles["p95"].Available || summary.Quantiles["p95"].N != 2 {
		t.Fatalf("small p95 should be unavailable: %+v", summary.Quantiles["p95"])
	}
	// Ensure retained pointer values are a deep copy.
	one = 99
	if got := *summary.Samples[0].Value; got != 1 {
		t.Fatalf("raw sample was aliased, got %v", got)
	}
}

func TestSummarizeR7MedianAndP95(t *testing.T) {
	samples := make([]Sample, 20)
	for i := range samples {
		value := float64(i + 1)
		samples[i] = Sample{ID: string(rune('a' + i)), Status: StatusPass, Value: &value, TelemetryComplete: true}
	}
	summary := Summarize(samples, SummaryConfig{})
	if !summary.Median.Available || summary.Median.Value != 10.5 {
		t.Fatalf("median = %+v", summary.Median)
	}
	p95 := summary.Quantiles["p95"]
	if !p95.Available || math.Abs(p95.Value-19.05) > 1e-12 {
		t.Fatalf("p95 = %+v, want 19.05", p95)
	}
}

func TestSuccessfulScopeDoesNotTreatFailedRunAsCheapSample(t *testing.T) {
	expensive, cheap := 100.0, 0.0
	summary := Summarize([]Sample{
		{Status: StatusPass, Value: &expensive, TelemetryComplete: true},
		{Status: StatusFail, Value: &cheap, TelemetryComplete: true},
	}, SummaryConfig{Scope: ScopeSuccessful, MinimumForMedian: 1})
	if summary.Eligible != 1 || summary.Median.Value != 100 {
		t.Fatalf("failed run contaminated per-pass metric: %+v", summary)
	}
}

func TestQuantileAndRatioInvalidInputs(t *testing.T) {
	if _, err := QuantileR7(nil, 0.5); err == nil {
		t.Fatal("empty quantile accepted")
	}
	if _, err := QuantileR7([]float64{math.NaN()}, 0.5); err == nil {
		t.Fatal("NaN quantile accepted")
	}
	zero, one := 0.0, 1.0
	if ratio := SafeRatio(&one, &zero); ratio.Available || ratio.Reason != "baseline_zero" {
		t.Fatalf("zero denominator not handled explicitly: %+v", ratio)
	}
	if ratio := SafeRatio(nil, &one); ratio.Available || ratio.Reason != "metric_missing" {
		t.Fatalf("missing numerator not handled explicitly: %+v", ratio)
	}
}
