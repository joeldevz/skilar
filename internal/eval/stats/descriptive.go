package stats

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	DefaultMinimumForMedian   = 3
	DefaultMinimumForQuantile = 20
)

// Summarize retains raw samples and computes only estimates with the declared
// minimum sample size. Quantiles use the deterministic R-7 method (the default
// method in R and NumPy's linear quantile): h=(n-1)q with linear interpolation.
func Summarize(samples []Sample, config SummaryConfig) Summary {
	config = normalizeSummaryConfig(config)
	result := Summary{
		Samples:   cloneSamples(samples),
		Total:     len(samples),
		Quantiles: make(map[string]Estimate, len(config.Quantiles)),
	}
	for _, sample := range samples {
		if !eligibleStatus(sample.Status, config.Scope) {
			result.ExcludedByStatus++
			continue
		}
		if sample.Value == nil || !sample.TelemetryComplete || math.IsNaN(*sample.Value) || math.IsInf(*sample.Value, 0) {
			result.Unavailable++
			continue
		}
		result.EligibleValues = append(result.EligibleValues, *sample.Value)
	}
	result.Eligible = len(result.EligibleValues)
	result.Median = estimateQuantile(result.EligibleValues, 0.5, config.MinimumForMedian)
	for _, quantile := range config.Quantiles {
		label := quantileLabel(quantile)
		minimum := config.MinimumForQuantile
		if quantile == 0.5 {
			minimum = config.MinimumForMedian
		}
		result.Quantiles[label] = estimateQuantile(result.EligibleValues, quantile, minimum)
	}
	return result
}

// QuantileR7 computes an R-7 quantile without changing values.
func QuantileR7(values []float64, quantile float64) (float64, error) {
	if len(values) == 0 {
		return 0, fmt.Errorf("quantile requires at least one value")
	}
	if quantile < 0 || quantile > 1 || math.IsNaN(quantile) {
		return 0, fmt.Errorf("quantile must be between 0 and 1")
	}
	sorted := append([]float64(nil), values...)
	for _, value := range sorted {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, fmt.Errorf("quantile values must be finite")
		}
	}
	sort.Float64s(sorted)
	if len(sorted) == 1 {
		return sorted[0], nil
	}
	position := float64(len(sorted)-1) * quantile
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower], nil
	}
	fraction := position - float64(lower)
	return sorted[lower] + fraction*(sorted[upper]-sorted[lower]), nil
}

// SafeRatio returns unavailable for a zero or missing denominator. Gate code
// compares candidate-ratio*baseline algebraically and therefore never needs to
// divide by a zero baseline.
func SafeRatio(candidate, baseline *float64) Estimate {
	if candidate == nil || baseline == nil {
		return Estimate{Reason: "metric_missing"}
	}
	if math.IsNaN(*candidate) || math.IsInf(*candidate, 0) || math.IsNaN(*baseline) || math.IsInf(*baseline, 0) {
		return Estimate{Reason: "metric_non_finite"}
	}
	if *baseline == 0 {
		return Estimate{Reason: "baseline_zero"}
	}
	return Estimate{Available: true, Value: *candidate / *baseline, N: 1}
}

func estimateQuantile(values []float64, quantile float64, minimum int) Estimate {
	if len(values) < minimum {
		return Estimate{N: len(values), Reason: "insufficient_samples"}
	}
	value, err := QuantileR7(values, quantile)
	if err != nil {
		return Estimate{N: len(values), Reason: "invalid_samples"}
	}
	return Estimate{Available: true, Value: value, N: len(values)}
}

func normalizeSummaryConfig(config SummaryConfig) SummaryConfig {
	if config.Scope == "" {
		config.Scope = ScopeAllCompleted
	}
	if config.MinimumForMedian <= 0 {
		config.MinimumForMedian = DefaultMinimumForMedian
	}
	if config.MinimumForQuantile <= 0 {
		config.MinimumForQuantile = DefaultMinimumForQuantile
	}
	if len(config.Quantiles) == 0 {
		config.Quantiles = []float64{0.5, 0.95}
	}
	return config
}

func eligibleStatus(status Status, scope Scope) bool {
	if scope == ScopeSuccessful {
		return status == StatusPass
	}
	return status == StatusPass || status == StatusFail
}

func cloneSamples(samples []Sample) []Sample {
	cloned := make([]Sample, len(samples))
	copy(cloned, samples)
	for i := range cloned {
		if samples[i].Value != nil {
			value := *samples[i].Value
			cloned[i].Value = &value
		}
	}
	return cloned
}

func quantileLabel(quantile float64) string {
	percent := quantile * 100
	return "p" + strings.TrimRight(strings.TrimRight(strconv.FormatFloat(percent, 'f', 6, 64), "0"), ".")
}
