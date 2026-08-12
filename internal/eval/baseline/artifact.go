package baseline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	ArtifactSchemaVersion = 1
	ArtifactKind          = "skynex-eval-baseline"
)

type Integrity struct {
	Algorithm string `json:"algorithm"`
	Digest    string `json:"digest"`
}

// Artifact stores sanitized raw repetitions plus optional aggregate JSON. Raw
// samples are intentionally opaque here so evolving result contracts do not
// force the persistence and compatibility layer to depend on runner.
type Artifact struct {
	SchemaVersion     int                        `json:"schema_version"`
	Kind              string                     `json:"kind"`
	Label             string                     `json:"label"`
	Suite             string                     `json:"suite"`
	CreatedAt         string                     `json:"created_at"`
	Fingerprint       Fingerprint                `json:"fingerprint"`
	FingerprintDigest string                     `json:"fingerprint_digest"`
	Samples           []json.RawMessage          `json:"samples"`
	Aggregates        map[string]json.RawMessage `json:"aggregates,omitempty"`
	Integrity         Integrity                  `json:"integrity"`
}

type artifactPayload struct {
	SchemaVersion     int                        `json:"schema_version"`
	Kind              string                     `json:"kind"`
	Label             string                     `json:"label"`
	Suite             string                     `json:"suite"`
	CreatedAt         string                     `json:"created_at"`
	Fingerprint       Fingerprint                `json:"fingerprint"`
	FingerprintDigest string                     `json:"fingerprint_digest"`
	Samples           []json.RawMessage          `json:"samples"`
	Aggregates        map[string]json.RawMessage `json:"aggregates,omitempty"`
}

// NewArtifact canonicalizes every raw sample and seals the artifact with a
// payload digest. createdAt is supplied by the caller to keep tests and captures
// reproducible.
func NewArtifact(label, suite string, createdAt time.Time, fingerprint Fingerprint, samples []json.RawMessage, aggregates map[string]json.RawMessage) (*Artifact, error) {
	if strings.TrimSpace(label) == "" || strings.TrimSpace(suite) == "" {
		return nil, fmt.Errorf("baseline label and suite are required")
	}
	if createdAt.IsZero() {
		return nil, fmt.Errorf("baseline creation time is required")
	}
	fingerprintDigest, err := fingerprint.Digest()
	if err != nil {
		return nil, fmt.Errorf("fingerprint: %w", err)
	}
	canonicalSamples, err := canonicalRawSlice(samples)
	if err != nil {
		return nil, fmt.Errorf("samples: %w", err)
	}
	if len(canonicalSamples) == 0 {
		return nil, fmt.Errorf("baseline requires at least one sample")
	}
	canonicalAggregates, err := canonicalRawMap(aggregates)
	if err != nil {
		return nil, fmt.Errorf("aggregates: %w", err)
	}
	artifact := &Artifact{
		SchemaVersion:     ArtifactSchemaVersion,
		Kind:              ArtifactKind,
		Label:             label,
		Suite:             suite,
		CreatedAt:         createdAt.UTC().Format(time.RFC3339Nano),
		Fingerprint:       fingerprint,
		FingerprintDigest: fingerprintDigest,
		Samples:           canonicalSamples,
		Aggregates:        canonicalAggregates,
		Integrity:         Integrity{Algorithm: "sha256"},
	}
	artifact.Integrity.Digest, err = artifact.payloadDigest()
	if err != nil {
		return nil, err
	}
	return artifact, nil
}

// Save validates integrity immediately before the atomic write.
func (a *Artifact) Save(path string, options IOOptions) error {
	if err := a.Validate(); err != nil {
		return fmt.Errorf("validate baseline: %w", err)
	}
	return SaveJSON(path, a, options)
}

// Load reads a bounded strict artifact and verifies both fingerprint and payload
// digests before returning any scores to a comparator.
func Load(path string, options IOOptions) (*Artifact, error) {
	options.Strict = true
	var artifact Artifact
	if err := LoadJSON(path, &artifact, options); err != nil {
		return nil, err
	}
	if err := artifact.Validate(); err != nil {
		return nil, fmt.Errorf("validate baseline: %w", err)
	}
	return &artifact, nil
}

