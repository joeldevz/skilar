package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/client"
)

type fakeAPI struct {
	sessions map[string]client.Session
	children map[string][]client.Session
	messages map[string][]client.Message
	statuses map[string]client.SessionStatus
	errors   map[string]error
	message  func(string) ([]client.Message, error)
}

func (f *fakeAPI) GetSessionContext(_ context.Context, id string) (*client.Session, error) {
	if err := f.errors["session:"+id]; err != nil {
		return nil, err
	}
	session, ok := f.sessions[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return &session, nil
}

func (f *fakeAPI) GetChildrenContext(_ context.Context, id string) ([]client.Session, error) {
	if err := f.errors["children:"+id]; err != nil {
		return nil, err
	}
	return append([]client.Session(nil), f.children[id]...), nil
}

func (f *fakeAPI) GetMessagesContext(_ context.Context, id string) ([]client.Message, error) {
	if f.message != nil {
		return f.message(id)
	}
	if err := f.errors["messages:"+id]; err != nil {
		return nil, err
	}
	return append([]client.Message(nil), f.messages[id]...), nil
}

func TestMessageCollectionStateIsExplicitAndSafe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*fakeAPI)
		want   MessageCollectionState
	}{
		{
			name: "complete", want: MessageCollectionComplete,
			mutate: func(*fakeAPI) {},
		},
		{
			name: "empty", want: MessageCollectionEmpty,
			mutate: func(api *fakeAPI) { api.messages["root"] = nil },
		},
		{
			name: "invalid", want: MessageCollectionInvalid,
			mutate: func(api *fakeAPI) { api.messages["root"][0].Info.SessionID = "wrong" },
		},
		{
			name: "failed", want: MessageCollectionFailed,
			mutate: func(api *fakeAPI) { api.errors["messages:root"] = errors.New("secret transport detail") },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			api := completeRootAPI()
			test.mutate(api)
			result, err := New(api, Options{StablePasses: 1, MaxPasses: 1}).Snapshot(context.Background(), "root", nil)
			if result == nil || len(result.Sessions) != 1 || result.Sessions[0].MessageCollection != test.want {
				t.Fatalf("message collection state = %#v, want %q", result, test.want)
			}
			if test.want == MessageCollectionFailed && (err == nil || !errors.Is(err, ErrMessageCollectionFailed) || strings.Contains(err.Error(), "secret")) {
				t.Fatalf("unsafe message collection error: %v", err)
			}
		})
	}
}

func TestExpectedRootMessagePreventsStableEmptyListing(t *testing.T) {
	t.Parallel()
	api := completeRootAPI()
	target := api.messages["root"][0]
	calls := 0
	api.message = func(id string) ([]client.Message, error) {
		calls++
		if calls == 1 {
			return nil, nil
		}
		return []client.Message{target}, nil
	}
	collector := New(api, Options{StablePasses: 1, MaxPasses: 3, PollInterval: time.Millisecond})
	collector.ExpectRootMessage(target.Info.ID)
	result, err := collector.Reconcile(context.Background(), "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.Passes != 2 || result.Sessions[0].MessageCollection != MessageCollectionComplete {
		t.Fatalf("calls/passes/state = %d/%d/%q", calls, result.Passes, result.Sessions[0].MessageCollection)
	}
}

func TestExpectedRootMessageDiagnosticDoesNotPersistIdentity(t *testing.T) {
	t.Parallel()
	const secretID = "secret-response-identity"
	collector := New(completeRootAPI(), Options{StablePasses: 1, MaxPasses: 1})
	collector.ExpectRootMessage(secretID)
	result, err := collector.Snapshot(context.Background(), "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !containsReason(result.IncompleteReasons, expectedRootMessageMissingReason) || strings.Contains(string(encoded), secretID) {
		t.Fatalf("unsafe expected-message diagnostic: %s", encoded)
	}
}

func (f *fakeAPI) GetSessionStatusesContext(context.Context) (map[string]client.SessionStatus, error) {
	if err := f.errors["statuses"]; err != nil {
		return nil, err
	}
	result := make(map[string]client.SessionStatus, len(f.statuses))
	for id, status := range f.statuses {
		result[id] = status
	}
	return result, nil
}

