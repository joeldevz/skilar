// Package qualjudge provides an optional, blinded qualitative opinion over
// evidence that has already passed through the deterministic evaluation
// pipeline. It deliberately contains no model-provider or tool integration.
package qualjudge

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/joeldevz/skynex/internal/eval/redact"
)

const (
	PromptVersion = "skynex-qualjudge-v1"

	DefaultMaxEvidenceBytes  = 64 << 10
	AbsoluteMaxEvidenceBytes = 256 << 10
	DefaultMaxOutputBytes    = 16 << 10
	AbsoluteMaxOutputBytes   = 64 << 10
	DefaultTimeout           = 30 * time.Second
	MaxTimeout               = 10 * time.Minute
	MaxRubricBytes           = 16 << 10
	MaxRationaleBytes        = 8 << 10
)

var (
	ErrInvalidRequest = errors.New("invalid qualitative judge request")
	ErrInvalidOutput  = errors.New("invalid qualitative judge output")
	ErrModel          = errors.New("qualitative judge model failed")
)

// Model is the entire provider boundary. CompletionRequest has no tool field,
// so implementations can only submit a text-only, strict-schema request.
// Implementations must honor context cancellation and must not add tools or
// ambient evaluator state to the provider request.
type Model interface {
	Identity() ModelIdentity
	Complete(context.Context, CompletionRequest) (CompletionResponse, error)
}

// ModelIdentity is recorded as provenance. Name should be a pinned provider
// model identifier; Revision can carry an additional immutable revision when
// the provider exposes one.
type ModelIdentity struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Revision string `json:"revision,omitempty"`
}

// CompletionRequest intentionally has no tools, credentials, variant label,
// baseline score, or deterministic verdict fields.
type CompletionRequest struct {
	SystemPrompt   string          `json:"system_prompt"`
	Input          string          `json:"input"`
	ResponseSchema json.RawMessage `json:"response_schema"`
	MaxOutputBytes int             `json:"max_output_bytes"`
}

type CompletionResponse struct {
	Output []byte
}

type Verdict string

const (
	VerdictPass         Verdict = "pass"
	VerdictFail         Verdict = "fail"
	VerdictInconclusive Verdict = "inconclusive"
)

// Request accepts only a SanitizedEvidence value created by
// NewSanitizedEvidence. BlindTerms contains configuration labels that must not
// reach the model; common experiment labels are always blinded as well.
type Request struct {
	Rubric         string
	Evidence       SanitizedEvidence
	BlindTerms     []string
	Timeout        time.Duration
	MaxOutputBytes int
}

type Result struct {
	Verdict       Verdict `json:"verdict"`
	Score         float64 `json:"score"`
	Confidence    float64 `json:"confidence"`
	Rationale     string  `json:"rationale"`
	Provider      string  `json:"provider"`
	Model         string  `json:"model"`
	ModelRevision string  `json:"model_revision,omitempty"`

	PromptVersion  string `json:"prompt_version"`
	PromptDigest   string `json:"prompt_digest"`
	ModelDigest    string `json:"model_digest"`
	EvidenceDigest string `json:"evidence_digest"`

	InputRedactions  []redact.Finding `json:"input_redactions,omitempty"`
	OutputRedactions []redact.Finding `json:"output_redactions,omitempty"`
}
