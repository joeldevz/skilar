package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientUsesCurrentSessionAPIAndPreservesTraceFields(t *testing.T) {
	t.Parallel()

	var requested []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("directory"); got != "/fixture/run-1" {
			t.Errorf("directory query = %q", got)
		}
		requested = append(requested, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /session":
			var body CreateSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			if body.Title != "runtime eval" {
				t.Errorf("title = %q", body.Title)
			}
			io.WriteString(w, `{"id":"ses_root","projectID":"p1","directory":"/fixture/run-1","title":"runtime eval","version":"1.18.16","time":{"created":1000,"updated":1001}}`)
		case "POST /session/ses_root/message":
			rawBody, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("read message body: %v", readErr)
			}
			var body SendMessageRequest
			if err := json.Unmarshal(rawBody, &body); err != nil {
				t.Errorf("decode message body: %v", err)
			}
			if body.Agent != "skynex-orchestrator" || len(body.Parts) != 1 || body.Parts[0].Text != "do it" {
				t.Errorf("unexpected message body: %#v", body)
			}
			var wire struct {
				Parts []map[string]json.RawMessage `json:"parts"`
			}
			if err := json.Unmarshal(rawBody, &wire); err != nil {
				t.Errorf("decode message wire body: %v", err)
			} else if len(wire.Parts) != 1 || len(wire.Parts[0]) != 2 || wire.Parts[0]["state"] != nil || wire.Parts[0]["tokens"] != nil || wire.Parts[0]["time"] != nil {
				t.Errorf("text part contains invalid zero-value fields: %s", rawBody)
			}
			io.WriteString(w, currentMessageJSON())
		case "GET /session/ses_root":
			io.WriteString(w, `{"id":"ses_root","projectID":"p1","directory":"/fixture/run-1","title":"runtime eval","version":"1.18.16","time":{"created":1000,"updated":1001}}`)
		case "GET /session/ses_root/children":
			io.WriteString(w, `[{"id":"ses_child","projectID":"p1","directory":"/fixture/run-1","parentID":"ses_root","title":"child","version":"1.18.16","time":{"created":1002,"updated":1003}}]`)
		case "GET /session/ses_root/message":
			io.WriteString(w, "["+currentMessageJSON()+"]")
		case "GET /session/status":
			io.WriteString(w, `{"ses_root":{"type":"retry","attempt":2,"message":"rate limited","next":1800}}`)
		case "GET /experimental/tool/ids":
			io.WriteString(w, `["bash","read","worker_failure_worker_result"]`)
		case "GET /mcp":
			io.WriteString(w, `{"ambient":{"status":"disabled"},"worker_failure":{"status":"connected"}}`)
		case "GET /provider":
			io.WriteString(w, `{"all":[{"id":"openai","key":"must-not-survive","options":{"apiKey":"must-not-survive"},"models":{"gpt-5.6-terra":{"id":"gpt-5.6-terra","providerID":"openai"}}}],"default":{"openai":"gpt-5.6-terra"},"connected":["openai"]}`)
		case "GET /global/health":
			io.WriteString(w, `{"healthy":true,"version":"1.18.16"}`)
		default:
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	c := New(Config{BaseURL: server.URL, Directory: "/fixture/run-1", RequestTimeout: time.Second})
	session, err := c.CreateSession("runtime eval")
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "ses_root" || session.CreatedAt.UnixMilli() != 1000 || session.ParentID != "" {
		t.Fatalf("session = %#v", session)
	}
	response, err := c.SendMessage(session.ID, "skynex-orchestrator", []Part{{Type: "text", Text: "do it"}})
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentMessage(t, *response)

	ctx := context.Background()
	if _, err := c.GetSessionContext(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	children, err := c.GetChildrenContext(ctx, session.ID)
	if err != nil || len(children) != 1 || children[0].ParentID != session.ID {
		t.Fatalf("children = %#v, err = %v", children, err)
	}
	messages, err := c.GetMessagesContext(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	assertCurrentMessage(t, messages[0])
	statuses, err := c.GetSessionStatusesContext(ctx)
	if err != nil || statuses[session.ID].Attempt != 2 || statuses[session.ID].Next != 1800 {
		t.Fatalf("statuses = %#v, err = %v", statuses, err)
	}
	toolIDs, err := c.GetToolIDsContext(ctx)
	if err != nil || fmt.Sprint(toolIDs) != "[bash read worker_failure_worker_result]" {
		t.Fatalf("tool IDs = %#v, err = %v", toolIDs, err)
	}
	mcp, err := c.GetMCPStatusCatalogContext(ctx)
	if err != nil || mcp.Statuses["worker_failure"] != MCPStatusConnected || mcp.Statuses["ambient"] != MCPStatusDisabled || mcp.SHA256 == "" {
		t.Fatalf("MCP status catalog = %#v, err = %v", mcp, err)
	}
	providers, err := c.GetProviderCatalogContext(ctx)
	if err != nil || len(providers.All) != 1 || providers.All[0].ID != "openai" || providers.All[0].Models["gpt-5.6-terra"].ID != "gpt-5.6-terra" || providers.SHA256 == "" {
		t.Fatalf("providers = %#v, err = %v", providers, err)
	}
	encodedProviders, err := json.Marshal(providers)
	if err != nil || bytes.Contains(encodedProviders, []byte("must-not-survive")) {
		t.Fatalf("safe provider catalog leaked opaque authority: %s, err=%v", encodedProviders, err)
	}
	safeProviderJSON, err := json.Marshal(struct {
		All       []ProviderSummary `json:"all"`
		Default   map[string]string `json:"default"`
		Connected []string          `json:"connected"`
	}{All: providers.All, Default: providers.Default, Connected: providers.Connected})
	if err != nil {
		t.Fatal(err)
	}
	safeProviderSum := sha256.Sum256(safeProviderJSON)
	if want := fmt.Sprintf("sha256:%x", safeProviderSum[:]); providers.SHA256 != want {
		t.Fatalf("provider digest commits to raw/secret-bearing JSON: got %s, want %s", providers.SHA256, want)
	}
	health, err := c.HealthInfoContext(ctx)
	if err != nil || !health.Healthy || health.Version != "1.18.16" {
		t.Fatalf("health = %#v, err = %v", health, err)
	}

	wantPaths := []string{
		"POST /session",
		"POST /session/ses_root/message",
		"GET /session/ses_root",
		"GET /session/ses_root/children",
		"GET /session/ses_root/message",
		"GET /session/status",
		"GET /experimental/tool/ids",
		"GET /mcp",
		"GET /provider",
		"GET /global/health",
	}
	if fmt.Sprint(requested) != fmt.Sprint(wantPaths) {
		t.Fatalf("requests = %v, want %v", requested, wantPaths)
	}
}

func TestMCPStatusCatalogIsStrictAndDropsFailureDetails(t *testing.T) {
	t.Parallel()
	const secret = "must-not-survive"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"z_failed":{"status":"failed","error":"`+secret+`"},"a_connected":{"status":"connected"}}`)
	}))
	defer server.Close()
	catalog, err := New(Config{BaseURL: server.URL}).GetMCPStatusCatalogContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses := map[string]MCPStatus{"a_connected": MCPStatusConnected, "z_failed": MCPStatusFailed}
	if fmt.Sprint(catalog.Statuses) != fmt.Sprint(wantStatuses) {
		t.Fatalf("statuses = %#v, want %#v", catalog.Statuses, wantStatuses)
	}
	encoded, err := json.Marshal(catalog)
	if err != nil || bytes.Contains(encoded, []byte(secret)) {
		t.Fatalf("sanitized MCP catalog leaked failure detail: %s, err=%v", encoded, err)
	}
	safeJSON, err := json.Marshal(wantStatuses)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(safeJSON)
	if want := fmt.Sprintf("sha256:%x", sum[:]); catalog.SHA256 != want {
		t.Fatalf("MCP digest = %s, want %s", catalog.SHA256, want)
	}

	malformed := []string{
		`null`,
		`[]`,
		`{"bad name":{"status":"connected"}}`,
		`{"a":{"status":"connected"},"a":{"status":"disabled"}}`,
		`{"a":{}}`,
		`{"a":{"status":"unknown"}}`,
		`{"a":{"status":"connected","extra":true}}`,
		`{"a":{"status":"failed"}}`,
		`{"a":{"status":"connected","error":"unexpected"}}`,
		`{"a":{"status":"failed","error":3}}`,
		`{"a":{"status":"connected","status":"disabled"}}`,
		`{"a":{"status":"connected"}} {}`,
	}
	for _, raw := range malformed {
		if _, err := decodeMCPStatusCatalog(json.RawMessage(raw)); err == nil {
			t.Errorf("malformed MCP status catalog accepted: %s", raw)
		}
	}
	_, err = decodeMCPStatusCatalog(json.RawMessage(`{"a":{"status":"failed","error":"` + secret + `","extra":true}}`))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("malformed MCP error leaked failure detail: %v", err)
	}
}

func TestVerifyRequiredAPIRequiresMCPStatusEndpoint(t *testing.T) {
	t.Parallel()
	document := json.RawMessage(`{"paths":{
		"/session":{"post":{}},"/session/{id}":{"get":{}},
		"/session/{id}/children":{"get":{}},"/session/{id}/message":{"get":{},"post":{}},
		"/session/status":{"get":{}},"/global/event":{"get":{}},
		"/experimental/tool/ids":{"get":{}},"/mcp":{"get":{}},"/provider":{"get":{}}
	}}`)
	routes, err := VerifyRequiredAPI(document)
	if err != nil || len(routes) != 10 {
		t.Fatalf("VerifyRequiredAPI() = %v, %v", routes, err)
	}
	missing := bytes.Replace(document, []byte(`,"/mcp":{"get":{}}`), nil, 1)
	if _, err := VerifyRequiredAPI(missing); err == nil || !strings.Contains(err.Error(), "GET /mcp") {
		t.Fatalf("missing /mcp error = %v", err)
	}
}

func TestProviderCatalogRejectsAmbiguousOrDanglingIdentity(t *testing.T) {
	valid := ProviderCatalog{
		All: []ProviderSummary{{ID: "openai", Models: map[string]ModelSummary{
			"gpt": {ID: "gpt", ProviderID: "openai"},
		}}},
		Default: map[string]string{"openai": "gpt"}, Connected: []string{"openai"},
	}
	for _, mutate := range []func(*ProviderCatalog){
		func(c *ProviderCatalog) { c.All[0].Models["gpt"] = ModelSummary{ID: "other", ProviderID: "openai"} },
		func(c *ProviderCatalog) { c.All[0].Models["gpt"] = ModelSummary{ID: "gpt"} },
		func(c *ProviderCatalog) { c.Connected = []string{"missing"} },
		func(c *ProviderCatalog) { c.Default = map[string]string{"openai": "missing"} },
	} {
		catalog := valid
		catalog.All = append([]ProviderSummary(nil), valid.All...)
		catalog.All[0].Models = map[string]ModelSummary{"gpt": valid.All[0].Models["gpt"]}
		catalog.Default = map[string]string{"openai": "gpt"}
		catalog.Connected = append([]string(nil), valid.Connected...)
		mutate(&catalog)
		if err := canonicalizeProviderCatalog(&catalog); err == nil {
			t.Fatalf("malformed provider catalog was accepted: %#v", catalog)
		}
	}
}

func currentMessageJSON() string {
	return `{
  "info": {
    "id": "msg_assistant", "sessionID": "ses_root", "role": "assistant",
    "parentID": "msg_user", "time": {"created": 1100, "completed": 1350},
    "modelID": "model-x", "providerID": "provider-x", "mode": "build",
    "path": {"cwd": "/fixture/run-1", "root": "/fixture/run-1"},
    "cost": 0.012, "tokens": {"input": 10, "output": 4, "reasoning": 2, "cache": {"read": 3, "write": 1}},
    "finish": "stop"
  },
  "parts": [
    {"id":"part_start","sessionID":"ses_root","messageID":"msg_assistant","type":"step-start","snapshot":"snap-before"},
    {"id":"part_text","sessionID":"ses_root","messageID":"msg_assistant","type":"text","text":"done","time":{"start":1110,"end":1120}},
    {"id":"part_tool","sessionID":"ses_root","messageID":"msg_assistant","type":"tool","callID":"call_1","tool":"edit","state":{"status":"completed","input":{"filePath":"a.go"},"output":"ok","title":"edit","metadata":{"exit":0},"time":{"start":1121,"end":1200}}},
    {"id":"part_step","sessionID":"ses_root","messageID":"msg_assistant","type":"step-finish","reason":"stop","cost":0.012,"tokens":{"input":10,"output":4,"reasoning":2,"cache":{"read":3,"write":1}}},
    {"id":"part_retry","sessionID":"ses_root","messageID":"msg_assistant","type":"retry","attempt":1,"error":{"name":"APIError","data":{"message":"retry"}},"time":{"created":1210}},
    {"id":"part_compact","sessionID":"ses_root","messageID":"msg_assistant","type":"compaction","auto":true}
  ]
}`
}

func assertCurrentMessage(t *testing.T, message Message) {
	t.Helper()
	if message.Info.ID != "msg_assistant" || message.Info.SessionID != "ses_root" || message.Info.Role != "assistant" {
		t.Fatalf("info IDs/role lost: %#v", message.Info)
	}
	if message.Info.Duration != 250*time.Millisecond {
		t.Errorf("duration = %s", message.Info.Duration)
	}
	if got := message.Info.Tokens; !got.Present || got.Total != 16 || got.Reasoning != 2 || got.CacheRead != 3 || got.CacheWrite != 1 {
		t.Errorf("message tokens = %#v", got)
	}
	if len(message.Parts) != 6 {
		t.Fatalf("parts = %#v", message.Parts)
	}
	if message.Parts[0].Type != "step-start" || message.Parts[0].Snapshot != "snap-before" {
		t.Errorf("step-start part = %#v", message.Parts[0])
	}
	tool := message.Parts[2]
	if tool.ID != "part_tool" || tool.CallID != "call_1" || tool.State.Status != "completed" || tool.ToolOutput != "ok" || string(tool.ToolInput) != `{"filePath":"a.go"}` {
		t.Errorf("tool part = %#v", tool)
	}
	step := message.Parts[3]
	if !step.Tokens.Present || step.Tokens.Reasoning != 2 || step.Tokens.Cache.Read != 3 {
		t.Errorf("step part = %#v", step)
	}
	if message.Parts[4].Attempt != 1 || message.Parts[4].Error == nil || message.Parts[4].Time.Created != 1210 {
		t.Errorf("retry part = %#v", message.Parts[4])
	}
	if !message.Parts[5].Auto {
		t.Errorf("compaction part = %#v", message.Parts[5])
	}
}

func TestClientGlobalEventSSE(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/global/event" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": heartbeat\n")
		io.WriteString(w, "id: evt-1\n")
		io.WriteString(w, `data: {"directory":"/fixture",`+"\n")
		io.WriteString(w, `data: "payload":{"type":"session.status","properties":{"sessionID":"s1","status":{"type":"busy"}}}}`+"\n\n")
	}))
	defer server.Close()

	stream, err := New(Config{BaseURL: server.URL}).OpenGlobalEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	event, err := stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Directory != "/fixture" || event.Payload.Type != "session.status" || event.SSEID != "evt-1" || event.ReceivedAt.IsZero() {
		t.Fatalf("event = %#v", event)
	}
	if _, err := stream.Next(); err != io.EOF {
		t.Fatalf("second Next error = %v, want EOF", err)
	}
}

func TestCompatibilityDocumentsAndHealthVersion(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/path":
			io.WriteString(w, `{"directory":"/fixture"}`)
		case "/config":
			io.WriteString(w, `{"model":"provider/model"}`)
		case "/agent":
			io.WriteString(w, `[{"name":"build"}]`)
		case "/doc":
			io.WriteString(w, `{"openapi":"3.1.0"}`)
		case "/global/health":
			io.WriteString(w, `{"healthy":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := New(Config{BaseURL: server.URL})

	for name, get := range map[string]func(context.Context) (RawDocument, error){
		"path": c.GetPathContext, "config": c.GetConfigContext,
		"agent": c.GetAgentsContext, "doc": c.GetOpenAPIDocumentContext,
	} {
		doc, err := get(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(doc.SHA256, "sha256:") || len(doc.Body) == 0 {
			t.Errorf("%s document = %#v", name, doc)
		}
	}
	if _, err := c.HealthInfoContext(context.Background()); err == nil || !strings.Contains(err.Error(), "missing version") {
		t.Fatalf("health error = %v", err)
	}
}

func TestHTTPErrorIncludesBoundedBody(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, strings.Repeat("x", 100_000))
	}))
	defer server.Close()
	_, err := New(Config{BaseURL: server.URL}).CreateSession("x")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if httpErr.StatusCode != http.StatusConflict || len(httpErr.Body) > 64<<10 {
		t.Fatalf("HTTP error = %#v", httpErr)
	}
}

func TestJSONResponsesRejectOversizeAndTrailingValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		body     string
		maxBytes int64
		want     string
	}{
		{name: "oversize", body: `{"id":"session-id-that-is-too-large"}`, maxBytes: 12, want: "exceeds 12 bytes"},
		{name: "trailing JSON", body: `{"id":"s1"} {"second":true}`, maxBytes: 128, want: "invalid character"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			_, err := New(Config{BaseURL: server.URL, MaxBodyBytes: test.maxBytes}).CreateSession("x")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}
