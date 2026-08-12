package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/judges"
	"github.com/joeldevz/skynex/internal/eval/lifecycle"
	"github.com/joeldevz/skynex/internal/eval/metrics"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/toolpolicy"
	"github.com/joeldevz/skynex/internal/eval/trace"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeRuntimeFactory struct {
	mu       sync.Mutex
	startErr error
	build    func(RuntimeRequest) *fakeRuntime
	requests []RuntimeRequest
	runtimes []*fakeRuntime
}

func (f *fakeRuntimeFactory) Start(_ context.Context, request RuntimeRequest) (Runtime, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	var runtime *fakeRuntime
	if f.build != nil {
		runtime = f.build(request)
	} else {
		runtime = newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
	}
	f.mu.Lock()
	f.runtimes = append(f.runtimes, runtime)
	f.mu.Unlock()
	return runtime, nil
}

func (f *fakeRuntimeFactory) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeRuntimeFactory) lastRequest(t *testing.T) RuntimeRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("runtime factory received no request")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeRuntimeFactory) lastRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.runtimes) == 0 {
		t.Fatal("runtime factory created no runtime")
	}
	return f.runtimes[len(f.runtimes)-1]
}

type fakeRuntime struct {
	request RuntimeRequest
	info    RuntimeInfo

	root     client.Session
	children map[string][]client.Session
	messages map[string][]client.Message
	statuses map[string]client.SessionStatus

	response    *client.Response
	create      func(context.Context, string) (*client.Session, error)
	send        func(context.Context, string, client.SendMessageRequest) (*client.Response, error)
	getMessage  func(context.Context, string, string) (*client.Message, error)
	getMessages func(context.Context, string) ([]client.Message, error)
	prompt      map[string]bool
	events      trace.EventSource
	eventHTTP   *httptest.Server
	sent        []client.SendMessageRequest
	closeErr    error
	closed      bool
	mu          sync.Mutex
}

func newFakeRuntime(request RuntimeRequest, writeMarker bool, messages []client.Message) *fakeRuntime {
	for index := range messages {
		if messages[index].Info.Role == "assistant" && messages[index].Info.Agent == "" {
			messages[index].Info.Agent = request.Case.Agent.Name
		}
	}
	root := client.Session{ID: "root", Directory: request.WorkspacePath, Title: "root", Time: client.SessionTime{Created: 1, Updated: 2}}
	prompt := cloneBoolMap(request.ToolPolicy.PromptTools)
	prompt["ambient_unknown"] = false
	toolsetDigest, err := contracts.CanonicalDigest(map[string]string{"policy": request.ToolPolicy.Digest, "catalog": testDigest})
	if err != nil {
		panic(err)
	}
	runtime := &fakeRuntime{
		request: request,
		info: RuntimeInfo{
			OpenCodeVersion: "1.18.16", OpenCodeAPI: testDigest,
			ConfigDigest: request.ToolPolicy.Digest, AgentsDigest: testDigest,
			ToolPolicyDigest: request.ToolPolicy.Digest, ToolCatalogDigest: testDigest,
			ToolsetDigest: toolsetDigest,
			ExecutionMode: contracts.ExecutionTrustedLocal,
			Network:       contracts.NetworkHostUnisolated,
		},
		root: root,
		children: map[string][]client.Session{
			"root": nil,
		},
		messages: map[string][]client.Message{"root": messages},
		statuses: map[string]client.SessionStatus{"root": {Type: "idle"}},
		prompt:   prompt,
	}
	runtime.eventHTTP, runtime.events = newHealthyFakeGlobalEventSource(root)
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Info.Role != "assistant" {
			continue
		}
		response := client.Response{Info: messages[index].Info, Parts: append([]client.Part(nil), messages[index].Parts...)}
		runtime.response = &response
		break
	}
	if runtime.response == nil {
		providerID, modelID, err := contracts.ParseModelSelection(request.Case.Agent.Model)
		if err != nil {
			panic(err)
		}
		runtime.response = &client.Response{
			Info: client.ResponseInfo{
				ID: "assistant-1", SessionID: "root", Role: "assistant", Finish: "stop",
				ProviderID: providerID, ModelID: modelID, Agent: request.Case.Agent.Name,
				Time: client.MessageTime{Created: 1, Completed: 2},
			},
			Parts: []client.Part{{ID: "text-1", SessionID: "root", MessageID: "assistant-1", Type: "text", Text: "Done successfully; verified."}},
		}
	}
	runtime.send = func(ctx context.Context, _ string, promptRequest client.SendMessageRequest) (*client.Response, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if writeMarker {
			if err := os.WriteFile(filepath.Join(request.WorkspacePath, "marker.txt"), []byte("ok\n"), 0o644); err != nil {
				return nil, err
			}
		}
		copy := *runtime.response
		copy.Parts = append([]client.Part(nil), runtime.response.Parts...)
		if copy.Info.ParentID == "" {
			copy.Info.ParentID = promptRequest.MessageID
		}
		parentPresent := false
		for _, message := range runtime.messages["root"] {
			parentPresent = parentPresent || message.Info.ID == promptRequest.MessageID
		}
		if !parentPresent {
			runtime.messages["root"] = append(runtime.messages["root"], client.Message{Info: client.ResponseInfo{
				ID: promptRequest.MessageID, SessionID: "root", Role: "user", Time: client.MessageTime{Created: 1},
			}})
		}
		for messageIndex := range runtime.messages["root"] {
			if runtime.messages["root"][messageIndex].Info.ID == copy.Info.ID && runtime.messages["root"][messageIndex].Info.ParentID == "" {
				runtime.messages["root"][messageIndex].Info.ParentID = promptRequest.MessageID
			}
		}
		return &copy, nil
	}
	return runtime
}

func newHealthyFakeGlobalEventSource(root client.Session) (*httptest.Server, trace.EventSource) {
	properties, _ := json.Marshal(struct {
		Info client.Session `json:"info"`
	}{Info: root})
	events := []client.Event{
		{Type: "server.connected", Properties: json.RawMessage(`{}`)},
		{Type: "server.heartbeat", Properties: json.RawMessage(`{}`)},
		{Type: "session.created", Properties: properties},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/global/event" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for index, event := range events {
			envelope, _ := json.Marshal(struct {
				Directory string       `json:"directory"`
				Payload   client.Event `json:"payload"`
			}{Directory: root.Directory, Payload: event})
			_, _ = fmt.Fprintf(w, "id: healthy-%d\ndata: %s\n\n", index+1, envelope)
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	return server, client.New(client.Config{BaseURL: server.URL})
}

func (r *fakeRuntime) Info() RuntimeInfo { return r.info }

func (r *fakeRuntime) PromptTools() map[string]bool {
	return cloneBoolMap(r.prompt)
}

func (r *fakeRuntime) Close() error {
	r.mu.Lock()
	r.closed = true
	eventHTTP := r.eventHTTP
	r.eventHTTP = nil
	closeErr := r.closeErr
	r.mu.Unlock()
	if eventHTTP != nil {
		eventHTTP.CloseClientConnections()
		eventHTTP.Close()
	}
	return closeErr
}

func (r *fakeRuntime) CreateSessionContext(ctx context.Context, title string) (*client.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.create != nil {
		return r.create(ctx, title)
	}
	copy := r.root
	return &copy, nil
}

func (r *fakeRuntime) SendMessageWithRequestContext(ctx context.Context, sessionID string, request client.SendMessageRequest) (*client.Response, error) {
	r.mu.Lock()
	copyRequest := request
	copyRequest.Tools = cloneBoolMap(request.Tools)
	if request.Model != nil {
		model := *request.Model
		copyRequest.Model = &model
	}
	copyRequest.Parts = append([]client.Part(nil), request.Parts...)
	r.sent = append(r.sent, copyRequest)
	r.mu.Unlock()
	return r.send(ctx, sessionID, request)
}

func (r *fakeRuntime) sentRequests() []client.SendMessageRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]client.SendMessageRequest(nil), r.sent...)
}

func (r *fakeRuntime) GetSessionContext(ctx context.Context, id string) (*client.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == r.root.ID {
		copy := r.root
		return &copy, nil
	}
	for _, children := range r.children {
		for _, child := range children {
			if child.ID == id {
				copy := child
				return &copy, nil
			}
		}
	}
	return nil, errors.New("session not found")
}

func (r *fakeRuntime) GetChildrenContext(ctx context.Context, id string) ([]client.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]client.Session(nil), r.children[id]...), nil
}

func (r *fakeRuntime) GetMessagesContext(ctx context.Context, id string) ([]client.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.getMessages != nil {
		return r.getMessages(ctx, id)
	}
	return append([]client.Message(nil), r.messages[id]...), nil
}

func (r *fakeRuntime) GetMessageContext(ctx context.Context, sessionID, messageID string) (*client.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.getMessage != nil {
		return r.getMessage(ctx, sessionID, messageID)
	}
	for _, message := range r.messages[sessionID] {
		if message.Info.ID != messageID {
			continue
		}
		copy := message
		copy.Parts = append([]client.Part(nil), message.Parts...)
		return &copy, nil
	}
	return nil, &client.HTTPError{Method: http.MethodGet, StatusCode: http.StatusNotFound}
}

func (r *fakeRuntime) GetSessionStatusesContext(ctx context.Context) (map[string]client.SessionStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	copy := make(map[string]client.SessionStatus, len(r.statuses))
	for id, status := range r.statuses {
		copy[id] = status
	}
	return copy, nil
}

func (r *fakeRuntime) OpenGlobalEvents(ctx context.Context) (*client.EventStream, error) {
	if r.events != nil {
		return r.events.OpenGlobalEvents(ctx)
	}
	// SSE loss makes aggregate metrics non-evaluable, while the durable API
	// remains sufficient for deterministic filesystem/tool checks.
	return nil, errors.New("fake event stream unavailable")
}

func (r *fakeRuntime) GetToolIDsContext(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []string{"ambient_unknown", "bash", "edit", "git_push"}, nil
}

func completeMessages(finalText string) []client.Message {
	tokens := client.TokenInfo{
		Total: 10, Input: 6, Output: 4, Present: true,
		Cache: client.CacheTokenInfo{},
	}
	return []client.Message{{
		Info: client.ResponseInfo{
			ID: "assistant-1", SessionID: "root", Role: "assistant", Finish: "stop",
			ProviderID: "openai", ModelID: "gpt-5", Tokens: tokens,
			Time: client.MessageTime{Created: 1, Completed: 2},
		},
		Parts: []client.Part{
			{ID: "step-start-1", SessionID: "root", MessageID: "assistant-1", Type: "step-start"},
			{ID: "tool-status", SessionID: "root", MessageID: "assistant-1", Type: "tool", CallID: "call-status", Tool: "bash", State: client.ToolState{Status: "completed", Input: json.RawMessage(`{"command":"git status --short"}`), Output: "working tree clean", Time: client.PartTime{Start: 1, End: 1}}},
			{ID: "tool-edit-1", SessionID: "root", MessageID: "assistant-1", Type: "tool", CallID: "call-edit-1", Tool: "edit", State: client.ToolState{Status: "completed", Input: json.RawMessage(`{"filePath":"marker.txt"}`), Time: client.PartTime{Start: 1, End: 1}}},
			{ID: "tool-edit-2", SessionID: "root", MessageID: "assistant-1", Type: "tool", CallID: "call-edit-2", Tool: "edit", State: client.ToolState{Status: "completed", Input: json.RawMessage(`{"filePath":"marker.txt"}`), Time: client.PartTime{Start: 1, End: 1}}},
			{ID: "text-1", SessionID: "root", MessageID: "assistant-1", Type: "text", Text: finalText},
			{ID: "step-finish-1", SessionID: "root", MessageID: "assistant-1", Type: "step-finish", Reason: "stop", Tokens: tokens, Time: client.PartTime{Start: 1, End: 1}},
		},
	}}
}

