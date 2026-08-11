package reporter

import (
	"bytes"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/runner"
)

func TestComputeDiffDoesNotTreatMissingCaseAsZeroScore(t *testing.T) {
	control := &runner.SuiteResult{PassRate: 1, TotalCost: 0, Items: []runner.CaseResult{
		{ID: "present", Item: "x", Score: 1, Status: "pass"},
		{ID: "removed", Item: "x", Score: 1, Status: "pass"},
	}}
	current := &runner.SuiteResult{PassRate: 0.5, TotalCost: 1, Items: []runner.CaseResult{
		{ID: "present", Item: "x", Score: 0, Status: "fail"},
		{ID: "new", Item: "x", Score: 1, Status: "pass"},
	}}
	report := ComputeDiff(control, current)
	if report.Summary.Regressed != 1 || report.Summary.MissingControl != 1 || report.Summary.MissingCurrent != 1 {
		t.Fatalf("unexpected summary: %+v", report.Summary)
	}
	for _, item := range report.Items {
		if (item.CaseID == "new" || item.CaseID == "removed") && (item.Comparable || item.Improved || item.Regressed) {
			t.Fatalf("missing item was numerically compared: %+v", item)
		}
	}
}

func TestWriteDiffSanitizesIdentifiersAndHandlesZeroCost(t *testing.T) {
	report := &DiffReport{
		Items:   []ItemDiff{{CaseID: "owned\x1b[2J", Reason: "missing\rcontrol"}},
		Summary: DiffSummary{MissingControl: 1},
	}
	var output bytes.Buffer
	if err := WriteDiff(&output, report); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\r') {
		t.Fatalf("terminal injection survived: %q", got)
	}
	if !strings.Contains(got, "ratio unavailable: zero baseline") || !strings.Contains(got, `missing\rcontrol`) {
		t.Fatalf("missing zero/sanitized output: %q", got)
	}
}
