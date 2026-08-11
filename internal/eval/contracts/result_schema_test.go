package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// This guards the boundary between the executable Go contract and the
// published JSON Schema. The runner's representative provenance extensions
// must be accepted by both, while a legacy unnamespaced producer must be
// rejected by both.
func TestRunResultProvenanceExtensionsMatchPublishedSchema(t *testing.T) {
	t.Parallel()
	pattern := publishedProvenanceExtensionPattern(t)

	result := validRunResultForContractsTest()
	result.Provenance.Model = "openai/model"
	result.Provenance.Provider = "openai"
	result.Provenance.Extensions = map[string]string{
		"x-agent-bundle-digest":               testDigest(),
		ProvenanceExtensionBillingMode:        BillingModeChatGPTSubscription,
		"x-effective-agents-digest":           testDigest(),
		"x-effective-config-digest":           testDigest(),
		"x-effective-provider-catalog-digest": testDigest(),
		"x-effective-tool-catalog-digest":     testDigest(),
		"x-effective-tool-catalog-status":     "unobserved",
		"x-effective-tool-policy-digest":      testDigest(),
		"x-experiment-manifest-digest":        testDigest(),
		"x-harness-bundle-digest":             testDigest(),
		"x-observed-model":                    "model",
		"x-observed-provider":                 "provider",
		ProvenanceExtensionProviderAuthMode:   ProviderAuthModeOpenAIOAuthCleanProfileV1,
		ProvenanceExtensionCredentialBoundary: CredentialBoundaryRuntimeReadable,
		ProvenanceExtensionAuthIsolation:      AuthIsolationDedicatedFreshTokenFailStopV1,
		"x-redaction":                         "sanitized",
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("representative run result violates Go contract: %v", err)
	}
	assertMarshaledExtensionNames(t, result, pattern, true)

	result.Provenance.Extensions["effective_tool_policy_digest"] = testDigest()
	if err := result.Validate(); err == nil {
		t.Fatal("legacy unnamespaced producer output passed the Go contract")
	}
	assertMarshaledExtensionNames(t, result, pattern, false)
}

func publishedProvenanceExtensionPattern(t *testing.T) *regexp.Regexp {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "schemas", "eval-result.schema.json"))
	if err != nil {
		t.Fatalf("resolve published result schema: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read published result schema: %v", err)
	}
	var schema struct {
		Definitions struct {
			Provenance struct {
				Properties struct {
					Extensions struct {
						PropertyNames struct {
							Pattern string `json:"pattern"`
						} `json:"propertyNames"`
					} `json:"extensions"`
				} `json:"properties"`
			} `json:"provenance"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode published result schema: %v", err)
	}
	if schema.Definitions.Provenance.Properties.Extensions.PropertyNames.Pattern == "" {
		t.Fatal("published result schema has no provenance.extensions propertyNames pattern")
	}
	pattern, err := regexp.Compile(schema.Definitions.Provenance.Properties.Extensions.PropertyNames.Pattern)
	if err != nil {
		t.Fatalf("compile published provenance extension pattern: %v", err)
	}
	return pattern
}

func assertMarshaledExtensionNames(t *testing.T, result RunResult, pattern *regexp.Regexp, want bool) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal representative result: %v", err)
	}
	var instance struct {
		Provenance struct {
			Extensions map[string]string `json:"extensions"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(raw, &instance); err != nil {
		t.Fatalf("decode representative result: %v", err)
	}
	allMatch := true
	for key := range instance.Provenance.Extensions {
		allMatch = allMatch && pattern.MatchString(key)
	}
	if allMatch != want {
		t.Fatalf("published provenance extension namespace match = %v, want %v; extensions = %#v", allMatch, want, instance.Provenance.Extensions)
	}
}