func TestReconcileDiscoversRecursivelyDeduplicatesAndTotalsUsage(t *testing.T) {
	t.Parallel()
	root := client.Session{ID: "root", Directory: "/fixture", Time: client.SessionTime{Created: 1, Updated: 10}}
	child := client.Session{ID: "child", ParentID: "root", Directory: "/fixture", Time: client.SessionTime{Created: 2, Updated: 9}}
	api := &fakeAPI{
		sessions: map[string]client.Session{"root": root, "child": child},
		children: map[string][]client.Session{
			// Duplicate discovery must not duplicate the child or its usage.
			"root": {child, child},
		},
		messages: map[string][]client.Message{
			"root": {
				assistantMessage("root", "m-root", "p-root", tokenInfo(10, 4, 2, 3, 1), 0.012, []client.Part{
					{
						ID: "tool-root", SessionID: "root", MessageID: "m-root", Type: "tool",
						CallID: "call-root", Tool: "edit", State: client.ToolState{
							Status: "completed", Input: []byte(`{"filePath":"main.go"}`), Output: "ok",
							Time: client.PartTime{Start: 3, End: 4},
						},
					},
					{ID: "retry-root", SessionID: "root", MessageID: "m-root", Type: "retry", Attempt: 1, Time: client.PartTime{Created: 19}},
				}),
			},
			"child": {
				assistantMessage("child", "m-child", "p-child", tokenInfo(5, 2, 1, 2, 0), 0.006, nil),
			},
		},
		statuses: map[string]client.SessionStatus{}, // absence means idle
		errors:   map[string]error{},
	}
	events := []client.GlobalEvent{
		{
			Directory: "/fixture", ReceivedAt: time.UnixMilli(20),
			Payload: client.Event{Type: "session.created", Properties: []byte(`{"info":{"id":"child","parentID":"root","directory":"/fixture","time":{"created":2,"updated":9}}}`)},
		},
		{
			Directory: "/fixture", ReceivedAt: time.UnixMilli(21),
			Payload: client.Event{Type: "session.status", Properties: []byte(`{"sessionID":"root","status":{"type":"retry","attempt":1,"message":"rate limited","next":22}}`)},
		},
		{
			Directory: "/fixture", ReceivedAt: time.UnixMilli(23),
			Payload: client.Event{Type: "session.compacted", Properties: []byte(`{"sessionID":"child"}`)},
		},
	}

	collector := New(api, Options{StablePasses: 2, MaxPasses: 3, PollInterval: time.Millisecond})
	result, err := collector.Reconcile(context.Background(), "root", events)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TelemetryComplete || len(result.IncompleteReasons) != 0 {
		t.Fatalf("incomplete trace: %v", result.IncompleteReasons)
	}
	if result.Passes != 2 || len(result.Sessions) != 2 {
		t.Fatalf("passes/sessions = %d/%d", result.Passes, len(result.Sessions))
	}
	wantUsage := Usage{Input: 15, Output: 6, Reasoning: 3, CacheRead: 5, CacheWrite: 1, Cost: 0.018, Records: 2}
	if !sameUsage(result.Totals, wantUsage) || result.Totals.Records != wantUsage.Records {
		t.Fatalf("totals = %#v, want %#v", result.Totals, wantUsage)
	}
	if len(result.Tools) != 1 || result.Tools[0].CallID != "call-root" || result.Tools[0].Status != "completed" {
		t.Fatalf("tools = %#v", result.Tools)
	}
	if len(result.Retries) != 1 || result.Retries[0].Attempt != 1 || result.Retries[0].Next != 22 || result.Retries[0].Source != "part+event" {
		t.Fatalf("retries = %#v", result.Retries)
	}
	if len(result.Compactions) != 1 || result.Compactions[0].SessionID != "child" {
		t.Fatalf("compactions = %#v", result.Compactions)
	}
}

