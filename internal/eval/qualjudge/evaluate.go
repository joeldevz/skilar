package qualjudge

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/joeldevz/skynex/internal/eval/redact"
)

type modelCallResult struct {
	response CompletionResponse
	err      error
}

// Evaluate obtains one optional qualitative opinion. It never executes
// deterministic checks and never receives their verdict or an experiment-side
// label. Callers add the returned opinion only after deterministic evaluation.
func Evaluate(ctx context.Context, model Model, request Request) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: context is nil", ErrInvalidRequest)
	}
	if nilModel(model) {
		return Result{}, fmt.Errorf("%w: model is nil", ErrInvalidRequest)
	}
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	identity := model.Identity()
	if err := validateIdentity(identity); err != nil {
		return Result{}, err
	}

	// Evidence is already sanitized, but this second pass is intentional: it
	// protects against future constructor changes and keeps the final model
	// boundary defense-in-depth.
	boundaryRedactor := redact.New(AbsoluteMaxEvidenceBytes)
	rubric, err := boundaryRedactor.Text(request.Rubric)
	if err != nil {
		return Result{}, fmt.Errorf("%w: rubric redaction failed", ErrInvalidRequest)
	}
	evidence := make([]EvidenceItem, len(request.Evidence.items))
	inputFindings := mergeFindings(request.Evidence.findings, rubric.Findings)
	for index, item := range request.Evidence.items {
		redacted, redactErr := boundaryRedactor.Text(item.Text)
		if redactErr != nil {
			return Result{}, fmt.Errorf("%w: evidence redaction failed", ErrInvalidRequest)
		}
		inputFindings = mergeFindings(inputFindings, redacted.Findings)
		evidence[index] = EvidenceItem{Kind: item.Kind, Text: redacted.Text}
	}

	completion, promptDigest, err := buildPrompt(rubric.Text, evidence, request.BlindTerms)
	if err != nil {
		return Result{}, err
	}
	completion.MaxOutputBytes = outputLimit(request.MaxOutputBytes)
	modelDigest, err := digestIdentity(identity)
	if err != nil {
		return Result{}, fmt.Errorf("%w: model identity encoding failed", ErrInvalidRequest)
	}

	timeout := request.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	callContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	responses := make(chan modelCallResult, 1)
	go func() {
		call := modelCallResult{}
		defer func() {
			if recover() != nil {
				call = modelCallResult{err: fmt.Errorf("model adapter panicked")}
			}
			responses <- call
		}()
		call.response, call.err = model.Complete(callContext, completion)
	}()

	var call modelCallResult
	select {
	case <-callContext.Done():
		return Result{}, fmt.Errorf("%w: %w", ErrModel, callContext.Err())
	case call = <-responses:
	}
	if callContext.Err() != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrModel, callContext.Err())
	}
	if call.err != nil {
		redactedError, redactErr := redact.New(4 << 10).Text(call.err.Error())
		if redactErr != nil {
			return Result{}, fmt.Errorf("%w", ErrModel)
		}
		return Result{}, fmt.Errorf("%w: %s", ErrModel, redactedError.Text)
	}

	parsed, err := parseOutput(call.response.Output, completion.MaxOutputBytes)
	if err != nil {
		return Result{}, err
	}
	outputRedactor := redact.New(MaxRationaleBytes)
	redactedRationale, err := outputRedactor.Text(*parsed.Rationale)
	if err != nil || len(redactedRationale.Text) > MaxRationaleBytes {
		return Result{}, fmt.Errorf("%w: rationale redaction failed", ErrInvalidOutput)
	}
	redactedRationale.Text = blind(redactedRationale.Text, request.BlindTerms)
	if len(redactedRationale.Text) > MaxRationaleBytes {
		return Result{}, fmt.Errorf("%w: filtered rationale exceeds its byte limit", ErrInvalidOutput)
	}

	return Result{
		Verdict:          parsed.Verdict,
		Score:            *parsed.Score,
		Confidence:       *parsed.Confidence,
		Rationale:        redactedRationale.Text,
		Provider:         identity.Provider,
		Model:            identity.Name,
		ModelRevision:    identity.Revision,
		PromptVersion:    PromptVersion,
		PromptDigest:     promptDigest,
		ModelDigest:      modelDigest,
		EvidenceDigest:   request.Evidence.digest,
		InputRedactions:  inputFindings,
		OutputRedactions: redactedRationale.Findings,
	}, nil
}

func validateRequest(request Request) error {
	if !request.Evidence.initialized || len(request.Evidence.items) == 0 || request.Evidence.digest == "" || request.Evidence.byteSize <= 0 || request.Evidence.byteSize > AbsoluteMaxEvidenceBytes {
		return fmt.Errorf("%w: evidence was not created by NewSanitizedEvidence", ErrInvalidRequest)
	}
	if strings.TrimSpace(request.Rubric) == "" || len(request.Rubric) > MaxRubricBytes || !utf8.ValidString(request.Rubric) || containsUnsafeControl(request.Rubric) {
		return fmt.Errorf("%w: rubric is invalid", ErrInvalidRequest)
	}
	if err := validateBlindTerms(request.BlindTerms); err != nil {
		return err
	}
	if request.Timeout < 0 || request.Timeout > MaxTimeout {
		return fmt.Errorf("%w: timeout is outside the allowed range", ErrInvalidRequest)
	}
	if request.MaxOutputBytes < 0 || request.MaxOutputBytes > AbsoluteMaxOutputBytes {
		return fmt.Errorf("%w: output byte limit is outside the allowed range", ErrInvalidRequest)
	}
	return nil
}

func validateIdentity(identity ModelIdentity) error {
	if !safeIdentityPart(identity.Provider, 128, false) || !safeIdentityPart(identity.Name, 256, false) || !safeIdentityPart(identity.Revision, 256, true) {
		return fmt.Errorf("%w: model identity is invalid", ErrInvalidRequest)
	}
	return nil
}

func safeIdentityPart(value string, max int, optional bool) bool {
	if value == "" {
		return optional
	}
	return len(value) <= max && utf8.ValidString(value) && strings.TrimSpace(value) == value && !containsUnsafeControl(value)
}

func digestIdentity(identity ModelIdentity) (string, error) {
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}

func outputLimit(value int) int {
	if value == 0 {
		return DefaultMaxOutputBytes
	}
	return value
}

func nilModel(model Model) bool {
	if model == nil {
		return true
	}
	value := reflect.ValueOf(model)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