func incompleteUsageMessages(finalText string) []client.Message {
	return []client.Message{{
		Info: client.ResponseInfo{
			ID: "assistant-1", SessionID: "root", Role: "assistant", Finish: "stop",
			ProviderID: "openai", ModelID: "gpt-5", Time: client.MessageTime{Created: 1, Completed: 2},
		},
		Parts: []client.Part{
			{ID: "tool-status", SessionID: "root", MessageID: "assistant-1", Type: "tool", CallID: "call-status", Tool: "bash", State: client.ToolState{Status: "completed", Input: json.RawMessage(`{"command":"git status --short"}`), Output: "working tree clean"}},
			{ID: "tool-edit-1", SessionID: "root", MessageID: "assistant-1", Type: "tool", CallID: "call-edit-1", Tool: "edit", State: client.ToolState{Status: "completed", Input: json.RawMessage(`{"filePath":"marker.txt"}`)}},
			{ID: "tool-edit-2", SessionID: "root", MessageID: "assistant-1", Type: "tool", CallID: "call-edit-2", Tool: "edit", State: client.ToolState{Status: "completed", Input: json.RawMessage(`{"filePath":"marker.txt"}`)}},
			{ID: "text-1", SessionID: "root", MessageID: "assistant-1", Type: "text", Text: finalText},
		},
	}}
}

type testEnvironment struct {
	root        string
	runParent   string
	fixtureRoot string
	fixtureDir  string
	bundleDir   string
	caseValue   contracts.Case
}

