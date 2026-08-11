package baseline

import (
	"strings"
	"testing"
)

func TestCompareFingerprintsAllowsOnlyDeclaredExperimentVariables(t *testing.T) {
	control := validFingerprint()
	candidate := control
	candidate.PromptDigest = digest("b")
	report := CompareFingerprints(control, candidate, []Field{FieldPromptDigest})
	if !report.Compatible || len(report.Mismatches) != 1 || !report.Mismatches[0].Allowed {
		t.Fatalf("declared prompt experiment rejected: %+v", report)
	}

	report = CompareFingerprints(control, candidate, nil)
	if report.Compatible || len(report.Errors) == 0 {
		t.Fatalf("undeclared difference accepted: %+v", report)
	}

	candidate = control
	candidate.HarnessBundleDigest = digest("c")
	report = CompareFingerprints(control, candidate, []Field{FieldHarnessBundleDigest})
	if report.Compatible || !containsError(report.Errors, "may not be an intentional difference") {
		t.Fatalf("trusted harness was allowed as experiment variable: %+v", report)
	}
}

func TestFingerprintDetectsBehaviorAndAuthorityChanges(t *testing.T) {
	control := validFingerprint()
	for name, mutate := range map[string]func(*Fingerprint){
		"manifest":       func(f *Fingerprint) { f.ExperimentManifestDigest = digest("d") },
		"evaluator":      func(f *Fingerprint) { f.EvaluatorBinaryDigest = digest("e") },
		"opencode api":   func(f *Fingerprint) { f.OpenCodeOpenAPIDigest = digest("f") },
		"llm judge used": func(f *Fingerprint) { f.LLMJudgeUsed = true; f.JudgeModel = "judge" },
		"cost authority": func(f *Fingerprint) { f.CalculatedCostUsed = true; f.PricingTableDigest = digest("9") },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := control
			mutate(&candidate)
			report := CompareFingerprints(control, candidate, nil)
			if report.Compatible {
				t.Fatalf("material change accepted: %+v", report)
			}
		})
	}
}

func TestFingerprintValidationRequiresCanonicalDigestsAndRequiredAuthorities(t *testing.T) {
	fingerprint := validFingerprint()
	fingerprint.HarnessBundleDigest = "sha256:ABC"
	if err := fingerprint.Validate(); err == nil || !strings.Contains(err.Error(), "harness_bundle_digest") {
		t.Fatalf("malformed digest accepted: %v", err)
	}
	fingerprint = validFingerprint()
	fingerprint.ExperimentManifestDigest = ""
	if err := fingerprint.Validate(); err == nil || !strings.Contains(err.Error(), "experiment_manifest_digest") {
		t.Fatalf("missing manifest accepted: %v", err)
	}
}

func TestFingerprintDigestIsStable(t *testing.T) {
	fingerprint := validFingerprint()
	left, err := fingerprint.Digest()
	if err != nil {
		t.Fatal(err)
	}
	right, err := fingerprint.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if left != right || !validSHA256Digest(left) {
		t.Fatalf("unstable/invalid digest: %q %q", left, right)
	}
}

func validFingerprint() Fingerprint {
	return Fingerprint{
		PromptDigest: digest("1"), AgentBundleDigest: digest("2"), HarnessBundleDigest: digest("3"),
		EvaluatorBinaryDigest: digest("4"), ExperimentManifestDigest: digest("5"),
		CaseSchemaVersion: 1, CaseDigest: digest("6"), FixtureDigest: digest("7"), SetupPolicyDigest: digest("8"),
		OpenCodeVersion: "1.2.3", OpenCodeBinaryDigest: digest("9"), OpenCodeOpenAPIDigest: digest("a"),
		EffectiveConfigDigest: digest("0"), EffectiveAgentsDigest: digest("1"),
		Model: "model", Provider: "provider", ToolsetDigest: digest("b"), PermissionPolicyDigest: digest("c"),
		ExecutionMode: "trusted-local", NetworkPolicy: "host-unisolated", JudgesDigest: digest("d"),
		ProviderAuthMode: "provider-environment", BillingMode: "api-usage",
		CredentialBoundary: "environment", AuthIsolation: "none",
		ProviderCatalogDigest: digest("f"),
		HostOS:                "linux", HostArch: "amd64", ToolchainsDigest: digest("e"),
	}
}

func TestCompareFingerprintsBindsEffectiveRuntimeDocuments(t *testing.T) {
	control := validFingerprint()
	candidate := control
	candidate.EffectiveConfigDigest = digest("2")
	if report := CompareFingerprints(control, candidate, nil); report.Compatible {
		t.Fatalf("unexplained effective config drift accepted: %+v", report)
	}

	// A derived document may change only when a relevant declared treatment was
	// actually realized. A declaration alone is not an escape hatch.
	if report := CompareFingerprints(control, candidate, []Field{FieldAgentBundleDigest}); report.Compatible {
		t.Fatalf("unrealized agent treatment hid config drift: %+v", report)
	}
	candidate.AgentBundleDigest = digest("3")
	if report := CompareFingerprints(control, candidate, []Field{FieldAgentBundleDigest}); !report.Compatible {
		t.Fatalf("derived effective config change rejected with realized treatment: %+v", report)
	}

	candidate = control
	candidate.EffectiveAgentsDigest = digest("4")
	candidate.ToolchainsDigest = digest("5")
	report := CompareFingerprints(control, candidate, []Field{FieldModel})
	if report.Compatible || !containsError(report.Errors, "toolchains_digest") {
		t.Fatalf("toolchain drift was hidden by a model treatment: %+v", report)
	}
}

func digest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}

func containsError(errors []string, fragment string) bool {
	for _, message := range errors {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}
