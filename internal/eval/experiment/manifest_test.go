package experiment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/stats"
)

func TestLoadRejectsMissingOrNullRequiredGate(t *testing.T) {
	manifest := validManifest()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	gates := document["gates"].(map[string]any)

	for _, test := range []struct {
		name  string
		value any
		omit  bool
	}{
		{name: "missing", omit: true},
		{name: "null", value: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			copyDocument := make(map[string]any, len(document))
			for key, value := range document {
				copyDocument[key] = value
			}
			copyGates := make(map[string]any, len(gates))
			for key, value := range gates {
				copyGates[key] = value
			}
			if test.omit {
				delete(copyGates, "critical_case_pass_rate")
			} else {
				copyGates["critical_case_pass_rate"] = test.value
			}
			copyDocument["gates"] = copyGates
			path := filepath.Join(t.TempDir(), "manifest.json")
			payload, err := json.Marshal(copyDocument)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "gates.critical_case_pass_rate") {
				t.Fatalf("Load() error = %v, want required gate error", err)
			}
		})
	}
}

func TestManifestSiblingBundleLayoutAndDriftDetection(t *testing.T) {
	capsule := t.TempDir()
	bundlesRoot := filepath.Join(capsule, "bundles")
	for _, name := range []string{"harness", "control", "candidate"} {
		dir := filepath.Join(bundlesRoot, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bundle.txt"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest := func(name string) string {
		t.Helper()
		snapshot, err := sandbox.DigestTree(filepath.Join(bundlesRoot, name), sandbox.SnapshotLimits{})
		if err != nil {
			t.Fatal(err)
		}
		return snapshot.Digest
	}
	manifest := validManifest()
	manifest.Harness = FrozenBundle{Root: "bundles/harness", Digest: digest("harness")}
	manifest.Control = FrozenBundle{Root: "bundles/control", Digest: digest("control")}
	manifest.Candidate = FrozenBundle{Root: "bundles/candidate", Digest: digest("candidate")}
	manifestPath := filepath.Join(capsule, "manifest.json")
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(manifestPath)
	if err != nil {
		t.Fatalf("load sibling manifest: %v", err)
	}

	frozen, err := loaded.VerifyBundles(filepath.Dir(manifestPath), sandbox.SnapshotLimits{})
	if err != nil {
		t.Fatalf("verify bundles: %v", err)
	}
	for _, bundle := range frozen.Bundles {
		if !strings.HasPrefix(bundle.AbsoluteRoot, bundlesRoot+string(filepath.Separator)) {
			t.Errorf("%s resolved outside sibling bundles container: %s", bundle.Name, bundle.AbsoluteRoot)
		}
	}
	if err := os.WriteFile(filepath.Join(bundlesRoot, "candidate", "bundle.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := frozen.VerifyUnchanged(); err == nil || !strings.Contains(err.Error(), "candidate bundle drifted") {
		t.Fatalf("expected candidate drift error, got %v", err)
	}
}

func TestVerifyBundlesRejectsEqualNestedAndAliasedRoots(t *testing.T) {
	capsule := t.TempDir()
	harnessRoot := filepath.Join(capsule, "harness")
	controlRoot := filepath.Join(harnessRoot, "nested-control")
	candidateRoot := filepath.Join(capsule, "candidate")
	for path, contents := range map[string]string{
		filepath.Join(harnessRoot, "harness.txt"):     "harness",
		filepath.Join(controlRoot, "control.txt"):     "control",
		filepath.Join(candidateRoot, "candidate.txt"): "candidate",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest := func(root string) string {
		t.Helper()
		snapshot, err := sandbox.DigestTree(root, sandbox.SnapshotLimits{})
		if err != nil {
			t.Fatal(err)
		}
		return snapshot.Digest
	}

	t.Run("equal", func(t *testing.T) {
		manifest := validManifest()
		manifest.Harness = FrozenBundle{Root: harnessRoot, Digest: digest(harnessRoot)}
		manifest.Control = FrozenBundle{Root: harnessRoot, Digest: digest(harnessRoot)}
		manifest.Candidate = FrozenBundle{Root: candidateRoot, Digest: digest(candidateRoot)}
		if _, err := manifest.VerifyBundles(capsule, sandbox.SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "must be disjoint") {
			t.Fatalf("equal bundle roots accepted: %v", err)
		}
	})

	t.Run("nested", func(t *testing.T) {
		manifest := validManifest()
		manifest.Harness = FrozenBundle{Root: harnessRoot, Digest: digest(harnessRoot)}
		manifest.Control = FrozenBundle{Root: controlRoot, Digest: digest(controlRoot)}
		manifest.Candidate = FrozenBundle{Root: candidateRoot, Digest: digest(candidateRoot)}
		if _, err := manifest.VerifyBundles(capsule, sandbox.SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "must be disjoint") {
			t.Fatalf("nested bundle roots accepted: %v", err)
		}
	})

	if runtime.GOOS != "windows" {
		t.Run("symlink alias", func(t *testing.T) {
			alias := filepath.Join(capsule, "harness-alias")
			if err := os.Symlink(harnessRoot, alias); err != nil {
				t.Fatal(err)
			}
			manifest := validManifest()
			manifest.Harness = FrozenBundle{Root: harnessRoot, Digest: digest(harnessRoot)}
			manifest.Control = FrozenBundle{Root: alias, Digest: digest(harnessRoot)}
			manifest.Candidate = FrozenBundle{Root: candidateRoot, Digest: digest(candidateRoot)}
			if _, err := manifest.VerifyBundles(capsule, sandbox.SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "must be disjoint") {
				t.Fatalf("aliased bundle roots accepted: %v", err)
			}
		})
	}
}

func TestLoadRejectsPublishedSchemaViolationBeyondGoDefaults(t *testing.T) {
	manifest := validManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatalf("valid in-memory manifest: %v", err)
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	// Zero means "use 0.95" only for an omitted in-memory optional field. An
	// explicitly serialized zero is invalid under the published schema.
	document["gates"].(map[string]any)["confidence"] = 0
	payload, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "published experiment schema") {
		t.Fatalf("Load() error = %v, want published schema rejection", err)
	}
}

func TestPlanIsDeterministicAndBalanced(t *testing.T) {
	manifest := validManifest()
	manifest.Runs = 10
	first, err := manifest.Plan([]string{"case_a", "case_b"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manifest.Plan([]string{"case_a", "case_b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Blocks) != 20 || len(second.Blocks) != 20 {
		t.Fatalf("unexpected block count: %d / %d", len(first.Blocks), len(second.Blocks))
	}
	for i := range first.Blocks {
		if strings.Join(variants(first.Blocks[i].Order), ",") != strings.Join(variants(second.Blocks[i].Order), ",") {
			t.Fatalf("plan differs at block %d", i)
		}
	}
	for _, caseID := range []string{"case_a", "case_b"} {
		controlFirst := 0
		for _, block := range first.Blocks {
			if block.CaseID == caseID && block.Order[0] == "control" {
				controlFirst++
			}
		}
		if controlFirst != 5 {
			t.Fatalf("%s control-first count=%d, want 5", caseID, controlFirst)
		}
	}
}

func TestTrustedLocalCannotClaimNetworkIsolation(t *testing.T) {
	manifest := validManifest()
	manifest.Execution.Network = "none"
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "host-unisolated") {
		t.Fatalf("expected trusted-local network error, got %v", err)
	}
}

func TestReleaseIntentRequiresHoldoutAndTenPairsPerCase(t *testing.T) {
	manifest := validManifest()
	manifest.Intent = IntentRelease
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "holdout") {
		t.Fatalf("release without holdout accepted: %v", err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	manifest.Holdout = &FrozenBundle{Root: "holdout", Digest: digest}
	manifest.HoldoutCaseCount = 1
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "at least 10") {
		t.Fatalf("release with fewer than ten pairs accepted: %v", err)
	}
	manifest.Runs = MinimumReleaseRuns
	configureProviderProxy(&manifest)
	configureReleaseGates(&manifest)
	if err := manifest.Validate(); err != nil {
		t.Fatalf("valid release manifest rejected: %v", err)
	}
}

func TestManifestCommitsCleanOAuthBillingAndCredentialBoundary(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Manifest)
		wantError string
	}{
		{
			name: "provider auth", mutate: func(manifest *Manifest) { manifest.Execution.ProviderAuth = "ambient-profile" },
			wantError: "provider_auth",
		},
		{
			name: "billing mode", mutate: func(manifest *Manifest) { manifest.Execution.BillingMode = "api-key" },
			wantError: "billing_mode",
		},
		{
			name: "credential boundary", mutate: func(manifest *Manifest) { manifest.Execution.CredentialBoundary = "ambient-readable" },
			wantError: "credential_boundary",
		},
		{
			name: "release runtime readable", mutate: func(manifest *Manifest) {
				manifest.Intent = IntentRelease
				manifest.Holdout = &FrozenBundle{Root: "holdout", Digest: "sha256:" + strings.Repeat("b", 64)}
				manifest.HoldoutCaseCount = 1
				manifest.Runs = MinimumReleaseRuns
				configureReleaseGates(manifest)
			},
			wantError: "provider-proxy",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validManifest()
			test.mutate(&manifest)
			if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want substring %q", err, test.wantError)
			}
		})
	}

	manifest := validManifest()
	configureProviderProxy(&manifest)
	if err := manifest.Validate(); err != nil {
		t.Fatalf("development provider-proxy boundary rejected: %v", err)
	}
}

