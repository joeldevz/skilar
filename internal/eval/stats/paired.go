package stats

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
)

const (
	BootstrapMedianMethod = "deterministic-clustered-percentile-bootstrap-median-v1"
	BootstrapMeanMethod   = "deterministic-clustered-percentile-bootstrap-mean-v1"
)

type PairedEstimator string

const (
	EstimatorMedian PairedEstimator = "median"
	EstimatorMean   PairedEstimator = "mean"
)

// PairedValue retains a block's control, candidate, and transformed delta.
type PairedValue struct {
	BlockID   string  `json:"block_id"`
	CaseID    string  `json:"case_id"`
	Control   float64 `json:"control"`
	Candidate float64 `json:"candidate"`
	Delta     float64 `json:"delta"`
}

// BootstrapConfig defines a reproducible case-clustered percentile bootstrap.
// MinimumPairs is enforced independently for every case cluster.
type BootstrapConfig struct {
	Confidence   float64 `json:"confidence"`
	Iterations   int     `json:"iterations"`
	MinimumPairs int     `json:"minimum_pairs"`
	Seed         uint64  `json:"seed"`
}

// PairedSummary preserves every pair while making the independent case count
// explicit. Estimate.N counts pairs; CI.N counts the case clusters resampled.
type PairedSummary struct {
	Pairs        []PairedValue   `json:"pairs"`
	ClusterCount int             `json:"cluster_count"`
	Estimator    PairedEstimator `json:"estimator,omitempty"`
	Estimate     Estimate        `json:"estimate,omitempty"`
	MedianDelta  Estimate        `json:"median_delta"`
	CI           Interval        `json:"confidence_interval"`
	Seed         uint64          `json:"seed"`
	Iterations   int             `json:"iterations"`
}

// SummarizePaired computes candidate-control deltas. Use SummarizeTransformed
// when a gate needs a non-inferiority transform such as candidate-ratio*control.
func SummarizePaired(pairs []PairedValue, config BootstrapConfig) PairedSummary {
	return SummarizeTransformed(pairs, config, func(control, candidate float64) float64 {
		return candidate - control
	})
}

// SummarizePairedMean is the paired authority for rate/count metrics where a
// median would hide rare events (for example, one retry among many zeroes).
func SummarizePairedMean(pairs []PairedValue, config BootstrapConfig) PairedSummary {
	return SummarizeTransformedMean(pairs, config, func(control, candidate float64) float64 {
		return candidate - control
	})
}

// SummarizeTransformed applies transform to every complete pair, summarizes
// repetitions within each case, then performs a deterministic percentile
// bootstrap over the case summaries. This treats cases, rather than correlated
// repetitions, as the independent sampling units. R-7 quantiles at alpha/2 and
// 1-alpha/2 form the interval. The method is deterministic for identical
// samples/config.
func SummarizeTransformed(pairs []PairedValue, config BootstrapConfig, transform func(control, candidate float64) float64) PairedSummary {
	return summarizeTransformed(pairs, config, transform, EstimatorMedian)
}

// SummarizeTransformedMean performs the same deterministic case-clustered
// bootstrap using the arithmetic mean within and across cases.
func SummarizeTransformedMean(pairs []PairedValue, config BootstrapConfig, transform func(control, candidate float64) float64) PairedSummary {
	return summarizeTransformed(pairs, config, transform, EstimatorMean)
}

