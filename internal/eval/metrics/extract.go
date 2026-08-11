package metrics

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/joeldevz/skynex/internal/eval/client"
)

// MetricsData is the legacy runner view. New code should retain Usage and the
// explicit cost values; CostUSD exists only to keep the pre-contract runner
// source-compatible and is zero when neither cost authority is available.
type MetricsData struct {
	TokensTotal        int          `json:"tokens_total"`
	TokensInput        int          `json:"tokens_input"`
	TokensOutput       int          `json:"tokens_output"`
	TokensReasoning    int          `json:"tokens_reasoning"`
	TokensCached       int          `json:"tokens_cached"`
	TokensCacheWrite   int          `json:"tokens_cache_write"`
	CostUSD            float64      `json:"cost_usd"`
	ProviderCost       CostValue    `json:"provider_cost"`
	CalculatedCost     CostValue    `json:"calculated_cost"`
	PricingTableDigest string       `json:"pricing_table_digest,omitempty"`
	DurationMS         int64        `json:"duration_ms"`
	ToolCallCount      int          `json:"tool_call_count"`
	SubagentCalls      []string     `json:"subagent_calls"`
	FilesWritten       []string     `json:"files_written"`
	Usage              UsageSummary `json:"usage"`
	TelemetryComplete  bool         `json:"telemetry_complete"`
}

// ExtractMetrics adapts the original flat OpenCode response into the normalized
// metrics model. It can prove parent-message completeness, but not descendant
// completeness; callers which collect a session tree should use Summarize.
func ExtractMetrics(response *client.Response, messages []client.Message) *MetricsData {
	return ExtractMetricsWithPricing(response, messages, DefaultPricingTable())
}