func TestSnapshotRejectsParallelRootSessionEvents(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		events []client.GlobalEvent
	}{
		{
			name: "created",
			events: []client.GlobalEvent{
				globalEvent("1", "session.created", `{"info":{"id":"root","directory":"/fixture"}}`),
				globalEvent("2", "session.created", `{"info":{"id":"parallel-root","directory":"/fixture"}}`),
			},
		},
		{
			name: "created and deleted",
			events: []client.GlobalEvent{
				globalEvent("1", "session.created", `{"info":{"id":"root","directory":"/fixture"}}`),
				globalEvent("2", "session.created", `{"info":{"id":"parallel-root","directory":"/fixture"}}`),
				globalEvent("3", "session.deleted", `{"info":{"id":"parallel-root","directory":"/fixture"}}`),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			api := completeRootAPI()
			result, err := New(api, Options{}).Snapshot(context.Background(), "root", test.events)
			if err != nil {
				t.Fatal(err)
			}
			want := "global event session.created references session outside root tree: parallel-root"
			if result.TelemetryComplete || result.StructurallyComplete() || !containsReason(result.IncompleteReasons, want) {
				t.Fatalf("parallel root was not fenced: %#v", result.IncompleteReasons)
			}
		})
	}
}

func TestValidateRootSessionAdmissionFailsBeforePromptForParallelRoot(t *testing.T) {
	t.Parallel()
	root := globalEvent("1", "session.created", `{"info":{"id":"root","directory":"/fixture"}}`)
	if err := ValidateRootSessionAdmission("root", []client.GlobalEvent{root}); err != nil {
		t.Fatalf("root-only admission failed: %v", err)
	}
	err := ValidateRootSessionAdmission("root", []client.GlobalEvent{
		root,
		globalEvent("2", "session.created", `{"info":{"id":"parallel-root","directory":"/fixture"}}`),
	})
	if err == nil || !errors.Is(err, ErrGlobalSessionIsolation) || !strings.Contains(err.Error(), "parallel-root") {
		t.Fatalf("parallel root passed admission: %v", err)
	}
}

