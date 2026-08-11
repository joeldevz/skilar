// Package trace reconciles a complete OpenCode session tree into deterministic
// evaluator evidence. Durable API state is authoritative; SSE events are kept
// as ordering evidence and checked against the final snapshot.
package trace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/joeldevz/skynex/internal/eval/client"
)

// API is the read-only OpenCode surface needed for final reconciliation.
type API interface {
	GetSessionContext(context.Context, string) (*client.Session, error)
	GetChildrenContext(context.Context, string) ([]client.Session, error)
	GetMessagesContext(context.Context, string) ([]client.Message, error)
	GetSessionStatusesContext(context.Context) (map[string]client.SessionStatus, error)
}

// EventSource can open the global OpenCode event stream.
type EventSource interface {
	OpenGlobalEvents(context.Context) (*client.EventStream, error)
}

// ErrGlobalSessionIsolation marks evidence that the private OpenCode server
// handled session-scoped activity outside (or after the watermark of) the
// reconciled root tree. Runners must classify this as an invalid sample.
var ErrGlobalSessionIsolation = errors.New("global session isolation fence violated")

const rootAdmissionQuietPeriod = 20 * time.Millisecond

// ValidateRootSessionAdmission checks the recorder history immediately after
// POST /session and before the first model prompt. At this point the only
// admitted session is the evaluator-created root; any other session identity
// proves the supposedly private server is not isolated.
func ValidateRootSessionAdmission(rootID string, events []client.GlobalEvent) error {
	if strings.TrimSpace(rootID) == "" {
		return fmt.Errorf("%w: root session ID is required", ErrGlobalSessionIsolation)
	}
	reasons := newReasonSet()
	events = deduplicateEvents(events, reasons)
	violations := newReasonSet()
	for _, violation := range validateGlobalSessionFence(events, map[string]struct{}{rootID: {}}, reasons) {
		violations.add(violation)
	}
	if !observedRootCreation(events, rootID) {
		violations.add("root session creation was not observed on global event stream: " + rootID)
	}
	for _, reason := range reasons.list() {
		violations.add(reason)
	}
	list := violations.list()
	if len(list) == 0 {
		return nil
	}
	return fmt.Errorf("%w: root admission: %s", ErrGlobalSessionIsolation, strings.Join(list, "; "))
}

// Options controls bounded final reconciliation.
type Options struct {
	StablePasses int
	MaxPasses    int
	PollInterval time.Duration
}

// Usage is an exact sum of deduplicated step-finish records.
type Usage struct {
	Input      int     `json:"input"`
	Output     int     `json:"output"`
	Reasoning  int     `json:"reasoning"`
	CacheRead  int     `json:"cache_read"`
	CacheWrite int     `json:"cache_write"`
	Cost       float64 `json:"cost_usd"`
	Records    int     `json:"records"`
}

