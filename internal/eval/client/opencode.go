// Package client implements the subset of the OpenCode HTTP API used by the
// evaluator.  The wire types intentionally retain IDs and usage records: the
// evaluator must be able to prove which session, message, part, and tool call
// contributed to a result.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL        = "http://127.0.0.1:4096"
	defaultRequestTimeout = 180 * time.Second
	defaultMaxBodyBytes   = int64(32 << 20)
	defaultMaxEventBytes  = 8 << 20
	messagePageLimit      = 50
	maxMessagePages       = 256
	maxMessagesPerSession = 10_000
	maxMessageCursorBytes = 4 << 10
)

// ErrMessageListGetFailed is the deliberately detail-free boundary error for
// paginated message retrieval. Transport bodies, URLs, session IDs, and opaque
// cursors must not cross into retained evaluator diagnostics.
var ErrMessageListGetFailed = errors.New("message list retrieval failed")

// CacheTokenInfo is the cache token breakdown emitted by OpenCode.
type CacheTokenInfo struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

// TokenInfo holds token usage information. CacheRead and CacheWrite are kept
// as compatibility fields for the existing metrics adapter; Cache is the
// current OpenCode wire representation.
type TokenInfo struct {
	Total      int            `json:"total,omitempty"`
	Input      int            `json:"input"`
	Output     int            `json:"output"`
	Reasoning  int            `json:"reasoning"`
	Cache      CacheTokenInfo `json:"cache"`
	CacheRead  int            `json:"cache_read,omitempty"`
	CacheWrite int            `json:"cache_write,omitempty"`

	// Present distinguishes a real all-zero usage record from an omitted one.
	Present bool `json:"-"`
}

// UnmarshalJSON accepts both the current nested cache object and the legacy
// flattened cache fields.
func (t *TokenInfo) UnmarshalJSON(data []byte) error {
	type tokenWire struct {
		Total      *int            `json:"total"`
		Input      *int            `json:"input"`
		Output     *int            `json:"output"`
		Reasoning  *int            `json:"reasoning"`
		Cache      *CacheTokenInfo `json:"cache"`
		CacheRead  *int            `json:"cache_read"`
		CacheWrite *int            `json:"cache_write"`
	}
	var wire tokenWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*t = TokenInfo{Present: true}
	if wire.Input != nil {
		t.Input = *wire.Input
	}
	if wire.Output != nil {
		t.Output = *wire.Output
	}
	if wire.Reasoning != nil {
		t.Reasoning = *wire.Reasoning
	}
	if wire.Cache != nil {
		t.Cache = *wire.Cache
		t.CacheRead = wire.Cache.Read
		t.CacheWrite = wire.Cache.Write
	}
	if wire.CacheRead != nil {
		t.CacheRead = *wire.CacheRead
		t.Cache.Read = *wire.CacheRead
	}
	if wire.CacheWrite != nil {
		t.CacheWrite = *wire.CacheWrite
		t.Cache.Write = *wire.CacheWrite
	}
	if wire.Total != nil {
		t.Total = *wire.Total
	} else {
		// OpenCode does not emit a total. Cache is a breakdown of input usage,
		// so adding it again would double count those tokens.
		t.Total = t.Input + t.Output + t.Reasoning
	}
	return nil
}

// MarshalJSON always writes the current OpenCode cache representation while
// retaining total for evaluator artifacts.
func (t TokenInfo) MarshalJSON() ([]byte, error) {
	cache := t.Cache
	if cache.Read == 0 && t.CacheRead != 0 {
		cache.Read = t.CacheRead
	}
	if cache.Write == 0 && t.CacheWrite != 0 {
		cache.Write = t.CacheWrite
	}
	type tokenWire struct {
		Total     int            `json:"total,omitempty"`
		Input     int            `json:"input"`
		Output    int            `json:"output"`
		Reasoning int            `json:"reasoning"`
		Cache     CacheTokenInfo `json:"cache"`
	}
	return json.Marshal(tokenWire{
		Total: t.Total, Input: t.Input, Output: t.Output,
		Reasoning: t.Reasoning, Cache: cache,
	})
}

// MessageTime contains millisecond Unix timestamps from OpenCode.
type MessageTime struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed,omitempty"`
}

// MessagePath identifies the filesystem view used for an assistant message.
type MessagePath struct {
	CWD  string `json:"cwd"`
	Root string `json:"root"`
}