func configureProviderProxy(manifest *Manifest) {
	manifest.Execution.CredentialBoundary = CredentialBoundaryProviderProxy
	manifest.Execution.Mode = "isolated-container"
	manifest.Execution.Network = "provider-proxy-only"
	manifest.Execution.ContainerImageDigest = "sha256:" + strings.Repeat("c", 64)
}

func configureReleaseGates(manifest *Manifest) {
	manifest.Gates.CriticalCasePassRate = 1
	manifest.Gates.PassToFailRegressions = 0
	manifest.Gates.ScopeViolations = 0
	manifest.Gates.FalseSuccesses = 0
	manifest.Gates.MaxParentPeakInputRatio = 0.70
	manifest.Gates.MaxTreeInputRatio = 1
	manifest.Gates.MaxCostRatio = 1
	manifest.Gates.MaxWallTimeRatio = 1.10
	manifest.Gates.MaxRetryRateRatio = 1
	manifest.Gates.Confidence = 0.95
	manifest.Gates.MinimumPairs = MinimumReleaseRuns
}

func TestReleaseIntentRejectsLaxGates(t *testing.T) {
	base := validManifest()
	base.Intent = IntentRelease
	base.Holdout = &FrozenBundle{Root: "holdout", Digest: "sha256:" + strings.Repeat("b", 64)}
	base.HoldoutCaseCount = 1
	base.Runs = MinimumReleaseRuns
	configureProviderProxy(&base)
	configureReleaseGates(&base)

	tests := []struct {
		name string
		edit func(*Manifest)
	}{
		{name: "critical pass rate", edit: func(m *Manifest) { m.Gates.CriticalCasePassRate = 0.99 }},
		{name: "pass to fail", edit: func(m *Manifest) { m.Gates.PassToFailRegressions = 1 }},
		{name: "parent ratio", edit: func(m *Manifest) { m.Gates.MaxParentPeakInputRatio = 0.71 }},
		{name: "confidence", edit: func(m *Manifest) { m.Gates.Confidence = 0.94 }},
		{name: "minimum pairs", edit: func(m *Manifest) { m.Gates.MinimumPairs = 9 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := base
			test.edit(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("lax release gate unexpectedly validated")
			}
		})
	}
}

