package metrics

import (
	"encoding/json"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/client"
)

func TestLegacyExtractionUsesCurrentUsageFieldsAndDoesNotDoubleCountResponse(t *testing.T) {
	user := client.Message{Info: client.ResponseInfo{ID: "u1", SessionID: "root", Role: "user"}}
	assistant := client.Message{
		Info: client.ResponseInfo{
			ID: "a1", SessionID: "root", Role: "assistant", ProviderID: "provider", ModelID: "unknown-model", Cost: 0.5,
			Tokens: client.TokenInfo{Present: true, Input: 10, Output: 2, Reasoning: 3, Cache: client.CacheTokenInfo{Read: 4, Write: 1}},
		},
		Parts: []client.Part{{Type: "tool", Tool: "task", ToolInput: json.RawMessage(`{"subagent_type":"coder"}`)}},
	}
	response := &client.Response{Info: assistant.Info, Parts: assistant.Parts}
	got := ExtractMetrics(response, []client.Message{user, assistant, assistant})
	if got.TokensInput != 10 || got.TokensOutput != 2 || got.TokensReasoning != 3 || got.TokensCached != 4 || got.TokensCacheWrite != 1 {
		t.Fatalf("token classes were lost/doubled: %+v", got)
	}
	if got.TokensTotal != 15 {
		t.Fatalf("cache breakdown was added to total: got %d, want 15", got.TokensTotal)
	}
	if got.ToolCallCount != 1 || len(got.SubagentCalls) != 1 || got.SubagentCalls[0] != "coder" {
		t.Fatalf("coordination was lost/doubled: %+v", got)
	}
	if got.Usage.Parent.Messages != 1 || got.Usage.Parent.Tokens.Input != 10 || got.Usage.Parent.Issues[0].Code != "duplicate_message" {
		t.Fatalf("message identity deduplication failed: %+v", got.Usage.Parent)
	}
	if got.ProviderCost.USD != 0.5 || !got.ProviderCost.Available {
		t.Fatalf("provider cost unavailable: %+v", got.ProviderCost)
	}
	if got.CalculatedCost.Available || got.CalculatedCost.Reason != "unknown_pricing" {
		t.Fatalf("unknown pricing did not remain unavailable: %+v", got.CalculatedCost)
	}
	if got.TelemetryComplete || got.Usage.Tree.Complete {
		t.Fatal("flat legacy extraction falsely claimed complete child telemetry")
	}
}
