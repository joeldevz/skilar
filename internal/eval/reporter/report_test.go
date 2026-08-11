package reporter

import (
	"bytes"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/gates"
	"github.com/joeldevz/skynex/internal/eval/stats"
)

func TestWriteComparisonIsTerminalSafeAndShowsUnavailableCI(t *testing.T) {
	report := ComparisonReport{
		SchemaVersion: 1,
		ExperimentID:  "exp\x1b[2J",
		Compatibility: baseline.CompatibilityReport{Errors: []string{"bad\rfield"}},
		Metrics: []MetricComparison{{
			Name: "cost\x1b]0;title\x07", Unit: "USD",
			Paired: stats.PairedSummary{CI: stats.Interval{Reason: "insufficient_pairs", N: 2}},
		}},
		Decision: gates.Decision{Status: gates.StatusInvalid, ExitCode: gates.ExitInvalid, Reasons: []gates.Reason{{Gate: "compat", Code: gates.ReasonCompatibility, Detail: "bad\bdetail"}}},
	}
	var output bytes.Buffer
	if err := WriteComparison(&output, report); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\r') || strings.ContainsRune(got, '\b') {
		t.Fatalf("terminal injection survived: %q", got)
	}
	for _, want := range []string{"Decision: invalid (exit 2)", `bad\rfield`, "unavailable (insufficient_pairs)", `bad\x08detail`} {
		if !strings.Contains(got, want) {
			t.Fatalf("output %q does not contain %q", got, want)
		}
	}
}
