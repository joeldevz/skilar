package stats

import (
	"fmt"

	"github.com/joeldevz/skynex/internal/eval/contracts"
)

// RunMetric projects one contract result into a numeric observation. available
// must be false for unknown pricing or other absent evidence; measured zero is
// returned as available=true.
type RunMetric func(contracts.RunResult) (value float64, available bool)

// SamplesFromRunResults validates each result and converts it without losing run
// identity. requireTelemetry should be true for tree usage/cost claims and false
// for evaluator-owned evidence such as wall time.
func SamplesFromRunResults(results []contracts.RunResult, metric RunMetric, requireTelemetry bool) ([]Sample, error) {
	if metric == nil {
		return nil, fmt.Errorf("metric extractor is required")
	}
	samples := make([]Sample, len(results))
	for i := range results {
		if err := results[i].Validate(); err != nil {
			return nil, fmt.Errorf("run result %d: %w", i, err)
		}
		status, err := statusFromContract(results[i].Status)
		if err != nil {
			return nil, fmt.Errorf("run result %d: %w", i, err)
		}
		value, available := metric(results[i])
		telemetryComplete := !requireTelemetry || results[i].TelemetryComplete
		var valuePointer *float64
		if available {
			valueCopy := value
			valuePointer = &valueCopy
		}
		samples[i] = Sample{
			ID: results[i].RunID, CaseID: results[i].CaseID, Variant: results[i].Variant,
			Repetition: results[i].Repetition, Status: status, Value: valuePointer,
			TelemetryComplete: telemetryComplete,
		}
	}
	return samples, nil
}

func OutcomesFromRunResults(results []contracts.RunResult) ([]Outcome, error) {
	outcomes := make([]Outcome, len(results))
	for i := range results {
		if err := results[i].Validate(); err != nil {
			return nil, fmt.Errorf("run result %d: %w", i, err)
		}
		status, err := statusFromContract(results[i].Status)
		if err != nil {
			return nil, err
		}
		outcomes[i] = Outcome{ID: results[i].RunID, CaseID: results[i].CaseID, Variant: results[i].Variant, Repetition: results[i].Repetition, Status: status}
	}
	return outcomes, nil
}

var (
	MetricParentFirstInput RunMetric = func(result contracts.RunResult) (float64, bool) {
		return float64(result.Usage.Parent.FirstInputTokens), true
	}
	MetricParentPeakInput RunMetric = func(result contracts.RunResult) (float64, bool) {
		return float64(result.Usage.Parent.PeakInputTokens), true
	}
	MetricTreeInput RunMetric = func(result contracts.RunResult) (float64, bool) {
		return float64(result.Usage.Tree.SumInputTokens), true
	}
	MetricParentProviderCost RunMetric = func(result contracts.RunResult) (float64, bool) {
		if !result.Provenance.ProviderCostUSDAuthoritative() || result.Usage.Parent.ProviderCostUSD == nil {
			return 0, false
		}
		return *result.Usage.Parent.ProviderCostUSD, true
	}
	MetricTreeProviderCost RunMetric = func(result contracts.RunResult) (float64, bool) {
		if !result.Provenance.ProviderCostUSDAuthoritative() || result.Usage.Tree.ProviderCostUSD == nil {
			return 0, false
		}
		return *result.Usage.Tree.ProviderCostUSD, true
	}
	MetricParentCalculatedCost RunMetric = func(result contracts.RunResult) (float64, bool) {
		if result.Usage.Parent.CalculatedCostUSD == nil {
			return 0, false
		}
		return *result.Usage.Parent.CalculatedCostUSD, true
	}
	MetricTreeCalculatedCost RunMetric = func(result contracts.RunResult) (float64, bool) {
		if result.Usage.Tree.CalculatedCostUSD == nil {
			return 0, false
		}
		return *result.Usage.Tree.CalculatedCostUSD, true
	}
	MetricWallMS RunMetric = func(result contracts.RunResult) (float64, bool) {
		return float64(result.Timing.WallMS), true
	}
	MetricRetries RunMetric = func(result contracts.RunResult) (float64, bool) {
		return float64(result.Coordination.Retries), true
	}
)

func statusFromContract(status contracts.RunStatus) (Status, error) {
	switch status {
	case contracts.RunStatusPass:
		return StatusPass, nil
	case contracts.RunStatusFail:
		return StatusFail, nil
	case contracts.RunStatusInvalid:
		return StatusInvalid, nil
	case contracts.RunStatusInconclusive:
		return StatusInconclusive, nil
	case contracts.RunStatusAborted:
		return StatusAborted, nil
	case contracts.RunStatusInfraError:
		return StatusInfraError, nil
	case contracts.RunStatusBudgetExhausted:
		return StatusBudgetExhausted, nil
	default:
		return "", fmt.Errorf("unsupported run status %q", status)
	}
}
