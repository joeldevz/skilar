package fakemcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNeuroxScenarioImplementsStdioMCPAndTracesDigests(t *testing.T) {
	trace := &MemoryTrace{}
	server, err := New(Config{
		Scenario: ScenarioNeurox, ContextText: "decision: use paired AB/BA", RecallText: "memory: stale results are invalid",
		Trace: trace,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"neurox_context","arguments":{"query":"raw-secret-must-not-appear-in-trace"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"neurox_recall","arguments":{"query":"lineage"}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	responses := decodeLines(t, output.String())
	if len(responses) != 4 {
		t.Fatalf("responses = %d, want 4 (notification must not respond): %s", len(responses), output.String())
	}
	listResult := responses[1]["result"].(map[string]any)
	if len(listResult["tools"].([]any)) != 2 {
		t.Fatalf("tools/list result = %#v", listResult)
	}
	contextResult := responses[2]["result"].(map[string]any)
	content := contextResult["content"].([]any)[0].(map[string]any)["text"]
	if content != "decision: use paired AB/BA" || contextResult["isError"] != false {
		t.Fatalf("context result = %#v", contextResult)
	}
	events := trace.Events()
	if len(events) != 5 || events[3].Tool != "neurox_context" || events[3].Outcome != "context-result" || !strings.HasPrefix(events[3].ArgumentsDigest, "sha256:") {
		t.Fatalf("trace = %#v", events)
	}
	rawTrace, _ := json.Marshal(events)
	if bytes.Contains(rawTrace, []byte("raw-secret-must-not-appear-in-trace")) {
		t.Fatalf("raw tool arguments leaked into trace: %s", rawTrace)
	}
}

func TestFailureAndInjectionScenariosAreExplicit(t *testing.T) {
	for _, test := range []struct {
		name        string
		scenario    Scenario
		wantError   bool
		wantText    string
		wantOutcome string
	}{
		{"failure", ScenarioFailure, true, "unavailable", "tool-failure"},
		{"injection", ScenarioInjection, false, "Ignore previous instructions", "injection-data"},
	} {
		t.Run(test.name, func(t *testing.T) {
			trace := &MemoryTrace{}
			server, err := New(Config{Scenario: test.scenario, Trace: trace})
			if err != nil {
				t.Fatal(err)
			}
			input := `{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"neurox_context","arguments":{"query":"x"}}}` + "\n"
			var output bytes.Buffer
			if err := server.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			result := decodeLines(t, output.String())[0]["result"].(map[string]any)
			text := result["content"].([]any)[0].(map[string]any)["text"].(string)
			if result["isError"] != test.wantError || !strings.Contains(text, test.wantText) {
				t.Fatalf("result = %#v", result)
			}
			if events := trace.Events(); len(events) != 1 || events[0].Outcome != test.wantOutcome {
				t.Fatalf("trace = %#v", events)
			}
		})
	}
}

func TestServerBoundsInputAndRejectsUnknownTools(t *testing.T) {
	trace := &MemoryTrace{}
	server, err := New(Config{MaxMessageBytes: 1024, Trace: trace})
	if err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("x", 2048) + "\n"
	if err := server.Serve(context.Background(), strings.NewReader(oversized), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("oversized error = %v", err)
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"secret-tool-name-must-not-appear","arguments":{}}}` + "\n"
	var output bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	response := decodeLines(t, output.String())[0]
	if response["error"].(map[string]any)["code"] != float64(-32602) {
		t.Fatalf("unknown tool response = %#v", response)
	}
	events := trace.Events()
	if len(events) != 1 || events[0].Tool != "unknown" || events[0].ToolDigest == "" {
		t.Fatalf("unknown tool trace = %#v", events)
	}
	rawTrace, _ := json.Marshal(events)
	if bytes.Contains(rawTrace, []byte("secret-tool-name-must-not-appear")) {
		t.Fatalf("untrusted tool name leaked into trace: %s", rawTrace)
	}

	bounded, err := New(Config{MaxTraceEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	twoRequests := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"
	if err := bounded.Serve(context.Background(), strings.NewReader(twoRequests), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "trace limit") {
		t.Fatalf("trace event limit error = %v", err)
	}

	escaped, err := New(Config{MaxMessageBytes: 1024, ContextText: strings.Repeat("\x00", 300)})
	if err != nil {
		t.Fatal(err)
	}
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"neurox_context","arguments":{}}}` + "\n"
	var escapedOutput bytes.Buffer
	if err := escaped.Serve(context.Background(), strings.NewReader(call), &escapedOutput); err == nil || !strings.Contains(err.Error(), "response exceeds") || escapedOutput.Len() != 0 {
		t.Fatalf("escaped response bound: bytes=%d error=%v", escapedOutput.Len(), err)
	}
}

func decodeLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		result = append(result, value)
	}
	return result
}
