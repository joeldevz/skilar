package contracts

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestCanonicalDigestStableAcrossMapOrder(t *testing.T) {
	t.Parallel()
	a := map[string]interface{}{"z": 1, "a": map[string]interface{}{"b": true, "a": "x"}}
	b := map[string]interface{}{"a": map[string]interface{}{"a": "x", "b": true}, "z": 1}
	digestA, err := CanonicalDigest(a)
	if err != nil {
		t.Fatal(err)
	}
	digestB, err := CanonicalDigest(b)
	if err != nil {
		t.Fatal(err)
	}
	if digestA != digestB {
		t.Fatalf("digests differ: %s != %s", digestA, digestB)
	}
}

func TestApplyOverrideLayersPrecedence(t *testing.T) {
	t.Parallel()
	base := validCaseForContractsTest()
	suiteRuns, caseRuns, cliRuns := 2, 3, 7
	resolved, err := ApplyOverrideLayers(base,
		OverrideLayer{Source: OverrideSuite, Values: CaseOverrides{RunsCount: &suiteRuns}},
		OverrideLayer{Source: OverrideCase, Values: CaseOverrides{RunsCount: &caseRuns}},
		OverrideLayer{Source: OverrideCLI, Values: CaseOverrides{RunsCount: &cliRuns}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Runs.Count != cliRuns {
		t.Fatalf("runs = %d, want CLI value %d", resolved.Runs.Count, cliRuns)
	}
	_, err = ApplyOverrideLayers(base,
		OverrideLayer{Source: OverrideCLI, Values: CaseOverrides{}},
		OverrideLayer{Source: OverrideSuite, Values: CaseOverrides{}},
	)
	if err == nil {
		t.Fatal("out-of-order overrides unexpectedly succeeded")
	}
}

func TestRunStatusExitCodesAreStable(t *testing.T) {
	t.Parallel()
	want := map[RunStatus]int{
		RunStatusPass: ExitSuccess, RunStatusFail: ExitFailed, RunStatusInvalid: ExitInvalid,
		RunStatusInconclusive: ExitInconclusive, RunStatusAborted: ExitAborted,
		RunStatusInfraError: ExitInfrastructure, RunStatusBudgetExhausted: ExitBudgetExhausted,
	}
	for status, code := range want {
		if !status.Valid() || status.ExitCode() != code {
			t.Errorf("status %q: valid=%v exit=%d, want %d", status, status.Valid(), status.ExitCode(), code)
		}
	}
}

func TestRunResultPassAllowsIncompleteEfficiencyTelemetry(t *testing.T) {
	t.Parallel()
	result := validRunResultForContractsTest()
	result.TelemetryComplete = false
	if err := result.Validate(); err != nil {
		t.Fatalf("mechanical pass with unavailable efficiency telemetry: %v", err)
	}
	result.Checks[0].Status = CheckStatusFail
	if err := result.Validate(); err == nil {
		t.Fatal("pass with failed hard check unexpectedly validated")
	}
}

func TestRunResultRejectsNonFiniteCost(t *testing.T) {
	t.Parallel()
	result := validRunResultForContractsTest()
	nonFiniteCost := math.NaN()
	result.Usage.Parent.ProviderCostUSD = &nonFiniteCost
	if err := result.Validate(); err == nil {
		t.Fatal("NaN cost unexpectedly validated")
	}
}

func TestRunResultRejectsNegativeParentFirstInputTokens(t *testing.T) {
	t.Parallel()
	result := validRunResultForContractsTest()
	result.Usage.Parent.FirstInputTokens = -1
	if err := result.Validate(); err == nil {
		t.Fatal("negative first input token count unexpectedly validated")
	}
	result = validRunResultForContractsTest()
	result.Usage.Parent.FirstInputTokens = 2
	result.Usage.Parent.PeakInputTokens = 1
	if err := result.Validate(); err == nil {
		t.Fatal("first input token count above peak unexpectedly validated")
	}
}

func TestCaseRejectsUnsupportedHTTPFakeMCP(t *testing.T) {
	testCase := validCaseForContractsTest()
	testCase.ToolPolicy.AllowedTools = []string{"worker_result"}
	testCase.ToolPolicy.FakeMCPs = []FakeMCP{{
		Name: "worker", Transport: "http", Tools: []string{"worker_result"}, URL: "http://127.0.0.1:9999",
	}}
	if err := testCase.Validate(); err == nil || !strings.Contains(err.Error(), "must be stdio") {
		t.Fatalf("unsupported HTTP fake MCP validation = %v", err)
	}
}

func TestRunResultBindsModelProviderAndOpenAIOAuth(t *testing.T) {
	result := validRunResultForContractsTest()
	result.Provenance.Provider = "other"
	if err := result.Validate(); err == nil || !strings.Contains(err.Error(), "parsed from provenance.model") {
		t.Fatalf("model/provider mismatch validation = %v", err)
	}

	result = validRunResultForContractsTest()
	result.Provenance.Model = "anthropic/model"
	result.Provenance.Provider = "anthropic"
	result.Provenance.Extensions = map[string]string{
		ProvenanceExtensionProviderAuthMode:   ProviderAuthModeOpenAIOAuthCleanProfileV1,
		ProvenanceExtensionBillingMode:        BillingModeChatGPTSubscription,
		ProvenanceExtensionCredentialBoundary: CredentialBoundaryRuntimeReadable,
		ProvenanceExtensionAuthIsolation:      AuthIsolationDedicatedFreshTokenFailStopV1,
	}
	if err := result.Validate(); err == nil || !strings.Contains(err.Error(), "must be openai") {
		t.Fatalf("non-OpenAI OAuth provenance validation = %v", err)
	}
}

func TestTokenUsageJSONUsesParentNestedFieldNames(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(TokenUsage{FirstInputTokens: 7})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["first_input_tokens"]; !ok {
		t.Fatalf("first_input_tokens missing from %s", encoded)
	}
	if _, redundant := fields["parent_first_input_tokens"]; redundant {
		t.Fatalf("parent prefix must come from usage.parent nesting: %s", encoded)
	}
}

func TestRunResultProvenanceExtensionsRequireXNamespace(t *testing.T) {
	t.Parallel()
	result := validRunResultForContractsTest()
	result.Provenance.Extensions = map[string]string{
		"x-effective-tool-policy-digest": testDigest(),
		"x-observed-provider":            "provider",
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("valid namespaced provenance extensions: %v", err)
	}

	for _, key := range []string{
		"effective-tool-policy-digest",
		"x-Uppercase",
		"x-non_ascii_\u00e9",
		"x-" + strings.Repeat("a", 128),
	} {
		t.Run(key, func(t *testing.T) {
			invalid := validRunResultForContractsTest()
			invalid.Provenance.Extensions = map[string]string{key: "value"}
			if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "provenance.extensions") {
				t.Fatalf("extension key %q: validation error = %v, want provenance.extensions namespace error", key, err)
			}
		})
	}
}

func TestRunResultRejectsInvalidProviderCatalogDigestExtension(t *testing.T) {
	t.Parallel()
	result := validRunResultForContractsTest()
	result.Provenance.Extensions = map[string]string{
		ProvenanceExtensionProviderCatalogDigest: "not-a-digest",
	}
	if err := result.Validate(); err == nil || !strings.Contains(err.Error(), ProvenanceExtensionProviderCatalogDigest) {
		t.Fatalf("provider catalog digest validation error = %v", err)
	}
}

func TestRunResultRuntimeCleanupAttestationUsesFixedStringVocabulary(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"true", "false"} {
		result := validRunResultForContractsTest()
		result.Provenance.Extensions = map[string]string{ProvenanceExtensionRuntimeCleanupAttested: value}
		if err := result.Validate(); err != nil {
			t.Fatalf("cleanup attestation %q rejected: %v", value, err)
		}
	}
	result := validRunResultForContractsTest()
	result.Provenance.Extensions = map[string]string{ProvenanceExtensionRuntimeCleanupAttested: "yes"}
	if err := result.Validate(); err == nil || !strings.Contains(err.Error(), ProvenanceExtensionRuntimeCleanupAttested) {
		t.Fatalf("invalid cleanup attestation error = %v", err)
	}
}