// ErrorInfo preserves provider and runtime error payloads without depending on
// a particular provider's schema.
type ErrorInfo struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ResponseInfo is the union of OpenCode user and assistant message metadata.
// Legacy evaluator fields remain available so callers can migrate gradually.
type ResponseInfo struct {
	ID         string      `json:"id"`
	SessionID  string      `json:"sessionID"`
	Role       string      `json:"role"`
	ParentID   string      `json:"parentID,omitempty"`
	Time       MessageTime `json:"time"`
	Error      *ErrorInfo  `json:"error,omitempty"`
	ModelID    string      `json:"modelID,omitempty"`
	ProviderID string      `json:"providerID,omitempty"`
	Mode       string      `json:"mode,omitempty"`
	Path       MessagePath `json:"path,omitempty"`
	Summary    bool        `json:"summary,omitempty"`
	Tokens     TokenInfo   `json:"tokens,omitempty"`
	Cost       float64     `json:"cost,omitempty"`
	Finish     string      `json:"finish,omitempty"`
	Agent      string      `json:"agent,omitempty"`

	// Duration is derived from Time for compatibility with the old evaluator.
	Duration time.Duration `json:"-"`
}

func (i *ResponseInfo) UnmarshalJSON(data []byte) error {
	type alias ResponseInfo
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*i = ResponseInfo(decoded)
	if i.Time.Completed >= i.Time.Created && i.Time.Created != 0 {
		i.Duration = time.Duration(i.Time.Completed-i.Time.Created) * time.Millisecond
	}
	return nil
}

// PartTime covers timestamps used by text, reasoning, retry, and tool parts.
type PartTime struct {
	Created   int64 `json:"created,omitempty"`
	Start     int64 `json:"start,omitempty"`
	End       int64 `json:"end,omitempty"`
	Compacted int64 `json:"compacted,omitempty"`
}

