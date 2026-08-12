package reporter

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/joeldevz/skynex/internal/eval/runner"
)

// DiffReport is the legacy unpaired view. Statistical authority lives in
// ComparisonReport/gates; this structure remains for the current CLI adapter.
type DiffReport struct {
	Items   []ItemDiff  `json:"items"`
	Summary DiffSummary `json:"summary"`
}

type ItemDiff struct {
	CaseID     string  `json:"case_id"`
	Item       string  `json:"item"`
	BaseScore  float64 `json:"base_score"`
	CurrScore  float64 `json:"current_score"`
	Delta      float64 `json:"delta"`
	BaseStatus string  `json:"base_status"`
	CurrStatus string  `json:"current_status"`
	Comparable bool    `json:"comparable"`
	Reason     string  `json:"reason,omitempty"`
	Regressed  bool    `json:"regressed"`
	Improved   bool    `json:"improved"`
	PassCount  int     `json:"pass_count"`
}

type DiffSummary struct {
	TotalCases     int     `json:"total_cases"`
	Improved       int     `json:"improved"`
	Regressed      int     `json:"regressed"`
	Unchanged      int     `json:"unchanged"`
	MissingControl int     `json:"missing_control"`
	MissingCurrent int     `json:"missing_current"`
	BasePassRate   float64 `json:"base_pass_rate"`
	CurrPassRate   float64 `json:"current_pass_rate"`
	DeltaPassRate  float64 `json:"delta_pass_rate"`
	BaseCost       float64 `json:"base_cost"`
	CurrCost       float64 `json:"current_cost"`
}

func ComputeDiff(control, current *runner.SuiteResult) *DiffReport {
	if control == nil || current == nil {
		return &DiffReport{Items: []ItemDiff{}}
	}
	report := &DiffReport{Items: []ItemDiff{}}
	controlCases := make(map[string]runner.CaseResult, len(control.Items))
	currentCases := make(map[string]struct{}, len(current.Items))
	for _, item := range control.Items {
		controlCases[item.ID] = item
	}
	for _, candidate := range current.Items {
		currentCases[candidate.ID] = struct{}{}
		base, exists := controlCases[candidate.ID]
		diff := ItemDiff{
			CaseID: candidate.ID, Item: candidate.Item, CurrScore: candidate.Score,
			CurrStatus: candidate.Status, PassCount: candidate.PassCount,
		}
		if !exists {
			diff.BaseStatus = "missing"
			diff.Reason = "missing_control_case"
			report.Summary.MissingControl++
			report.Items = append(report.Items, diff)
			continue
		}
		diff.BaseScore = base.Score
		diff.BaseStatus = base.Status
		diff.Delta = candidate.Score - base.Score
		diff.Comparable = true
		diff.Improved = (base.Status != "pass" && candidate.Status == "pass") || (base.Status == candidate.Status && diff.Delta > 0.01)
		diff.Regressed = (base.Status == "pass" && candidate.Status != "pass") || (base.Status == candidate.Status && diff.Delta < -0.01)
		switch {
		case diff.Improved:
			report.Summary.Improved++
		case diff.Regressed:
			report.Summary.Regressed++
		default:
			report.Summary.Unchanged++
		}
		report.Items = append(report.Items, diff)
	}
	for _, base := range control.Items {
		if _, exists := currentCases[base.ID]; exists {
			continue
		}
		report.Summary.MissingCurrent++
		report.Items = append(report.Items, ItemDiff{
			CaseID: base.ID, Item: base.Item, BaseScore: base.Score, BaseStatus: base.Status,
			CurrStatus: "missing", Reason: "missing_current_case",
		})
	}
	sort.Slice(report.Items, func(i, j int) bool { return report.Items[i].CaseID < report.Items[j].CaseID })
	report.Summary.TotalCases = len(report.Items)
	report.Summary.BasePassRate = control.PassRate
	report.Summary.CurrPassRate = current.PassRate
	report.Summary.DeltaPassRate = current.PassRate - control.PassRate
	report.Summary.BaseCost = control.TotalCost
	report.Summary.CurrCost = current.TotalCost
	return report
}

// WriteDiff renders the legacy diff without emitting ANSI sequences. Every
// untrusted identifier is terminal-sanitized and bounded.
func WriteDiff(writer io.Writer, report *DiffReport) error {
	if writer == nil {
		return fmt.Errorf("diff writer is nil")
	}
	if report == nil {
		_, err := fmt.Fprintln(writer, "No diff report available")
		return err
	}
	if _, err := fmt.Fprintln(writer, "Evaluation diff"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Pass rate: %.1f%% -> %.1f%% (%+.1f pp)\n",
		report.Summary.BasePassRate*100, report.Summary.CurrPassRate*100, report.Summary.DeltaPassRate*100); err != nil {
		return err
	}
	costDelta := report.Summary.CurrCost - report.Summary.BaseCost
	if report.Summary.BaseCost == 0 {
		if _, err := fmt.Fprintf(writer, "Cost: $%.6f -> $%.6f (%+.6f; ratio unavailable: zero baseline)\n",
			report.Summary.BaseCost, report.Summary.CurrCost, costDelta); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(writer, "Cost: $%.6f -> $%.6f (%+.2f%%)\n",
		report.Summary.BaseCost, report.Summary.CurrCost, costDelta/report.Summary.BaseCost*100); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Cases: %d improved, %d regressed, %d unchanged, %d missing control, %d missing current\n",
		report.Summary.Improved, report.Summary.Regressed, report.Summary.Unchanged,
		report.Summary.MissingControl, report.Summary.MissingCurrent); err != nil {
		return err
	}
	for _, item := range report.Items {
		if !item.Regressed && item.Reason == "" {
			continue
		}
		status := "regressed"
		if item.Reason != "" {
			status = item.Reason
		}
		if _, err := fmt.Fprintf(writer, "  %s: %s\n", SanitizeTerminal(item.CaseID), SanitizeTerminal(status)); err != nil {
			return err
		}
	}
	return nil
}

func PrintDiff(report *DiffReport) {
	_ = WriteDiff(os.Stdout, report)
}