func TestSnapshotRejectsDeletionOfReconciledRoot(t *testing.T) {
	t.Parallel()
	result, err := New(completeRootAPI(), Options{}).Snapshot(context.Background(), "root", []client.GlobalEvent{
		globalEvent("1", "session.created", `{"info":{"id":"root","directory":"/fixture"}}`),
		globalEvent("2", "session.deleted", `{"info":{"id":"root","directory":"/fixture"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "global event session.deleted removed reconciled session: root"
	if result.StructurallyComplete() || !containsReason(result.IncompleteReasons, want) {
		t.Fatalf("deleted root was accepted: %v", result.IncompleteReasons)
	}
}

func TestGlobalSessionFenceAllowsOnlyRootTreeAndIgnoresNestedToolData(t *testing.T) {
	t.Parallel()
	api := completeRootAPI()
	child := client.Session{ID: "child", ParentID: "root", Directory: "/fixture"}
	api.sessions[child.ID] = child
	api.children["root"] = []client.Session{child}
	api.messages[child.ID] = []client.Message{assistantMessage("child", "m-child", "p-child", tokenInfo(1, 1, 0, 0, 0), 0, nil)}
	events := []client.GlobalEvent{
		globalEvent("1", "server.connected", `{}`),
		globalEvent("2", "session.created", `{"info":{"id":"root","directory":"/fixture"}}`),
		globalEvent("3", "session.created", `{"info":{"id":"child","parentID":"root","directory":"/fixture"}}`),
		globalEvent("4", "message.part.updated", `{"part":{"id":"p-root","sessionID":"root","messageID":"m-root","type":"tool","state":{"status":"completed","input":{"sessionID":"not-an-owner"}}}}`),
	}

	result, err := New(api, Options{}).Snapshot(context.Background(), "root", events)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TelemetryComplete || !result.StructurallyComplete() {
		t.Fatalf("root-tree events produced a false fence violation: %v", result.IncompleteReasons)
	}
}

func TestSnapshotFailsClosedWhenObservedMessagesOrPartsAreRemoved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		api    *fakeAPI
		events []client.GlobalEvent
		want   string
	}{
		{
			name: "message",
			api: func() *fakeAPI {
				api := completeRootAPI()
				api.messages["root"] = nil
				return api
			}(),
			events: []client.GlobalEvent{
				globalEvent("1", "session.created", `{"info":{"id":"root","directory":"/fixture"}}`),
				globalEvent("2", "message.updated", `{"info":{"id":"removed-message","sessionID":"root","role":"assistant"}}`),
				globalEvent("3", "message.removed", `{"messageID":"removed-message"}`),
			},
			want: "observed message removed before final state: removed-message",
		},
		{
			name: "part",
			api:  completeRootAPI(),
			events: []client.GlobalEvent{
				globalEvent("1", "session.created", `{"info":{"id":"root","directory":"/fixture"}}`),
				globalEvent("2", "message.part.updated", `{"part":{"id":"removed-part","sessionID":"root","messageID":"m-root","type":"text","text":"hidden"}}`),
				globalEvent("3", "message.part.removed", `{"partID":"removed-part"}`),
			},
			want: "observed part removed before final state: removed-part",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := New(test.api, Options{}).Snapshot(context.Background(), "root", test.events)
			if err != nil {
				t.Fatal(err)
			}
			if result.TelemetryComplete || result.StructurallyComplete() || !containsReason(result.IncompleteReasons, test.want) {
				t.Fatalf("removed evidence was accepted: %#v", result.IncompleteReasons)
			}
		})
	}
}

func TestFinalizeRunEventsCatchesParallelRootCreatedAndDeletedAfterSnapshot(t *testing.T) {
	t.Parallel()
	result, err := New(completeRootAPI(), Options{}).Snapshot(context.Background(), "root", []client.GlobalEvent{
		globalEvent("1", "session.created", `{"info":{"id":"root","directory":"/fixture"}}`),
	})
	if err != nil || !result.TelemetryComplete {
		t.Fatalf("initial snapshot = %#v, err = %v", result, err)
	}

	err = FinalizeRunEvents(result, []client.GlobalEvent{
		globalEvent("1", "session.created", `{"info":{"id":"root","directory":"/fixture"}}`),
		globalEvent("2", "session.created", `{"info":{"id":"parallel-root","directory":"/fixture"}}`),
		globalEvent("3", "session.deleted", `{"info":{"id":"parallel-root","directory":"/fixture"}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "global session isolation fence") {
		t.Fatalf("final fence error = %v", err)
	}
	for _, want := range []string{
		"global event session.created references session outside root tree: parallel-root",
		"global event session.deleted references session outside root tree: parallel-root",
	} {
		if !containsReason(result.IncompleteReasons, want) {
			t.Errorf("final reasons missing %q: %v", want, result.IncompleteReasons)
		}
	}
	if result.TelemetryComplete || result.StructurallyComplete() {
		t.Fatalf("finalized trace accepted deleted parallel root: %#v", result)
	}
}

func TestFinalizeRunEventsRejectsLateMutationOfReconciledTree(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		lateEvent client.GlobalEvent
	}{
		{
			name:      "new message",
			lateEvent: globalEvent("2", "message.updated", `{"info":{"id":"late-message","sessionID":"root","role":"assistant"}}`),
		},
		{
			name:      "deleted root",
			lateEvent: globalEvent("2", "session.deleted", `{"info":{"id":"root","directory":"/fixture"}}`),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rootEvent := globalEvent("1", "session.created", `{"info":{"id":"root","directory":"/fixture"}}`)
			result, err := New(completeRootAPI(), Options{}).Snapshot(context.Background(), "root", []client.GlobalEvent{rootEvent})
			if err != nil || !result.TelemetryComplete {
				t.Fatalf("initial snapshot = %#v, err = %v", result, err)
			}
			err = FinalizeRunEvents(result, []client.GlobalEvent{rootEvent, test.lateEvent})
			want := fmt.Sprintf("global event %s for reconciled session arrived after final snapshot: root", test.lateEvent.Payload.Type)
			if err == nil || !containsReason(result.IncompleteReasons, want) || result.StructurallyComplete() {
				t.Fatalf("late tree mutation was accepted: reasons=%v err=%v", result.IncompleteReasons, err)
			}
		})
	}
}

func TestFinalizeRunEventsRequiresObservedRootCreation(t *testing.T) {
	t.Parallel()
	result, err := New(completeRootAPI(), Options{}).Snapshot(context.Background(), "root", nil)
	if err != nil || !result.TelemetryComplete {
		t.Fatalf("initial snapshot = %#v, err = %v", result, err)
	}
	err = FinalizeRunEvents(result, []client.GlobalEvent{{
		Payload: client.Event{Type: "server.connected", Properties: json.RawMessage(`{}`)},
	}})
	want := "root session creation was not observed on global event stream: root"
	if err == nil || !errors.Is(err, ErrGlobalSessionIsolation) || !containsReason(result.IncompleteReasons, want) {
		t.Fatalf("silent session stream was accepted: reasons=%v err=%v", result.IncompleteReasons, err)
	}
}

func TestGlobalSessionFenceHasNoCrossRunState(t *testing.T) {
	t.Parallel()
	first, err := New(completeRootAPI(), Options{}).Snapshot(context.Background(), "root", []client.GlobalEvent{
		globalEvent("1", "session.created", `{"info":{"id":"previous-run","directory":"/fixture"}}`),
	})
	if err != nil || first.StructurallyComplete() {
		t.Fatalf("first run should expose its own foreign session: %#v, err=%v", first, err)
	}
	second, err := New(completeRootAPI(), Options{}).Snapshot(context.Background(), "root", []client.GlobalEvent{
		globalEvent("1", "session.created", `{"info":{"id":"root","directory":"/fixture"}}`),
	})
	if err != nil || !second.TelemetryComplete || !second.StructurallyComplete() {
		t.Fatalf("fresh run inherited prior recorder state: %#v, err=%v", second, err)
	}
}

func TestSnapshotFailsClosedForMissingStepUsageAndRunningTool(t *testing.T) {
	t.Parallel()
	root := client.Session{ID: "root", Directory: "/fixture"}
	message := client.Message{
		Info: client.ResponseInfo{
			ID: "m1", SessionID: "root", Role: "assistant", Finish: "tool-calls",
			Time: client.MessageTime{Created: 1, Completed: 2}, Tokens: tokenInfo(1, 1, 0, 0, 0),
		},
		Parts: []client.Part{{
			ID: "tool-1", SessionID: "root", MessageID: "m1", Type: "tool", CallID: "call-1", Tool: "bash",
			State: client.ToolState{Status: "running", Input: []byte(`{"command":"go test ./..."}`), Time: client.PartTime{Start: 1}},
		}},
	}
	api := &fakeAPI{
		sessions: map[string]client.Session{"root": root}, children: map[string][]client.Session{},
		messages: map[string][]client.Message{"root": {message}}, statuses: map[string]client.SessionStatus{"root": {Type: "busy"}},
		errors: map[string]error{},
	}

	result, err := New(api, Options{}).Snapshot(context.Background(), "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.TelemetryComplete {
		t.Fatal("telemetry unexpectedly complete")
	}
	reasons := strings.Join(result.IncompleteReasons, "\n")
	for _, want := range []string{"assistant message has no step-finish usage", "tool part tool-1 is running", "session root is busy"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("reasons missing %q:\n%s", want, reasons)
		}
	}
	if result.Totals.Input != 1 || result.Totals.Output != 1 {
		t.Fatalf("diagnostic fallback totals = %#v", result.Totals)
	}
}