// ToolCall is the terminal or in-flight state of one tool call.
type ToolCall struct {
	SessionID string          `json:"session_id"`
	MessageID string          `json:"message_id"`
	PartID    string          `json:"part_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Status    string          `json:"status"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    string          `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
	Time      client.PartTime `json:"time"`
}

// Retry records a durable retry part or an observed retry status event.
type Retry struct {
	SessionID string            `json:"session_id"`
	MessageID string            `json:"message_id,omitempty"`
	PartID    string            `json:"part_id,omitempty"`
	Attempt   int               `json:"attempt"`
	Message   string            `json:"message,omitempty"`
	Next      int64             `json:"next,omitempty"`
	Error     *client.ErrorInfo `json:"error,omitempty"`
	Created   int64             `json:"created,omitempty"`
	Source    string            `json:"source"`
}

// Compaction records both durable compaction parts and completion events.
type Compaction struct {
	SessionID string    `json:"session_id"`
	MessageID string    `json:"message_id,omitempty"`
	PartID    string    `json:"part_id,omitempty"`
	Auto      bool      `json:"auto,omitempty"`
	Observed  time.Time `json:"observed_at,omitempty"`
	Source    string    `json:"source"`
}

// SessionTrace is one canonical session in the reconciled tree.
type SessionTrace struct {
	Session  client.Session       `json:"session"`
	Status   client.SessionStatus `json:"status"`
	Children []string             `json:"children"`
	Messages []client.Message     `json:"messages"`
	Usage    Usage                `json:"usage"`
}

// Trace is the final evidence object. A false TelemetryComplete value blocks
// any efficiency claim even when partial totals are available.
type Trace struct {
	RootSessionID     string               `json:"root_session_id"`
	Sessions          []SessionTrace       `json:"sessions"`
	Events            []client.GlobalEvent `json:"events,omitempty"`
	Tools             []ToolCall           `json:"tools,omitempty"`
	Retries           []Retry              `json:"retries,omitempty"`
	Compactions       []Compaction         `json:"compactions,omitempty"`
	Totals            Usage                `json:"totals"`
	TelemetryComplete bool                 `json:"telemetry_complete"`
	IncompleteReasons []string             `json:"incomplete_reasons,omitempty"`
	Diagnostics       []string             `json:"diagnostics,omitempty"`
	ReconciledAt      time.Time            `json:"reconciled_at"`
	Passes            int                  `json:"reconciliation_passes"`
}

// Collector recursively discovers, deduplicates, and reconciles a session
// tree. It is safe for independent collectors to use the same API client.
type Collector struct {
	api  API
	opts Options
}

// New creates a trace collector.
func New(api API, opts Options) *Collector {
	if opts.StablePasses <= 0 {
		opts.StablePasses = 2
	}
	if opts.MaxPasses <= 0 {
		opts.MaxPasses = 12
	}
	if opts.MaxPasses < opts.StablePasses {
		opts.MaxPasses = opts.StablePasses
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 50 * time.Millisecond
	}
	return &Collector{api: api, opts: opts}
}

// Snapshot performs one recursive discovery pass. Collection errors are
// returned together with partial, explicitly incomplete evidence.
func (c *Collector) Snapshot(ctx context.Context, rootID string, events []client.GlobalEvent) (*Trace, error) {
	if strings.TrimSpace(rootID) == "" {
		return nil, errors.New("root session ID is required")
	}
	reasons := newReasonSet()
	diagnostics := newReasonSet()
	events = deduplicateEvents(events, reasons)
	t := &Trace{
		RootSessionID: rootID,
		Events:        append([]client.GlobalEvent(nil), events...),
		ReconciledAt:  time.Now().UTC(),
	}
	var collectionErrors []error

	statuses, err := c.api.GetSessionStatusesContext(ctx)
	if err != nil {
		reasons.add("session status collection failed")
		collectionErrors = append(collectionErrors, fmt.Errorf("get session statuses: %w", err))
		statuses = map[string]client.SessionStatus{}
	}

	eventChildren := childrenFromEvents(events)
	queue := []string{rootID}
	seenSessions := make(map[string]struct{})
	seenMessages := make(map[string][]byte)
	seenParts := make(map[string][]byte)
	usageSessions := make(map[string]bool)

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			collectionErrors = append(collectionErrors, err)
			reasons.add("trace collection context ended")
			break
		}
		sessionID := queue[0]
		queue = queue[1:]
		if _, duplicate := seenSessions[sessionID]; duplicate {
			continue
		}
		seenSessions[sessionID] = struct{}{}

		session, getErr := c.api.GetSessionContext(ctx, sessionID)
		if getErr != nil {
			reasons.add("session metadata missing: " + sessionID)
			collectionErrors = append(collectionErrors, fmt.Errorf("get session %q: %w", sessionID, getErr))
			continue
		}
		if session.ID == "" {
			reasons.add("session has empty ID: " + sessionID)
			session.ID = sessionID
		}
		if session.ID != sessionID {
			reasons.add(fmt.Sprintf("session ID mismatch: requested %s got %s", sessionID, session.ID))
		}
		if sessionID == rootID && session.ParentID != "" {
			reasons.add("root session unexpectedly has a parent")
		}

		children, childrenErr := c.api.GetChildrenContext(ctx, sessionID)
		if childrenErr != nil {
			reasons.add("child discovery failed: " + sessionID)
			collectionErrors = append(collectionErrors, fmt.Errorf("get children for %q: %w", sessionID, childrenErr))
		}
		childIDs := make([]string, 0, len(children)+len(eventChildren[sessionID]))
		childSet := make(map[string]struct{})
		for _, child := range children {
			if child.ID == "" {
				reasons.add("child session has empty ID under: " + sessionID)
				continue
			}
			if child.ParentID != sessionID {
				reasons.add(fmt.Sprintf("child %s reports parent %s, expected %s", child.ID, child.ParentID, sessionID))
			}
			childSet[child.ID] = struct{}{}
		}
		for _, childID := range eventChildren[sessionID] {
			if _, found := childSet[childID]; !found {
				reasons.add(fmt.Sprintf("event child %s missing from final children of %s", childID, sessionID))
				childSet[childID] = struct{}{}
			}
		}
		for childID := range childSet {
			childIDs = append(childIDs, childID)
			queue = append(queue, childID)
		}
		sort.Strings(childIDs)

		messages, messagesErr := c.api.GetMessagesContext(ctx, sessionID)
		if messagesErr != nil {
			reasons.add("message collection failed: " + sessionID)
			collectionErrors = append(collectionErrors, fmt.Errorf("get messages for %q: %w", sessionID, messagesErr))
			messages = nil
		}
		messages = canonicalizeMessages(sessionID, messages, seenMessages, seenParts, reasons)
		sort.SliceStable(messages, func(i, j int) bool {
			if messages[i].Info.Time.Created != messages[j].Info.Time.Created {
				return messages[i].Info.Time.Created < messages[j].Info.Time.Created
			}
			return messages[i].Info.ID < messages[j].Info.ID
		})

		status := statuses[sessionID]
		if status.Type == "" {
			status.Type = "idle"
		}
		if status.Type != "idle" {
			reasons.add(fmt.Sprintf("session %s is %s", sessionID, status.Type))
		}

		sessionTrace := SessionTrace{
			Session: *session, Status: status, Children: childIDs, Messages: messages,
		}
		sessionTrace.Usage = inspectMessages(t, sessionID, messages, usageSessions, reasons, diagnostics)
		t.Totals.add(sessionTrace.Usage)
		t.Sessions = append(t.Sessions, sessionTrace)
	}

	sort.Slice(t.Sessions, func(i, j int) bool { return t.Sessions[i].Session.ID < t.Sessions[j].Session.ID })
	for _, session := range t.Sessions {
		if !usageSessions[session.Session.ID] {
			reasons.add("session has no complete usage record: " + session.Session.ID)
		}
	}
	validateObservedEvents(t, seenSessions, seenMessages, seenParts, reasons)
	validateGlobalSessionFence(t.Events, seenSessions, reasons)
	canonicalizeRetries(t, reasons)
	sortTraceDetails(t)
	t.IncompleteReasons = reasons.list()
	t.Diagnostics = diagnostics.list()
	t.TelemetryComplete = len(t.IncompleteReasons) == 0
	return t, errors.Join(collectionErrors...)
}

func canonicalizeRetries(t *Trace, reasons *reasonSet) {
	if len(t.Retries) == 0 {
		return
	}
	result := make([]Retry, 0, len(t.Retries))
	durableByAttempt := make(map[string][]int, len(t.Retries))
	events := make([]Retry, 0)
	for _, retry := range t.Retries {
		if retry.SessionID == "" || retry.Attempt < 0 {
			reasons.add("retry record has invalid session or attempt")
			result = append(result, retry)
			continue
		}
		if retry.Source == "event" {
			events = append(events, retry)
			continue
		}
		key := retry.SessionID + "\x00" + fmt.Sprintf("%d", retry.Attempt)
		durableByAttempt[key] = append(durableByAttempt[key], len(result))
		result = append(result, retry)
	}
	for _, event := range events {
		key := event.SessionID + "\x00" + fmt.Sprintf("%d", event.Attempt)
		matches := durableByAttempt[key]
		if len(matches) == 1 {
			existing := &result[matches[0]]
			existing.Message = event.Message
			existing.Next = event.Next
			existing.Source = "part+event"
			continue
		}
		if len(matches) > 1 {
			// Attempt counters may restart for each message. All distinct durable
			// parts remain authoritative; an SSE status without message identity
			// cannot safely be assigned to just one of them.
			continue
		}
		// An SSE-only retry is useful diagnostic evidence but is not a stable
		// metric: repeated status delivery has no durable identity to dedupe.
		result = append(result, event)
		reasons.add(fmt.Sprintf("retry event has no durable retry part: %s attempt %d", event.SessionID, event.Attempt))
	}
	t.Retries = result
}

// Reconcile repeats snapshots until the complete tree is quiescent and has an
// identical fingerprint for StablePasses consecutive observations.
func (c *Collector) Reconcile(ctx context.Context, rootID string, events []client.GlobalEvent) (*Trace, error) {
	frozen := append([]client.GlobalEvent(nil), events...)
	return c.reconcile(ctx, rootID, func() []client.GlobalEvent {
		return frozen
	})
}

// ReconcileRecorded reconciles against live snapshots of recorder. Keeping the
// global stream open through final reconciliation prevents a parallel session
// from escaping merely because it was created after the model response.
func (c *Collector) ReconcileRecorded(ctx context.Context, rootID string, recorder *Recorder) (*Trace, error) {
	if recorder == nil {
		return c.Reconcile(ctx, rootID, nil)
	}
	return c.reconcile(ctx, rootID, recorder.Snapshot)
}

func (c *Collector) reconcile(ctx context.Context, rootID string, eventSnapshot func() []client.GlobalEvent) (*Trace, error) {
	var previous string
	stable := 0
	var latest *Trace
	var latestErr error
	for pass := 1; pass <= c.opts.MaxPasses; pass++ {
		trace, err := c.Snapshot(ctx, rootID, eventSnapshot())
		if trace != nil {
			trace.Passes = pass
			latest = trace
		}
		latestErr = err
		if trace != nil {
			fingerprint := traceFingerprint(trace)
			if fingerprint == previous && err == nil {
				stable++
			} else {
				stable = 1
			}
			previous = fingerprint
			// Quiescence and durable tree integrity are independent from usage
			// completeness. A stable tree with missing token records is still valid
			// behavioral evidence; only metrics derived from those records are not.
			if stable >= c.opts.StablePasses && trace.StructurallyComplete() && err == nil {
				return trace, nil
			}
		}
		if pass == c.opts.MaxPasses {
			break
		}
		timer := time.NewTimer(c.opts.PollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if latest != nil {
				addIncomplete(latest, "final reconciliation context ended")
			}
			return latest, errors.Join(latestErr, ctx.Err())
		case <-timer.C:
		}
	}
	if latest == nil {
		return nil, errors.Join(latestErr, errors.New("final reconciliation produced no snapshot"))
	}
	addIncomplete(latest, "session tree did not become structurally stable and complete")
	return latest, errors.Join(latestErr, errors.New("session tree did not become structurally stable and complete"))
}

// StructurallyComplete reports whether the durable session tree, messages,
// tools, and ownership evidence are complete. Missing usage records are kept
// separate so mechanical behavior can remain evaluable while efficiency gates
// fail closed.
func (t *Trace) StructurallyComplete() bool {
	if t == nil {
		return false
	}
	for _, reason := range t.IncompleteReasons {
		if metricOnlyIncompleteReason(reason) {
			continue
		}
		return false
	}
	return true
}

func metricOnlyIncompleteReason(reason string) bool {
	return strings.HasPrefix(reason, "session has no complete usage record: ") ||
		strings.HasPrefix(reason, "step-finish tokens missing: ") ||
		strings.HasPrefix(reason, "assistant message has no step-finish usage: ") ||
		strings.HasPrefix(reason, "assistant message usage missing: ")
}

func canonicalizeMessages(sessionID string, messages []client.Message, seenMessages, seenParts map[string][]byte, reasons *reasonSet) []client.Message {
	result := make([]client.Message, 0, len(messages))
	for _, message := range messages {
		encodedMessage, _ := json.Marshal(message)
		if message.Info.ID == "" {
			reasons.add("message has empty ID in session: " + sessionID)
		} else if previous, found := seenMessages[message.Info.ID]; found {
			if !equalJSON(previous, encodedMessage) {
				reasons.add("conflicting duplicate message ID: " + message.Info.ID)
			}
			continue
		} else {
			seenMessages[message.Info.ID] = encodedMessage
		}
		if message.Info.SessionID != sessionID {
			reasons.add(fmt.Sprintf("message %s belongs to session %s, expected %s", message.Info.ID, message.Info.SessionID, sessionID))
		}
		parts := make([]client.Part, 0, len(message.Parts))
		for _, part := range message.Parts {
			if part.ID == "" {
				reasons.add("part has empty ID in message: " + message.Info.ID)
				parts = append(parts, part)
				continue
			}
			encoded, _ := json.Marshal(part)
			if previous, found := seenParts[part.ID]; found {
				if !equalJSON(previous, encoded) {
					reasons.add("conflicting duplicate part ID: " + part.ID)
				}
				continue
			}
			seenParts[part.ID] = encoded
			if part.SessionID != sessionID || part.MessageID != message.Info.ID {
				reasons.add("part ownership mismatch: " + part.ID)
			}
			parts = append(parts, part)
		}
		message.Parts = parts
		result = append(result, message)
	}
	return result
}

func inspectMessages(t *Trace, sessionID string, messages []client.Message, usageSessions map[string]bool, reasons, diagnostics *reasonSet) Usage {
	var usage Usage
	for _, message := range messages {
		stepUsage := Usage{}
		stepStarts := 0
		stepCount := 0
		for _, part := range message.Parts {
			switch part.Type {
			case "step-start":
				stepStarts++
			case "step-finish":
				if !part.Tokens.Present {
					reasons.add("step-finish tokens missing: " + part.ID)
					continue
				}
				stepUsage.add(usageFrom(part.Tokens, part.Cost))
				stepCount++
			case "tool":
				t.Tools = append(t.Tools, ToolCall{
					SessionID: sessionID, MessageID: message.Info.ID, PartID: part.ID,
					CallID: part.CallID, Tool: part.Tool, Status: part.State.Status,
					Input: cloneRaw(part.State.Input), Output: part.State.Output,
					Error: part.State.Error, Time: part.State.Time,
				})
				if part.CallID == "" {
					reasons.add("tool part has empty call ID: " + part.ID)
				}
				if part.State.Status != "completed" && part.State.Status != "error" {
					reasons.add(fmt.Sprintf("tool part %s is %s", part.ID, part.State.Status))
				}
			case "retry":
				t.Retries = append(t.Retries, Retry{
					SessionID: sessionID, MessageID: message.Info.ID, PartID: part.ID,
					Attempt: part.Attempt, Error: part.Error, Created: part.Time.Created,
					Source: "part",
				})
			case "compaction":
				t.Compactions = append(t.Compactions, Compaction{
					SessionID: sessionID, MessageID: message.Info.ID, PartID: part.ID,
					Auto: part.Auto, Source: "part",
				})
			}
		}
		if message.Info.Role != "assistant" {
			continue
		}
		if message.Info.Time.Completed == 0 && message.Info.Finish == "" && message.Info.Error == nil {
			reasons.add("assistant message is not complete: " + message.Info.ID)
		}
		if stepCount == 0 {
			if message.Info.Tokens.Present {
				// Preserve a partial total for diagnostics, but do not declare it
				// complete: current OpenCode must expose each step record.
				fallback := usageFrom(message.Info.Tokens, message.Info.Cost)
				usage.add(fallback)
				usageSessions[sessionID] = true
				reasons.add("assistant message has no step-finish usage: " + message.Info.ID)
			} else {
				reasons.add("assistant message usage missing: " + message.Info.ID)
			}
			continue
		}
		if stepStarts != stepCount {
			reasons.add(fmt.Sprintf("assistant message step boundary mismatch: %s (%d starts, %d finishes)", message.Info.ID, stepStarts, stepCount))
		}
		usage.add(stepUsage)
		usageSessions[sessionID] = true
		if message.Info.Tokens.Present {
			expected := usageFrom(message.Info.Tokens, message.Info.Cost)
			if !sameUsage(stepUsage, expected) {
				// Step-finish records are the durable per-request authority. Some
				// OpenCode releases expose only the last step in message.info.
				diagnostics.add("message aggregate differs from authoritative step usage: " + message.Info.ID)
			}
		} else {
			diagnostics.add("assistant aggregate usage missing: " + message.Info.ID)
		}
	}
	return usage
}

func usageFrom(tokens client.TokenInfo, cost float64) Usage {
	return Usage{
		Input: tokens.Input, Output: tokens.Output, Reasoning: tokens.Reasoning,
		CacheRead: tokens.CacheRead, CacheWrite: tokens.CacheWrite,
		Cost: cost, Records: 1,
	}
}

func (u *Usage) add(other Usage) {
	u.Input += other.Input
	u.Output += other.Output
	u.Reasoning += other.Reasoning
	u.CacheRead += other.CacheRead
	u.CacheWrite += other.CacheWrite
	u.Cost += other.Cost
	u.Records += other.Records
}

func sameUsage(a, b Usage) bool {
	return a.Input == b.Input && a.Output == b.Output && a.Reasoning == b.Reasoning &&
		a.CacheRead == b.CacheRead && a.CacheWrite == b.CacheWrite && math.Abs(a.Cost-b.Cost) < 1e-9
}

func childrenFromEvents(events []client.GlobalEvent) map[string][]string {
	result := make(map[string][]string)
	for _, event := range events {
		if event.Payload.Type != "session.created" && event.Payload.Type != "session.updated" {
			continue
		}
		var properties struct {
			Info client.Session `json:"info"`
		}
		if json.Unmarshal(event.Payload.Properties, &properties) == nil && properties.Info.ID != "" && properties.Info.ParentID != "" {
			result[properties.Info.ParentID] = appendUnique(result[properties.Info.ParentID], properties.Info.ID)
		}
	}
	return result
}

func validateObservedEvents(t *Trace, sessions map[string]struct{}, messages, parts map[string][]byte, reasons *reasonSet) {
	rootDirectory := ""
	for _, session := range t.Sessions {
		if session.Session.ID == t.RootSessionID {
			rootDirectory = session.Session.Directory
			break
		}
	}
	removedMessages := make(map[string]struct{})
	removedParts := make(map[string]struct{})
	for _, event := range t.Events {
		if rootDirectory != "" && event.Directory != "" && event.Directory != rootDirectory {
			continue
		}
		switch event.Payload.Type {
		case "message.removed":
			var p struct {
				MessageID string `json:"messageID"`
			}
			if json.Unmarshal(event.Payload.Properties, &p) == nil {
				removedMessages[p.MessageID] = struct{}{}
			}
		case "message.part.removed":
			var p struct {
				PartID string `json:"partID"`
			}
			if json.Unmarshal(event.Payload.Properties, &p) == nil {
				removedParts[p.PartID] = struct{}{}
			}
		}
	}
	for _, event := range t.Events {
		if rootDirectory != "" && event.Directory != "" && event.Directory != rootDirectory {
			continue
		}
		switch event.Payload.Type {
		case "message.updated":
			var p struct {
				Info client.ResponseInfo `json:"info"`
			}
			if json.Unmarshal(event.Payload.Properties, &p) != nil {
				continue
			}
			if _, inTree := sessions[p.Info.SessionID]; inTree {
				if _, found := messages[p.Info.ID]; !found {
					if _, removed := removedMessages[p.Info.ID]; removed {
						reasons.add("observed message removed before final state: " + p.Info.ID)
					} else {
						reasons.add("observed message missing from final state: " + p.Info.ID)
					}
				}
			}
		case "message.part.updated":
			var p struct {
				Part client.Part `json:"part"`
			}
			if json.Unmarshal(event.Payload.Properties, &p) != nil {
				continue
			}
			if _, inTree := sessions[p.Part.SessionID]; inTree {
				if _, found := parts[p.Part.ID]; !found {
					if _, removed := removedParts[p.Part.ID]; removed {
						reasons.add("observed part removed before final state: " + p.Part.ID)
					} else {
						reasons.add("observed part missing from final state: " + p.Part.ID)
					}
				}
			}
		case "session.status":
			var p struct {
				SessionID string               `json:"sessionID"`
				Status    client.SessionStatus `json:"status"`
			}
			decodeErr := json.Unmarshal(event.Payload.Properties, &p)
			_, inTree := sessions[p.SessionID]
			if decodeErr == nil && inTree && p.Status.Type == "retry" {
				t.Retries = append(t.Retries, Retry{
					SessionID: p.SessionID, Attempt: p.Status.Attempt,
					Message: p.Status.Message, Next: p.Status.Next, Source: "event",
				})
			}
		case "session.compacted":
			var p struct {
				SessionID string `json:"sessionID"`
			}
			decodeErr := json.Unmarshal(event.Payload.Properties, &p)
			_, inTree := sessions[p.SessionID]
			if decodeErr == nil && inTree {
				t.Compactions = append(t.Compactions, Compaction{
					SessionID: p.SessionID, Observed: event.ReceivedAt, Source: "event",
				})
			}
		}
	}
}

// FinalizeRunEvents applies the isolation fence to the complete recorder
// history after the private runtime has stopped. This catches a parallel root
// that was created (and possibly deleted) after the last reconciliation pass.
// Non-session events and events owned by the reconciled root tree are allowed.
func FinalizeRunEvents(t *Trace, events []client.GlobalEvent) error {
	if t == nil {
		return errors.New("cannot finalize global session fence without a trace")
	}
	reasons := newReasonSet()
	for _, reason := range t.IncompleteReasons {
		reasons.add(reason)
	}
	reconciledEvents := append([]client.GlobalEvent(nil), t.Events...)
	events = deduplicateEvents(events, reasons)
	sessions := make(map[string]struct{}, len(t.Sessions))
	for _, session := range t.Sessions {
		if session.Session.ID != "" {
			sessions[session.Session.ID] = struct{}{}
		}
	}
	violations := newReasonSet()
	for _, violation := range validateGlobalSessionFence(events, sessions, reasons) {
		violations.add(violation)
	}
	if !observedRootCreation(events, t.RootSessionID) {
		reason := "root session creation was not observed on global event stream: " + t.RootSessionID
		reasons.add(reason)
		violations.add(reason)
	}
	watermark := len(reconciledEvents)
	if len(events) < watermark || !sameGlobalEventPrefix(reconciledEvents, events[:minInt(watermark, len(events))]) {
		reason := "global event history changed after final reconciliation"
		reasons.add(reason)
		violations.add(reason)
		watermark = len(events)
	}
	for _, event := range events[watermark:] {
		for _, sessionID := range globalEventSessionIDs(event.Payload) {
			if _, inTree := sessions[sessionID]; !inTree {
				// The outside-tree fence above already records the more precise
				// ownership violation.
				continue
			}
			reason := fmt.Sprintf("global event %s for reconciled session arrived after final snapshot: %s", event.Payload.Type, sessionID)
			reasons.add(reason)
			violations.add(reason)
		}
	}
	t.Events = append([]client.GlobalEvent(nil), events...)
	t.IncompleteReasons = reasons.list()
	t.TelemetryComplete = len(t.IncompleteReasons) == 0
	violationList := violations.list()
	if len(violationList) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrGlobalSessionIsolation, strings.Join(violationList, "; "))
}

func sameGlobalEventPrefix(want, got []client.GlobalEvent) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		left, _ := json.Marshal(struct {
			Directory string
			Payload   client.Event
			SSEID     string
		}{want[i].Directory, want[i].Payload, want[i].SSEID})
		right, _ := json.Marshal(struct {
			Directory string
			Payload   client.Event
			SSEID     string
		}{got[i].Directory, got[i].Payload, got[i].SSEID})
		if !equalJSON(left, right) {
			return false
		}
	}
	return true
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func validateGlobalSessionFence(events []client.GlobalEvent, sessions map[string]struct{}, reasons *reasonSet) []string {
	violations := newReasonSet()
	for _, event := range events {
		for _, sessionID := range globalEventSessionIDs(event.Payload) {
			if _, inTree := sessions[sessionID]; inTree {
				if event.Payload.Type == "session.deleted" {
					reason := "global event session.deleted removed reconciled session: " + sessionID
					reasons.add(reason)
					violations.add(reason)
				}
				continue
			}
			reason := fmt.Sprintf("global event %s references session outside root tree: %s", event.Payload.Type, sessionID)
			reasons.add(reason)
			violations.add(reason)
		}
	}
	return violations.list()
}

func observedRootCreation(events []client.GlobalEvent, rootID string) bool {
	if rootID == "" {
		return false
	}
	for _, event := range events {
		if event.Payload.Type != "session.created" {
			continue
		}
		ids := globalEventSessionIDs(event.Payload)
		if len(ids) == 1 && ids[0] == rootID {
			return true
		}
	}
	return false
}

// globalEventSessionIDs extracts only schema-level ownership fields from the
// OpenCode event families used by the pinned API. It intentionally does not
// walk arbitrary JSON: tool input/output may legitimately contain a field
// named sessionID and is not event ownership evidence.
func globalEventSessionIDs(event client.Event) []string {
	var direct struct {
		SessionID string `json:"sessionID"`
	}
	var info struct {
		Info struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		} `json:"info"`
	}
	var part struct {
		Part struct {
			SessionID string `json:"sessionID"`
		} `json:"part"`
	}

	switch event.Type {
	case "session.created", "session.updated", "session.deleted":
		if json.Unmarshal(event.Properties, &info) == nil {
			return nonEmptyUnique(info.Info.ID)
		}
	case "message.updated":
		if json.Unmarshal(event.Properties, &info) == nil {
			return nonEmptyUnique(info.Info.SessionID)
		}
	case "message.part.updated":
		if json.Unmarshal(event.Properties, &part) == nil {
			return nonEmptyUnique(part.Part.SessionID)
		}
	case "message.removed", "message.part.delta", "message.part.removed",
		"permission.updated", "permission.replied", "question.asked", "question.replied", "question.rejected",
		"session.status", "session.idle", "session.compacted", "session.diff", "session.error",
		"todo.updated", "command.executed":
		if json.Unmarshal(event.Properties, &direct) == nil {
			return nonEmptyUnique(direct.SessionID)
		}
	}
	return nil
}

func nonEmptyUnique(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		result = appendUnique(result, value)
	}
	return result
}

func sortTraceDetails(t *Trace) {
	sort.Slice(t.Tools, func(i, j int) bool { return t.Tools[i].PartID < t.Tools[j].PartID })
	sort.Slice(t.Retries, func(i, j int) bool {
		if t.Retries[i].SessionID != t.Retries[j].SessionID {
			return t.Retries[i].SessionID < t.Retries[j].SessionID
		}
		if t.Retries[i].Attempt != t.Retries[j].Attempt {
			return t.Retries[i].Attempt < t.Retries[j].Attempt
		}
		return t.Retries[i].PartID < t.Retries[j].PartID
	})
	sort.Slice(t.Compactions, func(i, j int) bool {
		if t.Compactions[i].SessionID != t.Compactions[j].SessionID {
			return t.Compactions[i].SessionID < t.Compactions[j].SessionID
		}
		return t.Compactions[i].PartID < t.Compactions[j].PartID
	})
}

func traceFingerprint(t *Trace) string {
	payload := struct {
		Sessions    []SessionTrace
		Events      []client.GlobalEvent
		Tools       []ToolCall
		Retries     []Retry
		Compactions []Compaction
		Totals      Usage
		Complete    bool
		Reasons     []string
		Diagnostics []string
	}{t.Sessions, t.Events, t.Tools, t.Retries, t.Compactions, t.Totals, t.TelemetryComplete, t.IncompleteReasons, t.Diagnostics}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
}

func equalJSON(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return bytes.Equal(a, b)
	}
	leftCanonical, _ := json.Marshal(left)
	rightCanonical, _ := json.Marshal(right)
	return bytes.Equal(leftCanonical, rightCanonical)
}

func deduplicateEvents(events []client.GlobalEvent, reasons *reasonSet) []client.GlobalEvent {
	result := make([]client.GlobalEvent, 0, len(events))
	seen := make(map[string][]byte)
	for _, event := range events {
		if event.SSEID == "" {
			result = append(result, event)
			continue
		}
		encoded, _ := json.Marshal(struct {
			Directory string
			Payload   client.Event
		}{event.Directory, event.Payload})
		if previous, found := seen[event.SSEID]; found {
			if !equalJSON(previous, encoded) {
				reasons.add("conflicting duplicate SSE event ID: " + event.SSEID)
			}
			continue
		}
		seen[event.SSEID] = encoded
		result = append(result, event)
	}
	return result
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

type reasonSet map[string]struct{}

func newReasonSet() *reasonSet {
	r := reasonSet{}
	return &r
}

func (r *reasonSet) add(reason string) {
	if reason != "" {
		(*r)[reason] = struct{}{}
	}
}

func (r *reasonSet) list() []string {
	result := make([]string, 0, len(*r))
	for reason := range *r {
		result = append(result, reason)
	}
	sort.Strings(result)
	return result
}

func addIncomplete(t *Trace, reason string) {
	t.TelemetryComplete = false
	t.IncompleteReasons = appendUnique(t.IncompleteReasons, reason)
	sort.Strings(t.IncompleteReasons)
}

// Recorder captures global SSE events while a run is active.
type Recorder struct {
	cancel context.CancelFunc
	stream *client.EventStream
	done   chan struct{}
	notify chan struct{}

	mu       sync.Mutex
	events   []client.GlobalEvent
	err      error
	stopping bool
}

// StartRecorder starts event capture without making a model request.
func StartRecorder(ctx context.Context, source EventSource) (*Recorder, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := source.OpenGlobalEvents(streamCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	r := &Recorder{cancel: cancel, stream: stream, done: make(chan struct{}), notify: make(chan struct{}, 1)}
	go r.read()
	return r, nil
}

func (r *Recorder) read() {
	defer close(r.done)
	for {
		event, err := r.stream.Next()
		if err != nil {
			r.mu.Lock()
			stopping := r.stopping
			if !stopping && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
				r.err = err
			}
			r.mu.Unlock()
			return
		}
		r.mu.Lock()
		r.events = append(r.events, event)
		r.mu.Unlock()
		select {
		case r.notify <- struct{}{}:
		default:
		}
	}
}

// Snapshot returns the events observed so far without stopping capture.
func (r *Recorder) Snapshot() []client.GlobalEvent {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]client.GlobalEvent(nil), r.events...)
}

// WaitForSessionCreated proves that the stream is delivering session events
// before the first model prompt is sent. A stream that closes or times out
// before the root creation is observed cannot support the isolation fence.
func (r *Recorder) WaitForSessionCreated(ctx context.Context, sessionID string) error {
	if r == nil {
		return errors.New("global event recorder is unavailable")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("session ID is required")
	}
	for {
		if observedRootCreation(r.Snapshot(), sessionID) {
			// Drain the immediate event burst that accompanies root creation before
			// the caller sends the first prompt. Without this short quiet barrier,
			// a second root already queued on the SSE connection can race the
			// admission snapshot. Later activity remains covered by the live
			// recorder and final watermark fence.
			quiet := time.NewTimer(rootAdmissionQuietPeriod)
			for {
				select {
				case <-ctx.Done():
					if !quiet.Stop() {
						<-quiet.C
					}
					return fmt.Errorf("wait for root session admission quiet period: %w", ctx.Err())
				case <-r.done:
					if !quiet.Stop() {
						<-quiet.C
					}
					r.mu.Lock()
					err := r.err
					r.mu.Unlock()
					if err == nil {
						err = errors.New("global event stream ended")
					}
					return fmt.Errorf("global event stream ended during root admission: %w", err)
				case <-r.notify:
					if !quiet.Stop() {
						select {
						case <-quiet.C:
						default:
						}
					}
					quiet.Reset(rootAdmissionQuietPeriod)
				case <-quiet.C:
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for root session event: %w", ctx.Err())
		case <-r.done:
			r.mu.Lock()
			err := r.err
			r.mu.Unlock()
			if err == nil {
				err = errors.New("global event stream ended")
			}
			return fmt.Errorf("root session creation was not observed: %w", err)
		case <-r.notify:
		}
	}
}

// PrepareForRuntimeStop marks the next stream termination as intentional
// without closing the stream. The runner calls this immediately before it
// shuts down the private server, so events remain observable until the server
// can no longer accept API requests.
func (r *Recorder) PrepareForRuntimeStop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stopping = true
	r.mu.Unlock()
}

// Stop ends capture and returns a stable copy of all observed events. It is
// safe to call repeatedly.
func (r *Recorder) Stop() ([]client.GlobalEvent, error) {
	r.mu.Lock()
	r.stopping = true
	r.mu.Unlock()
	r.cancel()
	_ = r.stream.Close()
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]client.GlobalEvent(nil), r.events...), r.err
}