func TestManifestCommitsPublicAndHoldoutPopulationCounts(t *testing.T) {
	manifest := validManifest()
	manifest.PublicCaseCount = 0
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "public_case_count") {
		t.Fatalf("missing public population accepted: %v", err)
	}
	manifest = validManifest()
	manifest.HoldoutCaseCount = 1
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "without a holdout") {
		t.Fatalf("holdout count without bundle accepted: %v", err)
	}
	manifest.Holdout = &FrozenBundle{Root: "holdout", Digest: "sha256:" + strings.Repeat("b", 64)}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("committed holdout population rejected: %v", err)
	}
}

func TestManifestRejectsMissingOrUnknownIntent(t *testing.T) {
	manifest := validManifest()
	for _, intent := range []string{"", "production"} {
		manifest.Intent = intent
		if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "intent") {
			t.Fatalf("intent %q accepted: %v", intent, err)
		}
	}
}

func TestManifestRejectsDifferencesWithoutPerArmAuthority(t *testing.T) {
	for _, field := range []baseline.Field{baseline.FieldToolsetDigest, baseline.FieldPermissionPolicyDigest} {
		t.Run(string(field), func(t *testing.T) {
			manifest := validManifest()
			manifest.IntentionalDifferences = append(manifest.IntentionalDifferences, field)
			if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported field") {
				t.Fatalf("unbound per-arm difference accepted by Go validation: %v", err)
			}

			path := filepath.Join(t.TempDir(), "manifest.json")
			payload, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "published experiment schema") {
				t.Fatalf("unbound per-arm difference accepted by published schema: %v", err)
			}
		})
	}
}

func TestPublishedSchemaRejectsHoldoutSourceGitProvenance(t *testing.T) {
	manifest := validManifest()
	holdout := FrozenBundle{
		Root: "bundles/holdout", Digest: "sha256:" + strings.Repeat("c", 64),
		SourceGitSHA: strings.Repeat("d", 40),
	}
	manifest.Holdout = &holdout
	manifest.HoldoutCaseCount = 1
	path := filepath.Join(t.TempDir(), "manifest.json")
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "published experiment schema") {
		t.Fatalf("holdout source provenance accepted by published schema: %v", err)
	}
}

