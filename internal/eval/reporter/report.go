package reporter

import (
	"fmt"
	"io"
	"sort"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/gates"
	"github.com/joeldevz/skynex/internal/eval/stats"
)

// MetricComparison contains descriptive views and the paired authority. Each
// stats.Summary retains its raw repetitions.
type MetricComparison struct {
	Name      string              `json:"name"`
	Unit      string              `json:"unit"`
	Scope     stats.Scope         `json:"scope"`
	Control   stats.Summary       `json:"control"`
	Candidate stats.Summary       `json:"candidate"`
	Paired    stats.PairedSummary `json:"paired"`
}

type ComparisonReport struct {
	SchemaVersion int                                 `json:"schema_version"`
	ExperimentID  string                              `json:"experiment_id"`
	Compatibility baseline.CompatibilityReport        `json:"compatibility"`
	Reliability   map[string]stats.ReliabilitySummary `json:"reliability"`
	Metrics       []MetricComparison                  `json:"metrics"`
	Decision      gates.Decision                      `json:"decision"`
}

// WriteComparison writes a plain, terminal-safe report. Machine-readable data
// should be persisted with Save; this view intentionally contains no raw traces.
func WriteComparison(writer io.Writer, report ComparisonReport) error {
	if writer == nil {
		return fmt.Errorf("report writer is nil")
	}
	if _, err := fmt.Fprintf(writer, "Skynex evaluation: %s\n", SanitizeTerminal(report.ExperimentID)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Decision: %s (exit %d)\n", report.Decision.Status, report.Decision.ExitCode); err != nil {
		return err
	}
	if !report.Compatibility.Compatible {
		if _, err := fmt.Fprintln(writer, "Compatibility: invalid"); err != nil {
			return err
		}
		for _, message := range report.Compatibility.Errors {
			if _, err := fmt.Fprintf(writer, "  - %s\n", SanitizeTerminal(message)); err != nil {
				return err
			}
		}
	} else {
		if _, err := fmt.Fprintln(writer, "Compatibility: compatible"); err != nil {
			return err
		}
	}
	metrics := append([]MetricComparison(nil), report.Metrics...)
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].Name < metrics[j].Name })
	for _, metric := range metrics {
		name := SanitizeTerminal(metric.Name)
		unit := SanitizeTerminal(metric.Unit)
		estimator := metric.Paired.Estimator
		estimate := metric.Paired.Estimate
		if estimator == "" || !estimate.Available {
			estimator = stats.EstimatorMedian
			estimate = metric.Paired.MedianDelta
		}
		if estimate.Available && metric.Paired.CI.Available {
			if _, err := fmt.Fprintf(writer, "%s: paired %s %+.6g %s; %.1f%% CI [%+.6g, %+.6g], n=%d\n",
				name, estimator, estimate.Value, unit, metric.Paired.CI.Confidence*100,
				metric.Paired.CI.Lower, metric.Paired.CI.Upper, metric.Paired.CI.N); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(writer, "%s: unavailable (%s), n=%d\n", name, SanitizeTerminal(metric.Paired.CI.Reason), metric.Paired.CI.N); err != nil {
				return err
			}
		}
	}
	for _, reason := range report.Decision.Reasons {
		if _, err := fmt.Fprintf(writer, "Gate %s: %s — %s\n", SanitizeTerminal(reason.Gate), reason.Code, SanitizeTerminal(reason.Detail)); err != nil {
			return err
		}
	}
	return nil
}