// ExtractMetricsWithPricing is the legacy adapter with an explicit, versioned
// pricing table.
func ExtractMetricsWithPricing(response *client.Response, messages []client.Message, table PricingTable) *MetricsData {
	result := &MetricsData{
		SubagentCalls: []string{},
		FilesWritten:  []string{},
	}
	sessionID := "legacy-parent"
	if response != nil && response.Info.SessionID != "" {
		sessionID = response.Info.SessionID
	} else {
		for _, message := range messages {
			if message.Info.SessionID != "" {
				sessionID = message.Info.SessionID
				break
			}
		}
	}
	session := SessionUsage{SessionID: sessionID}
	seenParts := make(map[string]struct{}, len(messages))
	for i, message := range messages {
		coordinationKey := message.Info.SessionID + "\x00" + message.Info.ID
		if message.Info.ID == "" {
			coordinationKey = "anonymous-" + strconv.Itoa(i)
		}
		if _, duplicate := seenParts[coordinationKey]; !duplicate {
			collectCoordination(&result.ToolCallCount, &result.SubagentCalls, &result.FilesWritten, message.Parts)
			seenParts[coordinationKey] = struct{}{}
		}
		if !hasUsage(message.Info) {
			continue
		}
		tokens := fromClientTokens(message.Info.Tokens)
		providerCost := providerCostValue(message.Info.Cost)
		calculatedCost := table.CalculateCost(tokens, message.Info.ProviderID, message.Info.ModelID)
		messageID := message.Info.ID
		if messageID == "" {
			messageID = "message-" + strconv.Itoa(i+1)
		}
		messageSessionID := message.Info.SessionID
		if messageSessionID == "" {
			messageSessionID = session.SessionID
		}
		session.Messages = append(session.Messages, MessageUsage{
			SessionID:      messageSessionID,
			MessageID:      messageID,
			Sequence:       i,
			Provider:       message.Info.ProviderID,
			Model:          message.Info.ModelID,
			Tokens:         tokens,
			ProviderCost:   providerCost,
			CalculatedCost: calculatedCost,
			DurationMS:     message.Info.Duration.Milliseconds(),
		})
	}
	// Some compatible servers return the final response before the messages
	// endpoint reflects it. Use it only when no message evidence is available so
	// the same response is never counted twice.
	if len(session.Messages) == 0 && response != nil && hasUsage(response.Info) {
		tokens := fromClientTokens(response.Info.Tokens)
		session.Messages = append(session.Messages, MessageUsage{
			SessionID:      session.SessionID,
			MessageID:      responseMessageID(response),
			Provider:       response.Info.ProviderID,
			Model:          response.Info.ModelID,
			Tokens:         tokens,
			ProviderCost:   providerCostValue(response.Info.Cost),
			CalculatedCost: table.CalculateCost(tokens, response.Info.ProviderID, response.Info.ModelID),
			DurationMS:     response.Info.Duration.Milliseconds(),
		})
		collectCoordination(&result.ToolCallCount, &result.SubagentCalls, &result.FilesWritten, response.Parts)
	}
	parent := AggregateSession(session)
	// The legacy API has no child discovery proof. Copying the parent into Tree
	// would falsely claim complete tree telemetry, so tree is explicitly marked
	// incomplete while retaining the known parent values.
	tree := parent
	tree.Complete = false
	tree.Issues = append(tree.Issues, Issue{Code: "child_telemetry_unavailable", SessionID: session.SessionID, Fatal: true})
	result.Usage = UsageSummary{Parent: parent, Tree: tree}
	result.TelemetryComplete = tree.Complete
	result.ProviderCost = costValueFromTotal(parent.ProviderCost, CostSourceProvider, "provider_cost_unavailable")
	result.CalculatedCost = costValueFromTotal(parent.CalculatedCost, CostSourceCalculated, "calculated_cost_unavailable")
	if result.ProviderCost.Available {
		result.CostUSD = result.ProviderCost.USD
	} else if result.CalculatedCost.Available {
		result.CostUSD = result.CalculatedCost.USD
	}
	if digest, err := table.Digest(); err == nil {
		result.PricingTableDigest = digest
	}
	result.TokensInput = safeInt(parent.Tokens.Input)
	result.TokensOutput = safeInt(parent.Tokens.Output)
	result.TokensReasoning = safeInt(parent.Tokens.Reasoning)
	result.TokensCached = safeInt(parent.Tokens.CacheRead)
	result.TokensCacheWrite = safeInt(parent.Tokens.CacheWrite)
	result.TokensTotal = safeInt(nonCacheTotalTokens(parent.Tokens))
	if response != nil {
		result.DurationMS = response.Info.Duration.Milliseconds()
	} else {
		result.DurationMS = parent.DurationMS
	}
	return result
}

// CalculateCost preserves the original helper signature without unsafe model
// fallback. Unknown model identifiers return zero; availability is exposed by
// PricingTable.CalculateCost and must be used by gates/reporters.
func CalculateCost(tokens client.TokenInfo, model string) float64 {
	cost := DefaultPricingTable().CalculateCost(fromClientTokens(tokens), "", model)
	if !cost.Available {
		return 0
	}
	return cost.USD
}

// DefaultPricingTable is intentionally small and exact. It exists for backwards
// compatibility with the legacy names the old evaluator recognized; production
// experiments should load a reviewed table and record its Digest.
func DefaultPricingTable() PricingTable {
	inputSonnet, outputSonnet, cacheReadSonnet, cacheWriteSonnet := 3.0, 15.0, 0.30, 3.75
	inputOpus, outputOpus, cacheReadOpus, cacheWriteOpus := 15.0, 75.0, 1.5, 18.75
	inputHaiku, outputHaiku, cacheReadHaiku, cacheWriteHaiku := 0.25, 1.25, 0.025, 0.3125
	table, err := NewPricingTable("legacy-exact-v1", []Price{
		{Model: "sonnet", InputAccounting: InputExcludesCache, Rates: Rates{Input: &inputSonnet, Output: &outputSonnet, Reasoning: &outputSonnet, CacheRead: &cacheReadSonnet, CacheWrite: &cacheWriteSonnet}},
		{Model: "opus", InputAccounting: InputExcludesCache, Rates: Rates{Input: &inputOpus, Output: &outputOpus, Reasoning: &outputOpus, CacheRead: &cacheReadOpus, CacheWrite: &cacheWriteOpus}},
		{Model: "haiku", InputAccounting: InputExcludesCache, Rates: Rates{Input: &inputHaiku, Output: &outputHaiku, Reasoning: &outputHaiku, CacheRead: &cacheReadHaiku, CacheWrite: &cacheWriteHaiku}},
	})
	if err != nil {
		panic(fmt.Sprintf("invalid built-in pricing table: %v", err))
	}
	return table
}

