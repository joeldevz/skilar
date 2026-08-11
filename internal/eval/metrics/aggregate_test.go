package metrics

import (
	"math"
	"testing"
)

func TestAggregateSessionDeduplicatesExactMessagesAndSeparatesUsage(t *testing.T) {
	provider := CostValue{Available: true, USD: 0.4, Source: CostSourceProvider}
	calculated := CostValue{Available: true, USD: 0.3, Source: CostSourceCalculated}
	first := MessageUsage{
		SessionID: "parent", MessageID: "m1", Sequence: 1,
		Tokens:       TokenUsage{Input: 10, Output: 2, Reasoning: 1, CacheRead: 3, CacheWrite: 4},
		ProviderCost: provider, CalculatedCost: calculated, DurationMS: 5,
	}
	second := MessageUsage{
		SessionID: "parent", MessageID: "m2", Sequence: 2,
		Tokens:       TokenUsage{Input: 25, Output: 4, Reasoning: 2, CacheRead: 6, CacheWrite: 8},
		ProviderCost: provider, CalculatedCost: calculated, DurationMS: 7,
	}
	aggregate := AggregateSession(SessionUsage{SessionID: "parent", Messages: []MessageUsage{second, first, first}})
	if !aggregate.Complete {
		t.Fatalf("exact duplicate should not make telemetry incomplete: %+v", aggregate.Issues)
	}
	if aggregate.Messages != 2 || aggregate.Sessions != 1 {
		t.Fatalf("unexpected counts: messages=%d sessions=%d", aggregate.Messages, aggregate.Sessions)
	}
	want := (TokenUsage{Input: 35, Output: 6, Reasoning: 3, CacheRead: 9, CacheWrite: 12})
	if aggregate.Tokens != want {
		t.Fatalf("usage = %+v, want %+v", aggregate.Tokens, want)
	}
	if aggregate.FirstInputTokens != 10 || aggregate.PeakInputTokens != 25 {
		t.Fatalf("first/peak = %d/%d", aggregate.FirstInputTokens, aggregate.PeakInputTokens)
	}
	if !aggregate.ProviderCost.Available || aggregate.ProviderCost.USD != 0.8 ||
		!aggregate.CalculatedCost.Available || aggregate.CalculatedCost.USD != 0.6 {
		t.Fatalf("unexpected cost totals: provider=%+v calculated=%+v", aggregate.ProviderCost, aggregate.CalculatedCost)
	}
	if len(aggregate.Issues) != 1 || aggregate.Issues[0].Code != "duplicate_message" || aggregate.Issues[0].Fatal {
		t.Fatalf("unexpected issues: %+v", aggregate.Issues)
	}
}

func TestAggregateSessionConflictingAndMissingMessagesFailClosed(t *testing.T) {
	base := MessageUsage{SessionID: "s", MessageID: "m", Tokens: TokenUsage{Input: 10}}
	conflict := base
	conflict.Tokens.Input = 99
	missing := MessageUsage{SessionID: "s", Tokens: TokenUsage{Input: 500}}
	aggregate := AggregateSession(SessionUsage{SessionID: "s", Messages: []MessageUsage{base, conflict, missing}})
	if aggregate.Complete {
		t.Fatal("conflicting/missing identities must make telemetry incomplete")
	}
	if aggregate.Tokens.Input != 10 || aggregate.Messages != 1 {
		t.Fatalf("unsafe observations were counted: %+v", aggregate)
	}
	assertIssue(t, aggregate.Issues, "conflicting_message")
	assertIssue(t, aggregate.Issues, "missing_message_id")
}

