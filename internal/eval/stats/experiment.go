package stats

import (
	"fmt"
	"math/rand"

	"github.com/joeldevz/skynex/internal/eval/contracts"
)

const BalancedBlockedMethod = "balanced-blocked-ab-ba"

type Variant string

const (
	VariantControl   Variant = "control"
	VariantCandidate Variant = "candidate"
)

// BlockPlan is one paired block. Order is always a permutation of control and
// candidate and must be serialized within the block to preserve pairing.
type BlockPlan struct {
	ID         string    `json:"id"`
	CaseID     string    `json:"case_id"`
	Repetition int       `json:"repetition"`
	Order      []Variant `json:"order"`
}

// ExperimentPlan records enough information to replay randomized A/B order.
type ExperimentPlan struct {
	Method               string      `json:"method"`
	Seed                 uint64      `json:"seed"`
	RunsPerCase          int         `json:"runs_per_case"`
	SerializeWithinBlock bool        `json:"serialize_within_block"`
	Blocks               []BlockPlan `json:"blocks"`
}

// NewBalancedBlockedPlan creates approximately balanced AB/BA ordering per case
// and then shuffles repetition assignment deterministically from seed. For odd
// run counts, the one extra order is chosen by the seeded generator.
func NewBalancedBlockedPlan(caseIDs []string, runs int, seed uint64) (ExperimentPlan, error) {
	if len(caseIDs) == 0 {
		return ExperimentPlan{}, fmt.Errorf("at least one case is required")
	}
	if runs < 2 {
		return ExperimentPlan{}, fmt.Errorf("runs must be at least 2")
	}
	if runs > contracts.MaxRuns {
		return ExperimentPlan{}, fmt.Errorf("runs must not exceed %d", contracts.MaxRuns)
	}
	seen := make(map[string]struct{}, len(caseIDs))
	for _, caseID := range caseIDs {
		if caseID == "" {
			return ExperimentPlan{}, fmt.Errorf("case id is required")
		}
		if _, exists := seen[caseID]; exists {
			return ExperimentPlan{}, fmt.Errorf("duplicate case id %q", caseID)
		}
		seen[caseID] = struct{}{}
	}
	random := rand.New(rand.NewSource(int64(seed))) // #nosec G404 -- experiment reproducibility, not security.
	plan := ExperimentPlan{
		Method:               BalancedBlockedMethod,
		Seed:                 seed,
		RunsPerCase:          runs,
		SerializeWithinBlock: true,
	}
	for _, caseID := range caseIDs {
		orders := make([]bool, runs) // true means AB, false means BA.
		for i := 0; i < runs/2; i++ {
			orders[i] = true
		}
		if runs%2 == 1 {
			orders[runs-1] = random.Intn(2) == 0
		}
		random.Shuffle(len(orders), func(i, j int) { orders[i], orders[j] = orders[j], orders[i] })
		for repetition, controlFirst := range orders {
			order := []Variant{VariantCandidate, VariantControl}
			if controlFirst {
				order = []Variant{VariantControl, VariantCandidate}
			}
			plan.Blocks = append(plan.Blocks, BlockPlan{
				ID:         fmt.Sprintf("%s-%04d", caseID, repetition+1),
				CaseID:     caseID,
				Repetition: repetition + 1,
				Order:      order,
			})
		}
	}
	return plan, nil
}