func TestReconcileSeparatesStableTreeFromMetricCompleteness(t *testing.T) {
	t.Parallel()
	message := client.Message{
		Info: client.ResponseInfo{
			ID: "m1", SessionID: "root", Role: "assistant", Finish: "stop",
			ProviderID: "openai", ModelID: "gpt-5",
			Time: client.MessageTime{Created: 1, Completed: 2},
		},
		Parts: []client.Part{{ID: "text-1", SessionID: "root", MessageID: "m1", Type: "text", Text: "done"}},
	}
	api := &fakeAPI{
		sessions: map[string]client.Session{"root": {ID: "root"}},
		children: map[string][]client.Session{}, messages: map[string][]client.Message{"root": {message}},
		statuses: map[string]client.SessionStatus{"root": {Type: "idle"}}, errors: map[string]error{},
	}

	result, err := New(api, Options{StablePasses: 2, MaxPasses: 3, PollInterval: time.Millisecond}).Reconcile(context.Background(), "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.TelemetryComplete || !result.StructurallyComplete() || result.Passes != 2 {
		t.Fatalf("stable metric-gap trace = %#v", result)
	}
	if containsReason(result.IncompleteReasons, "session tree did not become structurally stable and complete") {
		t.Fatalf("metric-only gap was mislabeled as structural instability: %v", result.IncompleteReasons)
	}
}

