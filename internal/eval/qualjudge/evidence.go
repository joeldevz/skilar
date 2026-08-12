package qualjudge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/joeldevz/skynex/internal/eval/redact"
)

type EvidenceKind string

const (
	EvidenceFixture     EvidenceKind = "fixture"
	EvidenceAgent       EvidenceKind = "agent"
	EvidenceTool        EvidenceKind = "tool"
	EvidenceObservation EvidenceKind = "observation"
)

// EvidenceItem intentionally carries no run-side, configuration-side, or
// score label. Text is always treated as untrusted data regardless of Kind.
type EvidenceItem struct {
	Kind EvidenceKind `json:"kind"`
	Text string       `json:"text"`
}

type EvidenceOptions struct {
	MaxBytes     int
	KnownSecrets []string
}

// SanitizedEvidence is a bounded capability type. Its contents are private so
// Evaluate cannot accidentally receive raw evidence through a struct literal.
type SanitizedEvidence struct {
	items       []EvidenceItem
	digest      string
	findings    []redact.Finding
	byteSize    int
	initialized bool
}

// NewSanitizedEvidence bounds and redacts raw excerpts before they can be put
// in a Request. Evaluate applies a second defensive redaction pass.
func NewSanitizedEvidence(items []EvidenceItem, options EvidenceOptions) (SanitizedEvidence, error) {
	limit := options.MaxBytes
	if limit == 0 {
		limit = DefaultMaxEvidenceBytes
	}
	if limit < 0 || limit > AbsoluteMaxEvidenceBytes {
		return SanitizedEvidence{}, fmt.Errorf("%w: evidence byte limit is outside the allowed range", ErrInvalidRequest)
	}
	if len(items) == 0 {
		return SanitizedEvidence{}, fmt.Errorf("%w: at least one evidence excerpt is required", ErrInvalidRequest)
	}

	redactor := redact.New(limit, options.KnownSecrets...)
	result := make([]EvidenceItem, 0, len(items))
	counts := make(map[string]int)
	total := 0
	for _, item := range items {
		if !validEvidenceKind(item.Kind) {
			return SanitizedEvidence{}, fmt.Errorf("%w: unsupported evidence kind", ErrInvalidRequest)
		}
		if item.Text == "" || !utf8.ValidString(item.Text) {
			return SanitizedEvidence{}, fmt.Errorf("%w: evidence text must be non-empty UTF-8", ErrInvalidRequest)
		}
		total += len(item.Kind) + len(item.Text)
		if total > limit {
			return SanitizedEvidence{}, fmt.Errorf("%w: evidence exceeds its byte limit", ErrInvalidRequest)
		}
		redacted, err := redactor.Text(item.Text)
		if err != nil {
			return SanitizedEvidence{}, fmt.Errorf("%w: evidence redaction failed", ErrInvalidRequest)
		}
		for _, finding := range redacted.Findings {
			counts[finding.Kind] += finding.Count
		}
		result = append(result, EvidenceItem{Kind: item.Kind, Text: redacted.Text})
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return SanitizedEvidence{}, fmt.Errorf("%w: evidence encoding failed", ErrInvalidRequest)
	}
	if len(encoded) > limit {
		return SanitizedEvidence{}, fmt.Errorf("%w: redacted evidence exceeds its byte limit", ErrInvalidRequest)
	}
	return SanitizedEvidence{
		items:       result,
		digest:      digestBytes(encoded),
		findings:    findingsFromCounts(counts),
		byteSize:    len(encoded),
		initialized: true,
	}, nil
}

func (e SanitizedEvidence) Digest() string { return e.digest }

func (e SanitizedEvidence) ByteSize() int { return e.byteSize }

func (e SanitizedEvidence) Findings() []redact.Finding {
	return append([]redact.Finding(nil), e.findings...)
}