func TestAggregateSessionCostUnavailableIsExplicit(t *testing.T) {
	known := MessageUsage{
		SessionID: "s", MessageID: "m1", Tokens: TokenUsage{Input: 1},
		ProviderCost: CostValue{Available: true, USD: 1, Source: CostSourceProvider},
	}
	unknown := MessageUsage{SessionID: "s", MessageID: "m2", Tokens: TokenUsage{Input: 1}}
	aggregate := AggregateSession(SessionUsage{SessionID: "s", Messages: []MessageUsage{known, unknown}})
	if aggregate.ProviderCost.Available || aggregate.ProviderCost.USD != 0 {
		t.Fatalf("partial provider cost must not look authoritative: %+v", aggregate.ProviderCost)
	}
	if aggregate.ProviderCost.KnownUSD != 1 || aggregate.ProviderCost.UnavailableValues != 1 {
		t.Fatalf("partial evidence was not retained: %+v", aggregate.ProviderCost)
	}
}

func TestAggregateTreeCountsEachReachableSessionOnce(t *testing.T) {
	root := SessionUsage{SessionID: "root", Messages: []MessageUsage{{SessionID: "root", MessageID: "r1", Tokens: TokenUsage{Input: 10}}}}
	child := SessionUsage{SessionID: "child", ParentID: "root", Messages: []MessageUsage{{SessionID: "child", MessageID: "c1", Tokens: TokenUsage{Input: 20, Reasoning: 3}}}}
	aggregate := AggregateTree("root", []SessionUsage{child, root, child})
	if !aggregate.Complete {
		t.Fatalf("exact duplicate session should be safely deduplicated: %+v", aggregate.Issues)
	}
	if aggregate.Sessions != 2 || aggregate.Messages != 2 || aggregate.Tokens.Input != 30 || aggregate.Tokens.Reasoning != 3 {
		t.Fatalf("unexpected tree aggregate: %+v", aggregate)
	}
	assertIssue(t, aggregate.Issues, "duplicate_session")
	usage := Summarize("root", []SessionUsage{root, child})
	if usage.Parent.Tokens.Input != 10 || usage.Tree.Tokens.Input != 30 {
		t.Fatalf("parent/tree separation failed: %+v", usage)
	}
}

func TestAggregateTreeRejectsOrphansAndUnreachableSessions(t *testing.T) {
	root := SessionUsage{SessionID: "root", Messages: []MessageUsage{{SessionID: "root", MessageID: "r", Tokens: TokenUsage{Input: 1}}}}
	orphan := SessionUsage{SessionID: "orphan", ParentID: "missing", Messages: []MessageUsage{{SessionID: "orphan", MessageID: "o", Tokens: TokenUsage{Input: 1_000}}}}
	unlinked := SessionUsage{SessionID: "other", Messages: []MessageUsage{{SessionID: "other", MessageID: "u", Tokens: TokenUsage{Input: 2_000}}}}
	aggregate := AggregateTree("root", []SessionUsage{root, orphan, unlinked})
	if aggregate.Complete {
		t.Fatal("orphan/unlinked sessions must fail closed")
	}
	if aggregate.Tokens.Input != 1 || aggregate.Sessions != 1 {
		t.Fatalf("unreachable usage contaminated tree: %+v", aggregate)
	}
	assertIssue(t, aggregate.Issues, "orphan_session")
	assertIssue(t, aggregate.Issues, "unlinked_session")
	assertIssue(t, aggregate.Issues, "unreachable_session")
}

func TestAggregateSessionOverflowDoesNotWrap(t *testing.T) {
	aggregate := AggregateSession(SessionUsage{SessionID: "s", Messages: []MessageUsage{
		{SessionID: "s", MessageID: "a", Tokens: TokenUsage{Input: math.MaxInt64}},
		{SessionID: "s", MessageID: "b", Tokens: TokenUsage{Input: 1}},
	}})
	if aggregate.Complete || aggregate.Tokens.Input != math.MaxInt64 {
		t.Fatalf("overflow was not contained: %+v", aggregate)
	}
	assertIssue(t, aggregate.Issues, "usage_overflow")
}

func assertIssue(t *testing.T, issues []Issue, code string) {
	t.Helper()
	for _, issue := range issues {
		if issue.Code == code {
			return
		}
	}
	t.Fatalf("issue %q not found in %+v", code, issues)
}
