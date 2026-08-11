package baseline

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/joeldevz/skynex/internal/eval/contracts"
)

// EncodeRunResults validates every immutable repetition and rejects duplicate
// identities before making the baseline layer's opaque JSON samples.
func EncodeRunResults(results []contracts.RunResult) ([]json.RawMessage, error) {
	if len(results) == 0 {
		return nil, fmt.Errorf("at least one run result is required")
	}
	seenRunIDs := make(map[string]struct{}, len(results))
	seenRepetitions := make(map[string]struct{}, len(results))
	for i := range results {
		if err := results[i].Validate(); err != nil {
			return nil, fmt.Errorf("run result %d: %w", i, err)
		}
		if _, duplicate := seenRunIDs[results[i].RunID]; duplicate {
			return nil, fmt.Errorf("duplicate run_id %q", results[i].RunID)
		}
		seenRunIDs[results[i].RunID] = struct{}{}
		repetitionKey := fmt.Sprintf("%s\x00%s\x00%d", results[i].CaseID, results[i].Variant, results[i].Repetition)
		if _, duplicate := seenRepetitions[repetitionKey]; duplicate {
			return nil, fmt.Errorf("duplicate case/variant/repetition for %s/%s/%d", results[i].CaseID, results[i].Variant, results[i].Repetition)
		}
		seenRepetitions[repetitionKey] = struct{}{}
	}
	return EncodeSamples(results)
}

// DecodeRunResults restores and revalidates every retained repetition.
func DecodeRunResults(samples []json.RawMessage) ([]contracts.RunResult, error) {
	results := make([]contracts.RunResult, len(samples))
	for i, sample := range samples {
		decoded, err := contracts.DecodeRunResultJSON(sample)
		if err != nil {
			return nil, fmt.Errorf("sample %d: %w", i, err)
		}
		results[i] = decoded
	}
	// Reuse validation and uniqueness checks; its encoded bytes are intentionally
	// discarded because callers asked for typed results.
	if _, err := EncodeRunResults(results); err != nil {
		return nil, err
	}
	return results, nil
}

// NewRunArtifact is the contract-aware constructor used by baseline capture.
func NewRunArtifact(label, suite string, createdAt time.Time, fingerprint Fingerprint, results []contracts.RunResult, aggregates map[string]json.RawMessage) (*Artifact, error) {
	samples, err := EncodeRunResults(results)
	if err != nil {
		return nil, err
	}
	return NewArtifact(label, suite, createdAt, fingerprint, samples, aggregates)
}