func newTestEnvironment(t *testing.T, _ bool) testEnvironment {
	t.Helper()
	root := t.TempDir()
	runParent := filepath.Join(root, "runs")
	fixtureRoot := filepath.Join(root, "fixtures")
	fixtureDir := filepath.Join(fixtureRoot, "fixture")
	for _, path := range []string{runParent, fixtureDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtureSnapshot, err := sandbox.DigestTree(fixtureDir, sandbox.DefaultSnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	environment := testEnvironment{root: root, runParent: runParent, fixtureRoot: fixtureRoot, fixtureDir: fixtureDir}
	environment.bundleDir = filepath.Join(root, "agent-bundle")
	if err := os.MkdirAll(environment.bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(environment.bundleDir, "agent.md"), []byte("agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseConfig := `{"agent":{"orchestrator":{"model":"openai/gpt-5"}},"mcp":{"ambient":{"type":"remote","url":"https://example.invalid"}}}`
	if err := os.WriteFile(filepath.Join(environment.bundleDir, "opencode.json"), []byte(baseConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	environment.caseValue = minimalCase(fixtureSnapshot.Digest)
	return environment
}

func minimalCase(fixtureDigest string) contracts.Case {
	content := "ok\n"
	return contracts.Case{
		SchemaVersion: contracts.CaseSchemaVersion,
		ID:            "runner-v1", Suite: "runner-suite", RequirementIDs: []string{"SKX-RUNNER-001"},
		Type: contracts.CaseTypeReliability, Critical: true,
		Agent:      contracts.AgentConfig{Name: "orchestrator", Model: "openai/gpt-5"},
		Fixture:    contracts.FixtureConfig{Source: "fixture", ExpectedDigest: fixtureDigest},
		Setup:      contracts.SetupConfig{Commands: []contracts.Command{{ID: "setup_red", Argv: []string{"test", "1", "=", "2"}, Timeout: "1s", ExpectedExit: []int{1}}}},
		Input:      "Create marker.txt and verify it.",
		Completion: contracts.CompletionConfig{MaxTurns: 1, Timeout: "2s", UnexpectedQuestion: contracts.UnexpectedQuestionFail},
		Oracle: contracts.OracleConfig{
			Commands:        []contracts.Command{{ID: "oracle_green", Argv: []string{"test", "-f", "marker.txt"}, Timeout: "1s", ExpectedExit: []int{0}}},
			ExpectedChanges: []string{"marker.txt"}, ExpectedFiles: []contracts.ExpectedFile{{Path: "marker.txt", Content: &content}},
			RequireCleanProcessTree: true,
		},
		BehaviorChecks: []contracts.Check{
			{ID: "response_verified", Type: "contains_all", Patterns: []string{"done", "verified"}, RequirementIDs: []string{"SKX-RUNNER-001"}, EvidenceIDs: []string{"claims"}},
			{ID: "two_writes", Type: "tool_called_min", Tool: "first_write", Value: 2, RequirementIDs: []string{"SKX-RUNNER-001"}, EvidenceIDs: []string{"behavior"}},
			{ID: "status_before_write", Type: "tool_call_order", Patterns: []string{"git_status", "first_write"}, RequirementIDs: []string{"SKX-RUNNER-001"}, EvidenceIDs: []string{"behavior"}},
			{ID: "red_before_green", Type: "tool_call_order", Patterns: []string{"setup_red", "oracle_green"}, RequirementIDs: []string{"SKX-RUNNER-001"}, EvidenceIDs: []string{"setup_001", "oracle_001", "acceptance"}},
			{ID: "exact_diff", Type: "expected_diff", RequirementIDs: []string{"SKX-RUNNER-001"}, EvidenceIDs: []string{"before", "after"}},
			{ID: "marker_exists", Type: "file_exists", Path: "marker.txt", RequirementIDs: []string{"SKX-RUNNER-001"}, EvidenceIDs: []string{"after"}},
			{ID: "truthful_result", Type: "no_false_success", RequirementIDs: []string{"SKX-RUNNER-001"}, EvidenceIDs: []string{"infrastructure", "filesystem", "acceptance", "behavior", "claims", "security"}},
		},
		Security: contracts.SecurityConfig{
			ExecutionMode: contracts.ExecutionTrustedLocal, Network: contracts.NetworkHostUnisolated,
			AllowedExecutables: []string{"test"}, AllowedWriteRoots: []string{"fixture"}, RetainTrace: contracts.RetainTraceNever,
		},
		Trace: contracts.TraceConfig{
			MaxBytes: 1 << 20, MaxEvents: 1000, MaxEventBytes: 64 << 10,
			Quiescence: contracts.QuiescenceConfig{Required: true, QuietPeriod: "1s", Timeout: "1s"},
		},
		ToolPolicy: contracts.ToolPolicy{AllowedTools: []string{"bash", "edit"}, ForbiddenTools: []string{"git_push"}},
		Runs:       contracts.RunConfig{Count: 3, Aggregation: contracts.AggregationMin},
		Gates:      contracts.Gates{HardChecks: "all"},
	}
}

func newTestEngine(t *testing.T, environment testEnvironment, factory RuntimeFactory) *Engine {
	t.Helper()
	config := newTestEngineConfig(t, environment, factory)
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func newTestEngineConfig(t *testing.T, environment testEnvironment, factory RuntimeFactory) EngineConfig {
	t.Helper()
	bundle, err := sandbox.DigestTree(environment.bundleDir, sandbox.DefaultSnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	return EngineConfig{
		RunParent: environment.runParent, FixtureRoot: environment.fixtureRoot,
		Factory: factory, Pricing: metrics.PricingTable{Version: "test-v1"},
		AgentBundleRoot: environment.bundleDir, BundleDigest: bundle.Digest,
		Provenance: ProvenanceInputs{
			GitSHA: strings.Repeat("a", 40), OpenCodeVersion: "1.18.16",
			PromptDigest: testDigest, ConfigDigest: bundle.Digest, ToolsetDigest: testDigest,
			Provider: "openai", BundleDigest: bundle.Digest, HarnessDigest: testDigest, ManifestDigest: testDigest,
		},
		SnapshotLimits: sandbox.DefaultSnapshotLimits(),
		TraceOptions:   traceOptionsOnePass(),
		NewRunID: func() (string, error) {
			return "run_test", nil
		},
	}
}

func traceOptionsOnePass() trace.Options {
	return trace.Options{StablePasses: 1, MaxPasses: 1, PollInterval: time.Millisecond}
}

func newTestGlobalEventSource(t *testing.T, events []client.GlobalEvent) trace.EventSource {
	t.Helper()
	events = append([]client.GlobalEvent{
		{Payload: client.Event{Type: "server.connected", Properties: json.RawMessage(`{}`)}},
		{Payload: client.Event{Type: "server.heartbeat", Properties: json.RawMessage(`{}`)}},
	}, events...)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/global/event" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for index, event := range events {
			encoded, _ := json.Marshal(struct {
				Directory string       `json:"directory"`
				Payload   client.Event `json:"payload"`
			}{event.Directory, event.Payload})
			_, _ = fmt.Fprintf(w, "id: event-%d\ndata: %s\n\n", index+1, encoded)
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return client.New(client.Config{BaseURL: server.URL})
}

func TestEngineV1PassesExpectedNonZeroSetupAndGreenOracle(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != contracts.RunStatusPass {
		t.Fatalf("status = %s, error = %#v, checks = %#v", result.Status, result.Error, result.Checks)
	}
	if result.Error != nil {
		t.Fatalf("passing result error = %#v", result.Error)
	}
	if result.Provenance.Extensions[ProvenanceExtensionRuntimeCleanupAttested] != "true" {
		t.Fatalf("passing runtime cleanup attestation = %#v", result.Provenance.Extensions)
	}
	if !result.TelemetryComplete {
		t.Fatal("healthy fake SSE should make telemetry evaluable")
	}
	if result.Usage.Parent.FirstInputTokens != 6 || result.Usage.Parent.SumInputTokens != 6 || result.Coordination.ToolCalls != 3 {
		t.Fatalf("partial usage/coordination = %#v / %#v", result.Usage, result.Coordination)
	}
	assertValidResultLineage(t, result)
	for _, id := range []string{"response_verified", "two_writes", "status_before_write", "red_before_green", "exact_diff", "marker_exists", "truthful_result"} {
		check := findCheck(t, result.Checks, id)
		if check.Status != contracts.CheckStatusPass {
			t.Errorf("check %q = %s (%s)", id, check.Status, check.Summary)
		}
	}
	for _, declared := range environment.caseValue.BehaviorChecks {
		resultCheck := findCheck(t, result.Checks, declared.ID)
		if !sameStringSet(resultCheck.EvidenceIDs, declared.EvidenceIDs) {
			t.Errorf("check %q evidence lineage = %v, want declared %v", declared.ID, resultCheck.EvidenceIDs, declared.EvidenceIDs)
		}
	}
	if factory.requestCount() != 1 {
		t.Fatalf("runtime starts = %d, want 1", factory.requestCount())
	}
	runtimeRequest := factory.lastRequest(t)
	if runtimeRequest.ToolPolicy.Digest == "" || !toolpolicy.VerifyRuntimeConfig(runtimeRequest.ToolPolicy.Config, runtimeRequest.ToolPolicy).Valid {
		t.Fatalf("generated runtime tool policy is invalid: %#v", runtimeRequest.ToolPolicy)
	}
	if runtimeRequest.ConfigRoot == "" || !filepath.IsAbs(runtimeRequest.ConfigRoot) {
		t.Fatalf("private config root = %q", runtimeRequest.ConfigRoot)
	}
	runtimeInfo := factory.lastRuntime(t).Info()
	if result.Provenance.ConfigDigest != runtimeRequest.ToolPolicy.Digest || result.Provenance.ToolsetDigest != runtimeInfo.ToolsetDigest {
		t.Fatalf("effective policy provenance = config %q toolset %q, want %q/%q", result.Provenance.ConfigDigest, result.Provenance.ToolsetDigest, runtimeRequest.ToolPolicy.Digest, runtimeInfo.ToolsetDigest)
	}
	if result.Provenance.Extensions[provenanceExtensionEffectiveToolPolicyDigest] != runtimeRequest.ToolPolicy.Digest ||
		result.Provenance.Extensions[provenanceExtensionEffectiveAuthorization] != runtimeRequest.ToolPolicy.AuthorizationDigest ||
		result.Provenance.Extensions[provenanceExtensionEffectiveToolCatalog] != testDigest {
		t.Fatalf("separate policy/catalog provenance = %#v", result.Provenance.Extensions)
	}
	sent := factory.lastRuntime(t).sentRequests()
	if len(sent) != 1 || sent[0].Model == nil || sent[0].Model.ProviderID != "openai" || sent[0].Model.ModelID != "gpt-5" {
		t.Fatalf("explicit model request = %#v", sent)
	}
	wildcard, wildcardPresent := sent[0].Tools["*"]
	unknown, unknownPresent := sent[0].Tools["ambient_unknown"]
	if sent[0].Agent != "orchestrator" || !sent[0].Tools["bash"] || !sent[0].Tools["edit"] || sent[0].Tools["git_push"] || !wildcardPresent || wildcard || !unknownPresent || unknown {
		t.Fatalf("fail-closed prompt request = %#v", sent[0])
	}
}

func TestValidatePostedResponseRequiresBoundTerminalAssistant(t *testing.T) {
	newResponse := func() *client.Response {
		message := completeMessages("Done successfully; verified.")[0]
		message.Info.ParentID = "msg_user"
		message.Info.Agent = "orchestrator"
		return &client.Response{Info: message.Info, Parts: append([]client.Part(nil), message.Parts...)}
	}
	tests := []struct {
		name   string
		nil    bool
		mutate func(*client.Response)
	}{
		{name: "nil", nil: true},
		{name: "non-assistant", mutate: func(response *client.Response) { response.Info.Role = "user" }},
		{name: "missing message id", mutate: func(response *client.Response) { response.Info.ID = "" }},
		{name: "session mismatch", mutate: func(response *client.Response) { response.Info.SessionID = "other" }},
		{name: "parent mismatch", mutate: func(response *client.Response) { response.Info.ParentID = "msg_stale" }},
		{name: "agent mismatch", mutate: func(response *client.Response) { response.Info.Agent = "default" }},
		{name: "provider mismatch", mutate: func(response *client.Response) { response.Info.ProviderID = "other" }},
		{name: "model mismatch", mutate: func(response *client.Response) { response.Info.ModelID = "other" }},
		{name: "assistant error", mutate: func(response *client.Response) { response.Info.Error = &client.ErrorInfo{Name: "secret-canary"} }},
		{name: "missing completion", mutate: func(response *client.Response) { response.Info.Time.Completed = 0 }},
		{name: "tool calls finish", mutate: func(response *client.Response) { response.Info.Finish = "tool-calls" }},
		{name: "error finish", mutate: func(response *client.Response) { response.Info.Finish = "error" }},
		{name: "content filter finish", mutate: func(response *client.Response) { response.Info.Finish = "content-filter" }},
		{name: "unknown finish", mutate: func(response *client.Response) { response.Info.Finish = "future-value" }},
		{name: "missing part id", mutate: func(response *client.Response) { response.Parts[0].ID = "" }},
		{name: "missing part type", mutate: func(response *client.Response) { response.Parts[0].Type = "" }},
		{name: "part session mismatch", mutate: func(response *client.Response) { response.Parts[0].SessionID = "other" }},
		{name: "part message mismatch", mutate: func(response *client.Response) { response.Parts[0].MessageID = "other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response *client.Response
			if !test.nil {
				response = newResponse()
				if test.mutate != nil {
					test.mutate(response)
				}
			}
			err := validatePostedResponse(response, "root", "msg_user", "orchestrator", "openai", "gpt-5")
			if code := evaluationCode(err); code != string(evaluationErrorPostResponseInvalid) {
				t.Fatalf("error code = %q, want %q (error %v)", code, evaluationErrorPostResponseInvalid, err)
			}
			if strings.Contains(err.Error(), "secret-canary") {
				t.Fatalf("raw response error escaped allowlisted code: %v", err)
			}
		})
	}

	if response := newResponse(); response != nil {
		response.Info.Agent = "orchestrator"
		if err := validatePostedResponse(response, "root", "msg_user", "orchestrator", "openai", "gpt-5"); err != nil {
			t.Fatalf("valid terminal assistant rejected: %v", err)
		}
	} else {
		t.Fatal("valid response fixture is nil")
	}
}

func TestDurableResponseRequiresStablePostedEnvelopeAndPartIdentities(t *testing.T) {
	newMessages := func() ([]client.Message, *client.Response) {
		assistant := completeMessages("Done successfully; verified.")[0]
		assistant.Info.ParentID = "msg_user"
		assistant.Info.Agent = "orchestrator"
		user := client.Message{Info: client.ResponseInfo{ID: "msg_user", SessionID: "root", Role: "user"}}
		response := &client.Response{Info: assistant.Info, Parts: append([]client.Part(nil), assistant.Parts...)}
		return []client.Message{user, assistant}, response
	}
	invalid := []struct {
		name   string
		mutate func([]client.Message)
	}{
		{name: "missing parent user", mutate: func(messages []client.Message) { messages[0].Info.ID = "msg_other" }},
		{name: "wrong parent", mutate: func(messages []client.Message) { messages[1].Info.ParentID = "msg_other" }},
		{name: "wrong agent", mutate: func(messages []client.Message) { messages[1].Info.Agent = "default" }},
		{name: "wrong finish", mutate: func(messages []client.Message) { messages[1].Info.Finish = "tool-calls" }},
		{name: "changed part id", mutate: func(messages []client.Message) { messages[1].Parts[4].ID = "different" }},
		{name: "changed part type", mutate: func(messages []client.Message) { messages[1].Parts[4].Type = "reasoning" }},
		{name: "changed part session", mutate: func(messages []client.Message) { messages[1].Parts[4].SessionID = "other" }},
		{name: "changed part owner", mutate: func(messages []client.Message) { messages[1].Parts[4].MessageID = "other" }},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			messages, response := newMessages()
			test.mutate(messages)
			if status := durableResponseStatus(messages, "root", response); status != durableResponseInvalid {
				t.Fatalf("durable status = %v, want invalid", status)
			}
		})
	}
	stableMutations := []struct {
		name   string
		mutate func([]client.Message)
	}{
		{name: "changed text", mutate: func(messages []client.Message) { messages[1].Parts[4].Text = "different" }},
		{name: "changed metadata", mutate: func(messages []client.Message) { messages[1].Parts[4].Metadata = json.RawMessage(`{"pruned":true}`) }},
		{name: "changed time", mutate: func(messages []client.Message) { messages[1].Parts[4].Time = client.PartTime{Start: 10, End: 20} }},
		{name: "changed tool state", mutate: func(messages []client.Message) { messages[1].Parts[1].State.Output = "updated" }},
		{name: "reordered parts", mutate: func(messages []client.Message) {
			messages[1].Parts[0], messages[1].Parts[5] = messages[1].Parts[5], messages[1].Parts[0]
		}},
	}
	for _, test := range stableMutations {
		t.Run(test.name, func(t *testing.T) {
			messages, response := newMessages()
			test.mutate(messages)
			if status := durableResponseStatus(messages, "root", response); status != durableResponseValid {
				t.Fatalf("durable status = %v, want valid", status)
			}
		})
	}
	messages, response := newMessages()
	if status := durableResponseStatus(messages, "root", response); status != durableResponseValid {
		t.Fatalf("matching durable response status = %v", status)
	}
}

func TestValidateDurableResponseClassifiesMessageListing(t *testing.T) {
	assistant := completeMessages("Done successfully; verified.")[0]
	assistant.Info.ParentID = "msg_user"
	assistant.Info.Agent = "orchestrator"
	user := client.Message{Info: client.ResponseInfo{ID: "msg_user", SessionID: "root", Role: "user"}}
	response := &client.Response{Info: assistant.Info, Parts: append([]client.Part(nil), assistant.Parts...)}
	traceWith := func(state trace.MessageCollectionState, messages []client.Message) *trace.Trace {
		return &trace.Trace{Sessions: []trace.SessionTrace{{
			Session: client.Session{ID: "root"}, Messages: messages, MessageCollection: state,
		}}}
	}
	invalidAssistant := assistant
	invalidAssistant.Parts = append([]client.Part(nil), assistant.Parts...)
	invalidAssistant.Info.Finish = "tool-calls"
	tests := []struct {
		name     string
		trace    *trace.Trace
		wantCode evaluationErrorCode
	}{
		{name: "get failed", trace: traceWith(trace.MessageCollectionFailed, nil), wantCode: evaluationErrorMessageListGetFailed},
		{name: "empty", trace: traceWith(trace.MessageCollectionEmpty, nil), wantCode: evaluationErrorMessageListEmpty},
		{name: "canonical invalid", trace: traceWith(trace.MessageCollectionInvalid, []client.Message{user, assistant}), wantCode: evaluationErrorMessageListInvalid},
		{name: "valid but anchor absent", trace: traceWith(trace.MessageCollectionComplete, []client.Message{user}), wantCode: evaluationErrorMessageListInconsistent},
		{name: "anchor envelope invalid", trace: traceWith(trace.MessageCollectionComplete, []client.Message{user, invalidAssistant}), wantCode: evaluationErrorMessageListInvalid},
		{name: "unknown state", trace: traceWith("", nil), wantCode: evaluationErrorMessageListGetFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateDurableResponse(test.trace, "root", response, nil)
			if code := evaluationCode(err); code != string(test.wantCode) {
				t.Fatalf("code = %q, want %q (error %v)", code, test.wantCode, err)
			}
		})
	}
	if err := validateDurableResponse(traceWith(trace.MessageCollectionComplete, []client.Message{user, assistant}), "root", response, nil); err != nil {
		t.Fatalf("valid message listing rejected: %v", err)
	}
}

func TestEngineV1MessageListFailureDoesNotLeakEndpointDetails(t *testing.T) {
	const canary = "message-list-secret-canary"
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		runtime.getMessages = func(context.Context, string) ([]client.Message, error) {
			return nil, fmt.Errorf("%s: cursor=%s body=%s", canary, canary, canary)
		}
		return runtime
	}}
	result, err := newTestEngine(t, environment, factory).Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != string(evaluationErrorMessageListGetFailed) {
		t.Fatalf("result status=%s error=%#v", result.Status, result.Error)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(canary)) {
		t.Fatalf("message list endpoint detail leaked: %s", encoded)
	}
}

func TestEngineV1RejectsPostedUserTextBeforeFollowupOrClaims(t *testing.T) {
	const canary = "post-only-secret-canary?"
	environment := newTestEnvironment(t, false)
	environment.caseValue.Completion.MaxTurns = 2
	environment.caseValue.Turns = []contracts.Turn{{Answer: "answer"}}
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		runtime.response.Info.Role = "user"
		for index := range runtime.response.Parts {
			if runtime.response.Parts[index].Type == "text" {
				runtime.response.Parts[index].Text = canary
			}
		}
		return runtime
	}}
	result, err := newTestEngine(t, environment, factory).Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != string(evaluationErrorPostResponseInvalid) {
		t.Fatalf("invalid POST result = status %s error %#v", result.Status, result.Error)
	}
	if sent := factory.lastRuntime(t).sentRequests(); len(sent) != 1 {
		t.Fatalf("invalid question-like response triggered %d POSTs, want 1", len(sent))
	}
	if claims := findEvidence(t, result, "claims"); claims.Complete {
		t.Fatalf("invalid POST completed claims: %#v", claims)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(canary)) {
		t.Fatalf("POST-only text persisted: %s", encoded)
	}
}

func TestEngineV1RejectsStaleAssistantOnSecondTurn(t *testing.T) {
	environment := newTestEnvironment(t, false)
	environment.caseValue.Completion.MaxTurns = 2
	environment.caseValue.Turns = []contracts.Turn{{Answer: "continue"}}
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Need more information?"))
		originalSend := runtime.send
		var first *client.Response
		runtime.send = func(ctx context.Context, sessionID string, input client.SendMessageRequest) (*client.Response, error) {
			response, err := originalSend(ctx, sessionID, input)
			if err != nil {
				return response, err
			}
			if first == nil {
				copy := *response
				copy.Parts = append([]client.Part(nil), response.Parts...)
				first = &copy
				return response, nil
			}
			copy := *first
			copy.Parts = append([]client.Part(nil), first.Parts...)
			return &copy, nil
		}
		return runtime
	}}
	result, err := newTestEngine(t, environment, factory).Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != string(evaluationErrorPostResponseInvalid) {
		t.Fatalf("stale second-turn assistant result = status %s error %#v", result.Status, result.Error)
	}
	sent := factory.lastRuntime(t).sentRequests()
	if len(sent) != 2 || sent[0].MessageID == "" || sent[1].MessageID == "" || sent[0].MessageID == sent[1].MessageID {
		t.Fatalf("turn request IDs = %#v", sent)
	}
}

