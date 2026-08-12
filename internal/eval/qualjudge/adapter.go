package qualjudge

import (
	"fmt"

	"github.com/joeldevz/skynex/internal/eval/judges"
)

type CheckMetadata struct {
	ID             string
	Category       judges.Category
	RequirementIDs []string
	EvidenceIDs    []string
}

// ToCheckResult converts a qualitative Result to a soft deterministic-judge
// check. Inconclusive remains informational and cannot invalidate a passing
// deterministic verdict.
func ToCheckResult(result Result, metadata CheckMetadata) judges.CheckResult {
	id := metadata.ID
	if id == "" {
		id = "qualitative.opinion"
	}
	category := metadata.Category
	if category == "" {
		category = judges.CategoryBehavior
	}
	outcome := judges.OutcomeInvalid
	switch result.Verdict {
	case VerdictPass:
		outcome = judges.OutcomePass
	case VerdictFail:
		outcome = judges.OutcomeFail
	case VerdictInconclusive:
		outcome = judges.OutcomeInvalid
	}
	return judges.CheckResult{
		ID:             id,
		Category:       category,
		Outcome:        outcome,
		Hard:           false,
		Summary:        fmt.Sprintf("qualitative verdict=%s score=%.3f confidence=%.3f: %s", result.Verdict, result.Score, result.Confidence, result.Rationale),
		RequirementIDs: append([]string(nil), metadata.RequirementIDs...),
		EvidenceIDs:    append([]string(nil), metadata.EvidenceIDs...),
	}
}

// AddOpinion delegates status combination to judges.AddQualitativeOpinion,
// whose hard-failure/invalid state is authoritative. A qualitative pass can
// therefore never compensate for a deterministic fail or invalid result.
func AddOpinion(verdict judges.Verdict, result Result, metadata CheckMetadata) judges.Verdict {
	return judges.AddQualitativeOpinion(verdict, ToCheckResult(result, metadata))
}
