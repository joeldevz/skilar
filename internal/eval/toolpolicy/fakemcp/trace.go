package fakemcp

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type TraceEvent struct {
	Sequence        uint64   `json:"sequence"`
	Scenario        Scenario `json:"scenario"`
	Method          string   `json:"method"`
	MethodDigest    string   `json:"method_digest,omitempty"`
	Tool            string   `json:"tool,omitempty"`
	ToolDigest      string   `json:"tool_digest,omitempty"`
	ArgumentsBytes  int      `json:"arguments_bytes"`
	ArgumentsDigest string   `json:"arguments_digest"`
	Outcome         string   `json:"outcome"`
	ResponseDigest  string   `json:"response_digest"`
}

type TraceSink interface {
	Record(TraceEvent) error
}

type DiscardTrace struct{}

func (DiscardTrace) Record(TraceEvent) error { return nil }

// JSONLTrace writes one bounded, secret-free event per line. It is safe for
// concurrent calls and can be backed by a private run trace file.
type JSONLTrace struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

func NewJSONLTrace(writer io.Writer) *JSONLTrace {
	return &JSONLTrace{encoder: json.NewEncoder(writer)}
}

func (t *JSONLTrace) Record(event TraceEvent) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.encoder.Encode(event)
}

type MemoryTrace struct {
	mu     sync.Mutex
	events []TraceEvent
}

func (t *MemoryTrace) Record(event TraceEvent) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.events) >= defaultMaxTraceEvents {
		return fmt.Errorf("fake MCP memory trace limit exceeded")
	}
	t.events = append(t.events, event)
	return nil
}

func (t *MemoryTrace) Events() []TraceEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TraceEvent(nil), t.events...)
}