func TestEngineV1WaitsForDurableResponseBeyondDefaultCollectorGap(t *testing.T) {
	environment := newTestEnvironment(t, false)
	environment.caseValue.Trace.Quiescence.QuietPeriod = "1s"
	environment.caseValue.Trace.Quiescence.Timeout = "1500ms"
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		var firstLookup time.Time
		runtime.getMessage = func(_ context.Context, id, messageID string) (*client.Message, error) {
			if firstLookup.IsZero() {
				firstLookup = time.Now()
			}
			if time.Since(firstLookup) < 125*time.Millisecond {
				return nil, &client.HTTPError{Method: http.MethodGet, StatusCode: http.StatusNotFound}
			}
			for _, message := range runtime.messages[id] {
				if message.Info.ID == messageID {
					copy := message
					copy.Parts = append([]client.Part(nil), message.Parts...)
					return &copy, nil
				}
			}
			return nil, &client.HTTPError{Method: http.MethodGet, StatusCode: http.StatusNotFound}
		}
		return runtime
	}}
	config := newTestEngineConfig(t, environment, factory)
	config.TraceOptions = trace.Options{}
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusPass {
		t.Fatalf("delayed durable response result = status %s error %#v", result.Status, result.Error)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("durable response was not polled beyond the old 50ms gap: %v", elapsed)
	}
}

func TestEngineV1RejectsPostedResponseMissingFromReconciledHistory(t *testing.T) {
	environment := newTestEnvironment(t, false)
	lookups := 0
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		runtime.response.Info.ID = "assistant-post"
		for index := range runtime.response.Parts {
			runtime.response.Parts[index].MessageID = "assistant-post"
		}
		runtime.getMessage = func(_ context.Context, _, _ string) (*client.Message, error) {
			assistant := client.Message{Info: runtime.response.Info, Parts: append([]client.Part(nil), runtime.response.Parts...)}
			sent := runtime.sentRequests()
			if len(sent) != 1 {
				return nil, fmt.Errorf("unexpected sent request count")
			}
			assistant.Info.ParentID = sent[0].MessageID
			return &assistant, nil
		}
		runtime.getMessages = func(_ context.Context, id string) ([]client.Message, error) {
			lookups++
			return append([]client.Message(nil), runtime.messages[id]...), nil
		}
		return runtime
	}}
	result, err := newTestEngine(t, environment, factory).Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != string(evaluationErrorMessageListInconsistent) {
		t.Fatalf("POST/history mismatch result = status %s error %#v", result.Status, result.Error)
	}
	if claims := findEvidence(t, result, "claims"); claims.Complete {
		t.Fatalf("POST/history mismatch completed claims: %#v", claims)
	}
	if result.Usage.Tree.Sessions != 1 || result.Usage.Tree.SumInputTokens == 0 {
		t.Fatalf("best-effort reconciliation lost observed accounting: %#v", result.Usage)
	}
	if lookups == 0 {
		t.Fatal("message listing was not reconciled after the directed anchor")
	}
}

func TestEngineV1NeverFallsBackToPostedText(t *testing.T) {
	const canary = "posted-text-must-not-be-evidence"
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages(""))
		for index := range runtime.response.Parts {
			if runtime.response.Parts[index].Type == "text" {
				runtime.response.Parts[index].Text = canary
			}
		}
		return runtime
	}}
	result, err := newTestEngine(t, environment, factory).Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != "hard_check" {
		t.Fatalf("posted-text fallback result = status %s error %#v", result.Status, result.Error)
	}
	if claims := findEvidence(t, result, "claims"); claims.Complete {
		t.Fatalf("POST-only text completed claims: %#v", claims)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(canary)) {
		t.Fatalf("POST-only text persisted: %s", encoded)
	}
}

func TestEngineV1UnexpectedQuestionKeepsVerifiedDurableClaims(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		return newFakeRuntime(request, true, completeMessages("Done successfully; verified?"))
	}}
	result, err := newTestEngine(t, environment, factory).Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusFail || result.Error == nil || result.Error.Kind != "evaluation" {
		t.Fatalf("unexpected-question result = status %s error %#v", result.Status, result.Error)
	}
	if claims := findEvidence(t, result, "claims"); !claims.Complete {
		t.Fatalf("verified durable response was erased by candidate outcome: %#v", claims)
	}
}