func fromClientTokens(tokens client.TokenInfo) TokenUsage {
	cacheRead := tokens.CacheRead
	if cacheRead == 0 && tokens.Cache.Read != 0 {
		cacheRead = tokens.Cache.Read
	}
	cacheWrite := tokens.CacheWrite
	if cacheWrite == 0 && tokens.Cache.Write != 0 {
		cacheWrite = tokens.Cache.Write
	}
	return TokenUsage{
		Input:      int64(tokens.Input),
		Output:     int64(tokens.Output),
		Reasoning:  int64(tokens.Reasoning),
		CacheRead:  int64(cacheRead),
		CacheWrite: int64(cacheWrite),
	}
}

func providerCostValue(cost float64) CostValue {
	if cost < 0 {
		return CostValue{Source: CostSourceProvider, Reason: "invalid_provider_cost"}
	}
	// The legacy wire type cannot distinguish a reported free request from an
	// omitted number. Treat zero as unavailable instead of inventing evidence.
	if cost == 0 {
		return CostValue{Source: CostSourceProvider, Reason: "provider_cost_unavailable"}
	}
	return CostValue{Available: true, USD: cost, Source: CostSourceProvider}
}

func costValueFromTotal(total CostTotal, source CostSource, reason string) CostValue {
	if !total.Available {
		if len(total.UnavailableReasons) == 1 {
			reason = total.UnavailableReasons[0]
		}
		return CostValue{Source: source, Reason: reason}
	}
	return CostValue{Available: true, USD: total.USD, Source: source}
}

func collectCoordination(toolCalls *int, subagents, filesWritten *[]string, parts []client.Part) {
	for _, part := range parts {
		if part.Type != "tool" {
			continue
		}
		*toolCalls++
		var input map[string]any
		if err := json.Unmarshal(part.ToolInput, &input); err != nil {
			continue
		}
		if part.Tool == "task" {
			if subtype, ok := input["subagent_type"].(string); ok {
				*subagents = append(*subagents, subtype)
			}
		}
		if part.Tool == "write" || part.Tool == "edit" {
			if path, ok := input["filePath"].(string); ok {
				*filesWritten = append(*filesWritten, path)
			}
		}
	}
}

func safeInt(value int64) int {
	converted := int(value)
	if int64(converted) != value {
		if value < 0 {
			return -int(^uint(0)>>1) - 1
		}
		return int(^uint(0) >> 1)
	}
	return converted
}

func nonCacheTotalTokens(tokens TokenUsage) int64 {
	total := int64(0)
	for _, value := range []int64{tokens.Input, tokens.Output, tokens.Reasoning} {
		next, err := safeAdd(total, value)
		if err != nil {
			return int64(^uint64(0) >> 1)
		}
		total = next
	}
	return total
}

func hasUsage(info client.ResponseInfo) bool {
	return info.Tokens.Present || info.Tokens.Input != 0 || info.Tokens.Output != 0 || info.Tokens.Reasoning != 0 ||
		info.Tokens.CacheRead != 0 || info.Tokens.CacheWrite != 0 || info.ModelID != "" || info.Cost != 0
}

func responseMessageID(response *client.Response) string {
	if response != nil && response.Info.ID != "" {
		return response.Info.ID
	}
	return "response-1"
}