func TestRetryAttemptMayRestartAcrossMessages(t *testing.T) {
	t.Parallel()
	first := assistantMessage("root", "m1", "p1", tokenInfo(1, 1, 0, 0, 0), 0, []client.Part{
		{ID: "retry-1", SessionID: "root", MessageID: "m1", Type: "retry", Attempt: 1},
	})
	second := assistantMessage("root", "m2", "p2", tokenInfo(1, 1, 0, 0, 0), 0, []client.Part{
		{ID: "retry-2", SessionID: "root", MessageID: "m2", Type: "retry", Attempt: 1},
	})
	second.Info.Time = client.MessageTime{Created: 3, Completed: 4}
	api := &fakeAPI{
		sessions: map[string]client.Session{"root": {ID: "root"}}, children: map[string][]client.Session{},
		messages: map[string][]client.Message{"root": {first, second}},
		statuses: map[string]client.SessionStatus{"root": {Type: "idle"}}, errors: map[string]error{},
	}

	result, err := New(api, Options{}).Snapshot(context.Background(), "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TelemetryComplete || len(result.Retries) != 2 {
		t.Fatalf("distinct durable retries were collapsed: %#v", result)
	}
}

func TestReconcileRejectsStableButNonQuiescentTree(t *testing.T) {
	t.Parallel()
	message := assistantMessage("root", "m1", "p1", tokenInfo(1, 1, 0, 0, 0), 0, nil)
	api := &fakeAPI{
		sessions: map[string]client.Session{"root": {ID: "root"}},
		children: map[string][]client.Session{}, messages: map[string][]client.Message{"root": {message}},
		statuses: map[string]client.SessionStatus{"root": {Type: "busy"}}, errors: map[string]error{},
	}

	result, err := New(api, Options{StablePasses: 1, MaxPasses: 1, PollInterval: time.Millisecond}).Reconcile(context.Background(), "root", nil)
	if err == nil || result == nil || result.StructurallyComplete() {
		t.Fatalf("non-quiescent trace was accepted: result=%#v err=%v", result, err)
	}
	if !containsReason(result.IncompleteReasons, "session tree did not become structurally stable and complete") {
		t.Fatalf("missing structural stability reason: %v", result.IncompleteReasons)
	}
}

func TestSnapshotReturnsPartialTraceOnChildCollectionError(t *testing.T) {
	t.Parallel()
	root := client.Session{ID: "root"}
	api := &fakeAPI{
		sessions: map[string]client.Session{"root": root},
		children: map[string][]client.Session{},
		messages: map[string][]client.Message{"root": {assistantMessage("root", "m1", "p1", tokenInfo(1, 1, 0, 0, 0), 0, nil)}},
		statuses: map[string]client.SessionStatus{},
		errors:   map[string]error{"children:root": errors.New("API unavailable")},
	}

	result, err := New(api, Options{}).Snapshot(context.Background(), "root", nil)
	if err == nil || !strings.Contains(err.Error(), "API unavailable") {
		t.Fatalf("error = %v", err)
	}
	if result == nil || result.TelemetryComplete || !containsReason(result.IncompleteReasons, "child discovery failed: root") {
		t.Fatalf("partial trace = %#v", result)
	}
}