func TestRunResultRequiresConsistentNonSecretOAuthBillingProvenance(t *testing.T) {
	valid := validRunResultForContractsTest()
	valid.Provenance.Extensions = map[string]string{
		ProvenanceExtensionProviderAuthMode:   ProviderAuthModeOpenAIOAuthCleanProfileV1,
		ProvenanceExtensionBillingMode:        BillingModeChatGPTSubscription,
		ProvenanceExtensionCredentialBoundary: CredentialBoundaryRuntimeReadable,
		ProvenanceExtensionAuthIsolation:      AuthIsolationDedicatedFreshTokenFailStopV1,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid OAuth billing provenance rejected: %v", err)
	}
	if valid.Provenance.ProviderCostUSDAuthoritative() {
		t.Fatal("subscription provider cost unexpectedly authoritative")
	}
	zero := 0.0
	invalidCost := valid
	invalidCost.Usage.Tree.ProviderCostUSD = &zero
	if err := invalidCost.Validate(); err == nil || !strings.Contains(err.Error(), "must be omitted") {
		t.Fatalf("subscription provider USD was accepted as evidence: %v", err)
	}

	for name, extensions := range map[string]map[string]string{
		"missing billing": {
			ProvenanceExtensionProviderAuthMode: ProviderAuthModeOpenAIOAuthCleanProfileV1,
		},
		"missing auth": {
			ProvenanceExtensionBillingMode: BillingModeChatGPTSubscription,
		},
		"unknown auth": {
			ProvenanceExtensionProviderAuthMode:   "oauth-with-account-secret",
			ProvenanceExtensionBillingMode:        BillingModeChatGPTSubscription,
			ProvenanceExtensionCredentialBoundary: CredentialBoundaryRuntimeReadable,
			ProvenanceExtensionAuthIsolation:      AuthIsolationDedicatedFreshTokenFailStopV1,
		},
		"unknown billing": {
			ProvenanceExtensionProviderAuthMode:   ProviderAuthModeOpenAIOAuthCleanProfileV1,
			ProvenanceExtensionBillingMode:        "free",
			ProvenanceExtensionCredentialBoundary: CredentialBoundaryRuntimeReadable,
			ProvenanceExtensionAuthIsolation:      AuthIsolationDedicatedFreshTokenFailStopV1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := validRunResultForContractsTest()
			invalid.Provenance.Extensions = extensions
			if err := invalid.Validate(); err == nil {
				t.Fatalf("invalid provenance accepted: %#v", extensions)
			}
		})
	}
}

