package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Rates are USD per one million tokens. A nil rate is unavailable, not zero.
type Rates struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	Reasoning  *float64 `json:"reasoning,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

type InputAccounting string

const (
	// InputExcludesCache means Input is the non-cached input class and cache
	// counts are additional billing classes.
	InputExcludesCache InputAccounting = "input_excludes_cache"
	// InputIncludesCache means cache counts are a breakdown of Input. The
	// calculator subtracts them before applying the ordinary input rate.
	InputIncludesCache InputAccounting = "input_includes_cache"
)

// Price identifies an exact provider/model combination. Model matching is
// deliberately exact; silently mapping a new model to a similarly named model
// would turn unknown pricing into false precision.
type Price struct {
	Provider        string          `json:"provider"`
	Model           string          `json:"model"`
	InputAccounting InputAccounting `json:"input_accounting"`
	Rates           Rates           `json:"rates"`
}

// PricingTable is versioned provenance for independently calculated costs.
type PricingTable struct {
	Version string  `json:"version"`
	Prices  []Price `json:"prices"`
}

// NewPricingTable validates and canonicalizes a pricing table.
func NewPricingTable(version string, prices []Price) (PricingTable, error) {
	if strings.TrimSpace(version) == "" {
		return PricingTable{}, fmt.Errorf("pricing table version is required")
	}
	table := PricingTable{Version: version, Prices: append([]Price(nil), prices...)}
	seen := make(map[string]struct{}, len(table.Prices))
	for i := range table.Prices {
		table.Prices[i].Provider = normalizeID(table.Prices[i].Provider)
		table.Prices[i].Model = normalizeID(table.Prices[i].Model)
		if table.Prices[i].Model == "" {
			return PricingTable{}, fmt.Errorf("price %d: model is required", i)
		}
		key := priceKey(table.Prices[i].Provider, table.Prices[i].Model)
		if table.Prices[i].InputAccounting != InputExcludesCache && table.Prices[i].InputAccounting != InputIncludesCache {
			return PricingTable{}, fmt.Errorf("price %s: input_accounting must be %q or %q", key, InputExcludesCache, InputIncludesCache)
		}
		if _, ok := seen[key]; ok {
			return PricingTable{}, fmt.Errorf("duplicate price for provider/model %q", key)
		}
		seen[key] = struct{}{}
		if err := validateRates(table.Prices[i].Rates); err != nil {
			return PricingTable{}, fmt.Errorf("price %s: %w", key, err)
		}
	}
	sort.Slice(table.Prices, func(i, j int) bool {
		return priceKey(table.Prices[i].Provider, table.Prices[i].Model) < priceKey(table.Prices[j].Provider, table.Prices[j].Model)
	})
	return table, nil
}

// Digest returns a deterministic SHA-256 provenance digest.
func (t PricingTable) Digest() (string, error) {
	canonical, err := NewPricingTable(t.Version, t.Prices)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshal pricing table: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// CalculateCost calculates cost only for an exact known provider/model and only
// when every non-zero token class has a declared rate.
func (t PricingTable) CalculateCost(tokens TokenUsage, provider, model string) CostValue {
	if err := tokens.Validate(); err != nil {
		return unavailableCalculated("invalid_token_usage")
	}
	provider = normalizeID(provider)
	model = normalizeID(model)
	var matched *Price
	for i := range t.Prices {
		if normalizeID(t.Prices[i].Provider) == provider && normalizeID(t.Prices[i].Model) == model {
			matched = &t.Prices[i]
			break
		}
	}
	if matched == nil {
		return unavailableCalculated("unknown_pricing")
	}
	rates := &matched.Rates
	input := tokens.Input
	if matched.InputAccounting == InputIncludesCache {
		cached, err := safeAdd(tokens.CacheRead, tokens.CacheWrite)
		if err != nil || cached > tokens.Input {
			return unavailableCalculated("invalid_cache_breakdown")
		}
		input = tokens.Input - cached
	}
	total := 0.0
	classes := []struct {
		count int64
		rate  *float64
	}{
		{input, rates.Input},
		{tokens.Output, rates.Output},
		{tokens.Reasoning, rates.Reasoning},
		{tokens.CacheRead, rates.CacheRead},
		{tokens.CacheWrite, rates.CacheWrite},
	}
	for _, class := range classes {
		if class.count == 0 {
			continue
		}
		if class.rate == nil {
			return unavailableCalculated("missing_token_class_price")
		}
		total += float64(class.count) * *class.rate / 1_000_000
	}
	if math.IsNaN(total) || math.IsInf(total, 0) || total < 0 {
		return unavailableCalculated("invalid_calculated_cost")
	}
	return CostValue{Available: true, USD: total, Source: CostSourceCalculated}
}

func unavailableCalculated(reason string) CostValue {
	return CostValue{Source: CostSourceCalculated, Reason: reason}
}

func validateRates(r Rates) error {
	values := []*float64{r.Input, r.Output, r.Reasoning, r.CacheRead, r.CacheWrite}
	for _, value := range values {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return fmt.Errorf("rates must be finite and non-negative")
		}
	}
	return nil
}

func normalizeID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func priceKey(provider, model string) string {
	return provider + "\x00" + model
}