func (a *Artifact) Validate() error {
	if a == nil {
		return fmt.Errorf("baseline is nil")
	}
	if a.SchemaVersion != ArtifactSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", a.SchemaVersion)
	}
	if a.Kind != ArtifactKind {
		return fmt.Errorf("unexpected artifact kind %q", a.Kind)
	}
	if strings.TrimSpace(a.Label) == "" || strings.TrimSpace(a.Suite) == "" {
		return fmt.Errorf("label and suite are required")
	}
	if _, err := time.Parse(time.RFC3339Nano, a.CreatedAt); err != nil {
		return fmt.Errorf("invalid created_at: %w", err)
	}
	fingerprintDigest, err := a.Fingerprint.Digest()
	if err != nil {
		return fmt.Errorf("fingerprint: %w", err)
	}
	if fingerprintDigest != a.FingerprintDigest {
		return fmt.Errorf("fingerprint digest mismatch")
	}
	if len(a.Samples) == 0 {
		return fmt.Errorf("baseline requires at least one sample")
	}
	canonicalSamples, err := canonicalRawSlice(a.Samples)
	if err != nil {
		return fmt.Errorf("samples: %w", err)
	}
	canonicalAggregates, err := canonicalRawMap(a.Aggregates)
	if err != nil {
		return fmt.Errorf("aggregates: %w", err)
	}
	// Digest canonicalized data even if imported JSON used different whitespace or
	// object-key order.
	copyArtifact := *a
	copyArtifact.Samples = canonicalSamples
	copyArtifact.Aggregates = canonicalAggregates
	digest, err := copyArtifact.payloadDigest()
	if err != nil {
		return err
	}
	if a.Integrity.Algorithm != "sha256" || digest != a.Integrity.Digest {
		return fmt.Errorf("artifact integrity mismatch")
	}
	return nil
}

// EncodeSamples converts typed run results to canonical opaque samples.
func EncodeSamples[T any](values []T) ([]json.RawMessage, error) {
	result := make([]json.RawMessage, 0, len(values))
	for i := range values {
		encoded, err := CanonicalJSON(values[i])
		if err != nil {
			return nil, fmt.Errorf("sample %d: %w", i, err)
		}
		result = append(result, json.RawMessage(encoded))
	}
	return result, nil
}

// DecodeSamples strictly decodes each retained sample into a caller-owned
// contract type.
func DecodeSamples[T any](samples []json.RawMessage) ([]T, error) {
	result := make([]T, len(samples))
	for i, sample := range samples {
		decoder := json.NewDecoder(bytes.NewReader(sample))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&result[i]); err != nil {
			return nil, fmt.Errorf("sample %d: %w", i, err)
		}
	}
	return result, nil
}

func (a Artifact) payloadDigest() (string, error) {
	payload := artifactPayload{
		SchemaVersion: a.SchemaVersion, Kind: a.Kind, Label: a.Label, Suite: a.Suite,
		CreatedAt: a.CreatedAt, Fingerprint: a.Fingerprint, FingerprintDigest: a.FingerprintDigest,
		Samples: a.Samples, Aggregates: a.Aggregates,
	}
	data, err := CanonicalJSON(payload)
	if err != nil {
		return "", fmt.Errorf("marshal artifact payload: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func canonicalRawSlice(values []json.RawMessage) ([]json.RawMessage, error) {
	result := make([]json.RawMessage, len(values))
	for i, value := range values {
		canonical, err := canonicalRaw(value)
		if err != nil {
			return nil, fmt.Errorf("sample %d: %w", i, err)
		}
		result[i] = canonical
	}
	return result, nil
}

func canonicalRawMap(values map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if len(values) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make(map[string]json.RawMessage, len(values))
	for _, key := range keys {
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("aggregate key is empty")
		}
		canonical, err := canonicalRaw(values[key])
		if err != nil {
			return nil, fmt.Errorf("aggregate %q: %w", key, err)
		}
		result[key] = canonical
	}
	return result, nil
}

func canonicalRaw(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 {
		return nil, fmt.Errorf("empty JSON")
	}
	if err := rejectDuplicateKeys(value); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if _, object := decoded.(map[string]any); !object {
		return nil, fmt.Errorf("JSON value must be an object")
	}
	canonical, err := CanonicalJSON(decoded)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}
