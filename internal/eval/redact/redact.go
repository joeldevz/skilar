// Package redact removes common credential forms before evaluator evidence is
// persisted or sent to an optional qualitative judge. Redaction is a safety
// layer, not a reason to expose ambient credentials to an evaluated process.
package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

const DefaultMaxBytes = 32 << 20

type Finding struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type Result struct {
	Text     string    `json:"text"`
	Findings []Finding `json:"findings,omitempty"`
}

type Redactor struct {
	maxBytes int
	known    []string
}

// New creates a redactor. knownSecrets are literal sensitive values obtained
// by the control plane; values shorter than eight bytes are ignored to avoid
// destroying ordinary prose.
func New(maxBytes int, knownSecrets ...string) *Redactor {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	seen := make(map[string]struct{})
	known := make([]string, 0, len(knownSecrets))
	for _, secret := range knownSecrets {
		if len(secret) < 8 {
			continue
		}
		if _, duplicate := seen[secret]; duplicate {
			continue
		}
		seen[secret] = struct{}{}
		known = append(known, secret)
	}
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })
	return &Redactor{maxBytes: maxBytes, known: known}
}

func (r *Redactor) Text(input string) (Result, error) {
	if r == nil {
		r = New(0)
	}
	if len(input) > r.maxBytes {
		return Result{}, fmt.Errorf("redaction input exceeds %d bytes", r.maxBytes)
	}
	counts := make(map[string]int)
	output := input
	for _, secret := range r.known {
		occurrences := strings.Count(output, secret)
		if occurrences == 0 {
			continue
		}
		output = strings.ReplaceAll(output, secret, marker("known"))
		counts["known"] += occurrences
	}
	for _, rule := range textRules {
		output = rule.expression.ReplaceAllStringFunc(output, func(match string) string {
			// Keep existing markers stable when Text is called more than once.
			if strings.Contains(match, "[REDACTED:") {
				return match
			}
			counts[rule.kind]++
			if rule.keepPrefix {
				location := rule.expression.FindStringSubmatchIndex(match)
				if len(location) >= 4 && location[2] >= 0 {
					return match[location[2]:location[3]] + marker(rule.kind)
				}
			}
			return marker(rule.kind)
		})
	}
	return Result{Text: output, Findings: findings(counts)}, nil
}

// JSON decodes one bounded JSON value with number fidelity, redacts sensitive
// key values and strings recursively, and returns canonical compact JSON.
func (r *Redactor) JSON(input []byte) ([]byte, []Finding, error) {
	if r == nil {
		r = New(0)
	}
	if len(input) > r.maxBytes {
		return nil, nil, fmt.Errorf("redaction input exceeds %d bytes", r.maxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, fmt.Errorf("decode JSON for redaction: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, nil, fmt.Errorf("multiple JSON values are not allowed")
		}
		return nil, nil, fmt.Errorf("decode trailing JSON: %w", err)
	}
	counts := make(map[string]int)
	redacted, err := r.redactValue(value, counts, 0)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return nil, nil, fmt.Errorf("encode redacted JSON: %w", err)
	}
	return encoded, findings(counts), nil
}

func (r *Redactor) redactValue(value any, counts map[string]int, depth int) (any, error) {
	if depth > 128 {
		return nil, fmt.Errorf("JSON redaction exceeds depth 128")
	}
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			redactedKey, err := r.Text(key)
			if err != nil {
				return nil, err
			}
			for _, finding := range redactedKey.Findings {
				counts[finding.Kind] += finding.Count
			}
			if _, collision := result[redactedKey.Text]; collision {
				return nil, fmt.Errorf("JSON redaction produced a duplicate object key")
			}
			if isSensitiveKey(key) && child != nil {
				result[redactedKey.Text] = marker("sensitive-key")
				counts["sensitive-key"]++
				continue
			}
			redacted, err := r.redactValue(child, counts, depth+1)
			if err != nil {
				return nil, err
			}
			result[redactedKey.Text] = redacted
		}
		return result, nil
	case []any:
		result := make([]any, len(typed))
		for i, child := range typed {
			redacted, err := r.redactValue(child, counts, depth+1)
			if err != nil {
				return nil, err
			}
			result[i] = redacted
		}
		return result, nil
	case string:
		redacted, err := r.Text(typed)
		if err != nil {
			return nil, err
		}
		for _, finding := range redacted.Findings {
			counts[finding.Kind] += finding.Count
		}
		return redacted.Text, nil
	default:
		return value, nil
	}
}

type rule struct {
	kind       string
	expression *regexp.Regexp
	keepPrefix bool
}

var textRules = []rule{
	{kind: "pem-private-key", expression: regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----.*?-----END (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`)},
	{kind: "bearer", expression: regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/=-]{8,}`)},
	{kind: "openai-key", expression: regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{16,}\b`)},
	{kind: "github-token", expression: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)},
	{kind: "aws-access-key", expression: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{kind: "credential-url", expression: regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s/@:]+:[^\s/@]+@`)},
	{kind: "assignment", expression: regexp.MustCompile(`(?i)(\b(?:api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|password|passwd|secret|access|refresh|account[_-]?id|enterprise[_-]?url)\b[ \t]*[:=][ \t]*)(?:["']?)[^\s,"']{8,}(?:["']?)`), keepPrefix: true},
}

// sensitiveKeyNames contains both conventional credential fields and the
// fields used by OpenCode's OpenAI OAuth record.  The latter are deliberately
// redacted even when they are identifiers or URLs rather than bearer tokens:
// persisting a raw auth record must fail safe as a whole.
var sensitiveKeyNames = []string{
	"apikey",
	"accesstoken",
	"authtoken",
	"clientsecret",
	"password",
	"passwd",
	"secret",
	"privatekey",
	"accountid",
	"enterpriseurl",
}

var sensitiveExactKeyNames = map[string]struct{}{
	"access":  {},
	"refresh": {},
}

func isSensitiveKey(key string) bool {
	normalized := normalizeKey(key)
	if _, ok := sensitiveExactKeyNames[normalized]; ok {
		return true
	}
	for _, suffix := range sensitiveKeyNames {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

func normalizeKey(key string) string {
	var builder strings.Builder
	builder.Grow(len(key))
	for _, character := range key {
		switch {
		case character >= 'A' && character <= 'Z':
			builder.WriteByte(byte(character + ('a' - 'A')))
		case character >= 'a' && character <= 'z':
			builder.WriteByte(byte(character))
		case character >= '0' && character <= '9':
			builder.WriteByte(byte(character))
		}
	}
	return builder.String()
}

func marker(kind string) string { return "[REDACTED:" + kind + "]" }

func findings(counts map[string]int) []Finding {
	kinds := make([]string, 0, len(counts))
	for kind, count := range counts {
		if count > 0 {
			kinds = append(kinds, kind)
		}
	}
	sort.Strings(kinds)
	result := make([]Finding, 0, len(kinds))
	for _, kind := range kinds {
		result = append(result, Finding{Kind: kind, Count: counts[kind]})
	}
	return result
}