func TestRunResultRejectsSchemaCollectionLimits(t *testing.T) {
	t.Parallel()

	tooManyChecks := validRunResultForContractsTest()
	tooManyChecks.Status = RunStatusFail
	tooManyChecks.Error = &RunError{Kind: "failure", Message: "too many checks"}
	tooManyChecks.Checks = make([]CheckResult, maxResultChecks+1)
	if err := tooManyChecks.Validate(); err == nil || !strings.Contains(err.Error(), "checks") {
		t.Fatalf("oversized checks error = %v", err)
	}

	tooMuchEvidence := validRunResultForContractsTest()
	tooMuchEvidence.Status = RunStatusFail
	tooMuchEvidence.Error = &RunError{Kind: "failure", Message: "too much evidence"}
	tooMuchEvidence.Evidence.Items = make([]EvidenceItem, maxResultEvidenceItems+1)
	if err := tooMuchEvidence.Validate(); err == nil || !strings.Contains(err.Error(), "evidence.items") {
		t.Fatalf("oversized evidence error = %v", err)
	}
}

func TestRunResultPassRequiresHardCompleteEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*RunResult)
	}{
		{name: "no checks", mutate: func(result *RunResult) { result.Checks = []CheckResult{} }},
		{name: "no hard check", mutate: func(result *RunResult) { result.Checks[0].Hard = false }},
		{name: "no evidence ids", mutate: func(result *RunResult) { result.Checks[0].EvidenceIDs = []string{} }},
		{name: "incomplete evidence", mutate: func(result *RunResult) { result.Evidence.Items[0].Complete = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validRunResultForContractsTest()
			test.mutate(&result)
			if err := result.Validate(); err == nil {
				t.Fatal("unsupported pass result validated")
			}
		})
	}
}