func validEvidenceKind(kind EvidenceKind) bool {
	switch kind {
	case EvidenceFixture, EvidenceAgent, EvidenceTool, EvidenceObservation:
		return true
	default:
		return false
	}
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func findingsFromCounts(counts map[string]int) []redact.Finding {
	kinds := make([]string, 0, len(counts))
	for kind, count := range counts {
		if count > 0 {
			kinds = append(kinds, kind)
		}
	}
	sort.Strings(kinds)
	result := make([]redact.Finding, 0, len(kinds))
	for _, kind := range kinds {
		result = append(result, redact.Finding{Kind: kind, Count: counts[kind]})
	}
	return result
}

func mergeFindings(groups ...[]redact.Finding) []redact.Finding {
	counts := make(map[string]int)
	for _, group := range groups {
		for _, finding := range group {
			counts[finding.Kind] += finding.Count
		}
	}
	return findingsFromCounts(counts)
}

func validateBlindTerms(terms []string) error {
	if len(terms) > 32 {
		return fmt.Errorf("%w: too many blind terms", ErrInvalidRequest)
	}
	for _, term := range terms {
		if term == "" || len(term) > 128 || !utf8.ValidString(term) || strings.TrimSpace(term) != term {
			return fmt.Errorf("%w: invalid blind term", ErrInvalidRequest)
		}
		for _, char := range term {
			if char < 0x20 || char > 0x7e {
				return fmt.Errorf("%w: blind terms must be printable ASCII", ErrInvalidRequest)
			}
		}
	}
	return nil
}

var standardBlindTerms = []string{"candidate", "control", "baseline", "variant", "treatment"}

var standardLabeledScore = regexp.MustCompile(`(?i)\b(?:candidate|control|baseline|variant|treatment)[ \t]+(?:score|rating)[ \t]*(?::|=)?[ \t]*[-+]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)%?`)

func blind(value string, extra []string) string {
	value = standardLabeledScore.ReplaceAllString(value, "[BLINDED_SCORE]")
	for _, term := range standardBlindTerms {
		value = replaceASCIIFoldAll(value, term, "[BLINDED]")
	}
	for _, term := range extra {
		value = replaceTokenFold(value, term, "[BLINDED]")
	}
	return value
}

func replaceASCIIFoldAll(value, term, replacement string) string {
	if value == "" || term == "" {
		return value
	}
	lowerValue := asciiLower(value)
	lowerTerm := asciiLower(term)
	var output strings.Builder
	start := 0
	for {
		relative := strings.Index(lowerValue[start:], lowerTerm)
		if relative < 0 {
			output.WriteString(value[start:])
			return output.String()
		}
		index := start + relative
		output.WriteString(value[start:index])
		output.WriteString(replacement)
		start = index + len(term)
	}
}

// replaceTokenFold replaces an ASCII label case-insensitively only when it is
// not embedded in a larger identifier. This supports short A/B labels without
// destroying every matching letter in ordinary prose.
func replaceTokenFold(value, term, replacement string) string {
	if value == "" || term == "" {
		return value
	}
	lowerValue := asciiLower(value)
	lowerTerm := asciiLower(term)
	var output strings.Builder
	start := 0
	for {
		relative := strings.Index(lowerValue[start:], lowerTerm)
		if relative < 0 {
			output.WriteString(value[start:])
			break
		}
		index := start + relative
		end := index + len(term)
		if (index == 0 || !identifierByte(value[index-1])) && (end == len(value) || !identifierByte(value[end])) {
			output.WriteString(value[start:index])
			output.WriteString(replacement)
			start = end
			continue
		}
		output.WriteString(value[start:end])
		start = end
	}
	return output.String()
}

func asciiLower(value string) string {
	buffer := []byte(value)
	for index, char := range buffer {
		if char >= 'A' && char <= 'Z' {
			buffer[index] = char + ('a' - 'A')
		}
	}
	return string(buffer)
}

func identifierByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '_'
}
