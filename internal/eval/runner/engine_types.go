package runner

import (
	"context"
	"net/http"
	"time"

	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/lifecycle"
	"github.com/joeldevz/skynex/internal/eval/metrics"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/toolpolicy"
	"github.com/joeldevz/skynex/internal/eval/trace"
)

// RuntimeAPI is the exact OpenCode surface required by a run. Tests provide a
// fake implementation; production uses one private lifecycle.Server per run.
type RuntimeAPI interface {
	trace.API
	trace.EventSource
	CreateSessionContext(context.Context, string) (*client.Session, error)
	SendMessageWithRequestContext(context.Context, string, client.SendMessageRequest) (*client.Response, error)
	GetMessageContext(context.Context, string, string) (*client.Message, error)
	GetToolIDsContext(context.Context) ([]string, error)
}

type RuntimeInfo struct {
	OpenCodeVersion       string
	OpenCodeAPI           string
	ConfigDigest          string
	AgentsDigest          string
	ToolPolicyDigest      string
	ToolCatalogDigest     string
	ToolsetDigest         string
	ProviderCatalogDigest string
	ProviderAuthMode      string
	BillingMode           string
	CredentialBoundary    string
	AuthIsolation         string
	ExecutionMode         contracts.ExecutionMode
	Network               contracts.NetworkPolicy
}

type Runtime interface {
	RuntimeAPI
	Info() RuntimeInfo
	PromptTools() map[string]bool
	Close() error
}

type RuntimeRequest struct {
	WorkspacePath string
	RunPath       string
	Case          contracts.Case
	ConfigRoot    string
	ToolPolicy    toolpolicy.Effective
}

type RuntimeFactory interface {
	Start(context.Context, RuntimeRequest) (Runtime, error)
}

type ProvenanceInputs struct {
	GitSHA           string
	OpenCodeVersion  string
	PromptDigest     string
	ConfigDigest     string
	ToolsetDigest    string
	ToolchainsDigest string
	JudgeDigest      string
	// Provider, when set, is an assertion that every selected case uses this
	// provider. The case's provider/model pair remains authoritative.
	Provider       string
	BundleDigest   string
	HarnessDigest  string
	ManifestDigest string
}

type EngineConfig struct {
	RunParent             string
	FixtureRoot           string
	AgentBundleRoot       string
	BundleDigest          string
	Factory               RuntimeFactory
	ExecutableClosure     *ExecutableClosure
	WorkflowPlugin        *toolpolicy.ControlledPluginIdentity
	Pricing               metrics.PricingTable
	Provenance            ProvenanceInputs
	SnapshotLimits        sandbox.SnapshotLimits
	TraceOptions          trace.Options
	EventReadinessTimeout time.Duration
	TraceDir              string
	Clock                 func() time.Time
	NewRunID              func() (string, error)
}

type RunRequest struct {
	Variant     string
	Repetition  int
	RetainTrace bool
}

// ContractResult retains every repetition. It intentionally does not expose a
// scalar score which could compensate for a failed hard check.
type ContractResult struct {
	Suite    string                `json:"suite"`
	Samples  []contracts.RunResult `json:"samples"`
	Started  time.Time             `json:"started_at"`
	Ended    time.Time             `json:"ended_at"`
	Complete bool                  `json:"complete"`
}

// OpenCodeFactory starts the real local OpenCode runtime. Credential names
// must be explicitly allowlisted; values are never copied into oracle env.
type OpenCodeFactory struct {
	Binary          string
	ExpectedVersion string
	EnvAllowlist    []string
	Env             map[string]string
	HTTPClient      *http.Client
	StartupTimeout  time.Duration
	AllowImpure     bool
	WorkflowPlugin  *toolpolicy.ControlledPluginIdentity
	// OpenAIOAuthFile is an explicit credential capability pointing to a
	// dedicated profile containing exactly one OpenAI OAuth entry.
	OpenAIOAuthFile    string
	OpenAIOAuthSession *lifecycle.OpenAIOAuthSession
}

type openCodeRuntime struct {
	server      *lifecycle.Server
	client      *client.Client
	info        RuntimeInfo
	promptTools map[string]bool
}

func (r *openCodeRuntime) Info() RuntimeInfo { return r.info }

func (r *openCodeRuntime) PromptTools() map[string]bool { return cloneBoolMap(r.promptTools) }

func (r *openCodeRuntime) Close() error { return r.server.Stop() }

func (r *openCodeRuntime) CreateSessionContext(ctx context.Context, title string) (*client.Session, error) {
	return r.client.CreateSessionContext(ctx, title)
}

func (r *openCodeRuntime) SendMessageWithRequestContext(ctx context.Context, sessionID string, input client.SendMessageRequest) (*client.Response, error) {
	return r.client.SendMessageWithRequestContext(ctx, sessionID, input)
}

func (r *openCodeRuntime) GetSessionContext(ctx context.Context, id string) (*client.Session, error) {
	return r.client.GetSessionContext(ctx, id)
}

func (r *openCodeRuntime) GetChildrenContext(ctx context.Context, id string) ([]client.Session, error) {
	return r.client.GetChildrenContext(ctx, id)
}

func (r *openCodeRuntime) GetMessagesContext(ctx context.Context, id string) ([]client.Message, error) {
	return r.client.GetMessagesContext(ctx, id)
}

func (r *openCodeRuntime) GetMessageContext(ctx context.Context, sessionID, messageID string) (*client.Message, error) {
	return r.client.GetMessageContext(ctx, sessionID, messageID)
}

func (r *openCodeRuntime) GetSessionStatusesContext(ctx context.Context) (map[string]client.SessionStatus, error) {
	return r.client.GetSessionStatusesContext(ctx)
}

func (r *openCodeRuntime) OpenGlobalEvents(ctx context.Context) (*client.EventStream, error) {
	return r.client.OpenGlobalEvents(ctx)
}

func (r *openCodeRuntime) GetToolIDsContext(ctx context.Context) ([]string, error) {
	return r.client.GetToolIDsContext(ctx)
}
