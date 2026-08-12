package stats

import "testing"

func TestSummarizeReliabilitySeparatesInvalidAndFlakyRates(t *testing.T) {
	outcomes := []Outcome{
		{ID: "1", CaseID: "a", Variant: "candidate", Status: StatusPass},
		{ID: "2", CaseID: "a", Variant: "candidate", Status: StatusFail},
		{ID: "3", CaseID: "b", Variant: "candidate", Status: StatusPass},
		{ID: "4", CaseID: "c", Variant: "candidate", Status: StatusInvalid},
		{ID: "5", CaseID: "d", Variant: "candidate", Status: StatusInfraError},
	}
	summary := SummarizeReliability(outcomes)
	if !summary.PassRate.Available || summary.PassRate.Numerator != 2 || summary.PassRate.Denominator != 3 {
		t.Fatalf("pass rate = %+v", summary.PassRate)
	}
	if summary.InvalidRate.Value != 0.2 || summary.InfrastructureRate.Value != 0.2 {
		t.Fatalf("invalid/infra rates = %+v/%+v", summary.InvalidRate, summary.InfrastructureRate)
	}
	if summary.FlakyRate.Numerator != 1 || summary.FlakyRate.Denominator != 2 || len(summary.FlakyCaseVariants) != 1 {
		t.Fatalf("flaky rate = %+v cases=%v", summary.FlakyRate, summary.FlakyCaseVariants)
	}
}

func TestReliabilityEmptyRatesAreUnavailable(t *testing.T) {
	summary := SummarizeReliability(nil)
	if summary.PassRate.Available || summary.FlakyRate.Available || summary.InvalidRate.Available {
		t.Fatalf("empty populations became measured zero rates: %+v", summary)
	}
}
