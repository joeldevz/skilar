package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTextRedactsKnownAndStructuredSecretsBeforePersistence(t *testing.T) {
	known := "literal-super-secret-value"
	input := strings.Join([]string{
		"Authorization: Bearer abcdefghijklmnop",
		"OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz",
		"password=hunter-credential-123",
		"https://alice:correct-horse@example.test/repo",
		known,
	}, "\n")
	result, err := New(0, known).Text(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"abcdefghijklmnop", "abcdefghijklmnopqrstuvwxyz", "hunter-credential-123", "correct-horse", known} {
		if strings.Contains(result.Text, forbidden) {
			t.Fatalf("secret %q remained in %q", forbidden, result.Text)
		}
	}
	if len(result.Findings) < 4 {
		t.Fatalf("missing findings: %+v", result.Findings)
	}
	second, err := New(0, known).Text(result.Text)
	if err != nil {
		t.Fatal(err)
	}
	if second.Text != result.Text {
		t.Fatalf("redaction is not idempotent:\n%s\n%s", result.Text, second.Text)
	}
}

func TestJSONRedactsSensitiveKeysAndNestedText(t *testing.T) {
	input := []byte(`{"token":"Bearer abcdefghijklmnop","nested":{"client_secret":"do-not-persist-this","safe":42}}`)
	output, findings, err := New(0).JSON(input)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if strings.Contains(text, "abcdefghijklmnop") || strings.Contains(text, "do-not-persist-this") {
		t.Fatalf("secret persisted: %s", text)
	}
	if !strings.Contains(text, "[REDACTED:") || len(findings) == 0 {
		t.Fatalf("redaction evidence missing: %s %+v", text, findings)
	}
}

func TestRedactionInputIsBounded(t *testing.T) {
	if _, err := New(4).Text("12345"); err == nil {
		t.Fatal("expected bounded-input error")
	}
}

func TestJSONRedactsOpenAIOAuthRecordAndKnownCanaries(t *testing.T) {
	const canary = "oauth-canary-must-never-persist-7c18164a"
	input, err := json.Marshal(map[string]any{
		"openai": map[string]any{
			"type":          "oauth",
			"access":        "access-token-value-must-not-persist",
			"refresh":       "refresh-token-value-must-not-persist",
			"accountId":     "account-identifier-must-not-persist",
			"enterpriseUrl": "https://tenant.internal.example.test",
			"expires":       4_102_444_800_000,
			"metadata": map[string]any{
				"nested_canary": canary,
				"keys": map[string]any{
					canary: "safe-value",
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	output, findings, err := New(0, canary).JSON(input)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, forbidden := range []string{
		"access-token-value-must-not-persist",
		"refresh-token-value-must-not-persist",
		"account-identifier-must-not-persist",
		"tenant.internal.example.test",
		canary,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("OAuth material %q persisted in %s", forbidden, text)
		}
	}
	for _, safe := range []string{`"type":"oauth"`, `"expires":4102444800000`} {
		if !strings.Contains(text, safe) {
			t.Fatalf("non-secret OAuth metadata %s was unexpectedly removed: %s", safe, text)
		}
	}
	if countFinding(findings, "sensitive-key") != 4 || countFinding(findings, "known") != 2 {
		t.Fatalf("unexpected OAuth findings: %+v", findings)
	}
	if second, _, secondErr := New(0, canary).JSON(output); secondErr != nil || string(second) != text {
		t.Fatalf("OAuth JSON redaction is not idempotent: error=%v\nfirst=%s\nsecond=%s", secondErr, text, second)
	}
}

func TestJSONSensitiveOAuthKeyVariantsAreConservative(t *testing.T) {
	input := []byte(`{
		"ACCESS":"abcdefgh-value",
		"refresh":"abcdefgh-value",
		"openai_account_id":"abcdefgh-value",
		"openaiEnterpriseUrl":"https://private.example.test",
		"nestedClientSecret":"abcdefgh-value",
		"public_accessibility":"safe",
		"refreshRate":"safe"
	}`)
	output, findings, err := New(0).JSON(input)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if strings.Count(text, "[REDACTED:sensitive-key]") != 5 {
		t.Fatalf("OAuth key variants were not all redacted: %s (%+v)", text, findings)
	}
	for _, safe := range []string{`"public_accessibility":"safe"`, `"refreshRate":"safe"`} {
		if !strings.Contains(text, safe) {
			t.Fatalf("non-sensitive lookalike was redacted: %s", text)
		}
	}
}

func TestTextRedactsOAuthAssignments(t *testing.T) {
	input := strings.Join([]string{
		"access=access-token-value",
		"refresh: refresh-token-value",
		"accountId=account-identifier",
		"enterpriseUrl=https://tenant.example.test",
	}, "\n")
	result, err := New(0).Text(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"access-token-value", "refresh-token-value", "account-identifier", "tenant.example.test"} {
		if strings.Contains(result.Text, forbidden) {
			t.Fatalf("OAuth assignment %q remained in %q", forbidden, result.Text)
		}
	}
	if countFinding(result.Findings, "assignment") != 4 {
		t.Fatalf("unexpected findings: %+v", result.Findings)
	}
}

func countFinding(findings []Finding, kind string) int {
	for _, finding := range findings {
		if finding.Kind == kind {
			return finding.Count
		}
	}
	return 0
}
