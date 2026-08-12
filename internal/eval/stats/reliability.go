package stats

import "sort"

// SummarizeReliability treats pass/fail as completed decision outcomes. Invalid,
// inconclusive, aborted, budget, and infrastructure outcomes remain visible and
// never enter the pass-rate denominator. A case/variant is flaky only when its
// repetitions contain at least one pass and one fail.
func SummarizeReliability(outcomes []Outcome) ReliabilitySummary {
	result := ReliabilitySummary{
		Outcomes: append([]Outcome(nil), outcomes...),
		Counts:   make(map[Status]int),
	}
	type seenDecision struct{ pass, fail bool }
	groups := make(map[string]seenDecision)
	for _, outcome := range outcomes {
		result.Counts[outcome.Status]++
		key := outcome.CaseID + "\x00" + outcome.Variant
		decision := groups[key]
		switch outcome.Status {
		case StatusPass:
			decision.pass = true
		case StatusFail:
			decision.fail = true
		}
		groups[key] = decision
	}
	completed := result.Counts[StatusPass] + result.Counts[StatusFail]
	result.PassRate = makeRate(result.Counts[StatusPass], completed, "no_completed_outcomes")
	result.FailureRate = makeRate(result.Counts[StatusFail], completed, "no_completed_outcomes")
	result.InvalidRate = makeRate(result.Counts[StatusInvalid], len(outcomes), "no_outcomes")
	result.InconclusiveRate = makeRate(result.Counts[StatusInconclusive], len(outcomes), "no_outcomes")
	infrastructure := result.Counts[StatusInfraError] + result.Counts[StatusAborted] + result.Counts[StatusBudgetExhausted]
	result.InfrastructureRate = makeRate(infrastructure, len(outcomes), "no_outcomes")
	eligibleGroups := 0
	for key, decision := range groups {
		if !decision.pass && !decision.fail {
			continue
		}
		eligibleGroups++
		if decision.pass && decision.fail {
			result.FlakyCaseVariants = append(result.FlakyCaseVariants, key)
		}
	}
	sort.Strings(result.FlakyCaseVariants)
	result.FlakyRate = makeRate(len(result.FlakyCaseVariants), eligibleGroups, "no_completed_case_variants")
	return result
}

func makeRate(numerator, denominator int, reason string) Rate {
	if denominator == 0 {
		return Rate{Reason: reason}
	}
	return Rate{
		Available:   true,
		Value:       float64(numerator) / float64(denominator),
		Numerator:   numerator,
		Denominator: denominator,
	}
}
