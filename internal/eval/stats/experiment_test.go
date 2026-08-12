package stats

import (
	"reflect"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/contracts"
)

func TestBalancedBlockedPlanIsDeterministicPairedAndBalanced(t *testing.T) {
	left, err := NewBalancedBlockedPlan([]string{"case-a", "case-b"}, 10, 12345)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewBalancedBlockedPlan([]string{"case-a", "case-b"}, 10, 12345)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(left, right) {
		t.Fatal("same seed did not reproduce plan")
	}
	if left.Method != BalancedBlockedMethod || !left.SerializeWithinBlock || len(left.Blocks) != 20 {
		t.Fatalf("unexpected plan: %+v", left)
	}
	counts := map[string]map[Variant]int{}
	for _, block := range left.Blocks {
		if len(block.Order) != 2 || block.Order[0] == block.Order[1] {
			t.Fatalf("block is not paired: %+v", block)
		}
		if counts[block.CaseID] == nil {
			counts[block.CaseID] = map[Variant]int{}
		}
		counts[block.CaseID][block.Order[0]]++
	}
	for caseID, byFirst := range counts {
		if byFirst[VariantControl] != 5 || byFirst[VariantCandidate] != 5 {
			t.Fatalf("case %s is not balanced: %v", caseID, byFirst)
		}
	}
}

func TestBalancedBlockedPlanRejectsInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		cases []string
		runs  int
	}{
		{nil, 2},
		{[]string{"a"}, 1},
		{[]string{"a"}, contracts.MaxRuns + 1},
		{[]string{"a", "a"}, 2},
		{[]string{""}, 2},
	} {
		if _, err := NewBalancedBlockedPlan(test.cases, test.runs, 1); err == nil {
			t.Fatalf("accepted invalid cases=%v runs=%d", test.cases, test.runs)
		}
	}
}