func summarizeTransformed(pairs []PairedValue, config BootstrapConfig, transform func(control, candidate float64) float64, estimator PairedEstimator) PairedSummary {
	config = normalizeBootstrapConfig(config)
	result := PairedSummary{
		Pairs: append([]PairedValue(nil), pairs...), Estimator: estimator,
		Seed: config.Seed, Iterations: config.Iterations,
	}
	method := BootstrapMedianMethod
	if estimator == EstimatorMean {
		method = BootstrapMeanMethod
	}
	if err := ValidateBootstrapConfig(config); err != nil {
		setPairedEstimate(&result, Estimate{Reason: "invalid_bootstrap_config"})
		result.CI = Interval{Reason: "invalid_bootstrap_config", Method: method}
		return result
	}
	if estimator != EstimatorMedian && estimator != EstimatorMean {
		setPairedEstimate(&result, Estimate{Reason: "invalid_estimator"})
		result.CI = Interval{Reason: "invalid_estimator", Method: method}
		return result
	}
	if transform == nil {
		setPairedEstimate(&result, Estimate{Reason: "missing_transform"})
		result.CI = Interval{Reason: "missing_transform", Method: method}
		return result
	}
	valuesByCase := make(map[string][]float64)
	totalPairs := 0
	for i := range result.Pairs {
		if result.Pairs[i].CaseID == "" {
			setPairedEstimate(&result, Estimate{N: totalPairs, Reason: "missing_case_id"})
			result.CI = Interval{N: len(valuesByCase), Reason: "missing_case_id", Method: method}
			return result
		}
		if math.IsNaN(result.Pairs[i].Control) || math.IsInf(result.Pairs[i].Control, 0) || math.IsNaN(result.Pairs[i].Candidate) || math.IsInf(result.Pairs[i].Candidate, 0) {
			setPairedEstimate(&result, Estimate{N: totalPairs, Reason: "non_finite_pair"})
			result.CI = Interval{N: len(valuesByCase), Reason: "non_finite_pair", Method: method}
			return result
		}
		result.Pairs[i].Delta = transform(result.Pairs[i].Control, result.Pairs[i].Candidate)
		if math.IsNaN(result.Pairs[i].Delta) || math.IsInf(result.Pairs[i].Delta, 0) {
			setPairedEstimate(&result, Estimate{N: totalPairs, Reason: "non_finite_delta"})
			result.CI = Interval{N: len(valuesByCase), Reason: "non_finite_delta", Method: method}
			return result
		}
		valuesByCase[result.Pairs[i].CaseID] = append(valuesByCase[result.Pairs[i].CaseID], result.Pairs[i].Delta)
		totalPairs++
	}
	caseIDs := make([]string, 0, len(valuesByCase))
	for caseID := range valuesByCase {
		caseIDs = append(caseIDs, caseID)
	}
	sort.Strings(caseIDs)
	result.ClusterCount = len(caseIDs)
	caseSummaries := make([]float64, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		values := valuesByCase[caseID]
		if len(values) < config.MinimumPairs {
			setPairedEstimate(&result, Estimate{N: totalPairs, Reason: "insufficient_pairs"})
			result.CI = Interval{N: result.ClusterCount, Reason: "insufficient_pairs", Method: method}
			return result
		}
		summary, err := pairedStatistic(values, estimator)
		if err != nil {
			setPairedEstimate(&result, Estimate{N: totalPairs, Reason: "invalid_pairs"})
			result.CI = Interval{N: result.ClusterCount, Reason: "invalid_pairs", Method: method}
			return result
		}
		caseSummaries = append(caseSummaries, summary)
	}
	if result.ClusterCount < 2 {
		setPairedEstimate(&result, Estimate{N: totalPairs, Reason: "insufficient_clusters"})
		result.CI = Interval{N: result.ClusterCount, Reason: "insufficient_clusters", Method: method}
		return result
	}
	point, err := pairedStatistic(caseSummaries, estimator)
	if err != nil {
		setPairedEstimate(&result, Estimate{N: totalPairs, Reason: "invalid_pairs"})
		result.CI = Interval{N: result.ClusterCount, Reason: "invalid_pairs", Method: method}
		return result
	}
	setPairedEstimate(&result, Estimate{Available: true, Value: point, N: totalPairs})
	random := rand.New(rand.NewSource(int64(config.Seed))) // #nosec G404 -- deterministic statistical resampling.
	replicates := make([]float64, config.Iterations)
	resample := make([]float64, len(caseSummaries))
	for iteration := 0; iteration < config.Iterations; iteration++ {
		for i := range resample {
			resample[i] = caseSummaries[random.Intn(len(caseSummaries))]
		}
		replicates[iteration], _ = pairedStatistic(resample, estimator)
	}
	alpha := (1 - config.Confidence) / 2
	lower, lowerErr := QuantileR7(replicates, alpha)
	upper, upperErr := QuantileR7(replicates, 1-alpha)
	if lowerErr != nil || upperErr != nil {
		result.CI = Interval{N: result.ClusterCount, Reason: "bootstrap_failed", Method: method}
		return result
	}
	result.CI = Interval{
		Available:  true,
		Lower:      lower,
		Upper:      upper,
		Confidence: config.Confidence,
		Method:     method,
		N:          result.ClusterCount,
	}
	return result
}

func setPairedEstimate(result *PairedSummary, estimate Estimate) {
	result.Estimate = estimate
	if result.Estimator == EstimatorMedian {
		result.MedianDelta = estimate
	}
}

func pairedStatistic(values []float64, estimator PairedEstimator) (float64, error) {
	if estimator == EstimatorMedian {
		return QuantileR7(values, 0.5)
	}
	if estimator != EstimatorMean || len(values) == 0 {
		return 0, fmt.Errorf("unsupported estimator %q", estimator)
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values)), nil
}

func normalizeBootstrapConfig(config BootstrapConfig) BootstrapConfig {
	if config.Confidence == 0 {
		config.Confidence = 0.95
	}
	if config.Iterations == 0 {
		config.Iterations = 10_000
	}
	if config.MinimumPairs == 0 {
		config.MinimumPairs = 5
	}
	return config
}

// ValidateBootstrapConfig can be used at manifest-validation time, before a
// potentially expensive experiment begins.
func ValidateBootstrapConfig(config BootstrapConfig) error {
	config = normalizeBootstrapConfig(config)
	if config.Confidence <= 0.5 || config.Confidence >= 1 || math.IsNaN(config.Confidence) {
		return fmt.Errorf("confidence must be greater than 0.5 and less than 1")
	}
	if config.Iterations < 1000 || config.Iterations > 1_000_000 {
		return fmt.Errorf("iterations must be between 1000 and 1000000")
	}
	if config.MinimumPairs < 2 {
		return fmt.Errorf("minimum pairs must be at least 2")
	}
	return nil
}