// ToolState is the complete state machine returned for an OpenCode tool part.
type ToolState struct {
	Status      string          `json:"status"`
	Input       json.RawMessage `json:"input,omitempty"`
	Raw         string          `json:"raw,omitempty"`
	Output      string          `json:"output,omitempty"`
	Error       string          `json:"error,omitempty"`
	Title       string          `json:"title,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Time        PartTime        `json:"time,omitempty"`
	Attachments json.RawMessage `json:"attachments,omitempty"`
}

// Part represents every durable message part relevant to evaluation. Unknown
// fields remain in Raw so API additions do not silently disappear from traces.
type Part struct {
	ID        string `json:"id,omitempty"`
	SessionID string `json:"sessionID,omitempty"`
	MessageID string `json:"messageID,omitempty"`
	Type      string `json:"type"`

	Text      string          `json:"text,omitempty"`
	Synthetic bool            `json:"synthetic,omitempty"`
	Ignored   bool            `json:"ignored,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Time      PartTime        `json:"time,omitempty"`

	CallID string    `json:"callID,omitempty"`
	Tool   string    `json:"tool,omitempty"`
	State  ToolState `json:"state,omitempty"`

	Reason   string     `json:"reason,omitempty"`
	Snapshot string     `json:"snapshot,omitempty"`
	Cost     float64    `json:"cost,omitempty"`
	Tokens   TokenInfo  `json:"tokens,omitempty"`
	Attempt  int        `json:"attempt,omitempty"`
	Error    *ErrorInfo `json:"error,omitempty"`
	Auto     bool       `json:"auto,omitempty"`

	Name        string `json:"name,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	Description string `json:"description,omitempty"`
	Agent       string `json:"agent,omitempty"`

	// Compatibility projections used by the existing runner and metrics code.
	ToolInput  json.RawMessage `json:"-"`
	ToolOutput string          `json:"-"`
	ToolError  string          `json:"-"`

	Raw json.RawMessage `json:"-"`
}

func (p *Part) UnmarshalJSON(data []byte) error {
	type alias Part
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = Part(decoded)
	p.ToolInput = cloneRaw(p.State.Input)
	p.ToolOutput = p.State.Output
	p.ToolError = p.State.Error
	p.Raw = cloneRaw(data)
	return nil
}

// MarshalJSON emits only fields valid for the concrete part. In particular,
// a text prompt must not acquire zero-valued state/time/token objects that a
// strict OpenCode request validator can reject.
func (p Part) MarshalJSON() ([]byte, error) {
	payload := make(map[string]any)
	if len(p.Raw) != 0 {
		_ = json.Unmarshal(p.Raw, &payload)
	}
	payload["type"] = p.Type
	putString(payload, "id", p.ID)
	putString(payload, "sessionID", p.SessionID)
	putString(payload, "messageID", p.MessageID)
	putString(payload, "text", p.Text)
	putString(payload, "callID", p.CallID)
	putString(payload, "tool", p.Tool)
	putString(payload, "reason", p.Reason)
	putString(payload, "snapshot", p.Snapshot)
	putString(payload, "name", p.Name)
	putString(payload, "prompt", p.Prompt)
	putString(payload, "description", p.Description)
	putString(payload, "agent", p.Agent)
	if p.Synthetic {
		payload["synthetic"] = true
	}
	if p.Ignored {
		payload["ignored"] = true
	}
	if len(p.Metadata) != 0 {
		payload["metadata"] = p.Metadata
	}
	if hasPartTime(p.Time) {
		payload["time"] = p.Time
	}
	if p.Type == "tool" || p.State.Status != "" {
		state := p.State
		if len(state.Input) == 0 && len(p.ToolInput) != 0 {
			state.Input = p.ToolInput
		}
		payload["state"] = state
	}
	if p.Type == "step-finish" || p.Tokens.Present {
		payload["cost"] = p.Cost
		payload["tokens"] = p.Tokens
	}
	if p.Type == "retry" {
		payload["attempt"] = p.Attempt
	}
	if p.Error != nil {
		payload["error"] = p.Error
	}
	if p.Type == "compaction" {
		payload["auto"] = p.Auto
	}
	return json.Marshal(payload)
}

// SessionTime contains OpenCode's millisecond session timestamps.
type SessionTime struct {
	Created    int64 `json:"created"`
	Updated    int64 `json:"updated"`
	Compacting int64 `json:"compacting,omitempty"`
}

// Session represents an OpenCode session and its parent relationship.
type Session struct {
	ID        string      `json:"id"`
	ProjectID string      `json:"projectID,omitempty"`
	Directory string      `json:"directory,omitempty"`
	ParentID  string      `json:"parentID,omitempty"`
	Title     string      `json:"title,omitempty"`
	Version   string      `json:"version,omitempty"`
	Time      SessionTime `json:"time"`

	// CreatedAt is retained for compatibility and derived from Time.Created.
	CreatedAt time.Time `json:"-"`
}

func (s *Session) UnmarshalJSON(data []byte) error {
	type alias Session
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*s = Session(decoded)
	if s.Time.Created != 0 {
		s.CreatedAt = time.UnixMilli(s.Time.Created).UTC()
	}
	return nil
}

// Message contains message metadata and its ordered durable parts.
type Message struct {
	Info  ResponseInfo `json:"info"`
	Parts []Part       `json:"parts"`
}

// Response is the synchronous message response returned by OpenCode.
type Response = Message

// SessionStatus is one of idle, busy, or retry. An absent entry in the
// /session/status map means idle in the current OpenCode implementation.
type SessionStatus struct {
	Type    string `json:"type"`
	Attempt int    `json:"attempt,omitempty"`
	Message string `json:"message,omitempty"`
	Next    int64  `json:"next,omitempty"`
}

// HealthInfo is returned by /global/health.
type HealthInfo struct {
	Healthy bool   `json:"healthy"`
	Version string `json:"version"`
}

// ProviderCatalog retains only identifiers needed for a no-model preflight.
// Provider keys, headers, and opaque options returned by OpenCode are
// deliberately not represented and therefore cannot be persisted by callers.
// SHA256 commits to this sanitized view, never to secret-bearing raw JSON.
type ProviderCatalog struct {
	All       []ProviderSummary `json:"all"`
	Default   map[string]string `json:"default"`
	Connected []string          `json:"connected"`
	SHA256    string            `json:"sha256,omitempty"`
}

type ProviderSummary struct {
	ID     string                  `json:"id"`
	Models map[string]ModelSummary `json:"models"`
}

type ModelSummary struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

// MCPStatus is the non-secret state exposed by GET /mcp in the pinned
// OpenCode API. Failure details are deliberately discarded by the client.
type MCPStatus string

const (
	MCPStatusConnected               MCPStatus = "connected"
	MCPStatusDisabled                MCPStatus = "disabled"
	MCPStatusFailed                  MCPStatus = "failed"
	MCPStatusNeedsAuth               MCPStatus = "needs_auth"
	MCPStatusNeedsClientRegistration MCPStatus = "needs_client_registration"
)

// MCPStatusCatalog is a sanitized view of GET /mcp. SHA256 commits only to
// the canonical name-to-status map and never to server-provided error text.
type MCPStatusCatalog struct {
	Statuses map[string]MCPStatus `json:"statuses"`
	SHA256   string               `json:"sha256"`
}

// RawDocument is a bounded endpoint capture together with its content digest.
// It is used by compatibility probes without interpreting rapidly changing
// configuration, agent, or OpenAPI schemas.
type RawDocument struct {
	ContentType string          `json:"content_type,omitempty"`
	Body        json.RawMessage `json:"body"`
	SHA256      string          `json:"sha256"`
}

// Event preserves an OpenCode event's discriminant and payload.
type Event struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// GlobalEvent is the envelope emitted by /global/event.
type GlobalEvent struct {
	Directory  string    `json:"directory"`
	Payload    Event     `json:"payload"`
	ReceivedAt time.Time `json:"received_at"`
	SSEID      string    `json:"sse_id,omitempty"`
}

// Config configures an OpenCode client.
type Config struct {
	BaseURL        string
	Directory      string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	MaxBodyBytes   int64
	MaxEventBytes  int
	Username       string
	Password       string
}

// Client wraps HTTP calls to opencode serve.
type Client struct {
	baseURL        string
	directory      string
	httpClient     *http.Client
	requestTimeout time.Duration
	maxBodyBytes   int64
	maxEventBytes  int
	username       string
	password       string
}

// New creates a client with explicit configuration.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.MaxEventBytes <= 0 {
		cfg.MaxEventBytes = defaultMaxEventBytes
	}
	return &Client{
		baseURL:        strings.TrimRight(cfg.BaseURL, "/"),
		directory:      cfg.Directory,
		httpClient:     cfg.HTTPClient,
		requestTimeout: cfg.RequestTimeout,
		maxBodyBytes:   cfg.MaxBodyBytes,
		maxEventBytes:  cfg.MaxEventBytes,
		username:       cfg.Username,
		password:       cfg.Password,
	}
}

// NewClient creates a new Client with default configuration.
func NewClient() *Client { return New(Config{}) }

// NewClientWithBaseURL creates a new Client with a custom base URL.
func NewClientWithBaseURL(baseURL string) *Client { return New(Config{BaseURL: baseURL}) }

// CreateSession creates a root session with the given title.
func (c *Client) CreateSession(title string) (*Session, error) {
	ctx, cancel := c.requestContext(context.Background())
	defer cancel()
	return c.CreateSessionContext(ctx, title)
}

// CreateSessionContext creates a root session with the given title.
func (c *Client) CreateSessionContext(ctx context.Context, title string) (*Session, error) {
	return c.CreateSessionWithRequestContext(ctx, CreateSessionRequest{Title: title})
}

// CreateSessionRequest is the current POST /session body.
type CreateSessionRequest struct {
	ParentID string `json:"parentID,omitempty"`
	Title    string `json:"title,omitempty"`
}

// CreateSessionWithRequestContext creates a root or child session.
func (c *Client) CreateSessionWithRequestContext(ctx context.Context, input CreateSessionRequest) (*Session, error) {
	var session Session
	if err := c.doJSON(ctx, http.MethodPost, "/session", input, &session, http.StatusOK, http.StatusCreated); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &session, nil
}

// SendMessage sends a message to a session and waits for the response.
func (c *Client) SendMessage(sessionID, agent string, parts []Part) (*Response, error) {
	ctx, cancel := c.requestContext(context.Background())
	defer cancel()
	return c.SendMessageContext(ctx, sessionID, agent, parts)
}

// SendMessageRequest is the supported subset of POST /session/:id/message.
type SendMessageRequest struct {
	MessageID string          `json:"messageID,omitempty"`
	Model     *ModelSelection `json:"model,omitempty"`
	Agent     string          `json:"agent,omitempty"`
	NoReply   bool            `json:"noReply,omitempty"`
	System    string          `json:"system,omitempty"`
	Tools     map[string]bool `json:"tools,omitempty"`
	Parts     []Part          `json:"parts"`
}

// ModelSelection is the exact provider/model pair accepted by OpenCode's
// message endpoint. Model IDs may themselves contain slashes.
type ModelSelection struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// SendMessageContext sends the legacy evaluator request shape to the current
// singular message endpoint.
func (c *Client) SendMessageContext(ctx context.Context, sessionID, agent string, parts []Part) (*Response, error) {
	return c.SendMessageWithRequestContext(ctx, sessionID, SendMessageRequest{Agent: agent, Parts: parts})
}

// SendMessageWithRequestContext sends a fully specified prompt request.
func (c *Client) SendMessageWithRequestContext(ctx context.Context, sessionID string, input SendMessageRequest) (*Response, error) {
	var response Response
	path := "/session/" + url.PathEscape(sessionID) + "/message"
	if err := c.doJSON(ctx, http.MethodPost, path, input, &response, http.StatusOK); err != nil {
		return nil, fmt.Errorf("send message for session %q: %w", sessionID, err)
	}
	return &response, nil
}

// GetSessionContext retrieves a session by ID.
func (c *Client) GetSessionContext(ctx context.Context, sessionID string) (*Session, error) {
	var session Session
	path := "/session/" + url.PathEscape(sessionID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &session, http.StatusOK); err != nil {
		return nil, fmt.Errorf("get session %q: %w", sessionID, err)
	}
	return &session, nil
}

// GetChildrenContext returns the direct children of a session.
func (c *Client) GetChildrenContext(ctx context.Context, sessionID string) ([]Session, error) {
	var sessions []Session
	path := "/session/" + url.PathEscape(sessionID) + "/children"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &sessions, http.StatusOK); err != nil {
		return nil, fmt.Errorf("get children for session %q: %w", sessionID, err)
	}
	return sessions, nil
}

// GetMessages retrieves all messages for a session.
func (c *Client) GetMessages(sessionID string) ([]Message, error) {
	ctx, cancel := c.requestContext(context.Background())
	defer cancel()
	return c.GetMessagesContext(ctx, sessionID)
}

// GetMessagesContext retrieves all messages for a session.
func (c *Client) GetMessagesContext(ctx context.Context, sessionID string) ([]Message, error) {
	basePath := "/session/" + url.PathEscape(sessionID) + "/message"
	pages := make([][]Message, 0, 1)
	seenCursors := make(map[string]struct{})
	var before string
	var bodyBytes int64
	totalMessages := 0

	for pageNumber := 0; ; pageNumber++ {
		if pageNumber >= maxMessagePages {
			return nil, ErrMessageListGetFailed
		}
		remainingBytes := c.maxBodyBytes - bodyBytes
		if remainingBytes <= 0 {
			return nil, ErrMessageListGetFailed
		}

		query := make(url.Values, 2)
		query.Set("limit", fmt.Sprintf("%d", messagePageLimit))
		if before != "" {
			query.Set("before", before)
		}
		path := basePath + "?" + query.Encode()
		page, header, size, err := c.getMessagesPageContext(ctx, path, remainingBytes)
		if err != nil {
			return nil, safeMessageListError(err)
		}
		bodyBytes += size
		if len(page) > messagePageLimit {
			return nil, ErrMessageListGetFailed
		}
		if totalMessages > maxMessagesPerSession-len(page) {
			return nil, ErrMessageListGetFailed
		}
		totalMessages += len(page)
		pages = append(pages, page)

		cursorValues := header.Values("X-Next-Cursor")
		if len(cursorValues) == 0 {
			break
		}
		if len(cursorValues) != 1 || !validMessageCursor(cursorValues[0]) {
			return nil, ErrMessageListGetFailed
		}
		if len(page) == 0 {
			return nil, ErrMessageListGetFailed
		}
		before = cursorValues[0]
		if _, duplicate := seenCursors[before]; duplicate {
			return nil, ErrMessageListGetFailed
		}
		seenCursors[before] = struct{}{}
	}

	// OpenCode serves the newest page first, while each page is chronological.
	// Reversing page order reconstructs the endpoint's unpaginated chronology.
	messages := make([]Message, 0, totalMessages)
	for index := len(pages) - 1; index >= 0; index-- {
		messages = append(messages, pages[index]...)
	}
	return messages, nil
}

func safeMessageListError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return errors.Join(ErrMessageListGetFailed, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return errors.Join(ErrMessageListGetFailed, context.DeadlineExceeded)
	default:
		return ErrMessageListGetFailed
	}
}

func (c *Client) getMessagesPageContext(ctx context.Context, path string, maxBodyBytes int64) ([]Message, http.Header, int64, error) {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, nil, 0, &HTTPError{Method: http.MethodGet, URL: req.URL.String(), StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(responseBody))}
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read response: %w", err)
	}
	if int64(len(responseBody)) > maxBodyBytes {
		return nil, nil, 0, fmt.Errorf("aggregate response body exceeds %d bytes", c.maxBodyBytes)
	}
	trimmed := bytes.TrimSpace(responseBody)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, nil, 0, errors.New("decode response: expected message array")
	}
	var messages []Message
	if err := json.Unmarshal(trimmed, &messages); err != nil {
		return nil, nil, 0, fmt.Errorf("decode response: %w", err)
	}
	return messages, resp.Header.Clone(), int64(len(responseBody)), nil
}

func validMessageCursor(cursor string) bool {
	if cursor == "" || len(cursor) > maxMessageCursorBytes || strings.TrimSpace(cursor) != cursor {
		return false
	}
	for index := 0; index < len(cursor); index++ {
		if cursor[index] < 0x21 || cursor[index] == 0x7f {
			return false
		}
	}
	return true
}

// GetMessageContext retrieves one message by its durable identity. The
// directed endpoint is used to anchor a synchronous prompt response before
// the complete message listing is reconciled independently.
func (c *Client) GetMessageContext(ctx context.Context, sessionID, messageID string) (*Message, error) {
	var message Message
	path := "/session/" + url.PathEscape(sessionID) + "/message/" + url.PathEscape(messageID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &message, http.StatusOK); err != nil {
		return nil, fmt.Errorf("get message for session %q: %w", sessionID, err)
	}
	return &message, nil
}

// GetSessionStatusesContext returns active session statuses. A session absent
// from this map is idle.
func (c *Client) GetSessionStatusesContext(ctx context.Context) (map[string]SessionStatus, error) {
	statuses := make(map[string]SessionStatus)
	if err := c.doJSON(ctx, http.MethodGet, "/session/status", nil, &statuses, http.StatusOK); err != nil {
		return nil, fmt.Errorf("get session statuses: %w", err)
	}
	return statuses, nil
}

// GetToolIDsContext returns the complete effective tool catalogue. The runner
// expands it into a fail-closed prompt map so tools introduced by a plugin or
// future OpenCode release cannot inherit ambient authority.
func (c *Client) GetToolIDsContext(ctx context.Context) ([]string, error) {
	document, err := c.GetToolIDsDocumentContext(ctx)
	if err != nil {
		return nil, err
	}
	var tools []string
	if err := json.Unmarshal(document.Body, &tools); err != nil {
		return nil, fmt.Errorf("decode tool IDs: %w", err)
	}
	return tools, nil
}

// GetToolIDsDocumentContext captures the exact effective tool catalogue. The
// raw digest lets doctor and the runner prove that policy binding used the
// same catalogue observed during compatibility probing.
func (c *Client) GetToolIDsDocumentContext(ctx context.Context) (RawDocument, error) {
	document, err := c.getRawDocument(ctx, "/experimental/tool/ids")
	if err != nil {
		return RawDocument{}, fmt.Errorf("get tool IDs: %w", err)
	}
	return document, nil
}

// GetMCPStatusCatalogContext initializes the configured MCP catalog through
// OpenCode's read-only status endpoint. In pinned OpenCode 1.18.16 a
// connected status is published only after initialize and listTools have
// succeeded and the returned definitions have been cached.
func (c *Client) GetMCPStatusCatalogContext(ctx context.Context) (MCPStatusCatalog, error) {
	document, err := c.getRawDocument(ctx, "/mcp")
	if err != nil {
		return MCPStatusCatalog{}, fmt.Errorf("get MCP status catalog: %w", err)
	}
	statuses, err := decodeMCPStatusCatalog(document.Body)
	if err != nil {
		return MCPStatusCatalog{}, fmt.Errorf("%w: %v", ErrInvalidMCPStatusCatalog, err)
	}
	safeJSON, err := json.Marshal(statuses)
	if err != nil {
		return MCPStatusCatalog{}, fmt.Errorf("%w: encode sanitized status catalog: %v", ErrInvalidMCPStatusCatalog, err)
	}
	sum := sha256.Sum256(safeJSON)
	return MCPStatusCatalog{
		Statuses: statuses,
		SHA256:   fmt.Sprintf("sha256:%x", sum[:]),
	}, nil
}

func decodeMCPStatusCatalog(raw json.RawMessage) (map[string]MCPStatus, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode status catalog")
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("status catalog must be an object")
	}
	statuses := make(map[string]MCPStatus)
	for entry := 0; decoder.More(); entry++ {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode status catalog entry %d", entry)
		}
		name, ok := token.(string)
		if !ok || !validMCPServerName(name) {
			return nil, fmt.Errorf("status catalog entry %d has an invalid server name", entry)
		}
		if _, duplicate := statuses[name]; duplicate {
			return nil, fmt.Errorf("duplicate MCP server %q", name)
		}
		status, err := decodeMCPStatus(decoder)
		if err != nil {
			return nil, fmt.Errorf("MCP server %q: %w", name, err)
		}
		statuses[name] = status
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, fmt.Errorf("decode status catalog closing object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("status catalog contains trailing JSON")
	}
	return statuses, nil
}

func decodeMCPStatus(decoder *json.Decoder) (MCPStatus, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", fmt.Errorf("decode status object")
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return "", fmt.Errorf("status must be an object")
	}
	var status MCPStatus
	seenStatus := false
	seenError := false
	seenFields := make(map[string]struct{}, 2)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", fmt.Errorf("decode status field")
		}
		field, ok := token.(string)
		if !ok {
			return "", fmt.Errorf("status contains a non-string field")
		}
		if _, duplicate := seenFields[field]; duplicate {
			return "", fmt.Errorf("status contains a duplicate field")
		}
		seenFields[field] = struct{}{}
		switch field {
		case "status":
			if err := decoder.Decode(&status); err != nil {
				return "", fmt.Errorf("status discriminator must be a string")
			}
			seenStatus = true
		case "error":
			var discarded string
			if err := decoder.Decode(&discarded); err != nil {
				return "", fmt.Errorf("error detail must be a string")
			}
			seenError = true
		default:
			return "", fmt.Errorf("status contains an unknown field")
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return "", fmt.Errorf("decode status closing object")
	}
	if !seenStatus {
		return "", fmt.Errorf("status discriminator is missing")
	}
	switch status {
	case MCPStatusConnected, MCPStatusDisabled, MCPStatusNeedsAuth:
		if seenError {
			return "", fmt.Errorf("status has an unexpected error detail")
		}
	case MCPStatusFailed, MCPStatusNeedsClientRegistration:
		if !seenError {
			return "", fmt.Errorf("status is missing its required error detail")
		}
	default:
		return "", fmt.Errorf("status discriminator is unsupported")
	}
	return status, nil
}

func validMCPServerName(name string) bool {
	if len(name) == 0 || len(name) > 128 || !asciiAlphaNumeric(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !asciiAlphaNumeric(name[i]) && !strings.ContainsRune("_.:-", rune(name[i])) {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func requireJSONEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected end of JSON")
	}
	return nil
}

// GetProviderCatalogContext probes configured providers and models without
// creating a session or issuing a completion. The returned type intentionally
// drops credential-bearing provider fields.
func (c *Client) GetProviderCatalogContext(ctx context.Context) (ProviderCatalog, error) {
	document, err := c.getRawDocument(ctx, "/provider")
	if err != nil {
		return ProviderCatalog{}, fmt.Errorf("get provider catalog: %w", err)
	}
	var catalog ProviderCatalog
	if err := json.Unmarshal(document.Body, &catalog); err != nil {
		return ProviderCatalog{}, fmt.Errorf("%w: decode provider catalog: %v", ErrInvalidProviderCatalog, err)
	}
	if err := canonicalizeProviderCatalog(&catalog); err != nil {
		return ProviderCatalog{}, fmt.Errorf("%w: %v", ErrInvalidProviderCatalog, err)
	}
	safeJSON, err := json.Marshal(struct {
		All       []ProviderSummary `json:"all"`
		Default   map[string]string `json:"default"`
		Connected []string          `json:"connected"`
	}{All: catalog.All, Default: catalog.Default, Connected: catalog.Connected})
	if err != nil {
		return ProviderCatalog{}, fmt.Errorf("%w: encode sanitized provider catalog: %v", ErrInvalidProviderCatalog, err)
	}
	sum := sha256.Sum256(safeJSON)
	catalog.SHA256 = fmt.Sprintf("sha256:%x", sum[:])
	return catalog, nil
}

func canonicalizeProviderCatalog(catalog *ProviderCatalog) error {
	seenProviders := make(map[string]struct{}, len(catalog.All))
	for i := range catalog.All {
		provider := &catalog.All[i]
		if strings.TrimSpace(provider.ID) == "" {
			return fmt.Errorf("provider id is empty")
		}
		if _, duplicate := seenProviders[provider.ID]; duplicate {
			return fmt.Errorf("duplicate provider id %q", provider.ID)
		}
		seenProviders[provider.ID] = struct{}{}
		for key, model := range provider.Models {
			if strings.TrimSpace(key) == "" || strings.TrimSpace(model.ID) == "" {
				return fmt.Errorf("provider %q contains an empty model id", provider.ID)
			}
			if key != model.ID {
				return fmt.Errorf("provider %q model key %q differs from id %q", provider.ID, key, model.ID)
			}
			if model.ProviderID != provider.ID {
				return fmt.Errorf("provider %q model %q claims provider %q", provider.ID, key, model.ProviderID)
			}
		}
	}
	sort.Slice(catalog.All, func(i, j int) bool { return catalog.All[i].ID < catalog.All[j].ID })
	seenConnected := make(map[string]struct{}, len(catalog.Connected))
	for _, id := range catalog.Connected {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("connected provider id is empty")
		}
		if _, duplicate := seenConnected[id]; duplicate {
			return fmt.Errorf("duplicate connected provider id %q", id)
		}
		seenConnected[id] = struct{}{}
		if _, exists := seenProviders[id]; !exists {
			return fmt.Errorf("connected provider %q is absent from catalog", id)
		}
	}
	sort.Strings(catalog.Connected)
	for providerID, modelID := range catalog.Default {
		providerIndex := sort.Search(len(catalog.All), func(index int) bool { return catalog.All[index].ID >= providerID })
		if providerIndex == len(catalog.All) || catalog.All[providerIndex].ID != providerID {
			return fmt.Errorf("default provider %q is absent from catalog", providerID)
		}
		if _, exists := catalog.All[providerIndex].Models[modelID]; !exists {
			return fmt.Errorf("default model %q is absent from provider %q", modelID, providerID)
		}
	}
	return nil
}

// GetPathContext captures the server's effective path view.
func (c *Client) GetPathContext(ctx context.Context) (RawDocument, error) {
	return c.getRawDocument(ctx, "/path")
}

// GetConfigContext captures the server's effective merged configuration.
func (c *Client) GetConfigContext(ctx context.Context) (RawDocument, error) {
	return c.getRawDocument(ctx, "/config")
}

// GetAgentsContext captures the effective agent catalogue.
func (c *Client) GetAgentsContext(ctx context.Context) (RawDocument, error) {
	return c.getRawDocument(ctx, "/agent")
}

// GetOpenAPIDocumentContext captures the exact OpenAPI document served at
// /doc. The digest can be used as an API compatibility fingerprint.
func (c *Client) GetOpenAPIDocumentContext(ctx context.Context) (RawDocument, error) {
	return c.getRawDocument(ctx, "/doc")
}

// Health preserves the old bool API while requiring the versioned health
// payload used by the evaluator's compatibility diagnostics.
func (c *Client) Health() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := c.HealthInfoContext(ctx)
	if err != nil {
		return false, err
	}
	return info.Healthy, nil
}

// HealthInfoContext returns health and the exact OpenCode server version.
func (c *Client) HealthInfoContext(ctx context.Context) (HealthInfo, error) {
	var info HealthInfo
	if err := c.doJSON(ctx, http.MethodGet, "/global/health", nil, &info, http.StatusOK); err != nil {
		return HealthInfo{}, fmt.Errorf("health check: %w", err)
	}
	if !info.Healthy {
		return info, errors.New("health check reported unhealthy")
	}
	if strings.TrimSpace(info.Version) == "" {
		return info, errors.New("health response is missing version")
	}
	return info, nil
}

// HTTPError is returned for non-successful OpenCode responses.
type HTTPError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s returned status %d: %s", e.Method, e.URL, e.StatusCode, e.Body)
}

func (c *Client) doJSON(ctx context.Context, method, path string, input, output any, statuses ...int) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if !containsStatus(statuses, resp.StatusCode) {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &HTTPError{Method: method, URL: req.URL.String(), StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(responseBody))}
	}
	if output == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if int64(len(responseBody)) > c.maxBodyBytes {
		return fmt.Errorf("response body exceeds %d bytes", c.maxBodyBytes)
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) getRawDocument(ctx context.Context, path string) (RawDocument, error) {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return RawDocument{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return RawDocument{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return RawDocument{}, &HTTPError{Method: http.MethodGet, URL: req.URL.String(), StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(responseBody))}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBodyBytes+1))
	if err != nil {
		return RawDocument{}, fmt.Errorf("read %s response: %w", path, err)
	}
	if int64(len(body)) > c.maxBodyBytes {
		return RawDocument{}, fmt.Errorf("read %s response: body exceeds %d bytes", path, c.maxBodyBytes)
	}
	sum := sha256.Sum256(body)
	return RawDocument{
		ContentType: resp.Header.Get("Content-Type"),
		Body:        append(json.RawMessage(nil), body...),
		SHA256:      fmt.Sprintf("sha256:%x", sum[:]),
	}, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(c.baseURL + path)
	if err != nil {
		return nil, fmt.Errorf("parse request URL: %w", err)
	}
	if c.directory != "" {
		query := u.Query()
		query.Set("directory", c.directory)
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	return req, nil
}

func (c *Client) requestContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, c.requestTimeout)
}

func containsStatus(statuses []int, status int) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// EventStream is a bounded SSE reader for /global/event.
type EventStream struct {
	response *http.Response
	scanner  *bufio.Scanner
	maxBytes int
	nextMu   sync.Mutex
	stateMu  sync.Mutex
	closed   bool
}

// OpenGlobalEvents opens the current global SSE event stream. The caller must
// close the stream; cancelling ctx also closes the underlying response body.
func (c *Client) OpenGlobalEvents(ctx context.Context) (*EventStream, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/global/event", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open global event stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, &HTTPError{Method: http.MethodGet, URL: req.URL.String(), StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(responseBody))}
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), c.maxEventBytes)
	return &EventStream{response: resp, scanner: scanner, maxBytes: c.maxEventBytes}, nil
}

// Next blocks until the next complete SSE data record arrives.
func (s *EventStream) Next() (GlobalEvent, error) {
	s.nextMu.Lock()
	defer s.nextMu.Unlock()
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return GlobalEvent{}, io.EOF
	}
	s.stateMu.Unlock()
	var dataLines []string
	dataBytes := 0
	var eventID string
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" {
			if len(dataLines) == 0 {
				continue
			}
			return decodeGlobalEvent(strings.Join(dataLines, "\n"), eventID)
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			if len(dataLines) != 0 {
				dataBytes++ // newline inserted between adjacent data fields
			}
			dataBytes += len(value)
			if dataBytes > s.maxBytes {
				return GlobalEvent{}, fmt.Errorf("read global event stream: event exceeds %d bytes", s.maxBytes)
			}
			dataLines = append(dataLines, value)
		case "id":
			eventID = value
		}
	}
	if err := s.scanner.Err(); err != nil {
		return GlobalEvent{}, fmt.Errorf("read global event stream: %w", err)
	}
	if len(dataLines) != 0 {
		return decodeGlobalEvent(strings.Join(dataLines, "\n"), eventID)
	}
	return GlobalEvent{}, io.EOF
}

func decodeGlobalEvent(data, eventID string) (GlobalEvent, error) {
	var event GlobalEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return GlobalEvent{}, fmt.Errorf("decode global event: %w", err)
	}
	if event.Payload.Type == "" {
		return GlobalEvent{}, errors.New("decode global event: payload type is empty")
	}
	event.ReceivedAt = time.Now().UTC()
	event.SSEID = eventID
	return event, nil
}

// Close closes the event response body. It is safe to call more than once.
func (s *EventStream) Close() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.response.Body.Close()
}

// ExtractText concatenates all text parts from a slice of parts.
func ExtractText(parts []Part) string {
	var result strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}

func putString(payload map[string]any, key, value string) {
	if value != "" {
		payload[key] = value
	}
}

func hasPartTime(value PartTime) bool {
	return value.Created != 0 || value.Start != 0 || value.End != 0 || value.Compacted != 0
}