func TestSnapshotTreatsStepFinishAsUsageAuthority(t *testing.T) {
	t.Parallel()
	first := tokenInfo(10, 3, 1, 2, 1)
	last := tokenInfo(5, 2, 0, 1, 0)
	message := client.Message{
		// This models OpenCode versions whose assistant aggregate contains only
		// the last step even though every durable step-finish part is present.
		Info: client.ResponseInfo{
			ID: "m1", SessionID: "root", Role: "assistant", Finish: "stop",
			Time: client.MessageTime{Created: 1, Completed: 4}, Tokens: last, Cost: 0.02,
		},
		Parts: []client.Part{
			{ID: "start-1", SessionID: "root", MessageID: "m1", Type: "step-start"},
			{ID: "finish-1", SessionID: "root", MessageID: "m1", Type: "step-finish", Tokens: first, Cost: 0.03},
			{ID: "start-2", SessionID: "root", MessageID: "m1", Type: "step-start"},
			{ID: "finish-2", SessionID: "root", MessageID: "m1", Type: "step-finish", Tokens: last, Cost: 0.02},
		},
	}
	api := &fakeAPI{
		sessions: map[string]client.Session{"root": {ID: "root"}},
		children: map[string][]client.Session{}, messages: map[string][]client.Message{"root": {message}},
		statuses: map[string]client.SessionStatus{}, errors: map[string]error{},
	}

	result, err := New(api, Options{}).Snapshot(context.Background(), "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TelemetryComplete {
		t.Fatalf("step-complete trace rejected: %v", result.IncompleteReasons)
	}
	if result.Totals.Input != 15 || result.Totals.Output != 5 || result.Totals.Records != 2 || result.Totals.Cost != 0.05 {
		t.Fatalf("authoritative totals = %#v", result.Totals)
	}
	if len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0], "aggregate differs") {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
}

func completeRootAPI() *fakeAPI {
	return &fakeAPI{
		sessions: map[string]client.Session{
			"root": {ID: "root", Directory: "/fixture"},
		},
		children: map[string][]client.Session{"root": nil},
		messages: map[string][]client.Message{
			"root": {assistantMessage("root", "m-root", "p-root", tokenInfo(1, 1, 0, 0, 0), 0, nil)},
		},
		statuses: map[string]client.SessionStatus{"root": {Type: "idle"}},
		errors:   map[string]error{},
	}
}

func globalEvent(id, eventType, properties string) client.GlobalEvent {
	return client.GlobalEvent{
		Directory: "/fixture", SSEID: id, ReceivedAt: time.UnixMilli(1),
		Payload: client.Event{Type: eventType, Properties: json.RawMessage(properties)},
	}
}

func assistantMessage(sessionID, messageID, partID string, tokens client.TokenInfo, cost float64, extra []client.Part) client.Message {
	parts := []client.Part{{ID: partID + "-start", SessionID: sessionID, MessageID: messageID, Type: "step-start"}}
	parts = append(parts, extra...)
	parts = append(parts, client.Part{
		ID: partID, SessionID: sessionID, MessageID: messageID, Type: "step-finish",
		Reason: "stop", Cost: cost, Tokens: tokens,
	})
	return client.Message{
		Info: client.ResponseInfo{
			ID: messageID, SessionID: sessionID, Role: "assistant", Finish: "stop", Cost: cost,
			Time: client.MessageTime{Created: 1, Completed: 2}, Tokens: tokens,
		},
		Parts: parts,
	}
}

func tokenInfo(input, output, reasoning, cacheRead, cacheWrite int) client.TokenInfo {
	return client.TokenInfo{
		Total: input + output + reasoning, Input: input, Output: output, Reasoning: reasoning,
		Cache:     client.CacheTokenInfo{Read: cacheRead, Write: cacheWrite},
		CacheRead: cacheRead, CacheWrite: cacheWrite, Present: true,
	}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func ExampleTrace_TelemetryComplete() {
	trace := Trace{TelemetryComplete: false, IncompleteReasons: []string{"child discovery failed: child-1"}}
	fmt.Println(trace.TelemetryComplete, trace.IncompleteReasons[0])
	// Output: false child discovery failed: child-1
}