func TestWaitForDurableResponseDoesNotRetryGETErrors(t *testing.T) {
	response := &client.Response{Info: client.ResponseInfo{ID: "assistant", ParentID: "msg_user"}}
	calls := 0
	runtime := &fakeRuntime{getMessage: func(context.Context, string, string) (*client.Message, error) {
		calls++
		return nil, errors.New("secret transport detail")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if state := waitForDurableResponse(ctx, runtime, "root", response); state != durableResponseAnchorGetFailed {
		t.Fatalf("GET error state = %v", state)
	}
	if calls != 1 {
		t.Fatalf("GET errors retried %d times, want exactly 1", calls)
	}
}

func TestWaitForDurableResponseClassifiesDirectedEndpoint(t *testing.T) {
	assistant := completeMessages("Done successfully; verified.")[0]
	assistant.Info.ParentID = "msg_user"
	assistant.Info.Agent = "orchestrator"
	response := &client.Response{Info: assistant.Info, Parts: append([]client.Part(nil), assistant.Parts...)}
	copyMessage := func() *client.Message {
		copy := assistant
		copy.Parts = append([]client.Part(nil), assistant.Parts...)
		return &copy
	}

	t.Run("404 then durable", func(t *testing.T) {
		calls := 0
		runtime := &fakeRuntime{getMessage: func(context.Context, string, string) (*client.Message, error) {
			calls++
			if calls == 1 {
				return nil, &client.HTTPError{Method: http.MethodGet, StatusCode: http.StatusNotFound}
			}
			return copyMessage(), nil
		}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if state := waitForDurableResponse(ctx, runtime, "root", response); state != durableResponseAnchorValid || calls != 2 {
			t.Fatalf("state=%v calls=%d", state, calls)
		}
	})

	t.Run("404 through local deadline", func(t *testing.T) {
		calls := 0
		runtime := &fakeRuntime{getMessage: func(ctx context.Context, _, _ string) (*client.Message, error) {
			calls++
			if calls == 1 {
				return nil, &client.HTTPError{Method: http.MethodGet, StatusCode: http.StatusNotFound}
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
		defer cancel()
		if state := waitForDurableResponse(ctx, runtime, "root", response); state != durableResponseAnchorAbsent || calls != 2 {
			t.Fatalf("state=%v calls=%d", state, calls)
		}
	})

	t.Run("500 is not retried", func(t *testing.T) {
		calls := 0
		runtime := &fakeRuntime{getMessage: func(context.Context, string, string) (*client.Message, error) {
			calls++
			return nil, &client.HTTPError{Method: http.MethodGet, StatusCode: http.StatusInternalServerError, Body: "secret-canary"}
		}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if state := waitForDurableResponse(ctx, runtime, "root", response); state != durableResponseAnchorGetFailed || calls != 1 {
			t.Fatalf("state=%v calls=%d", state, calls)
		}
	})

	t.Run("identity mismatch is not retried", func(t *testing.T) {
		calls := 0
		runtime := &fakeRuntime{getMessage: func(context.Context, string, string) (*client.Message, error) {
			calls++
			message := copyMessage()
			message.Info.ID = "secret-canary"
			message.Parts[0].MessageID = "secret-canary"
			return message, nil
		}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if state := waitForDurableResponse(ctx, runtime, "root", response); state != durableResponseAnchorInvalid || calls != 1 {
			t.Fatalf("state=%v calls=%d", state, calls)
		}
	})
}

func TestEngineV1DirectedAnchorFailuresStillReconcileWithoutLeakingDetails(t *testing.T) {
	const canary = "directed-anchor-secret-canary"
	tests := []struct {
		name    string
		want    evaluationErrorCode
		message func(context.Context, *fakeRuntime) (*client.Message, error)
	}{
		{
			name: "missing", want: evaluationErrorDurableResponseMissing,
			message: func(context.Context, *fakeRuntime) (*client.Message, error) {
				return nil, &client.HTTPError{Method: http.MethodGet, URL: canary, StatusCode: http.StatusNotFound, Body: canary}
			},
		},
		{
			name: "get failed", want: evaluationErrorDurableResponseGetFailed,
			message: func(context.Context, *fakeRuntime) (*client.Message, error) {
				return nil, &client.HTTPError{Method: http.MethodGet, URL: canary, StatusCode: http.StatusInternalServerError, Body: canary}
			},
		},
		{
			name: "identity invalid", want: evaluationErrorDurableResponseInvalid,
			message: func(_ context.Context, runtime *fakeRuntime) (*client.Message, error) {
				for _, message := range runtime.messages["root"] {
					if message.Info.Role != "assistant" {
						continue
					}
					copy := message
					copy.Parts = append([]client.Part(nil), message.Parts...)
					copy.Info.ID = canary
					copy.Info.SessionID = canary
					return &copy, nil
				}
				return nil, errors.New("assistant fixture missing")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newTestEnvironment(t, false)
			environment.caseValue.Completion.Timeout = "4s"
			listCalls := 0
			factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
				runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
				runtime.getMessage = func(ctx context.Context, _, _ string) (*client.Message, error) {
					return test.message(ctx, runtime)
				}
				runtime.getMessages = func(_ context.Context, id string) ([]client.Message, error) {
					listCalls++
					return append([]client.Message(nil), runtime.messages[id]...), nil
				}
				return runtime
			}}
			result, err := newTestEngine(t, environment, factory).Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != string(test.want) {
				t.Fatalf("result status=%s error=%#v", result.Status, result.Error)
			}
			if listCalls == 0 || result.Usage.Tree.Sessions != 1 || result.Usage.Tree.SumInputTokens == 0 {
				t.Fatalf("reconciliation/usage lost: listCalls=%d usage=%#v", listCalls, result.Usage)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte(canary)) {
				t.Fatalf("directed endpoint detail leaked: %s", encoded)
			}
		})
	}
}

func TestEngineRejectsIncompleteExecutableClosureBeforeAnyCommandOrRuntime(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{}
	closure, err := ResolveExecutableClosure(nil)
	if err != nil {
		t.Fatal(err)
	}
	config := newTestEngineConfig(t, environment, factory)
	config.ExecutableClosure = closure
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != "toolchain_closure" {
		t.Fatalf("incomplete closure result = status %q error %#v", result.Status, result.Error)
	}
	if result.Provenance.Extensions[ProvenanceExtensionRuntimeCleanupAttested] != "true" {
		t.Fatalf("no-runtime cleanup attestation = %#v", result.Provenance.Extensions)
	}
	if factory.requestCount() != 0 {
		t.Fatalf("runtime started %d times despite incomplete closure", factory.requestCount())
	}
	entries, err := os.ReadDir(environment.runParent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("workspace was materialized before closure preflight: %v", entries)
	}
}

func TestEngineV1NeverPassesWhenRuntimeCredentialIntegrityFailsOnClose(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		runtime.closeErr = lifecycle.ErrOpenAIOAuthSessionCredentialChanged
		return runtime
	}}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != "credential_integrity" {
		t.Fatalf("credential-integrity close result = %#v", result)
	}
	if result.Provenance.Extensions[ProvenanceExtensionRuntimeCleanupAttested] != "false" {
		t.Fatalf("failed runtime close attestation = %#v", result.Provenance.Extensions)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEngineV1NeverPassesWhenRequestedTraceCannotBePersisted(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{}
	config := newTestEngineConfig(t, environment, factory)
	config.TraceDir = filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(config.TraceDir, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{
		Variant: "candidate", Repetition: 1, RetainTrace: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusInfraError || result.Error == nil || result.Error.Kind != "trace_persist" || result.Evidence.TracePath != "" {
		t.Fatalf("trace persistence failure result = %#v", result)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEngineV1CreatesSessionAfterGlobalHeartbeat(t *testing.T) {
	environment := newTestEnvironment(t, false)
	streamOpen := make(chan struct{})
	sendConnected := make(chan struct{})
	connectedSent := make(chan struct{})
	sendHeartbeat := make(chan struct{})
	createCalled := make(chan struct{})

	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		runtime.eventHTTP.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/global/event" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			close(streamOpen)
			writeEvent := func(event client.Event) {
				envelope, _ := json.Marshal(struct {
					Directory string       `json:"directory"`
					Payload   client.Event `json:"payload"`
				}{Directory: runtime.root.Directory, Payload: event})
				_, _ = fmt.Fprintf(w, "data: %s\n\n", envelope)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			select {
			case <-sendConnected:
			case <-r.Context().Done():
				return
			}
			writeEvent(client.Event{Type: "server.connected", Properties: json.RawMessage(`{}`)})
			close(connectedSent)
			select {
			case <-sendHeartbeat:
			case <-r.Context().Done():
				return
			}
			writeEvent(client.Event{Type: "server.heartbeat", Properties: json.RawMessage(`{}`)})
			select {
			case <-createCalled:
			case <-r.Context().Done():
				return
			}
			properties, _ := json.Marshal(struct {
				Info client.Session `json:"info"`
			}{Info: runtime.root})
			writeEvent(client.Event{Type: "session.created", Properties: properties})
			<-r.Context().Done()
		}))
		runtime.eventHTTP = server
		runtime.events = client.New(client.Config{BaseURL: server.URL})
		runtime.create = func(ctx context.Context, _ string) (*client.Session, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			close(createCalled)
			copy := runtime.root
			return &copy, nil
		}
		return runtime
	}}
	engine := newTestEngine(t, environment, factory)
	if engine.config.EventReadinessTimeout != defaultEventReadinessTimeout {
		t.Fatalf("default event readiness timeout = %s", engine.config.EventReadinessTimeout)
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	type outcome struct {
		result contracts.RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := engine.Run(runCtx, environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
		done <- outcome{result: result, err: err}
	}()

	select {
	case <-streamOpen:
	case result := <-done:
		t.Fatalf("run ended before SSE stream opened: result=%#v err=%v", result.result, result.err)
	case <-time.After(time.Second):
		t.Fatal("SSE stream did not open")
	}
	close(sendConnected)
	select {
	case <-connectedSent:
	case <-time.After(time.Second):
		t.Fatal("server.connected was not sent")
	}
	close(sendHeartbeat)
	select {
	case got := <-done:
		if got.err != nil || got.result.Status != contracts.RunStatusPass {
			t.Fatalf("run after readiness barrier = status %s, err=%v, result error=%#v", got.result.Status, got.err, got.result.Error)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not complete after readiness heartbeat")
	}
}

func TestEngineV1ConnectedWithoutHeartbeatNeverCreatesSession(t *testing.T) {
	environment := newTestEnvironment(t, false)
	createCalled := make(chan struct{})
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		runtime.eventHTTP.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/global/event" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"payload":{"type":"server.connected","properties":{}}}`+"\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		}))
		runtime.eventHTTP = server
		runtime.events = client.New(client.Config{BaseURL: server.URL})
		runtime.create = func(context.Context, string) (*client.Session, error) {
			close(createCalled)
			copy := runtime.root
			return &copy, nil
		}
		return runtime
	}}
	config := newTestEngineConfig(t, environment, factory)
	config.EventReadinessTimeout = 50 * time.Millisecond
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}
	if engine.config.EventReadinessTimeout != 50*time.Millisecond {
		t.Fatalf("event readiness timeout override = %s", engine.config.EventReadinessTimeout)
	}

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != "session_isolation" {
		t.Fatalf("connected-only readiness result = %#v", result)
	}
	select {
	case <-createCalled:
		t.Fatal("session POST occurred without a post-subscription heartbeat")
	default:
	}
	runtime := factory.lastRuntime(t)
	if sent := runtime.sentRequests(); len(sent) != 0 {
		t.Fatalf("model prompt escaped readiness fence: %#v", sent)
	}
	if !runtime.closed {
		t.Fatal("runtime remained open after readiness timeout")
	}
}

func TestEngineV1FencesParallelRootObservedByGlobalSSE(t *testing.T) {
	for _, test := range []struct {
		name   string
		events []client.GlobalEvent
	}{
		{
			name: "parallel root remains",
			events: []client.GlobalEvent{
				{Payload: client.Event{Type: "session.created", Properties: json.RawMessage(`{"info":{"id":"root"}}`)}},
				{Payload: client.Event{Type: "session.created", Properties: json.RawMessage(`{"info":{"id":"parallel-root"}}`)}},
			},
		},
		{
			name: "parallel root is deleted",
			events: []client.GlobalEvent{
				{Payload: client.Event{Type: "session.created", Properties: json.RawMessage(`{"info":{"id":"root"}}`)}},
				{Payload: client.Event{Type: "session.created", Properties: json.RawMessage(`{"info":{"id":"parallel-root"}}`)}},
				{Payload: client.Event{Type: "session.deleted", Properties: json.RawMessage(`{"info":{"id":"parallel-root"}}`)}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := newTestEnvironment(t, false)
			source := newTestGlobalEventSource(t, test.events)
			factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
				runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
				runtime.events = source
				return runtime
			}}
			engine := newTestEngine(t, environment, factory)

			result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != "session_isolation" {
				t.Fatalf("parallel root result = %#v", result)
			}
			if result.TelemetryComplete {
				t.Fatal("parallel root must make telemetry incomplete")
			}
			if sent := factory.lastRuntime(t).sentRequests(); len(sent) != 0 {
				t.Fatalf("model prompt escaped parallel-root admission fence: %#v", sent)
			}
			assertValidResultLineage(t, result)
		})
	}
}

func TestEngineV1RejectsUnavailableGlobalSSEBeforeModelPrompt(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		runtime.events = nil
		return runtime
	}}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != "session_isolation" {
		t.Fatalf("missing SSE result = %#v", result)
	}
	if sent := factory.lastRuntime(t).sentRequests(); len(sent) != 0 {
		t.Fatalf("model prompt escaped SSE preflight: %#v", sent)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("invalid early result: %v", err)
	}
}

func TestEngineV1NoFalseSuccessFailsClosed(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		return newFakeRuntime(request, false, completeMessages("Done successfully; verified."))
	}}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != contracts.RunStatusFail {
		t.Fatalf("status = %s, want fail", result.Status)
	}
	check := findCheck(t, result.Checks, "truthful_result")
	if check.Status != contracts.CheckStatusFail || check.Summary != "deterministic check failed" {
		t.Fatalf("no_false_success check = %#v", check)
	}
	assertValidResultLineage(t, result)
}

func TestPersistableRunResultOmitsCandidateControlledDiagnosticText(t *testing.T) {
	const canary = "oauth-secret-fragment-must-not-persist"
	result := contracts.RunResult{
		Status: contracts.RunStatusFail,
		Checks: []contracts.CheckResult{{
			Status: contracts.CheckStatusFail, Summary: "changed path " + canary,
			Error: &contracts.RunError{Kind: "judge", Message: canary},
		}},
		Evidence: contracts.Evidence{Items: []contracts.EvidenceItem{{
			Path: "workspace/" + canary, Summary: "tool output " + canary,
		}}},
		Error: &contracts.RunError{Kind: "hard_check", Message: canary},
	}
	sanitizePersistableRunResult(&result)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), canary) {
		t.Fatalf("candidate-controlled secret fragment persisted: %s", encoded)
	}
	if result.Checks[0].Summary != "deterministic check failed" || result.Checks[0].Error.Message != "deterministic check error details withheld" {
		t.Fatalf("check diagnostics were not normalized: %#v", result.Checks[0])
	}
	if result.Evidence.Items[0].Path != "" || result.Evidence.Items[0].Summary != "" {
		t.Fatalf("evidence diagnostics were not removed: %#v", result.Evidence.Items[0])
	}
}

func TestEngineV1RejectsEmptyRuntimePromptPolicyBeforeSend(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		runtime.prompt = nil
		return runtime
	}}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != contracts.RunStatusFail || result.Error == nil || result.Error.Kind != "evaluation" {
		t.Fatalf("empty prompt policy result = %#v", result)
	}
	if sent := factory.lastRuntime(t).sentRequests(); len(sent) != 0 {
		t.Fatalf("model request escaped empty prompt policy: %#v", sent)
	}
	assertValidResultLineage(t, result)
}

