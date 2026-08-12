package stats

import (
	"fmt"
	"reflect"
	"testing"
)

func TestPairedBootstrapIsDeterministicAndPreservesPairs(t *testing.T) {
	pairs := make([]PairedValue, 10)
	for index := range pairs {
		pairs[index] = PairedValue{
			BlockID: fmt.Sprintf("b-%02d", index), CaseID: fmt.Sprintf("case-%d", index/5),
			Control: float64(10 + index), Candidate: float64(8 + index),
		}
	}
	config := BootstrapConfig{Confidence: 0.95, Iterations: 2_000, MinimumPairs: 5, Seed: 77}
	left := SummarizePaired(pairs, config)
	right := SummarizePaired(pairs, config)
	if !reflect.DeepEqual(left, right) {
		t.Fatal("bootstrap is not deterministic")
	}
	if !left.MedianDelta.Available || left.MedianDelta.Value != -2 || !left.CI.Available || left.CI.Lower != -2 || left.CI.Upper != -2 {
		t.Fatalf("unexpected paired result: %+v", left)
	}
	if len(left.Pairs) != len(pairs) || left.Pairs[0].Delta != -2 {
		t.Fatalf("raw pairs/deltas not retained: %+v", left.Pairs)
	}
	if left.ClusterCount != 2 || left.Estimate.N != 10 || left.CI.N != 2 || left.CI.Method != BootstrapMedianMethod {
		t.Fatalf("cluster accounting is wrong: %+v", left)
	}
}

func TestPairedBootstrapRequiresEnoughPairs(t *testing.T) {
	summary := SummarizePaired([]PairedValue{{CaseID: "case-a", Control: 1, Candidate: 0}}, BootstrapConfig{Iterations: 1000, MinimumPairs: 2, Confidence: 0.95})
	if summary.CI.Available || summary.CI.Reason != "insufficient_pairs" || summary.MedianDelta.Available {
		t.Fatalf("small paired sample got authority: %+v", summary)
	}
}

func TestPairedBootstrapEnforcesMinimumWithinEveryCase(t *testing.T) {
	pairs := make([]PairedValue, 0, 10)
	pairs = append(pairs, PairedValue{BlockID: "a-00", CaseID: "case-a", Control: 1, Candidate: 0})
	for repetition := 0; repetition < 9; repetition++ {
		pairs = append(pairs, PairedValue{BlockID: fmt.Sprintf("b-%02d", repetition), CaseID: "case-b", Control: 1, Candidate: 0})
	}
	summary := SummarizePaired(pairs, BootstrapConfig{Iterations: 1000, MinimumPairs: 5, Confidence: 0.95})
	if summary.CI.Available || summary.CI.Reason != "insufficient_pairs" || summary.ClusterCount != 2 {
		t.Fatalf("an undersized case was hidden by the total pair count: %+v", summary)
	}
}

func TestPairedBootstrapRequiresMultipleCaseClusters(t *testing.T) {
	pairs := make([]PairedValue, 5)
	for index := range pairs {
		pairs[index] = PairedValue{BlockID: fmt.Sprintf("b-%02d", index), CaseID: "only-case", Control: 1, Candidate: 0}
	}
	summary := SummarizePaired(pairs, BootstrapConfig{Iterations: 1000, MinimumPairs: 5, Confidence: 0.95})
	if summary.CI.Available || summary.CI.Reason != "insufficient_clusters" || summary.ClusterCount != 1 || summary.CI.N != 1 {
		t.Fatalf("single case cluster got authority: %+v", summary)
	}
}

func TestPairedBootstrapRejectsMissingCaseID(t *testing.T) {
	summary := SummarizePaired([]PairedValue{{Control: 1, Candidate: 0}}, BootstrapConfig{Iterations: 1000, MinimumPairs: 2, Confidence: 0.95})
	if summary.CI.Available || summary.CI.Reason != "missing_case_id" {
		t.Fatalf("pair without case identity got authority: %+v", summary)
	}
}

func TestPairedMeanBootstrapDoesNotHideRareEvents(t *testing.T) {
	pairs := make([]PairedValue, 20)
	for index := range pairs {
		pairs[index] = PairedValue{
			BlockID: fmt.Sprintf("b-%02d", index), CaseID: fmt.Sprintf("case-%d", index/5),
			Control: 0, Candidate: 0,
		}
	}
	pairs[0].Candidate = 1
	summary := SummarizePairedMean(pairs, BootstrapConfig{Iterations: 10_000, MinimumPairs: 5, Confidence: 0.95, Seed: 7})
	if summary.Estimator != EstimatorMean || !summary.Estimate.Available || summary.Estimate.Value != 0.05 || !summary.CI.Available {
		t.Fatalf("mean retry summary = %+v", summary)
	}
	if summary.CI.Upper <= 0 || summary.CI.Method != BootstrapMeanMethod {
		t.Fatalf("rare retry was hidden by bootstrap: %+v", summary.CI)
	}
}

func TestClusteredBootstrapDoesNotTreatRepeatedRunsAsIndependent(t *testing.T) {
	pairs := make([]PairedValue, 0, 300)
	for caseIndex, delta := range []float64{-1, 0, 1} {
		for repetition := 0; repetition < 100; repetition++ {
			pairs = append(pairs, PairedValue{
				BlockID: fmt.Sprintf("case-%d-run-%03d", caseIndex, repetition),
				CaseID:  fmt.Sprintf("case-%d", caseIndex),
				Control: 0, Candidate: delta,
			})
		}
	}
	summary := SummarizePaired(pairs, BootstrapConfig{Iterations: 10_000, MinimumPairs: 5, Confidence: 0.95, Seed: 19})
	if !summary.CI.Available || summary.CI.Lower != -1 || summary.CI.Upper != 1 {
		t.Fatalf("correlated repetitions narrowed the case-level CI: %+v", summary.CI)
	}
	if summary.ClusterCount != 3 || summary.CI.N != 3 || summary.Estimate.N != 300 {
		t.Fatalf("case clusters and repetitions were conflated: %+v", summary)
	}
}

func TestClusteredMeanWeightsCasesRatherThanRepetitionCounts(t *testing.T) {
	pairs := make([]PairedValue, 0, 55)
	for repetition := 0; repetition < 5; repetition++ {
		pairs = append(pairs, PairedValue{BlockID: fmt.Sprintf("a-%02d", repetition), CaseID: "case-a", Candidate: 0})
	}
	for repetition := 0; repetition < 50; repetition++ {
		pairs = append(pairs, PairedValue{BlockID: fmt.Sprintf("b-%02d", repetition), CaseID: "case-b", Candidate: 10})
	}
	summary := SummarizePairedMean(pairs, BootstrapConfig{Iterations: 2_000, MinimumPairs: 5, Confidence: 0.95, Seed: 23})
	if !summary.Estimate.Available || summary.Estimate.Value != 5 {
		t.Fatalf("cases were weighted by repetition count: %+v", summary.Estimate)
	}
}

func TestValidateBootstrapConfig(t *testing.T) {
	invalid := []BootstrapConfig{
		{Confidence: 0.5, Iterations: 1000, MinimumPairs: 2},
		{Confidence: 0.95, Iterations: 999, MinimumPairs: 2},
		{Confidence: 0.95, Iterations: 1000, MinimumPairs: 1},
	}
	for _, config := range invalid {
		if err := ValidateBootstrapConfig(config); err == nil {
			t.Fatalf("accepted invalid config %+v", config)
		}
	}
}
