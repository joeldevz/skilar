package metrics

import "testing"

func TestPricingUsesExactModelAndEveryTokenClass(t *testing.T) {
	input, output, reasoning, read, write := 1.0, 2.0, 3.0, 4.0, 5.0
	table, err := NewPricingTable("test-v1", []Price{{
		Provider: " Example ", Model: " Model-V1 ",
		InputAccounting: InputExcludesCache,
		Rates:           Rates{Input: &input, Output: &output, Reasoning: &reasoning, CacheRead: &read, CacheWrite: &write},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cost := table.CalculateCost(TokenUsage{Input: 1_000_000, Output: 1_000_000, Reasoning: 1_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000}, "example", "model-v1")
	if !cost.Available || cost.USD != 15 {
		t.Fatalf("cost = %+v, want $15 available", cost)
	}
	unknown := table.CalculateCost(TokenUsage{Input: 1}, "example", "model-v2")
	if unknown.Available || unknown.Reason != "unknown_pricing" {
		t.Fatalf("unknown model must be unavailable: %+v", unknown)
	}
	// Similar names are not fuzzy-matched.
	unknown = table.CalculateCost(TokenUsage{Input: 1}, "example", "prefix-model-v1-suffix")
	if unknown.Available {
		t.Fatalf("fuzzy pricing match occurred: %+v", unknown)
	}
}

func TestPricingMissingUsedClassIsUnavailableButUnusedClassMayBeMissing(t *testing.T) {
	input := 2.0
	table, err := NewPricingTable("test-v1", []Price{{Provider: "p", Model: "m", InputAccounting: InputExcludesCache, Rates: Rates{Input: &input}}})
	if err != nil {
		t.Fatal(err)
	}
	if cost := table.CalculateCost(TokenUsage{Input: 500_000}, "p", "m"); !cost.Available || cost.USD != 1 {
		t.Fatalf("unused missing classes should not matter: %+v", cost)
	}
	if cost := table.CalculateCost(TokenUsage{Input: 1, Output: 1}, "p", "m"); cost.Available || cost.Reason != "missing_token_class_price" {
		t.Fatalf("used missing class should be unavailable: %+v", cost)
	}
}

func TestPricingDigestCanonicalizesOrderAndRejectsDuplicates(t *testing.T) {
	zero := 0.0
	a := Price{Provider: "p", Model: "a", InputAccounting: InputExcludesCache, Rates: Rates{Input: &zero}}
	b := Price{Provider: "p", Model: "b", InputAccounting: InputExcludesCache, Rates: Rates{Input: &zero}}
	left, _ := NewPricingTable("v", []Price{a, b})
	right, _ := NewPricingTable("v", []Price{b, a})
	leftDigest, _ := left.Digest()
	rightDigest, _ := right.Digest()
	if leftDigest != rightDigest {
		t.Fatalf("digest depends on input order: %s != %s", leftDigest, rightDigest)
	}
	if _, err := NewPricingTable("v", []Price{a, a}); err == nil {
		t.Fatal("duplicate price accepted")
	}
}

func TestPricingCanTreatCacheAsInputBreakdownWithoutDoubleBilling(t *testing.T) {
	input, read, write := 1.0, 4.0, 5.0
	table, err := NewPricingTable("v", []Price{{
		Provider: "p", Model: "m", InputAccounting: InputIncludesCache,
		Rates: Rates{Input: &input, CacheRead: &read, CacheWrite: &write},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cost := table.CalculateCost(TokenUsage{Input: 10_000_000, CacheRead: 3_000_000, CacheWrite: 1_000_000}, "p", "m")
	if !cost.Available || cost.USD != 23 {
		t.Fatalf("cache breakdown was double-counted: %+v", cost)
	}
	invalid := table.CalculateCost(TokenUsage{Input: 1, CacheRead: 2}, "p", "m")
	if invalid.Available || invalid.Reason != "invalid_cache_breakdown" {
		t.Fatalf("impossible cache breakdown accepted: %+v", invalid)
	}
}