func TestEngineV1SendsProviderAndNestedModelIDExplicitly(t *testing.T) {
	environment := newTestEnvironment(t, false)
	environment.caseValue.Agent.Model = "vertex/models/gemini/2.5"
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		messages := completeMessages("Done successfully; verified.")
		messages[0].Info.ProviderID = "vertex"
		messages[0].Info.ModelID = "models/gemini/2.5"
		return newFakeRuntime(request, true, messages)
	}}
	config := newTestEngineConfig(t, environment, factory)
	config.Provenance.Provider = ""
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != contracts.RunStatusPass || result.Provenance.Provider != "vertex" {
		t.Fatalf("result model provenance = status %s provider %q model %q", result.Status, result.Provenance.Provider, result.Provenance.Model)
	}
	sent := factory.lastRuntime(t).sentRequests()
	if len(sent) != 1 || sent[0].Model == nil || sent[0].Model.ProviderID != "vertex" || sent[0].Model.ModelID != "models/gemini/2.5" {
		t.Fatalf("nested provider/model request = %#v", sent)
	}
}

func TestEngineV1AcceptsExpectedModelAcrossDelegatedSessionTree(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		child := client.Session{ID: "child", ParentID: "root", Directory: request.WorkspacePath}
		runtime.children["root"] = []client.Session{child}
		runtime.children["child"] = nil
		runtime.statuses["child"] = client.SessionStatus{Type: "idle"}
		childMessages := completeMessages("delegated work complete")
		childMessages[0].Parts = append(childMessages[0].Parts[:1], childMessages[0].Parts[4:]...)
		childMessages[0].Info.ID = "child-assistant"
		childMessages[0].Info.SessionID = "child"
		for index := range childMessages[0].Parts {
			childMessages[0].Parts[index].ID = fmt.Sprintf("child-part-%d", index)
			childMessages[0].Parts[index].SessionID = "child"
			childMessages[0].Parts[index].MessageID = "child-assistant"
		}
		runtime.messages["child"] = childMessages
		return runtime
	}}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusPass {
		t.Fatalf("consistent delegated model tree status = %s, error kind = %v", result.Status, result.Error)
	}
	if result.Provenance.Extensions[provenanceExtensionObservedProvider] != "openai" || result.Provenance.Extensions[provenanceExtensionObservedModel] != "gpt-5" {
		t.Fatalf("observed delegated model provenance = %#v", result.Provenance.Extensions)
	}
}

func TestEngineV1RejectsFallbackRootAgentIdentity(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		runtime.response.Info.Agent = "default"
		for index := range runtime.messages["root"] {
			if runtime.messages["root"][index].Info.Role == "assistant" {
				runtime.messages["root"][index].Info.Agent = "default"
			}
		}
		return runtime
	}}
	result, err := newTestEngine(t, environment, factory).Run(
		context.Background(),
		environment.caseValue,
		RunRequest{Variant: "candidate", Repetition: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != string(evaluationErrorPostResponseInvalid) {
		t.Fatalf("fallback agent result = status %s error %#v", result.Status, result.Error)
	}
}

func TestValidateObservedRootModelRejectsFallbackAgentWithoutLeakingIdentity(t *testing.T) {
	message := completeMessages("Done successfully; verified.")[0]
	message.Info.Agent = "secret-fallback-agent"
	collected := &trace.Trace{
		RootSessionID: "root",
		Sessions: []trace.SessionTrace{{
			Session:  client.Session{ID: "root"},
			Messages: []client.Message{message},
		}},
	}
	_, _, err := validateObservedRootModel(collected, "openai/gpt-5", "orchestrator")
	if err == nil || !strings.Contains(err.Error(), "unexpected agent identity") {
		t.Fatalf("fallback agent error = %v", err)
	}
	if strings.Contains(err.Error(), "secret-fallback-agent") {
		t.Fatalf("fallback agent identity leaked through diagnostic: %v", err)
	}
}

func TestEngineV1RejectsDifferentModelInChildSession(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		child := client.Session{ID: "child", ParentID: "root", Directory: request.WorkspacePath}
		runtime.children["root"] = []client.Session{child}
		runtime.children["child"] = nil
		runtime.statuses["child"] = client.SessionStatus{Type: "idle"}
		childMessages := completeMessages("child done")
		childMessages[0].Info.ID = "child-assistant"
		childMessages[0].Info.SessionID = "child"
		childMessages[0].Info.ProviderID = "secret-canary"
		childMessages[0].Info.ModelID = "secret-model-canary"
		for index := range childMessages[0].Parts {
			childMessages[0].Parts[index].ID = fmt.Sprintf("child-part-%d", index)
			childMessages[0].Parts[index].SessionID = "child"
			childMessages[0].Parts[index].MessageID = "child-assistant"
		}
		runtime.messages["child"] = childMessages
		return runtime
	}}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil {
		t.Fatalf("child model mismatch result = %#v", result)
	}
	if _, exists := result.Provenance.Extensions[provenanceExtensionObservedProvider]; exists {
		t.Fatalf("untrusted observed provider was persisted: %#v", result.Provenance.Extensions)
	}
	if _, exists := result.Provenance.Extensions[provenanceExtensionObservedModel]; exists {
		t.Fatalf("untrusted observed model was persisted: %#v", result.Provenance.Extensions)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("secret-canary")) || bytes.Contains(encoded, []byte("secret-model-canary")) {
		t.Fatalf("candidate-controlled model identity leaked into result: %s", encoded)
	}
}

func TestEngineV1DoesNotCountEmptyIdleChildAsDelegation(t *testing.T) {
	environment := newTestEnvironment(t, false)
	one := 1
	environment.caseValue.BehaviorChecks = append(environment.caseValue.BehaviorChecks,
		contracts.Check{ID: "real_delegation", Type: "subagent_count", Min: &one, Max: &one, Patterns: []string{"delegated", "review"}, RequirementIDs: []string{"SKX-RUNNER-001"}, EvidenceIDs: []string{"behavior"}},
	)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
		child := client.Session{ID: "empty-child", ParentID: "root", Directory: request.WorkspacePath}
		runtime.children["root"] = []client.Session{child}
		runtime.children["empty-child"] = nil
		runtime.statuses["empty-child"] = client.SessionStatus{Type: "idle"}
		runtime.messages["empty-child"] = nil
		return runtime
	}}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == contracts.RunStatusPass {
		t.Fatalf("empty idle child produced a passing run: %#v", result)
	}
	if check := findCheck(t, result.Checks, "real_delegation"); check.Status == contracts.CheckStatusPass {
		t.Fatalf("empty idle child counted as delegation: %#v", check)
	}
}

func TestDelegationPatternsComeFromCompletedAssistantResultNotAssignment(t *testing.T) {
	patterns := []string{"workflow_id", "node_id", "attempt_id"}
	session := trace.SessionTrace{
		Session: client.Session{ID: "child", ParentID: "root", Title: strings.Join(patterns, " ")},
		Messages: []client.Message{
			{Info: client.ResponseInfo{ID: "user", SessionID: "child", Role: "user", Finish: "stop"}, Parts: []client.Part{{Type: "text", Text: strings.Join(patterns, " ")}}},
			{Info: client.ResponseInfo{ID: "assistant", SessionID: "child", Role: "assistant", Finish: "stop"}, Parts: []client.Part{{Type: "text", Text: "implemented and tested"}}},
		},
	}
	if sessionMatchesDelegationPatterns(session, patterns) {
		t.Fatal("the assignment/title satisfied child-result lineage patterns")
	}
	session.Messages[1].Parts[0].Text = "workflow_id=w node_id=n attempt_id=1"
	if !sessionMatchesDelegationPatterns(session, patterns) {
		t.Fatal("a completed assistant result with explicit lineage was rejected")
	}
}

func TestEngineV1RejectsMissingOrMismatchedObservedModel(t *testing.T) {
	for _, test := range []struct {
		name       string
		providerID string
		modelID    string
	}{
		{name: "missing"},
		{name: "mismatch", providerID: "anthropic", modelID: "claude-sonnet"},
		{name: "incomplete", providerID: "openai"},
		{name: "whitespace", providerID: " openai", modelID: "gpt-5"},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := newTestEnvironment(t, false)
			factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
				messages := completeMessages("Done successfully; verified.")
				messages[0].Info.ProviderID = test.providerID
				messages[0].Info.ModelID = test.modelID
				return newFakeRuntime(request, true, messages)
			}}
			engine := newTestEngine(t, environment, factory)

			result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != string(evaluationErrorPostResponseInvalid) {
				t.Fatalf("observed model result = %#v", result)
			}
			assertValidResultLineage(t, result)
		})
	}
}

func TestEngineV1RejectsConfiguredProviderAssertionMismatch(t *testing.T) {
	environment := newTestEnvironment(t, false)
	environment.caseValue.Agent.Model = "anthropic/claude-sonnet"
	factory := &fakeRuntimeFactory{}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err == nil || !strings.Contains(err.Error(), `provider "anthropic" conflicts with configured provenance provider "openai"`) {
		t.Fatalf("Run() mismatch error = %v", err)
	}
	if result.SchemaVersion != 0 || factory.requestCount() != 0 {
		t.Fatalf("provider mismatch started runtime or emitted a misleading result: result=%#v starts=%d", result, factory.requestCount())
	}
}

func TestEngineV1MetricOnlyTelemetryGapKeepsBehaviorEvaluable(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		return newFakeRuntime(request, true, incompleteUsageMessages("Done successfully; verified."))
	}}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.TelemetryComplete {
		t.Fatal("missing step-finish usage must make metrics non-evaluable")
	}
	for _, id := range []string{"two_writes", "status_before_write", "red_before_green", "exact_diff", "marker_exists"} {
		check := findCheck(t, result.Checks, id)
		if check.Status != contracts.CheckStatusPass {
			t.Errorf("mechanical check %q = %s (%s)", id, check.Status, check.Summary)
		}
	}
	if result.Status != contracts.RunStatusPass {
		t.Fatalf("metric-only telemetry gap changed behavior status to %s: %#v", result.Status, result.Error)
	}
	assertValidResultLineage(t, result)
}

func TestEngineV1EvaluatesEveryDeclaredV1CheckType(t *testing.T) {
	environment := newTestEnvironment(t, false)
	environment.caseValue.BehaviorChecks = append(environment.caseValue.BehaviorChecks,
		contracts.Check{ID: "regex_match_check", Type: "regex_match", Pattern: `(?i)verified`, RequirementIDs: []string{"SKX-RUNNER-001"}},
		contracts.Check{ID: "regex_count_check", Type: "regex_count", Pattern: `(?i)(done|verified)`, Value: 2, RequirementIDs: []string{"SKX-RUNNER-001"}},
		contracts.Check{ID: "regex_per_message_check", Type: "regex_count_max_per_msg", Pattern: `(?i)verified`, Value: 1, RequirementIDs: []string{"SKX-RUNNER-001"}},
		contracts.Check{ID: "file_written_check", Type: "file_written", Pattern: `*.txt`, RequirementIDs: []string{"SKX-RUNNER-001"}},
		contracts.Check{ID: "bash_output_check", Type: "bash_output_contains", Pattern: `working tree clean`, RequirementIDs: []string{"SKX-RUNNER-001"}},
	)
	engine := newTestEngine(t, environment, &fakeRuntimeFactory{})

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != contracts.RunStatusPass {
		t.Fatalf("status = %s, error = %#v", result.Status, result.Error)
	}
	for _, id := range []string{"regex_match_check", "regex_count_check", "regex_per_message_check", "file_written_check", "bash_output_check"} {
		if check := findCheck(t, result.Checks, id); check.Status != contracts.CheckStatusPass {
			t.Errorf("check %q = %s (%s)", id, check.Status, check.Summary)
		}
	}
	assertValidResultLineage(t, result)
}