func TestRunResultRejectsPassingCheckWithError(t *testing.T) {
	result := validRunResultForContractsTest()
	result.Checks[0].Error = &RunError{Kind: "judge_error", Message: "failed", Retryable: false}
	if err := result.Validate(); err == nil || !strings.Contains(err.Error(), "checks[0].error") {
		t.Fatalf("passing check with error validation = %v", err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRunResultJSON(raw); err == nil || !strings.Contains(err.Error(), "checks[0].error") {
		t.Fatalf("imported passing check with error validation = %v", err)
	}
}

func TestDecodeRunResultAcceptsExplicitNullPassingCheckError(t *testing.T) {
	result := validRunResultForContractsTest()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	checks := document["checks"].([]any)
	checks[0].(map[string]any)["error"] = nil
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRunResultJSON(raw); err != nil {
		t.Fatalf("schema-valid explicit null check error was rejected: %v", err)
	}
}

func validCaseForContractsTest() Case {
	content := "done"
	result := Case{
		SchemaVersion: CaseSchemaVersion,
		ID:            "case", Suite: "suite", RequirementIDs: []string{"REQ-1"}, Type: CaseTypeBehavior,
		Critical: true,
		Agent:    AgentConfig{Name: "agent", Model: "provider/model"},
		Fixture: FixtureConfig{
			Source: "fixture", InitialGit: true,
			ExpectedDigest: testDigest(),
			GitSeed:        GitSeed{Tracked: []GitSeedFile{}, Staged: []GitSeedFile{}, Untracked: []GitSeedFile{}, Ignored: []GitSeedFile{}},
		},
		Setup: SetupConfig{Commands: []Command{}},
		Input: "do it", Turns: []Turn{},
		Completion: CompletionConfig{MaxTurns: 3, Timeout: "3m", UnexpectedQuestion: UnexpectedQuestionFail},
		Oracle: OracleConfig{
			Commands: []Command{}, ExpectedChanges: []string{"result.txt"}, ForbiddenChanges: []string{},
			ExpectedFiles: []ExpectedFile{{Path: "result.txt", Content: &content}}, RequireCleanProcessTree: true,
		},
		BehaviorChecks: []Check{{ID: "honest", RequirementIDs: []string{"REQ-1"}, Type: "no_false_success", Hard: boolPointer(true)}},
		Security: SecurityConfig{
			ExecutionMode: ExecutionTrustedLocal, Network: NetworkHostUnisolated,
			AllowedExecutables: []string{}, AllowedWriteRoots: []string{"fixture"}, RetainTrace: RetainTraceSanitizedOnFailure,
		},
		Trace:      TraceConfig{MaxBytes: 1024, MaxEvents: 10, MaxEventBytes: 512, Quiescence: QuiescenceConfig{Required: true, QuietPeriod: "1s", Timeout: "10s"}},
		ToolPolicy: ToolPolicy{AllowedTools: []string{}, ForbiddenTools: []string{}, FakeMCPs: []FakeMCP{}},
		Runs:       RunConfig{Count: 1, Aggregation: AggregationMedian}, Gates: Gates{HardChecks: "all"},
	}
	result.Normalize()
	return result
}

func validRunResultForContractsTest() RunResult {
	digest := testDigest()
	return RunResult{
		SchemaVersion: ResultSchemaVersion,
		RunID:         "run", CaseID: "case", Variant: "candidate", Repetition: 1, Status: RunStatusPass,
		Provenance: Provenance{
			GitSHA: "599f3e2e9b41906cba5b7064941542e39c020bd6", CaseDigest: digest, PromptDigest: digest, ConfigDigest: digest,
			FixtureDigest: digest, OpenCodeVersion: "1.0", Model: "openai/model", Provider: "openai",
			ToolsetDigest: digest, PricingTableDigest: digest,
			ExecutionMode: ExecutionTrustedLocal, Network: NetworkHostUnisolated,
			Host: HostProvenance{OS: "linux", Arch: "amd64"},
		},
		Checks: []CheckResult{{
			ID: "check", Type: "command", Status: CheckStatusPass, Hard: true,
			Summary: "passed", RequirementIDs: []string{"REQ-1"}, EvidenceIDs: []string{"evidence"},
		}},
		Evidence: Evidence{
			DiffDigest: digest, TraceDigest: digest,
			Items: []EvidenceItem{{ID: "evidence", Kind: "command", Source: EvidenceEvaluator, Digest: digest, Complete: true}},
		},
		TelemetryComplete: true,
	}
}

func testDigest() string {
	return "sha256:0000000000000000000000000000000000000000000000000000000000000000"
}

func boolPointer(value bool) *bool {
	return &value
}