func TestModelAssignmentMustExactlyMatchDeclaredArmDifferences(t *testing.T) {
	tests := []struct {
		name        string
		differences []baseline.Field
		assignment  *ModelAssignment
		wantError   string
	}{
		{
			name: "same provider model change", differences: []baseline.Field{baseline.FieldModel},
			assignment: &ModelAssignment{Control: "openai/control", Candidate: "openai/candidate"},
		},
		{
			name: "provider and model change", differences: []baseline.Field{baseline.FieldModel, baseline.FieldProvider},
			assignment: &ModelAssignment{Control: "openai/model", Candidate: "anthropic/model"},
		},
		{
			name: "declared without assignment", differences: []baseline.Field{baseline.FieldModel},
			wantError: "model_assignment is required",
		},
		{
			name: "provider change undeclared", differences: []baseline.Field{baseline.FieldModel},
			assignment: &ModelAssignment{Control: "openai/model", Candidate: "anthropic/model"},
			wantError:  "provider must be declared",
		},
		{
			name: "provider declared but unchanged", differences: []baseline.Field{baseline.FieldModel, baseline.FieldProvider},
			assignment: &ModelAssignment{Control: "openai/control", Candidate: "openai/candidate"},
			wantError:  "keeps the same provider",
		},
		{
			name: "assignment without model declaration", differences: []baseline.Field{baseline.FieldPromptDigest},
			assignment: &ModelAssignment{Control: "openai/control", Candidate: "openai/candidate"},
			wantError:  "requires model",
		},
		{
			name: "whitespace rejected like case schema", differences: []baseline.Field{baseline.FieldModel},
			assignment: &ModelAssignment{Control: "openai/control model", Candidate: "openai/candidate"},
			wantError:  "whitespace",
		},
		{
			name: "oversized rejected like case schema", differences: []baseline.Field{baseline.FieldModel},
			assignment: &ModelAssignment{Control: "openai/" + strings.Repeat("x", 256), Candidate: "openai/candidate"},
			wantError:  "256 bytes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validManifest()
			manifest.IntentionalDifferences = test.differences
			manifest.ModelAssignment = test.assignment
			bundleTreatment := false
			for _, field := range test.differences {
				bundleTreatment = bundleTreatment || field == baseline.FieldPromptDigest || field == baseline.FieldAgentBundleDigest
			}
			if !bundleTreatment {
				manifest.Candidate.Digest = manifest.Control.Digest
			}
			err := manifest.Validate()
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func validManifest() Manifest {
	digest := "sha256:" + strings.Repeat("a", 64)
	candidateDigest := "sha256:" + strings.Repeat("b", 64)
	return Manifest{
		SchemaVersion:          SchemaVersion,
		ID:                     "lean-orchestrator-v1",
		Suite:                  "skynex-orchestrator",
		Intent:                 IntentDevelopment,
		Harness:                FrozenBundle{Root: "bundles/harness", Digest: digest},
		Control:                FrozenBundle{Root: "bundles/control", Digest: digest},
		Candidate:              FrozenBundle{Root: "bundles/candidate", Digest: candidateDigest},
		IntentionalDifferences: []baseline.Field{baseline.FieldPromptDigest, baseline.FieldAgentBundleDigest},
		PublicCaseCount:        19,
		PublicCasesDigest:      digest,
		CriticalCaseIDs:        []string{"skx_critical"},
		HoldoutCaseCount:       0,
		Runs:                   5,
		Randomization:          Randomization{Method: "balanced-blocked-ab-ba", Seed: "42", SerializeWithinBlock: true},
		Execution: Execution{
			Mode: "trusted-local", Network: "host-unisolated", Concurrency: 1, OpenCodeVersion: "1.18.16",
			ProviderAuth: ProviderAuthOpenAIOAuthCleanProfileV1, BillingMode: BillingModeChatGPTSubscription,
			CredentialBoundary:    CredentialBoundaryRuntimeReadable,
			EvaluatorBinaryDigest: digest, OpenCodeBinaryDigest: digest, OpenCodeOpenAPIDigest: digest,
			ToolchainsDigest: digest,
		},
		Gates: Gates{
			CriticalCasePassRate: 1, MaxParentPeakInputRatio: .7, MaxTreeInputRatio: 1,
			MaxCostRatio: 1, MaxWallTimeRatio: 1.1, MaxRetryRateRatio: 1,
		},
	}
}

func variants(values []stats.Variant) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}