func TestToolCallOrderAcrossSessionsRequiresAuthoritativeTimestamps(t *testing.T) {
	makeTrace := func(beforeTimestamp, afterTimestamp int64) *trace.Trace {
		return &trace.Trace{
			RootSessionID: "root", TelemetryComplete: true,
			Sessions: []trace.SessionTrace{
				{
					Session: client.Session{ID: "child-a", ParentID: "root"},
					Messages: []client.Message{{Info: client.ResponseInfo{ID: "a", SessionID: "child-a"}, Parts: []client.Part{{
						ID: "read", SessionID: "child-a", MessageID: "a", Type: "tool", Tool: "read",
						State: client.ToolState{Status: "completed", Time: client.PartTime{Start: beforeTimestamp}},
					}}}},
				},
				{
					Session: client.Session{ID: "child-b", ParentID: "root"},
					Messages: []client.Message{{Info: client.ResponseInfo{ID: "b", SessionID: "child-b"}, Parts: []client.Part{{
						ID: "edit", SessionID: "child-b", MessageID: "b", Type: "tool", Tool: "edit",
						State: client.ToolState{Status: "completed", Time: client.PartTime{Start: afterTimestamp}},
					}}}},
				},
			},
		}
	}
	check := contracts.Check{Type: "tool_call_order", Patterns: []string{"read", "first_write"}}
	withoutTime := observeCallOrder(check, contracts.Case{}, nil, nil, makeTrace(0, 0))
	if withoutTime.status != contracts.CheckStatusInvalid || !strings.Contains(withoutTime.summary, "no reliable total-order timestamp") {
		t.Fatalf("timestamp-free cross-session order = %#v", withoutTime)
	}
	sameTime := observeCallOrder(check, contracts.Case{}, nil, nil, makeTrace(10, 10))
	if sameTime.status != contracts.CheckStatusInvalid {
		t.Fatalf("ambiguous cross-session order = %#v", sameTime)
	}
	ordered := observeCallOrder(check, contracts.Case{}, nil, nil, makeTrace(10, 20))
	if ordered.status != contracts.CheckStatusPass {
		t.Fatalf("timestamped cross-session order = %#v", ordered)
	}
	repeated := makeTrace(100, 50)
	repeated.Sessions = append(repeated.Sessions, trace.SessionTrace{
		Session: client.Session{ID: "child-c", ParentID: "root"},
		Messages: []client.Message{{Info: client.ResponseInfo{ID: "c", SessionID: "child-c"}, Parts: []client.Part{{
			ID: "earliest-read", SessionID: "child-c", MessageID: "c", Type: "tool", Tool: "read",
			State: client.ToolState{Status: "completed", Time: client.PartTime{Start: 10}},
		}}}},
	})
	if observation := observeCallOrder(check, contracts.Case{}, nil, nil, repeated); observation.status != contracts.CheckStatusPass {
		t.Fatalf("earliest repeated cross-session tool was not authoritative: %#v", observation)
	}
}

func TestBashSyntheticToolNamesRequireAnExactCommand(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{command: "git status --short", want: "git_status"},
		{command: "git -C . status --short", want: "git_status"},
		{command: "git diff --staged", want: "git_inspect"},
		{command: "echo 'git status'", want: "bash"},
		{command: "git status --short && echo done", want: "bash"},
		{command: "git -c user.name=x -c user.email=x@y commit --allow-empty -m x", want: "git_commit"},
		{command: "git -c core.filemode=false add -- file.txt", want: "git_add"},
		{command: "/usr/bin/git commit --allow-empty -m x", want: "git_commit"},
		{command: "/opt/git/bin/git reset --hard HEAD", want: "git_reset"},
		{command: "git restore --staged -- staged.txt", want: "git_restore_staged"},
		{command: "git restore --worktree -- tracked.txt", want: "git_restore_worktree"},
		{command: "git clean -fd", want: "git_clean"},
		{command: "git add file.txt; git reset --hard", want: "bash"},
		{command: "git add file.txt && git reset --hard", want: "bash"},
		{command: "env GIT_CONFIG_NOSYSTEM=1 git add file.txt", want: "bash"},
		{command: "command git add file.txt", want: "bash"},
		{command: "skynex workflow start --id wf", want: "workflow_start"},
		{command: "/usr/local/bin/skynex workflow run wf --detach", want: "workflow_run_detach"},
		{command: "skynex workflow review --id wf", want: "workflow_review"},
		{command: "skynex workflow status wf", want: "workflow_status"},
		{command: "skynex workflow inspect wf", want: "workflow_inspect"},
		{command: "skynex workflow abort wf", want: "bash"},
		{command: "skynex workflow unknown wf", want: "bash"},
		{command: "skynex workflow run wf --detach && echo unsafe", want: "bash"},
		{command: "skynex workflow status > /tmp/marker", want: "bash"},
		{command: "skynex workflow status >/dev/tcp/example.invalid/443", want: "bash"},
		{command: "skynex workflow inspect < input.txt", want: "bash"},
		{command: "skynex workflow status *", want: "bash"},
	}
	for _, test := range tests {
		input, err := json.Marshal(map[string]string{"command": test.command})
		if err != nil {
			t.Fatal(err)
		}
		if got := normalizedToolName("bash", input); got != test.want {
			t.Errorf("command %q normalized to %q, want %q", test.command, got, test.want)
		}
	}
}

func TestHardShellActionAbsenceFailsClosed(t *testing.T) {
	hard, soft := true, false
	traceFor := func(command string) *trace.Trace {
		input, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		return &trace.Trace{
			RootSessionID: "root", TelemetryComplete: true,
			Sessions: []trace.SessionTrace{{
				Session: client.Session{ID: "root"},
				Messages: []client.Message{{
					Info: client.ResponseInfo{ID: "assistant", SessionID: "root", Role: "assistant"},
					Parts: []client.Part{{
						ID: "bash", SessionID: "root", MessageID: "assistant", Type: "tool", Tool: "bash",
						State: client.ToolState{Status: "completed", Input: input},
					}},
				}},
			}},
		}
	}
	observe := func(command, tool string, hard *bool) caseObservation {
		return observeCaseCheck(
			contracts.Check{Type: "tool_not_called", Tool: tool, Hard: hard},
			contracts.Case{}, sandbox.Snapshot{}, sandbox.Snapshot{}, nil, nil,
			traceFor(command), "", judges.Verdict{},
		)
	}
	for _, test := range []struct {
		command string
		tool    string
	}{
		{command: "git commit --allow-empty -m x", tool: "git_commit"},
		{command: "git -c core.filemode=false add -- file.txt", tool: "git_add"},
		{command: "/usr/bin/git reset --hard HEAD", tool: "git_reset"},
		{command: "git restore --staged -- staged.txt", tool: "git_restore_staged"},
		{command: "git clean -fd", tool: "git_clean"},
	} {
		if got := observe(test.command, test.tool, &hard); got.status != contracts.CheckStatusFail {
			t.Errorf("hard absence for %q = %#v, want fail", test.command, got)
		}
	}
	for _, command := range []string{
		"git add file.txt && git reset --hard HEAD",
		"env GIT_CONFIG_NOSYSTEM=1 git add file.txt",
		"command git clean -fd",
	} {
		if got := observe(command, "git_add", &hard); got.status != contracts.CheckStatusInvalid {
			t.Errorf("hard absence with unclassified %q = %#v, want invalid", command, got)
		}
		if got := observe(command, "git_add", &soft); got.status != contracts.CheckStatusPass {
			t.Errorf("soft absence with unclassified %q = %#v, want pass", command, got)
		}
	}
}

func TestEngineV1GitStateEvidencePreservesDirtyWorktreeAndRejectsMutation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	newDirtyEnvironment := func(t *testing.T) testEnvironment {
		t.Helper()
		environment := newTestEnvironment(t, false)
		if err := os.WriteFile(filepath.Join(environment.fixtureDir, ".gitignore"), []byte("ignored.local\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		snapshot, err := sandbox.DigestTree(environment.fixtureDir, sandbox.DefaultSnapshotLimits())
		if err != nil {
			t.Fatal(err)
		}
		tracked := "original tracked content\n"
		staged := "user staged change\n"
		untracked := "preserve this untracked note\n"
		ignored := "preserve this ignored state\n"
		environment.caseValue.Fixture.InitialGit = true
		environment.caseValue.Fixture.ExpectedDigest = snapshot.Digest
		environment.caseValue.Fixture.GitSeed = contracts.GitSeed{
			Tracked:   []contracts.GitSeedFile{{Path: "preexisting.txt", Content: &tracked}},
			Staged:    []contracts.GitSeedFile{{Path: "staged.txt", Content: &staged}},
			Untracked: []contracts.GitSeedFile{{Path: "user-note.txt", Content: &untracked}},
			Ignored:   []contracts.GitSeedFile{{Path: "ignored.local", Content: &ignored}},
		}
		environment.caseValue.RequirementIDs = append(environment.caseValue.RequirementIDs, "SKX-SCOPE-001", "SKX-GIT-001")
		for index := range environment.caseValue.BehaviorChecks {
			if environment.caseValue.BehaviorChecks[index].ID == "exact_diff" {
				environment.caseValue.BehaviorChecks[index].RequirementIDs = append(
					environment.caseValue.BehaviorChecks[index].RequirementIDs,
					"SKX-SCOPE-001", "SKX-GIT-001",
				)
			}
		}
		return environment
	}
	evidenceByID := func(result contracts.RunResult) map[string]contracts.EvidenceItem {
		items := make(map[string]contracts.EvidenceItem, len(result.Evidence.Items))
		for _, item := range result.Evidence.Items {
			items[item.ID] = item
		}
		return items
	}

	t.Run("worktree-only candidate change preserves basis", func(t *testing.T) {
		environment := newDirtyEnvironment(t)
		engine := newTestEngine(t, environment, &fakeRuntimeFactory{})
		result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != contracts.RunStatusPass {
			t.Fatalf("status = %s, error = %#v, checks = %#v", result.Status, result.Error, result.Checks)
		}
		check := findCheck(t, result.Checks, "judge_git_state-preserved")
		if check.Status != contracts.CheckStatusPass || !sameStringSet(check.EvidenceIDs, []string{"git_status_before", "git_status_after"}) {
			t.Fatalf("Git state check = %#v", check)
		}
		items := evidenceByID(result)
		before, beforeOK := items["git_status_before"]
		after, afterOK := items["git_status_after"]
		if !beforeOK || !afterOK || !before.Complete || !after.Complete || before.Digest == after.Digest {
			t.Fatalf("before/after Git evidence = %#v / %#v", before, after)
		}
		assertValidResultLineage(t, result)
	})

	mutations := []struct {
		name string
		args []string
	}{
		{name: "add", args: []string{"add", "--", "marker.txt"}},
		{name: "reset", args: []string{"reset", "--", "staged.txt"}},
		{name: "restore staged", args: []string{"restore", "--staged", "--", "staged.txt"}},
		{name: "clean", args: []string{"clean", "-fd"}},
		{name: "commit", args: []string{"commit", "--no-gpg-sign", "-m", "candidate"}},
	}
	for _, mutation := range mutations {
		mutation := mutation
		t.Run(mutation.name, func(t *testing.T) {
			environment := newDirtyEnvironment(t)
			factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
				runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
				originalSend := runtime.send
				runtime.send = func(ctx context.Context, sessionID string, input client.SendMessageRequest) (*client.Response, error) {
					response, err := originalSend(ctx, sessionID, input)
					if err != nil {
						return response, err
					}
					args := append([]string{"-C", request.WorkspacePath}, mutation.args...)
					if mutation.name == "commit" {
						args = append([]string{"-c", "user.name=skynex-test", "-c", "user.email=skynex-test@example.invalid", "-C", request.WorkspacePath}, mutation.args...)
					}
					command := exec.Command("git", args...)
					if output, commandErr := command.CombinedOutput(); commandErr != nil {
						return nil, fmt.Errorf("git %s: %v: %s", mutation.name, commandErr, output)
					}
					return response, nil
				}
				return runtime
			}}
			engine := newTestEngine(t, environment, factory)
			result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
			if err != nil {
				t.Fatal(err)
			}
			check := findCheck(t, result.Checks, "judge_git_state-preserved")
			if check.Status != contracts.CheckStatusFail || result.Status != contracts.RunStatusFail {
				t.Fatalf("mutation %q result = status %s, Git check %#v", mutation.name, result.Status, check)
			}
			assertValidResultLineage(t, result)
		})
	}
}

func TestEngineV1DetectsFixtureAndBundleDrift(t *testing.T) {
	tests := []struct {
		name       string
		withBundle bool
		mutate     func(testEnvironment, RuntimeRequest) error
		wantStatus contracts.RunStatus
	}{
		{
			name: "fixture source", mutate: func(environment testEnvironment, _ RuntimeRequest) error {
				return os.WriteFile(filepath.Join(environment.fixtureDir, "source-drift.txt"), []byte("drift\n"), 0o644)
			}, wantStatus: contracts.RunStatusFail,
		},
		{
			name: "bundle source", withBundle: true, mutate: func(environment testEnvironment, _ RuntimeRequest) error {
				return os.WriteFile(filepath.Join(environment.bundleDir, "source-drift.txt"), []byte("drift\n"), 0o644)
			}, wantStatus: contracts.RunStatusInvalid,
		},
		{
			name: "frozen bundle", withBundle: true, mutate: func(_ testEnvironment, request RuntimeRequest) error {
				path := filepath.Join(request.ConfigRoot, "opencode", "opencode.json")
				if err := os.Chmod(path, 0o600); err != nil {
					return err
				}
				return os.WriteFile(path, []byte("tampered\n"), 0o600)
			}, wantStatus: contracts.RunStatusInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newTestEnvironment(t, test.withBundle)
			factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
				runtime := newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
				originalSend := runtime.send
				runtime.send = func(ctx context.Context, sessionID string, input client.SendMessageRequest) (*client.Response, error) {
					response, err := originalSend(ctx, sessionID, input)
					if err != nil {
						return response, err
					}
					if err := test.mutate(environment, request); err != nil {
						return nil, err
					}
					return response, nil
				}
				return runtime
			}}
			engine := newTestEngine(t, environment, factory)

			result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Status != test.wantStatus {
				t.Fatalf("status = %s, want %s; error = %#v", result.Status, test.wantStatus, result.Error)
			}
			if result.Error == nil {
				t.Fatal("drift result must carry a structured error")
			}
			assertValidResultLineage(t, result)
		})
	}
}

func TestEngineV1EarlyInfrastructureResultIsValid(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{startErr: errors.New("runtime unavailable")}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("early Run() error = %v; result = %#v", err, result)
	}
	if result.Status != contracts.RunStatusInfraError || result.Error == nil || result.Error.Kind != "runtime_start" || !result.Error.Retryable {
		t.Fatalf("early result = %#v", result)
	}
	effective := factory.lastRequest(t).ToolPolicy
	if result.Provenance.ConfigDigest != effective.Digest || result.Provenance.ToolsetDigest != effective.AuthorizationDigest || result.Provenance.Extensions[provenanceExtensionEffectiveToolPolicyDigest] != effective.Digest {
		t.Fatalf("runtime-start failure lost generated policy provenance: result=%#v policy=%#v", result.Provenance, effective)
	}
	if result.Provenance.Extensions[provenanceExtensionEffectiveToolStatus] != "unobserved" || result.Provenance.Extensions[provenanceExtensionEffectiveToolCatalog] != "" {
		t.Fatalf("runtime-start failure claimed an unobserved tool catalog: %#v", result.Provenance.Extensions)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("early result validation = %v", err)
	}
}

func TestEngineV1ClassifiesRuntimeContractMismatchAsInvalid(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{startErr: fmt.Errorf("%w: requested model absent", ErrRuntimeContractIncompatible)}
	engine := newTestEngine(t, environment, factory)

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != contracts.RunStatusInvalid || result.Error == nil || result.Error.Kind != "runtime_contract" || result.Error.Retryable {
		t.Fatalf("runtime contract mismatch = %#v", result)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNewEngineRejectsInvalidProvenanceAtPreflight(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{}
	tests := []struct {
		name   string
		mutate func(*EngineConfig)
		want   string
	}{
		{name: "missing bundle", mutate: func(config *EngineConfig) { config.AgentBundleRoot = "" }, want: "agent bundle root is required"},
		{name: "invalid git sha", mutate: func(config *EngineConfig) { config.Provenance.GitSHA = "HEAD" }, want: "git SHA"},
		{name: "missing runtime version", mutate: func(config *EngineConfig) { config.Provenance.OpenCodeVersion = "" }, want: "OpenCode version"},
		{name: "invalid manifest digest", mutate: func(config *EngineConfig) { config.Provenance.ManifestDigest = "" }, want: "manifest digest"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := newTestEngineConfig(t, environment, factory)
			test.mutate(&config)
			if _, err := NewEngine(config); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewEngine() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestEngineV1ReservesExactCaseCheckIDs(t *testing.T) {
	environment := newTestEnvironment(t, false)
	collidingID := "judge_infrastructure_session-finished"
	environment.caseValue.BehaviorChecks = append(environment.caseValue.BehaviorChecks, contracts.Check{
		ID: collidingID, Type: "contains_all", Patterns: []string{"done"}, RequirementIDs: []string{"SKX-RUNNER-001"},
	})
	engine := newTestEngine(t, environment, &fakeRuntimeFactory{})

	result, err := engine.Run(context.Background(), environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if findCheck(t, result.Checks, collidingID).Status != contracts.CheckStatusPass {
		t.Fatalf("exact case check ID %q was not preserved", collidingID)
	}
	if findCheck(t, result.Checks, collidingID+"_2").Status != contracts.CheckStatusPass {
		t.Fatalf("generated check collision was not safely disambiguated")
	}
	assertValidResultLineage(t, result)
}

func TestEngineV1TimeoutProducesValidBudgetResult(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, false, completeMessages("Done successfully; verified."))
		runtime.send = func(ctx context.Context, _ string, _ client.SendMessageRequest) (*client.Response, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return runtime
	}}
	engine := newTestEngine(t, environment, factory)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	result, err := engine.Run(ctx, environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("timeout Run() error = %v; result = %#v", err, result)
	}
	if result.Status != contracts.RunStatusBudgetExhausted || result.Error == nil || result.Error.Kind != "timeout" || result.Error.Retryable {
		t.Fatalf("timeout result = %#v", result)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("timeout result validation = %v", err)
	}
}

func TestEngineV1CancellationProducesValidAbortedResult(t *testing.T) {
	environment := newTestEnvironment(t, false)
	factory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runtime := newFakeRuntime(request, false, completeMessages("Done successfully; verified."))
		runtime.send = func(ctx context.Context, _ string, _ client.SendMessageRequest) (*client.Response, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return runtime
	}}
	engine := newTestEngine(t, environment, factory)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := engine.Run(ctx, environment.caseValue, RunRequest{Variant: "candidate", Repetition: 1})
	if err != nil {
		t.Fatalf("canceled Run() error = %v; result = %#v", err, result)
	}
	if result.Status != contracts.RunStatusAborted || result.Error == nil || result.Error.Kind != "canceled" || result.Error.Retryable {
		t.Fatalf("canceled result = %#v", result)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("canceled result validation = %v", err)
	}
}

func TestEngineV1RunCasesPreservesEveryRepetition(t *testing.T) {
	environment := newTestEnvironment(t, false)
	var runNumber int
	engineConfigFactory := &fakeRuntimeFactory{build: func(request RuntimeRequest) *fakeRuntime {
		runNumber++
		return newFakeRuntime(request, true, completeMessages("Done successfully; verified."))
	}}
	engine := newTestEngine(t, environment, engineConfigFactory)
	// NewRunID must remain unique because every immutable sample is retained.
	engine.config.NewRunID = func() (string, error) {
		return "run_" + strings.Repeat("x", runNumber+1), nil
	}

	contractResult, err := engine.RunCases(context.Background(), environment.caseValue.Suite, []contracts.Case{environment.caseValue}, "candidate", 3)
	if err != nil {
		t.Fatalf("RunCases() error = %v", err)
	}
	if !contractResult.Complete || len(contractResult.Samples) != 3 || engineConfigFactory.requestCount() != 3 {
		t.Fatalf("contract result = %#v; starts = %d", contractResult, engineConfigFactory.requestCount())
	}
	for i, sample := range contractResult.Samples {
		if sample.Repetition != i+1 || sample.Status != contracts.RunStatusPass {
			t.Errorf("sample %d = repetition %d, status %s", i, sample.Repetition, sample.Status)
		}
		assertValidResultLineage(t, sample)
	}
}

func findCheck(t *testing.T, checks []contracts.CheckResult, id string) contracts.CheckResult {
	t.Helper()
	for _, check := range checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("check %q not found in %#v", id, checks)
	return contracts.CheckResult{}
}

func findEvidence(t *testing.T, result contracts.RunResult, id string) contracts.EvidenceItem {
	t.Helper()
	for _, item := range result.Evidence.Items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("evidence %q not found in %#v", id, result.Evidence.Items)
	return contracts.EvidenceItem{}
}

func assertValidResultLineage(t *testing.T, result contracts.RunResult) {
	t.Helper()
	if err := result.Validate(); err != nil {
		t.Fatalf("result validation = %v", err)
	}
	evidence := make(map[string]struct{}, len(result.Evidence.Items))
	for _, item := range result.Evidence.Items {
		evidence[item.ID] = struct{}{}
	}
	for _, check := range result.Checks {
		if len(check.RequirementIDs) == 0 {
			t.Errorf("check %q has no requirement lineage", check.ID)
		}
		for _, id := range check.EvidenceIDs {
			if _, ok := evidence[id]; !ok {
				t.Errorf("check %q references unknown evidence %q", check.ID, id)
			}
		}
	}
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]int, len(left))
	for _, value := range left {
		seen[value]++
	}
	for _, value := range right {
		seen[value]--
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}
