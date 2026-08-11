package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/cases"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/experiment"
	"github.com/joeldevz/skynex/internal/eval/gates"
	"github.com/joeldevz/skynex/internal/eval/lifecycle"
	"github.com/joeldevz/skynex/internal/eval/runner"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/stats"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPaidCommandsRequireExplicitOptInBeforeRunner(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "run", args: []string{"run", "--case", "does-not-matter"}},
		{name: "baseline", args: []string{"baseline", "--suite", "does-not-matter"}},
		{name: "ab", args: []string{"ab", "--manifest", "does-not-exist.json"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := 0
			deps := dependencies{runModel: func(context.Context, modelRunSpec) (modelRunResult, error) {
				called++
				return modelRunResult{}, errors.New("must not run")
			}}
			var stdout, stderr bytes.Buffer
			exit := runCLI(context.Background(), test.args, deps, &stdout, &stderr)
			if exit != contracts.ExitInvalid {
				t.Fatalf("exit=%d, stdout=%s, stderr=%s", exit, stdout.String(), stderr.String())
			}
			if called != 0 {
				t.Fatalf("runner called %d times without opt-in", called)
			}
			var output envelope
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatal(err)
			}
			if output.Error == nil || output.Error.Kind != "model_calls_not_allowed" {
				t.Fatalf("unexpected output: %+v", output)
			}
		})
	}
}

func TestRunOptInReachesInjectedRunner(t *testing.T) {
	root := projectRoot(t)
	openAIOAuthFile := writeOpenAIOAuthFile(t, t.TempDir())
	called := 0
	deps := dependencies{runModel: func(_ context.Context, spec modelRunSpec) (modelRunResult, error) {
		called++
		if len(spec.Cases) != 1 || spec.Cases[0].ID != "skx_low_direct" {
			t.Fatalf("unexpected selection: %+v", spec.Cases)
		}
		if spec.LegacyModelLabel != "openai/gpt-5.6-terra" {
			t.Fatalf("legacy model default = %q", spec.LegacyModelLabel)
		}
		if spec.OpenAIOAuthFile != openAIOAuthFile {
			t.Fatalf("OAuth source = %q, want %q", spec.OpenAIOAuthFile, openAIOAuthFile)
		}
		return modelRunResult{Result: runner.ContractResult{
			Suite: spec.Suite, Samples: []contracts.RunResult{validRun("run-current", "current", contracts.RunStatusPass)},
			Started: time.Unix(1, 0).UTC(), Ended: time.Unix(2, 0).UTC(), Complete: true,
		}, CostEvidenceComplete: true}, nil
	}}
	var stdout, stderr bytes.Buffer
	exit := runCLI(context.Background(), []string{
		"run", "--allow-model-calls", "--case", "skx_low_direct",
		"--cases-dir", filepath.Join(root, "eval", "cases"), "--openai-oauth", openAIOAuthFile,
	}, deps, &stdout, &stderr)
	if exit != contracts.ExitSuccess || called != 1 {
		t.Fatalf("exit=%d called=%d stdout=%s stderr=%s", exit, called, stdout.String(), stderr.String())
	}
}

func TestOpenAIOAuthRejectsAmbientProviderEnvironment(t *testing.T) {
	set := newFlagSet("oauth-conflict")
	flags := bindModelFlags(set)
	if err := parseFlagSet(set, []string{"--openai-oauth", "auth.json", "--provider-env", "OPENAI_API_KEY"}); err != nil {
		t.Fatal(err)
	}
	if _, err := buildModelSpec(flags, "current", 1, false); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("OAuth plus ambient provider environment accepted: %v", err)
	}
}

func TestProviderEnvironmentRejectsTraceRetention(t *testing.T) {
	set := newFlagSet("provider-trace")
	flags := bindModelFlags(set)
	if err := parseFlagSet(set, []string{"--provider-env", "OPENAI_API_KEY", "--retain-trace"}); err != nil {
		t.Fatal(err)
	}
	if _, err := buildModelSpec(flags, "current", 1, false); err == nil || !strings.Contains(err.Error(), "provider environment forbids persisted traces") {
		t.Fatalf("provider credential plus trace retention accepted: %v", err)
	}
}

func TestProviderEnvironmentRejectsRuntimeInjectionVariables(t *testing.T) {
	for _, name := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "BASH_ENV", "HTTP_PROXY"} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseEnvNames(name); err == nil || !strings.Contains(err.Error(), "runtime isolation") {
				t.Fatalf("provider environment %q accepted: %v", name, err)
			}
		})
	}
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "AWS_SESSION_TOKEN"} {
		if got, err := parseEnvNames(name); err != nil || len(got) != 1 || got[0] != name {
			t.Fatalf("supported provider credential %q = %v, %v", name, got, err)
		}
	}
	if _, err := parseEnvNames("UNRELATED_SECRET"); err == nil || !strings.Contains(err.Error(), "not a supported provider credential") {
		t.Fatalf("unknown provider environment accepted: %v", err)
	}
}

func TestOAuthCommandsRejectUSDCostCapBeforeExternalWork(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "run",
			args: []string{
				"run", "--allow-model-calls", "--case", "unused",
				"--openai-oauth", "unused-auth.json", "--cost-cap", "1",
			},
		},
		{
			name: "baseline",
			args: []string{
				"baseline", "--allow-model-calls", "--suite", "unused",
				"--openai-oauth", "unused-auth.json", "--cost-cap", "1",
			},
		},
		{
			name: "ab",
			args: []string{
				"ab", "--allow-model-calls", "--manifest", "unused-manifest.json",
				"--openai-oauth", "unused-auth.json", "--cost-cap", "1",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probeCalls, modelCalls := 0, 0
			deps := dependencies{
				probeRuntime: func(context.Context, doctorOptions) (doctorResult, error) {
					probeCalls++
					return doctorResult{}, errors.New("must not probe")
				},
				runModel: func(context.Context, modelRunSpec) (modelRunResult, error) {
					modelCalls++
					return modelRunResult{}, errors.New("must not run")
				},
			}
			var stdout, stderr bytes.Buffer
			exit := runCLI(context.Background(), test.args, deps, &stdout, &stderr)
			if exit != contracts.ExitInvalid || probeCalls != 0 || modelCalls != 0 {
				t.Fatalf("exit=%d probes=%d models=%d stdout=%s stderr=%s", exit, probeCalls, modelCalls, stdout.String(), stderr.String())
			}
			var output envelope
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatal(err)
			}
			if output.Error == nil || output.Error.Kind != "subscription_cost_cap_unsupported" ||
				!strings.Contains(output.Error.Message, "scheduled samples only") ||
				!strings.Contains(output.Error.Message, "not provider calls, tokens, or quota") {
				t.Fatalf("unexpected error: %+v", output.Error)
			}
		})
	}
}

func TestABRejectsImpureRuntimeBeforeManifestProbeOrModel(t *testing.T) {
	probeCalls, modelCalls := 0, 0
	deps := dependencies{
		probeRuntime: func(context.Context, doctorOptions) (doctorResult, error) {
			probeCalls++
			return doctorResult{}, errors.New("must not probe")
		},
		runModel: func(context.Context, modelRunSpec) (modelRunResult, error) {
			modelCalls++
			return modelRunResult{}, errors.New("must not run")
		},
	}
	var stdout, stderr bytes.Buffer
	exit := runCLI(context.Background(), []string{
		"ab", "--allow-model-calls", "--allow-impure", "--manifest", "does-not-exist.json",
	}, deps, &stdout, &stderr)
	if exit != contracts.ExitInvalid || probeCalls != 0 || modelCalls != 0 {
		t.Fatalf("exit=%d probes=%d models=%d stdout=%s stderr=%s", exit, probeCalls, modelCalls, stdout.String(), stderr.String())
	}
	var output envelope
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.Error == nil || output.Error.Kind != "impure_ab_forbidden" {
		t.Fatalf("unexpected error: %+v", output.Error)
	}
}

func TestABRequiresCleanOAuthBeforeManifestProbeOrModel(t *testing.T) {
	probeCalls, modelCalls := 0, 0
	deps := dependencies{
		probeRuntime: func(context.Context, doctorOptions) (doctorResult, error) {
			probeCalls++
			return doctorResult{}, errors.New("must not probe")
		},
		runModel: func(context.Context, modelRunSpec) (modelRunResult, error) {
			modelCalls++
			return modelRunResult{}, errors.New("must not run")
		},
	}
	var stdout, stderr bytes.Buffer
	exit := runCLI(context.Background(), []string{
		"ab", "--allow-model-calls", "--manifest", "does-not-exist.json",
	}, deps, &stdout, &stderr)
	if exit != contracts.ExitInvalid || probeCalls != 0 || modelCalls != 0 {
		t.Fatalf("exit=%d probes=%d models=%d stdout=%s stderr=%s", exit, probeCalls, modelCalls, stdout.String(), stderr.String())
	}
	var output envelope
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.Error == nil || output.Error.Kind != "openai_oauth_required" {
		t.Fatalf("unexpected error: %+v", output.Error)
	}
}

func TestEffectiveABModelsRejectNonOpenAIAndIncludeLegacyFallback(t *testing.T) {
	manifest := experiment.Manifest{}
	models, err := effectiveOpenAIModels(&manifest, []contracts.Case{
		{Agent: contracts.AgentConfig{Model: "openai/model-a"}},
		{Migration: &contracts.LegacyMigration{}},
		{Agent: contracts.AgentConfig{Model: "openai/model-a"}},
	}, "openai/legacy")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(models) != fmt.Sprint([]string{"openai/model-a", "openai/legacy"}) {
		t.Fatalf("effective models = %v", models)
	}
	manifest.ModelAssignment = &experiment.ModelAssignment{Control: "openai/control", Candidate: "anthropic/candidate"}
	if _, err := effectiveOpenAIModels(&manifest, nil, "openai/legacy"); err == nil || !strings.Contains(err.Error(), "provider openai") {
		t.Fatalf("non-OpenAI assignment accepted: %v", err)
	}
	if err := requireLocalABCredentialBoundary(experiment.Execution{
		CredentialBoundary: experiment.CredentialBoundaryProviderProxy,
	}); err == nil || !strings.Contains(err.Error(), "cannot satisfy") {
		t.Fatalf("local A/B accepted a false provider-proxy claim: %v", err)
	}
}

func TestDoctorUsesReadOnlyProbeAndStableClassification(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		openAIOAuthFile := writeOpenAIOAuthFile(t, t.TempDir())
		called := 0
		deps := dependencies{probeRuntime: func(_ context.Context, options doctorOptions) (doctorResult, error) {
			called++
			if options.OpenAIOAuthFile != openAIOAuthFile {
				t.Fatalf("doctor OAuth source = %q, want %q", options.OpenAIOAuthFile, openAIOAuthFile)
			}
			return doctorResult{
				Healthy: true, Version: options.ExpectedVersion, ExpectedVersion: options.ExpectedVersion,
				CapturedAt: "2026-08-10T00:00:00Z", ModelCalls: 0,
				Endpoints: []doctorEndpoint{{Name: "/experimental/tool/ids", Digest: "sha256:" + strings.Repeat("a", 64)}},
			}, nil
		}}
		var stdout, stderr bytes.Buffer
		exit := runCLI(context.Background(), []string{"doctor", "--openai-oauth", openAIOAuthFile}, deps, &stdout, &stderr)
		if exit != 0 || called != 1 || stderr.Len() != 0 {
			t.Fatalf("exit=%d called=%d stdout=%s stderr=%s", exit, called, stdout.String(), stderr.String())
		}
		if !bytes.Contains(stdout.Bytes(), []byte(`"model_calls":0`)) {
			t.Fatalf("doctor output does not attest zero model calls: %s", stdout.String())
		}
		if !bytes.Contains(stdout.Bytes(), []byte(`/experimental/tool/ids`)) {
			t.Fatalf("doctor output lacks tool-catalog evidence: %s", stdout.String())
		}
	})

	tests := []struct {
		name string
		err  error
		exit int
		kind string
	}{
		{name: "version mismatch", err: &lifecycle.VersionMismatchError{Got: "2", Expected: "1"}, exit: contracts.ExitInvalid, kind: "opencode_version_mismatch"},
		{name: "API incompatible", err: &openCodeCompatibilityError{cause: errors.New("required route absent")}, exit: contracts.ExitInvalid, kind: "opencode_api_incompatible"},
		{name: "unavailable", err: errors.New("binary missing"), exit: contracts.ExitInfrastructure, kind: "opencode_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps := dependencies{probeRuntime: func(context.Context, doctorOptions) (doctorResult, error) {
				return doctorResult{}, test.err
			}}
			var stdout, stderr bytes.Buffer
			exit := runCLI(context.Background(), []string{"doctor"}, deps, &stdout, &stderr)
			if exit != test.exit {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
			}
			var output envelope
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatal(err)
			}
			if output.Error == nil || output.Error.Kind != test.kind {
				t.Fatalf("unexpected error: %+v", output.Error)
			}
		})
	}
}

func TestDoctorOpenAPIContractRequiresEveryRunnerRoute(t *testing.T) {
	document := json.RawMessage(`{"paths":{
		"/session":{"post":{}},
		"/session/{sessionID}":{"get":{}},
		"/session/{sessionID}/children":{"get":{}},
		"/session/{sessionID}/message":{"get":{},"post":{}},
		"/session/status":{"get":{}},
		"/global/event":{"get":{}},
		"/experimental/tool/ids":{"get":{}},
		"/provider":{"get":{}}
	}}`)
	routes, err := verifyRequiredOpenCodeAPI(document)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 9 {
		t.Fatalf("verified routes = %v", routes)
	}
	missing := bytes.Replace(document, []byte(`"post":{}`), []byte{}, 1)
	if _, err := verifyRequiredOpenCodeAPI(missing); err == nil || !strings.Contains(err.Error(), "POST /session") {
		t.Fatalf("missing session POST error = %v", err)
	}
}

func TestValidateCoversV1LegacySchemasAndFixturesWithoutModel(t *testing.T) {
	root := projectRoot(t)
	result, err := commandValidate([]string{
		"--cases-dir", filepath.Join(root, "eval", "cases"),
		"--fixtures-dir", filepath.Join(root, "eval", "fixtures"),
		"--schemas-dir", filepath.Join(root, "schemas"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.V1Count == 0 || result.LegacyCount == 0 || result.CaseCount != result.V1Count+result.LegacyCount {
		t.Fatalf("unexpected case counts: %+v", result)
	}
	if len(result.Schemas) != 3 || len(result.Fixtures) == 0 {
		t.Fatalf("missing schema/fixture evidence: %+v", result)
	}
	foundLegacyEmpty := false
	for _, fixture := range result.Fixtures {
		if fixture.Source == "_legacy-empty" && fixture.LegacyOnly && contracts.IsDigest(fixture.Digest) {
			foundLegacyEmpty = true
		}
	}
	if !foundLegacyEmpty {
		t.Fatal("validation omitted the synthetic empty fixture used by migrated legacy cases")
	}
	for name, digest := range map[string]string{
		"cases": result.CasesDigest, "schemas": result.SchemasDigest, "fixtures": result.FixturesDigest,
	} {
		if !contracts.IsDigest(digest) {
			t.Fatalf("%s digest is invalid: %q", name, digest)
		}
	}
}

func TestValidateEnforcesPublishedCaseSchemaBeyondGoSemantics(t *testing.T) {
	root := projectRoot(t)
	loaded, err := cases.LoadSuiteContracts(filepath.Join(root, "eval", "cases", "skynex-orchestrator"))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) == 0 {
		t.Fatal("suite contains no v1 cases")
	}
	mutant := loaded[0]
	// A disabled qualitative judge has no runtime effect, while the published
	// interchange schema still bounds every serialized model label. This proves
	// commandValidate executes both independent layers without weakening the Go
	// executable-name contract.
	mutant.LLMJudge = &contracts.LLMJudge{Enabled: false, Model: strings.Repeat("x", 257)}
	if err := mutant.Validate(); err != nil {
		t.Fatalf("mutation unexpectedly rejected before schema validation: %v", err)
	}
	if err := validateV1CasesAgainstPublishedSchema(filepath.Join(root, "schemas"), []contracts.Case{mutant}); err == nil || !strings.Contains(err.Error(), "llm_judge") {
		t.Fatalf("published schema did not reject schema-only mutation: %v", err)
	}
}

func TestValidateSchemasCompilesEveryPublishedDraft202012Schema(t *testing.T) {
	const validSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`
	tests := []struct {
		name    string
		invalid string
	}{
		{
			name: "eval-result.schema.json",
			invalid: `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",` +
				`"properties":{"broken":{"$ref":"#/$defs/missing"}}}`,
		},
		{
			name: "eval-experiment.schema.json",
			invalid: `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",` +
				`"properties":{"broken":{"type":"array","maxItems":"one"}}}`,
		},
	}
	required := []string{"eval-case.schema.json", "eval-result.schema.json", "eval-experiment.schema.json"}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range required {
				document := validSchema
				if name == test.name {
					document = test.invalid
				}
				if err := os.WriteFile(filepath.Join(root, name), []byte(document), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := validateSchemas(root); err == nil || !strings.Contains(err.Error(), test.name) {
				t.Fatalf("invalid published schema was not rejected with its filename: %v", err)
			}
		})
	}
}

func TestPublishedSchemasAcceptCanonicalV1Instances(t *testing.T) {
	root := projectRoot(t)
	loaded, err := cases.LoadSuiteContracts(filepath.Join(root, "eval", "cases", "skynex-orchestrator"))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) == 0 {
		t.Fatal("suite contains no v1 cases")
	}
	publicCasesDigest, err := publicCaseSetDigest(loaded)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	manifest := experiment.Manifest{
		SchemaVersion: experiment.SchemaVersion,
		Intent:        experiment.IntentDevelopment,
		ID:            "schema-test",
		Suite:         "skynex-orchestrator",
		Harness:       experiment.FrozenBundle{Root: "harness", Digest: digest},
		Control:       experiment.FrozenBundle{Root: "control", Digest: digest},
		Candidate:     experiment.FrozenBundle{Root: "candidate", Digest: "sha256:" + strings.Repeat("b", 64)},
		IntentionalDifferences: []baseline.Field{
			baseline.FieldPromptDigest, baseline.FieldAgentBundleDigest,
		},
		PublicCaseCount:   19,
		PublicCasesDigest: publicCasesDigest,
		CriticalCaseIDs:   declaredCriticalCaseIDs(loaded),
		HoldoutCaseCount:  0,
		Runs:              2,
		Randomization: experiment.Randomization{
			Method: "balanced-blocked-ab-ba", Seed: "1", SerializeWithinBlock: true,
		},
		Execution: experiment.Execution{
			Mode: string(contracts.ExecutionTrustedLocal), Network: string(contracts.NetworkHostUnisolated),
			Concurrency: 1, OpenCodeVersion: defaultOpenCodeVersion,
			ProviderAuth:          experiment.ProviderAuthOpenAIOAuthCleanProfileV1,
			BillingMode:           experiment.BillingModeChatGPTSubscription,
			CredentialBoundary:    experiment.CredentialBoundaryRuntimeReadable,
			EvaluatorBinaryDigest: digest, OpenCodeBinaryDigest: digest, OpenCodeOpenAPIDigest: digest,
			ToolchainsDigest: digest,
		},
		Gates: experiment.Gates{
			CriticalCasePassRate: 1, PassToFailRegressions: 0, ScopeViolations: 0, FalseSuccesses: 0,
			MaxParentPeakInputRatio: 1, MaxTreeInputRatio: 1, MaxCostRatio: 1,
			MaxWallTimeRatio: 1, MaxRetryRateRatio: 1, Confidence: .95, MinimumPairs: 2,
		},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("canonical manifest is semantically invalid: %v", err)
	}

	instances := []struct {
		schema string
		value  any
	}{
		{schema: "eval-case.schema.json", value: loaded[0]},
		{schema: "eval-result.schema.json", value: validRun("schema-run", "candidate", contracts.RunStatusPass)},
		{schema: "eval-experiment.schema.json", value: manifest},
	}
	for _, test := range instances {
		t.Run(test.schema, func(t *testing.T) {
			schema, err := compilePublishedSchema(filepath.Join(root, "schemas", test.schema))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(instance); err != nil {
				t.Fatalf("canonical instance violates published schema: %v", err)
			}
		})
	}

	experimentSchema, err := compilePublishedSchema(filepath.Join(root, "schemas", "eval-experiment.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	release := manifest
	release.Intent = experiment.IntentRelease
	release.Runs = experiment.MinimumReleaseRuns
	release.Gates.CriticalCasePassRate = 1
	release.Gates.PassToFailRegressions = 0
	release.Gates.ScopeViolations = 0
	release.Gates.FalseSuccesses = 0
	release.Gates.MaxParentPeakInputRatio = 0.7
	release.Gates.MaxTreeInputRatio = 1
	release.Gates.MaxCostRatio = 1
	release.Gates.MaxWallTimeRatio = 1.1
	release.Gates.MaxRetryRateRatio = 1
	release.Gates.Confidence = 0.95
	release.Gates.MinimumPairs = experiment.MinimumReleaseRuns
	release.Holdout = &experiment.FrozenBundle{Root: "holdout", Digest: digest}
	release.HoldoutCaseCount = 1
	rawRelease, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	releaseInstance, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawRelease))
	if err != nil {
		t.Fatal(err)
	}
	if err := experimentSchema.Validate(releaseInstance); err == nil || !strings.Contains(err.Error(), "credential_boundary") {
		t.Fatalf("published schema accepted release with runtime-readable credentials: %v", err)
	}
	release.Execution.CredentialBoundary = experiment.CredentialBoundaryProviderProxy
	release.Execution.Mode = "isolated-container"
	release.Execution.Network = "provider-proxy-only"
	release.Execution.ContainerImageDigest = digest
	rawRelease, err = json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	releaseInstance, err = jsonschema.UnmarshalJSON(bytes.NewReader(rawRelease))
	if err != nil {
		t.Fatal(err)
	}
	if err := experimentSchema.Validate(releaseInstance); err != nil {
		t.Fatalf("published schema rejected release provider-proxy boundary: %v", err)
	}
}

func TestResultSchemaRequiresParentFirstInputEvidence(t *testing.T) {
	root := projectRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "schemas", "eval-result.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Defs map[string]struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	tokenUsage, ok := document.Defs["tokenUsage"]
	if !ok {
		t.Fatal("eval-result schema lacks tokenUsage definition")
	}
	if _, ok := tokenUsage.Properties["first_input_tokens"]; !ok {
		t.Fatal("tokenUsage schema lacks first_input_tokens")
	}
	foundRequired := false
	for _, field := range tokenUsage.Required {
		if field == "first_input_tokens" {
			foundRequired = true
			break
		}
	}
	if !foundRequired {
		t.Fatal("tokenUsage.first_input_tokens is not required")
	}
}

func TestResultSchemaRejectsPassingCheckWithError(t *testing.T) {
	root := projectRoot(t)
	schema, err := compilePublishedSchema(filepath.Join(root, "schemas", "eval-result.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	result := validRun("schema-pass-error", "candidate", contracts.RunStatusPass)
	result.Checks[0].Error = &contracts.RunError{Kind: "judge_error", Message: "failed", Retryable: false}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(instance); err == nil {
		t.Fatal("published schema accepted a passing check with an error")
	}
}

func TestExperimentSchemaRequiresIntentAndReleaseEvidence(t *testing.T) {
	root := projectRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "schemas", "eval-experiment.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
		AllOf []json.RawMessage `json:"allOf"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{}
	for _, field := range document.Required {
		required[field] = true
	}
	if !required["intent"] || strings.Join(document.Properties["intent"].Enum, ",") != "development,release" {
		t.Fatalf("experiment schema does not freeze intent: required=%v enum=%v", required["intent"], document.Properties["intent"].Enum)
	}
	conditional, err := json.Marshal(document.AllOf)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range [][]byte{[]byte(`"release"`), []byte(`"holdout"`), []byte(`"minimum":10`)} {
		if !bytes.Contains(conditional, token) {
			t.Fatalf("release schema condition lacks %s: %s", token, conditional)
		}
	}
}

func TestCompareAndReportAreOffline(t *testing.T) {
	directory := t.TempDir()
	controlPath := filepath.Join(directory, "control.json")
	candidatePath := filepath.Join(directory, "candidate.json")
	fingerprint := validFingerprint()
	created := time.Unix(1_700_000_000, 0).UTC()
	writeArtifact(t, controlPath, "control", fingerprint, validRun("control-run", "control", contracts.RunStatusPass), created)
	writeArtifact(t, candidatePath, "candidate", fingerprint, validRun("candidate-run", "candidate", contracts.RunStatusPass), created)

	modelCalls := 0
	deps := dependencies{runModel: func(context.Context, modelRunSpec) (modelRunResult, error) {
		modelCalls++
		return modelRunResult{}, errors.New("offline command invoked runner")
	}}
	for _, args := range [][]string{{"report", "--input", candidatePath}} {
		var stdout, stderr bytes.Buffer
		exit := runCLI(context.Background(), args, deps, &stdout, &stderr)
		if exit != contracts.ExitSuccess {
			t.Fatalf("%v exit=%d stdout=%s stderr=%s", args, exit, stdout.String(), stderr.String())
		}
		if !json.Valid(stdout.Bytes()) {
			t.Fatalf("%v emitted invalid JSON: %s", args, stdout.String())
		}
	}
	var stdout, stderr bytes.Buffer
	exit := runCLI(context.Background(), []string{
		"compare", "--control", controlPath, "--candidate", candidatePath,
	}, deps, &stdout, &stderr)
	if exit != contracts.ExitInvalid || !json.Valid(stdout.Bytes()) {
		t.Fatalf("compare without manifest exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	var output envelope
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.Error == nil || output.Error.Kind != "manifest_required" {
		t.Fatalf("compare without manifest did not fail closed: %+v", output)
	}
	if modelCalls != 0 {
		t.Fatalf("offline commands invoked runner %d times", modelCalls)
	}
}

func TestCompareHelpRequiresManifest(t *testing.T) {
	commands, ok := usageDocument()["commands"].(map[string]string)
	if !ok {
		t.Fatal("usage commands have unexpected type")
	}
	if !strings.Contains(commands["compare"], "--manifest PATH") || strings.Contains(commands["compare"], "[--manifest PATH]") {
		t.Fatalf("compare usage does not require a manifest: %q", commands["compare"])
	}
}

func TestHelpDoesNotAdvertiseUSDCapForOAuthSubscription(t *testing.T) {
	runtimeOptions, ok := usageDocument()["runtime_options"].([]string)
	if !ok {
		t.Fatal("usage runtime_options have unexpected type")
	}
	limitations, ok := usageDocument()["limitations"].([]string)
	if !ok {
		t.Fatal("usage limitations have unexpected type")
	}
	if !strings.Contains(strings.Join(runtimeOptions, "\n"), "unsupported with --openai-oauth") {
		t.Fatalf("cost-cap OAuth limitation is missing: %v", runtimeOptions)
	}
	joined := strings.Join(limitations, "\n")
	if !strings.Contains(joined, "no authoritative per-request USD") ||
		!strings.Contains(joined, "bound scheduled work") ||
		!strings.Contains(joined, "not provider calls/tokens/quota") {
		t.Fatalf("subscription quota/cost semantics are missing: %v", limitations)
	}
}

func TestDirectEvidenceDigestIsPathAndVariantIndependent(t *testing.T) {
	makeSpec := func(variant, casesDir, fixturesDir, agentBundle string) modelRunSpec {
		t.Helper()
		set := newFlagSet("test-direct-digest")
		flags := bindModelFlags(set)
		if err := parseFlagSet(set, []string{
			"--cases-dir", casesDir, "--fixtures-dir", fixturesDir, "--agent-bundle", agentBundle,
		}); err != nil {
			t.Fatal(err)
		}
		spec, err := buildModelSpec(flags, variant, 1, false)
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}
	control := makeSpec("control", "relative/cases", "/tmp/fixtures-a", "bundle-control")
	candidate := makeSpec("candidate", "/different/cases", "fixtures-b", "/different/bundle-candidate")
	if control.ManifestDigest != candidate.ManifestDigest {
		t.Fatalf("exploratory provenance depends on path spelling or arm: %s != %s", control.ManifestDigest, candidate.ManifestDigest)
	}
	if !contracts.IsDigest(control.ManifestDigest) {
		t.Fatalf("exploratory provenance digest is invalid: %q", control.ManifestDigest)
	}
}

func TestManifestConformanceRejectsExploratoryArtifact(t *testing.T) {
	manifestDigest := "sha256:" + strings.Repeat("a", 64)
	artifact := &baseline.Artifact{
		Label: "control",
		Aggregates: map[string]json.RawMessage{
			evaluationAuthorityAggregateKey: encodeEvaluationAuthority(evaluationAuthorityMetadata{
				Mode: evaluationAuthorityExploratory, Reason: "not frozen",
			}),
		},
	}
	if err := requireFrozenManifestAuthority(artifact, manifestDigest, experiment.IntentDevelopment); err == nil || !strings.Contains(err.Error(), "exploratory") {
		t.Fatalf("exploratory artifact accepted as authoritative: %v", err)
	}
	artifact.Aggregates[evaluationAuthorityAggregateKey] = encodeEvaluationAuthority(evaluationAuthorityMetadata{
		Mode: evaluationAuthorityDevelopment, Intent: experiment.IntentDevelopment, ManifestDigest: manifestDigest,
	})
	if err := requireFrozenManifestAuthority(artifact, manifestDigest, experiment.IntentDevelopment); err != nil {
		t.Fatalf("matching frozen authority rejected: %v", err)
	}
	if err := requireFrozenManifestAuthority(artifact, manifestDigest, experiment.IntentRelease); err == nil {
		t.Fatal("development artifact accepted as release authority")
	}
}

func TestArtifactFingerprintBindingCoversPublicRuntimeProvenance(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	otherDigest := "sha256:" + strings.Repeat("c", 64)
	samples := []contracts.RunResult{
		validRun("binding-1", "candidate", contracts.RunStatusPass),
		validRun("binding-2", "candidate", contracts.RunStatusPass),
	}
	for index := range samples {
		samples[index].Provenance.Extensions = map[string]string{
			provenanceExtensionAgentBundleDigest:     digest,
			provenanceExtensionHarnessBundleDigest:   "sha256:" + strings.Repeat("b", 64),
			provenanceExtensionManifestDigest:        "sha256:" + strings.Repeat("b", 64),
			provenanceExtensionEffectiveAgentsDigest: digest,
			provenanceExtensionEffectiveConfigDigest: digest,
			provenanceExtensionToolchainsDigest:      "sha256:" + strings.Repeat("b", 64),
		}
	}
	samples[1].Repetition = 2
	unobservedCatalog, err := contracts.CanonicalDigest(map[string]string{"status": "unobserved"})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := validFingerprint()
	fingerprint.PromptDigest = digest
	fingerprint.AgentBundleDigest = digest
	fingerprint.OpenCodeOpenAPIDigest = digest
	fingerprint.ToolsetDigest = digest
	fingerprint.EffectiveConfigDigest = digest
	fingerprint.EffectiveAgentsDigest = digest
	fingerprint.JudgesDigest = digest
	fingerprint.PricingTableDigest = digest
	fingerprint.ProviderCatalogDigest = unobservedCatalog
	artifact := &baseline.Artifact{Label: "candidate", Fingerprint: fingerprint}
	if err := validateArtifactFingerprintBinding(artifact, samples); err != nil {
		t.Fatalf("matching public samples were rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*baseline.Fingerprint)
	}{
		{name: "opencode version", mutate: func(value *baseline.Fingerprint) { value.OpenCodeVersion = "9.9.9" }},
		{name: "opencode API", mutate: func(value *baseline.Fingerprint) { value.OpenCodeOpenAPIDigest = otherDigest }},
		{name: "effective config", mutate: func(value *baseline.Fingerprint) { value.EffectiveConfigDigest = otherDigest }},
		{name: "effective agents", mutate: func(value *baseline.Fingerprint) { value.EffectiveAgentsDigest = otherDigest }},
		{name: "toolchains", mutate: func(value *baseline.Fingerprint) { value.ToolchainsDigest = otherDigest }},
		{name: "model", mutate: func(value *baseline.Fingerprint) { value.Model = "openai/other" }},
		{name: "provider", mutate: func(value *baseline.Fingerprint) { value.Provider = "other" }},
		{name: "toolset", mutate: func(value *baseline.Fingerprint) { value.ToolsetDigest = otherDigest }},
		{name: "execution", mutate: func(value *baseline.Fingerprint) { value.ExecutionMode = string(contracts.ExecutionIsolatedContainer) }},
		{name: "network", mutate: func(value *baseline.Fingerprint) { value.NetworkPolicy = string(contracts.NetworkNone) }},
		{name: "auth", mutate: func(value *baseline.Fingerprint) {
			value.ProviderAuthMode = contracts.ProviderAuthModeOpenAIOAuthCleanProfileV1
		}},
		{name: "billing", mutate: func(value *baseline.Fingerprint) { value.BillingMode = contracts.BillingModeChatGPTSubscription }},
		{name: "credential boundary", mutate: func(value *baseline.Fingerprint) {
			value.CredentialBoundary = contracts.CredentialBoundaryRuntimeReadable
		}},
		{name: "auth isolation", mutate: func(value *baseline.Fingerprint) {
			value.AuthIsolation = contracts.AuthIsolationDedicatedFreshTokenFailStopV1
		}},
		{name: "provider catalog", mutate: func(value *baseline.Fingerprint) { value.ProviderCatalogDigest = otherDigest }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mismatched := fingerprint
			test.mutate(&mismatched)
			if err := mismatched.Validate(); err != nil {
				t.Fatalf("test did not retain a structurally valid fingerprint: %v", err)
			}
			if err := validateArtifactFingerprintBinding(&baseline.Artifact{Label: "candidate", Fingerprint: mismatched}, samples); err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("fingerprint/sample divergence was accepted: %v", err)
			}
		})
	}

	mixed := append([]contracts.RunResult(nil), samples...)
	mixed[1].Provenance.OpenCodeVersion = "1.18.15"
	if err := validateArtifactFingerprintBinding(artifact, mixed); err == nil || !strings.Contains(err.Error(), "non-uniform opencode_version") {
		t.Fatalf("mixed public values were accepted: %v", err)
	}
	mixed = append([]contracts.RunResult(nil), samples...)
	mixed[1].Provenance.Extensions = map[string]string{
		provenanceExtensionAgentBundleDigest:               digest,
		provenanceExtensionHarnessBundleDigest:             fingerprint.HarnessBundleDigest,
		provenanceExtensionManifestDigest:                  fingerprint.ExperimentManifestDigest,
		contracts.ProvenanceExtensionProviderCatalogDigest: otherDigest,
		provenanceExtensionEffectiveAgentsDigest:           digest,
		provenanceExtensionEffectiveConfigDigest:           digest,
		provenanceExtensionToolchainsDigest:                fingerprint.ToolchainsDigest,
	}
	if err := validateArtifactFingerprintBinding(artifact, mixed); err == nil || !strings.Contains(err.Error(), "non-uniform provider_catalog_digest presence") {
		t.Fatalf("mixed provider-catalog presence was accepted: %v", err)
	}

	withBundleBindings := append([]contracts.RunResult(nil), samples...)
	for index := range withBundleBindings {
		withBundleBindings[index].Provenance.Extensions = map[string]string{
			provenanceExtensionAgentBundleDigest:     fingerprint.AgentBundleDigest,
			provenanceExtensionHarnessBundleDigest:   fingerprint.HarnessBundleDigest,
			provenanceExtensionManifestDigest:        fingerprint.ExperimentManifestDigest,
			provenanceExtensionEffectiveAgentsDigest: fingerprint.EffectiveAgentsDigest,
			provenanceExtensionEffectiveConfigDigest: samples[index].Provenance.ConfigDigest,
			provenanceExtensionToolchainsDigest:      fingerprint.ToolchainsDigest,
		}
	}
	if err := validateArtifactFingerprintBinding(artifact, withBundleBindings); err != nil {
		t.Fatalf("matching bundle extensions were rejected: %v", err)
	}
	withBundleBindings[1].Provenance.Extensions[provenanceExtensionHarnessBundleDigest] = otherDigest
	if err := validateArtifactFingerprintBinding(artifact, withBundleBindings); err == nil || !strings.Contains(err.Error(), "non-uniform harness_bundle_digest") {
		t.Fatalf("mixed bundle extension was accepted: %v", err)
	}
}

func TestExperimentPopulationRequiresEveryFrozenPair(t *testing.T) {
	control := []contracts.RunResult{validRun("control-1", "control", contracts.RunStatusPass)}
	candidate := []contracts.RunResult{validRun("candidate-1", "candidate", contracts.RunStatusPass)}
	if gate := experimentPopulationGate(control, candidate, 2, 1); gate.Status != gates.StatusInvalid {
		t.Fatalf("incomplete frozen population passed: %+v", gate)
	}
	control = append(control, validRun("control-2", "control", contracts.RunStatusPass))
	control[1].Repetition = 2
	candidate = append(candidate, validRun("candidate-2", "candidate", contracts.RunStatusPass))
	candidate[1].Repetition = 2
	if gate := experimentPopulationGate(control, candidate, 2, 1); gate.Status != gates.StatusPass {
		t.Fatalf("complete frozen population rejected: %+v", gate)
	}
	if gate := experimentPopulationGate(control, candidate, 2, 2); gate.Status != gates.StatusInvalid || !strings.Contains(gate.Detail, "manifest committed 2") {
		t.Fatalf("self-consistent subset passed a two-case manifest: %+v", gate)
	}
}

func TestResultSetExitUsesStableStatusClasses(t *testing.T) {
	tests := []struct {
		status contracts.RunStatus
		exit   int
	}{
		{contracts.RunStatusPass, 0}, {contracts.RunStatusFail, 1}, {contracts.RunStatusInvalid, 2},
		{contracts.RunStatusInconclusive, 3}, {contracts.RunStatusAborted, 4},
		{contracts.RunStatusInfraError, 5}, {contracts.RunStatusBudgetExhausted, 6},
	}
	for _, test := range tests {
		if got := resultSetExit([]contracts.RunResult{{Status: test.status}}); got != test.exit {
			t.Fatalf("status %s: got %d, want %d", test.status, got, test.exit)
		}
	}
	if got := resultSetExit([]contracts.RunResult{{Status: contracts.RunStatusInconclusive}, {Status: contracts.RunStatusFail}}); got != contracts.ExitFailed {
		t.Fatalf("fail did not outrank inconclusive: exit %d", got)
	}
}

func TestContextTerminationOutranksInfrastructureWrapper(t *testing.T) {
	tests := []struct {
		name string
		err  error
		exit int
		kind string
	}{
		{name: "canceled", err: context.Canceled, exit: contracts.ExitAborted, kind: "aborted"},
		{name: "deadline", err: context.DeadlineExceeded, exit: contracts.ExitBudgetExhausted, kind: "budget_exhausted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exit, kind := classifyCommandError(infraf("wrapped_infrastructure", test.err))
			if exit != test.exit || kind != test.kind {
				t.Fatalf("cause %v classified as exit=%d kind=%q, want %d/%q", test.err, exit, kind, test.exit, test.kind)
			}
		})
	}
}

func TestRunCLIPreservesDataWhenContextTerminates(t *testing.T) {
	tests := []struct {
		name    string
		context func(t *testing.T) (context.Context, func())
		exit    int
		status  contracts.RunStatus
		kind    string
	}{
		{
			name: "canceled", exit: contracts.ExitAborted, status: contracts.RunStatusAborted, kind: "aborted",
			context: func(t *testing.T) (context.Context, func()) {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				return ctx, cancel
			},
		},
		{
			name: "deadline", exit: contracts.ExitBudgetExhausted, status: contracts.RunStatusBudgetExhausted, kind: "budget_exhausted",
			context: func(t *testing.T) (context.Context, func()) {
				t.Helper()
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				return ctx, cancel
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, terminate := test.context(t)
			defer terminate()
			deps := dependencies{probeRuntime: func(context.Context, doctorOptions) (doctorResult, error) {
				terminate()
				return doctorResult{Healthy: true, Version: "partial-result", ModelCalls: 0}, nil
			}}
			var stdout, stderr bytes.Buffer
			exit := runCLI(ctx, []string{"doctor"}, deps, &stdout, &stderr)
			if exit != test.exit {
				t.Fatalf("exit=%d, want %d; stdout=%s stderr=%s", exit, test.exit, stdout.String(), stderr.String())
			}
			var output struct {
				Status string          `json:"status"`
				Data   json.RawMessage `json:"data"`
				Error  *errorBody      `json:"error"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatal(err)
			}
			var preserved doctorResult
			if err := json.Unmarshal(output.Data, &preserved); err != nil {
				t.Fatalf("terminal context discarded command data: %v; envelope=%s", err, stdout.String())
			}
			if output.Status != string(test.status) || output.Error == nil || output.Error.Kind != test.kind || preserved.Version != "partial-result" {
				t.Fatalf("terminal envelope = status %q error %+v data %+v", output.Status, output.Error, preserved)
			}
		})
	}
}

func TestSingleFrozenArtifactHasEvidenceOnlyAuthority(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	artifact := &baseline.Artifact{
		Label: "candidate",
		Aggregates: map[string]json.RawMessage{
			evaluationAuthorityAggregateKey: encodeEvaluationAuthority(evaluationAuthorityMetadata{
				Mode: evaluationAuthorityDevelopment, Intent: experiment.IntentDevelopment, ManifestDigest: digest,
			}),
		},
	}
	authority, err := reportedArtifactAuthority(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if authority != evaluationAuthorityDevelopment+"-evidence-only" {
		t.Fatalf("development artifact produced authority %q", authority)
	}
	artifact.Aggregates[evaluationAuthorityAggregateKey] = encodeEvaluationAuthority(evaluationAuthorityMetadata{
		Mode: evaluationAuthorityRelease, Intent: experiment.IntentRelease, ManifestDigest: digest,
	})
	if _, err := reportedArtifactAuthority(artifact); err == nil || !strings.Contains(err.Error(), "attestation") {
		t.Fatalf("self-claimed release artifact accepted: %v", err)
	}
}

func TestDeclaredCostCapFailsClosedWhenCostEvidenceIsMissing(t *testing.T) {
	result := modelRunResult{
		Result:     runner.ContractResult{Samples: []contracts.RunResult{validRun("run", "current", contracts.RunStatusPass)}},
		CostCapUSD: 1, CostEvidenceComplete: false,
	}
	if got := result.CLIExitCode(); got != contracts.ExitInvalid {
		t.Fatalf("got exit %d, want invalid", got)
	}
}

func TestSubscriptionCalculatedCostIsNotObservedSpend(t *testing.T) {
	sample := validRun("subscription-cost", "current", contracts.RunStatusPass)
	calculated := 12.34
	sample.Usage.Parent.CalculatedCostUSD = &calculated
	sample.Usage.Tree.CalculatedCostUSD = &calculated
	sample.Usage.Parent.ProviderCostUSD = nil
	sample.Usage.Tree.ProviderCostUSD = nil
	sample.Provenance.Extensions = map[string]string{
		contracts.ProvenanceExtensionBillingMode: contracts.BillingModeChatGPTSubscription,
	}
	if cost, available := observedSampleCost(sample); available || cost != 0 {
		t.Fatalf("counterfactual API cost became observed subscription spend: cost=%v available=%v", cost, available)
	}
}

func TestComparisonRejectsInvalidControlEvidence(t *testing.T) {
	result := evidenceValidityGate("control_evidence", []contracts.RunResult{{
		RunID: "control-invalid", Status: contracts.RunStatusInvalid,
	}})
	if result.Status != gates.StatusInvalid || result.Name != "control_evidence" {
		t.Fatalf("invalid control evidence was not rejected: %+v", result)
	}
}

func TestEvidenceValidityGateRejectsUnsupportedPassEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*contracts.RunResult)
	}{
		{name: "no hard checks", mutate: func(run *contracts.RunResult) { run.Checks = []contracts.CheckResult{} }},
		{name: "incomplete hard evidence", mutate: func(run *contracts.RunResult) { run.Evidence.Items[0].Complete = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := validRun("unsupported-pass", "candidate", contracts.RunStatusPass)
			test.mutate(&run)
			result := evidenceValidityGate("candidate_evidence", []contracts.RunResult{run})
			if result.Status != gates.StatusInvalid || result.Reason != gates.ReasonEvidenceInvalid {
				t.Fatalf("unsupported pass evidence reached gates: %+v", result)
			}
		})
	}
}

func TestHoldoutDiagnosticsRedactCaseAndBlockIDs(t *testing.T) {
	reference := "holdout_0001"
	references := map[string]struct{}{reference: {}}
	runs := []contracts.RunResult{{CaseID: reference, Repetition: 7}}
	redacted := redactHoldoutText("candidate is missing block "+reference+"-0007 for "+reference, runs, nil, references)
	if strings.Contains(redacted, reference) || !strings.Contains(redacted, "[holdout-block]") || !strings.Contains(redacted, "[holdout]") {
		t.Fatalf("holdout diagnostic was not redacted: %q", redacted)
	}
}

func TestABOutputTargetsAreNoClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.json")
	if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveABOutputs(path); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("existing result target was accepted: %v", err)
	}
}

func TestABOutputReservationRejectsTargetReplacedDuringCampaign(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "campaign.control.json")
	unused := filepath.Join(directory, "campaign.partial.json")
	reservations, err := reserveABOutputs(target, unused)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reservations.Close() }()

	if _, err := reserveABOutputs(target); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("concurrent reservation accepted: %v", err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("external evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(directory, "staged.json")
	if err := os.WriteFile(staged, []byte(`{"owned":"result"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reservations.PublishStaged(target, staged); err == nil || !strings.Contains(err.Error(), "retire owned result placeholder") {
		t.Fatalf("replaced target was accepted: %v", err)
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "external evidence" {
		t.Fatalf("external target was overwritten: %q", contents)
	}
	if err := reservations.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unused); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unused reservation was not cleaned up: %v", err)
	}
}

func TestABOutputLocationsMustBeOutsideEveryFrozenBundle(t *testing.T) {
	directory := t.TempDir()
	bundles := make([]experiment.VerifiedBundle, 0, 4)
	for _, name := range []string{"harness", "control", "candidate", "holdout"} {
		root := filepath.Join(directory, name)
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		bundles = append(bundles, experiment.VerifiedBundle{Name: name, AbsoluteRoot: root})
	}
	for _, bundle := range bundles {
		if _, err := validateABOutputLocations(filepath.Join(bundle.AbsoluteRoot, "results", "ab"), bundles); err == nil || !strings.Contains(err.Error(), "outside frozen "+bundle.Name) {
			t.Fatalf("output inside frozen %s was accepted: %v", bundle.Name, err)
		}
	}

	outside := filepath.Join(directory, "results", "ab")
	paths, err := validateABOutputLocations(outside, bundles)
	if err != nil {
		t.Fatalf("outside output rejected: %v", err)
	}
	if paths.Control != outside+".control.json" || paths.Comparison != outside+".comparison.json" {
		t.Fatalf("unexpected output paths: %+v", paths)
	}
}

func TestABOutputLocationsRejectSymlinkAliasIntoFrozenBundle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available without privileges on Windows")
	}
	directory := t.TempDir()
	harness := filepath.Join(directory, "harness")
	if err := os.Mkdir(harness, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "alias")
	if err := os.Symlink(harness, alias); err != nil {
		t.Fatal(err)
	}
	bundles := []experiment.VerifiedBundle{{Name: "harness", AbsoluteRoot: harness}}
	if _, err := validateABOutputLocations(filepath.Join(alias, "results", "ab"), bundles); err == nil || !strings.Contains(err.Error(), "outside frozen harness") {
		t.Fatalf("symlinked output inside frozen harness was accepted: %v", err)
	}
}

func TestConfigBundleExcludesGeneratedDependenciesAndRejectsCredentials(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "opencode.json"), []byte(`{"agent":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/does/not/matter", filepath.Join(source, "node_modules", "generated-link")); err != nil {
		t.Fatal(err)
	}
	if err := copyConfigBundle(source, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "opencode.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated dependencies were copied: %v", err)
	}

	credentialSource := t.TempDir()
	if err := os.WriteFile(filepath.Join(credentialSource, "opencode.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialSource, ".env"), []byte("TOKEN=secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyConfigBundle(credentialSource, t.TempDir()); err == nil || !strings.Contains(err.Error(), "credential-like") {
		t.Fatalf("credential-like bundle was accepted: %v", err)
	}
	if err := rejectCredentialLikeTree(credentialSource); err == nil || !strings.Contains(err.Error(), "credential-like") {
		t.Fatalf("credential-like frozen A/B bundle was accepted: %v", err)
	}
}

func TestPinnedRuntimeRejectsBinaryDriftBeforeStartingRuntime(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(binary, []byte("\x7fELF-not-the-frozen-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	started := false
	factory := pinnedRuntimeFactory{
		Inner: runtimeFactoryFunc(func(context.Context, runner.RuntimeRequest) (runner.Runtime, error) {
			started = true
			return nil, errors.New("must not start")
		}),
		Binary: binary, ExpectedBinaryDigest: "sha256:" + strings.Repeat("0", 64),
	}
	if _, err := factory.Start(context.Background(), runner.RuntimeRequest{}); err == nil || !strings.Contains(err.Error(), "binary digest mismatch") {
		t.Fatalf("unexpected pin result: %v", err)
	}
	if started {
		t.Fatal("runtime started after the binary pin failed")
	}
}

func TestOpenCodeBinaryDigestIncludesCmdShimTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "opencode.exe")
	launcher := filepath.Join(directory, "opencode")
	if err := os.WriteFile(target, []byte("\x7fELF-target-v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\n\"" + target + "\" \"$@\"\n# cmd-shim-target=" + target + "\n"
	if err := os.WriteFile(launcher, []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveOpenCodeBinary(launcher)
	if err != nil {
		t.Fatal(err)
	}
	first := resolved.Digest
	if err := os.WriteFile(target, []byte("\x7fELF-target-v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Revalidate(); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("cmd-shim target snapshot accepted drift: %v", err)
	}
	second, err := binaryDigest(launcher)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("cmd-shim target drift left closure digest unchanged: %s", first)
	}
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := binaryDigest(launcher); err == nil || !strings.Contains(err.Error(), "without a pinned cmd-shim target") {
		t.Fatalf("unbound launcher was accepted: %v", err)
	}
}

func TestResolvedOpenCodeBinaryExecutesFromDivergentWorkingDirectory(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	absoluteBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	relativeBinary, err := filepath.Rel(workingDirectory, absoluteBinary)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveOpenCodeBinary(relativeBinary)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved.Path) || filepath.Clean(resolved.Path) != resolved.Path {
		t.Fatalf("resolved binary path is not canonical and absolute: %q", resolved.Path)
	}
	command := exec.Command(resolved.Path, "-test.run=^$")
	command.Dir = t.TempDir()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("resolved binary failed from divergent cwd: %v\n%s", err, output)
	}
}

func TestResolvedOpenCodeBinaryRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	link := filepath.Join(directory, "opencode")
	if err := os.Symlink(os.Args[0], link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveOpenCodeBinary(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink --binary was accepted: %v", err)
	}
}

func TestResolvedOpenCodeBinaryDetectsAtomicReplacement(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "opencode")
	if err := os.WriteFile(binary, []byte("\x7fELF-frozen-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveOpenCodeBinary(binary)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(replacement, []byte("\x7fELF-replacement-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, binary); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Revalidate(); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("atomic --binary replacement was accepted: %v", err)
	}
}

func TestDeferredEngineSampleIsRetainedFailClosed(t *testing.T) {
	sample := validRun("paid-sample", "candidate", contracts.RunStatusPass)
	retained, err := prepareDeferredEngineSample(sample)
	if err != nil {
		t.Fatal(err)
	}
	if retained == nil {
		t.Fatal("valid paid sample was discarded after deferred cleanup error")
	}
	if retained.Status != contracts.RunStatusInfraError || retained.Error == nil || retained.Error.Kind != "deferred_runner_error" {
		t.Fatalf("deferred sample remained successful: %+v", retained)
	}
	if err := retained.Validate(); err != nil {
		t.Fatalf("retained deferred sample is invalid: %v", err)
	}
}

func TestDeferredEngineSampleDiscardsInvalidAndIntegrityDrift(t *testing.T) {
	malformed := validRun("malformed", "candidate", contracts.RunStatusPass)
	malformed.RunID = ""
	if retained, err := prepareDeferredEngineSample(malformed); retained != nil || err == nil {
		t.Fatalf("malformed sample retained=%+v error=%v", retained, err)
	}

	for _, kind := range []string{"toolchain_drift", "evaluation"} {
		drift := validRun("drift-"+kind, "candidate", contracts.RunStatusPass)
		drift.Status = contracts.RunStatusInvalid
		drift.Error = &contracts.RunError{Kind: kind, Message: "integrity drift"}
		if err := drift.Validate(); err != nil {
			t.Fatalf("test drift sample is not valid: %v", err)
		}
		if retained, err := prepareDeferredEngineSample(drift); retained != nil || err != nil {
			t.Fatalf("invalid %s sample retained=%+v error=%v", kind, retained, err)
		}
	}
}

func TestDeferredPrivateCleanupMarksReturnedModelResult(t *testing.T) {
	cost := 0.01
	invalid := validRun("invalid-sample", "candidate", contracts.RunStatusPass)
	invalid.Status = contracts.RunStatusInvalid
	invalid.Error = &contracts.RunError{Kind: "evaluation", Message: "bundle drift"}
	run := modelRunResult{
		Result: runner.ContractResult{
			Suite: "suite", Complete: true,
			Samples: []contracts.RunResult{validRun("paid-sample", "candidate", contracts.RunStatusPass), invalid},
		},
		CostEvidenceComplete:  true,
		PublishedObservedCost: &cost,
	}
	markDeferredModelRunFailure(&run)
	if run.Result.Complete || run.CostEvidenceComplete || run.PublishedObservedCost != nil {
		t.Fatalf("deferred cleanup left model result complete: %+v", run)
	}
	if len(run.Result.Samples) != 1 {
		t.Fatalf("private cleanup retained invalid samples: %+v", run.Result.Samples)
	}
	if sample := run.Result.Samples[0]; sample.Status != contracts.RunStatusInfraError || sample.Error == nil || sample.Error.Kind != "deferred_runner_error" {
		t.Fatalf("private cleanup retained PASS: %+v", sample)
	}
}

func TestPreparedAgentBundleRevalidationDetectsDrift(t *testing.T) {
	bundle := t.TempDir()
	path := filepath.Join(bundle, "opencode.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := sandbox.DigestTree(bundle, sandbox.DefaultSnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := revalidatePreparedAgentBundle(bundle, snapshot.Digest); err != nil {
		t.Fatalf("unchanged bundle rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("{\"drift\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := revalidatePreparedAgentBundle(bundle, snapshot.Digest); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("bundle drift accepted: %v", err)
	}
}

func TestModelNeutralCaseDigestAllowsOnlyModelChanges(t *testing.T) {
	repository := projectRoot(t)
	loaded, err := cases.LoadContract(filepath.Join(repository, "eval", "cases", "skynex-orchestrator", "skx_low_direct.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	control := *loaded
	candidate := *loaded
	candidate.Agent.Model = "anthropic/candidate-model"
	if controlDigest, candidateDigest := modelNeutralCaseDigest([]contracts.Case{control}), modelNeutralCaseDigest([]contracts.Case{candidate}); controlDigest != candidateDigest {
		t.Fatalf("model-only change altered neutral case digest: %s != %s", controlDigest, candidateDigest)
	}
	candidate.Input += " materially changed contract"
	if controlDigest, candidateDigest := modelNeutralCaseDigest([]contracts.Case{control}), modelNeutralCaseDigest([]contracts.Case{candidate}); controlDigest == candidateDigest {
		t.Fatalf("non-model contract change was hidden by neutralization: %s", controlDigest)
	}
}

func TestAllCompletedMetricReportUsesSamePairsAsGate(t *testing.T) {
	control := validRun("control-fail", "control", contracts.RunStatusFail)
	candidate := validRun("candidate-fail", "candidate", contracts.RunStatusFail)
	control.Coordination.Retries = 1
	candidate.Coordination.Retries = 2
	definition := metricDefinition{
		name: "retry_rate", extractor: stats.MetricRetries, requireTelemetry: true,
		scope: stats.ScopeAllCompleted, estimator: stats.EstimatorMean,
	}
	pairs, reportPairs, err := buildMetricPairs([]contracts.RunResult{control}, []contracts.RunResult{candidate}, definition)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || len(reportPairs) != 1 || reportPairs[0].Control != 1 || reportPairs[0].Candidate != 2 {
		t.Fatalf("gate/report pair populations diverged: gates=%#v report=%#v", pairs, reportPairs)
	}
}

func TestFingerprintBindingCanonicalizesPerCasePoliciesAndEffectiveDocuments(t *testing.T) {
	first := validRun("run-a", "candidate", contracts.RunStatusPass)
	second := validRun("run-b", "candidate", contracts.RunStatusPass)
	second.CaseID = "case-b"
	second.Repetition = 2
	second.Provenance.ToolsetDigest = "sha256:" + strings.Repeat("c", 64)
	second.Provenance.ConfigDigest = "sha256:" + strings.Repeat("d", 64)
	secondAgentsDigest := "sha256:" + strings.Repeat("e", 64)
	samples := []contracts.RunResult{first, second}
	fingerprint := validFingerprint()
	fingerprint.AgentBundleDigest = first.Provenance.PromptDigest
	for index := range samples {
		samples[index].Provenance.Extensions = map[string]string{
			provenanceExtensionAgentBundleDigest:     fingerprint.AgentBundleDigest,
			provenanceExtensionHarnessBundleDigest:   fingerprint.HarnessBundleDigest,
			provenanceExtensionManifestDigest:        fingerprint.ExperimentManifestDigest,
			provenanceExtensionEffectiveAgentsDigest: fingerprint.EffectiveAgentsDigest,
			provenanceExtensionEffectiveConfigDigest: samples[index].Provenance.ConfigDigest,
			provenanceExtensionToolchainsDigest:      fingerprint.ToolchainsDigest,
		}
	}
	samples[1].Provenance.Extensions[provenanceExtensionEffectiveAgentsDigest] = secondAgentsDigest
	fingerprint.PromptDigest = first.Provenance.PromptDigest
	fingerprint.OpenCodeOpenAPIDigest = first.Provenance.OpenCodeAPIDigest
	fingerprint.EffectiveConfigDigest = uniformDigest(samples, func(sample contracts.RunResult) string { return sample.Provenance.ConfigDigest })
	fingerprint.EffectiveAgentsDigest = uniformDigest(samples, func(sample contracts.RunResult) string {
		return sample.Provenance.Extensions[provenanceExtensionEffectiveAgentsDigest]
	})
	fingerprint.ToolsetDigest = uniformDigest(samples, func(sample contracts.RunResult) string { return sample.Provenance.ToolsetDigest })
	fingerprint.JudgesDigest = first.Provenance.JudgeDigest
	fingerprint.PricingTableDigest = first.Provenance.PricingTableDigest
	fingerprint.ProviderCatalogDigest, _ = contracts.CanonicalDigest(map[string]string{"status": "unobserved"})
	if err := validateFingerprintSampleBinding(fingerprint, samples, "heterogeneous tool policy test"); err != nil {
		t.Fatalf("canonical per-case runtime binding rejected: %v", err)
	}
}

func TestFrozenCaseBindingRejectsSubstitutedPublicIdentity(t *testing.T) {
	repository := projectRoot(t)
	loaded, err := cases.LoadContract(filepath.Join(repository, "eval", "cases", "skynex-orchestrator", "skx_low_direct.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	first := *loaded
	second := first
	second.ID = "substitutable_public_case"
	second.Critical = false
	firstDigest, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := second.Digest()
	if err != nil {
		t.Fatal(err)
	}
	firstRun := validRun("first", "candidate", contracts.RunStatusPass)
	firstRun.CaseID, firstRun.Provenance.CaseDigest = first.ID, firstDigest
	firstRun.Provenance.FixtureDigest = first.Fixture.ExpectedDigest
	firstRun.Provenance.Model = first.Agent.Model
	firstRun.Provenance.Provider, _, _ = contracts.ParseModelSelection(first.Agent.Model)
	firstRun.Provenance.Extensions = map[string]string{"x-observed-provider": firstRun.Provenance.Provider, "x-observed-model": strings.TrimPrefix(first.Agent.Model, firstRun.Provenance.Provider+"/")}
	secondRun := validRun("second", "candidate", contracts.RunStatusPass)
	secondRun.CaseID, secondRun.Repetition, secondRun.Provenance.CaseDigest = second.ID, 2, secondDigest
	secondRun.Provenance.FixtureDigest = second.Fixture.ExpectedDigest
	secondRun.Provenance.Model = second.Agent.Model
	secondRun.Provenance.Provider, _, _ = contracts.ParseModelSelection(second.Agent.Model)
	secondRun.Provenance.Extensions = map[string]string{"x-observed-provider": secondRun.Provenance.Provider, "x-observed-model": strings.TrimPrefix(second.Agent.Model, secondRun.Provenance.Provider+"/")}
	publicCases := []contracts.Case{first, second}
	if err := validateArtifactCaseContracts("candidate", []contracts.RunResult{firstRun, secondRun}, publicCases, nil, false); err != nil {
		t.Fatalf("valid frozen population rejected: %v", err)
	}
	secondRun.CaseID = "easy_substitute"
	if err := validateArtifactCaseContracts("candidate", []contracts.RunResult{firstRun, secondRun}, publicCases, nil, false); err == nil || !strings.Contains(err.Error(), "outside the frozen public catalog") {
		t.Fatalf("substituted public case identity accepted: %v", err)
	}
	secondRun.CaseID = second.ID
	secondRun.Provenance.Model = "openai/easier-model"
	if err := validateArtifactCaseContracts("candidate", []contracts.RunResult{firstRun, secondRun}, publicCases, nil, false); err == nil || !strings.Contains(err.Error(), "model/provider") {
		t.Fatalf("substituted public case model accepted: %v", err)
	}
	secondRun.Provenance.Model = second.Agent.Model
	secondRun.Provenance.FixtureDigest = "sha256:" + strings.Repeat("0", 64)
	if err := validateArtifactCaseContracts("candidate", []contracts.RunResult{firstRun, secondRun}, publicCases, nil, false); err == nil || !strings.Contains(err.Error(), "fixture digest") {
		t.Fatalf("substituted public fixture accepted: %v", err)
	}
}

func TestABExecutesFrozenExternalHoldoutAndReportsOnlyAggregateIdentity(t *testing.T) {
	repository := projectRoot(t)
	baseCase, err := cases.LoadContract(filepath.Join(repository, "eval", "cases", "skynex-orchestrator", "skx_low_direct.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	holdoutCase := *baseCase
	holdoutCase.ID = "skx_external_holdout_001"
	holdoutCase.Extensions = map[string]any{"x-visibility": "external-holdout"}
	const (
		holdoutCheckSecret = "HOLDOUT_CHECK_SUMMARY_SECRET"
		holdoutErrorSecret = "HOLDOUT_ERROR_MESSAGE_SECRET"
		holdoutTextSecret  = "HOLDOUT_ARBITRARY_TEXT_SECRET"
	)
	if err := holdoutCase.Validate(); err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	harnessSource := filepath.Join(directory, "harness-source")
	holdoutSource := filepath.Join(directory, "holdout-source")
	writeFrozenCaseBundle(t, repository, harnessSource, *baseCase)
	writeFrozenCaseBundle(t, repository, holdoutSource, holdoutCase)
	controlSource := filepath.Join(directory, "control-source")
	candidateSource := filepath.Join(directory, "candidate-source")
	for index, root := range []string{controlSource, candidateSource} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		content := fmt.Sprintf("{\"x-test-arm\":%d}\n", index)
		if err := os.WriteFile(filepath.Join(root, "opencode.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(directory, "opencode")
	if err := os.WriteFile(binary, []byte("\x7fELF-frozen-opencode-test-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	openAPISHA := "sha256:" + strings.Repeat("c", 64)
	capsuleRoot := filepath.Join(directory, "capsule")
	if _, err := commandFreeze(context.Background(), []string{
		"--output-dir", capsuleRoot,
		"--harness", harnessSource, "--control", controlSource, "--candidate", candidateSource,
		"--holdout", holdoutSource, "--id", "holdout-test", "--suite", baseCase.Suite,
		"--runs", "2", "--seed", "7", "--control-model", "openai/control-model",
		"--candidate-model", "openai/candidate-model", "--binary", binary,
		"--opencode-openapi-digest", openAPISHA,
	}); err != nil {
		t.Fatalf("freeze test capsule: %v", err)
	}
	manifestPath := filepath.Join(capsuleRoot, "manifest.json")
	loadedManifest, err := experiment.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := *loadedManifest
	harnessRoot := filepath.Join(capsuleRoot, "bundles", "harness")
	holdoutRoot := filepath.Join(capsuleRoot, "bundles", "holdout")
	holdoutDigest := manifest.Holdout.Digest
	binarySHA := manifest.Execution.OpenCodeBinaryDigest
	evaluatorSHA := manifest.Execution.EvaluatorBinaryDigest
	if fmt.Sprint(manifest.IntentionalDifferences) != fmt.Sprint([]baseline.Field{
		baseline.FieldAgentBundleDigest, baseline.FieldModel, baseline.FieldPromptDigest,
	}) {
		t.Fatalf("freeze did not normalize default bundle treatment: %v", manifest.IntentionalDifferences)
	}
	openAIOAuthFile := writeOpenAIOAuthFile(t, directory)

	probeCalls := 0
	modelCalls := 0
	holdoutCalls := 0
	deps := dependencies{
		probeRuntime: func(_ context.Context, options doctorOptions) (doctorResult, error) {
			probeCalls++
			if options.ResolvedBinary == nil || options.Binary != options.ResolvedBinary.Path {
				t.Fatalf("preflight did not receive the once-resolved binary: %+v", options)
			}
			if fmt.Sprint(options.Models) != fmt.Sprint([]string{manifest.ModelAssignment.Control, manifest.ModelAssignment.Candidate}) {
				t.Fatalf("preflight models = %v", options.Models)
			}
			if options.OpenAIOAuthFile != openAIOAuthFile {
				t.Fatalf("preflight OAuth source = %q, want %q", options.OpenAIOAuthFile, openAIOAuthFile)
			}
			return doctorResult{
				Healthy: true, Version: options.ExpectedVersion, ExpectedVersion: options.ExpectedVersion,
				CapturedAt: time.Unix(1, 0).UTC().Format(time.RFC3339Nano), ModelCalls: 0,
				Endpoints: []doctorEndpoint{{Name: "/doc", Digest: openAPISHA}},
			}, nil
		},
		runModel: func(runContext context.Context, spec modelRunSpec) (modelRunResult, error) {
			modelCalls++
			if err := runContext.Err(); err != nil {
				return modelRunResult{}, err
			}
			if len(spec.Cases) != 1 {
				t.Fatalf("A/B block selected %d cases", len(spec.Cases))
			}
			if spec.ExpectedOpenCodeBinaryDigest != binarySHA || spec.ExpectedOpenCodeAPIDigest != openAPISHA ||
				spec.ExpectedToolchainsDigest != manifest.Execution.ToolchainsDigest || spec.ExecutableClosure == nil || spec.ResolvedBinary == nil {
				t.Fatalf("runtime pins were not forwarded: %+v", spec)
			}
			if spec.OpenAIOAuthFile != openAIOAuthFile {
				t.Fatalf("runtime OAuth source = %q, want %q", spec.OpenAIOAuthFile, openAIOAuthFile)
			}
			testCase := spec.Cases[0]
			expectedModel := manifest.ModelAssignment.Control
			if spec.Variant == string(stats.VariantCandidate) {
				expectedModel = manifest.ModelAssignment.Candidate
			}
			if testCase.Agent.Model != expectedModel {
				t.Fatalf("%s arm model = %q, want %q", spec.Variant, testCase.Agent.Model, expectedModel)
			}
			if testCase.ID == holdoutCase.ID {
				holdoutCalls++
				if spec.RetainTrace {
					t.Fatal("holdout trace retention was not disabled")
				}
				if spec.FixtureRoot != filepath.Join(holdoutRoot, "fixtures") {
					t.Fatalf("holdout fixture root = %q", spec.FixtureRoot)
				}
			} else if spec.RetainTrace {
				t.Fatal("runtime-readable OAuth persisted a public trace")
			}
			caseDigest, digestErr := testCase.Digest()
			if digestErr != nil {
				t.Fatal(digestErr)
			}
			sample := validRun(fmt.Sprintf("run-%02d", modelCalls), spec.Variant, contracts.RunStatusPass)
			if spec.Variant == string(stats.VariantCandidate) {
				sample.Usage.Parent.FirstInputTokens = 5
				sample.Usage.Parent.PeakInputTokens = 5
				sample.Usage.Parent.SumInputTokens = 5
			}
			sample.CaseID = testCase.ID
			sample.Repetition = spec.RepetitionStart
			sample.Provenance.CaseDigest = caseDigest
			sample.Provenance.PromptDigest = spec.VerifiedBundleDigest
			sample.Provenance.FixtureDigest = testCase.Fixture.ExpectedDigest
			sample.Provenance.OpenCodeVersion = spec.ExpectedVersion
			sample.Provenance.OpenCodeAPIDigest = spec.ExpectedOpenCodeAPIDigest
			sample.Provenance.Model = testCase.Agent.Model
			sample.Provenance.Provider, _, _ = strings.Cut(testCase.Agent.Model, "/")
			sample.Provenance.Extensions = make(map[string]string)
			sample.Provenance.Extensions[contracts.ProvenanceExtensionProviderAuthMode] = contracts.ProviderAuthModeOpenAIOAuthCleanProfileV1
			sample.Provenance.Extensions[contracts.ProvenanceExtensionBillingMode] = contracts.BillingModeChatGPTSubscription
			sample.Provenance.Extensions[contracts.ProvenanceExtensionCredentialBoundary] = contracts.CredentialBoundaryRuntimeReadable
			sample.Provenance.Extensions[contracts.ProvenanceExtensionAuthIsolation] = contracts.AuthIsolationDedicatedFreshTokenFailStopV1
			sample.Provenance.Extensions[contracts.ProvenanceExtensionProviderCatalogDigest] = "sha256:" + strings.Repeat("d", 64)
			sample.Provenance.Extensions["x-observed-provider"] = sample.Provenance.Provider
			sample.Provenance.Extensions["x-observed-model"] = strings.TrimPrefix(sample.Provenance.Model, sample.Provenance.Provider+"/")
			sample.Provenance.Extensions[provenanceExtensionAgentBundleDigest] = spec.VerifiedBundleDigest
			sample.Provenance.Extensions[provenanceExtensionHarnessBundleDigest] = spec.HarnessDigest
			sample.Provenance.Extensions[provenanceExtensionManifestDigest] = spec.ManifestDigest
			sample.Provenance.Extensions[provenanceExtensionEffectiveAgentsDigest] = sample.Provenance.ConfigDigest
			sample.Provenance.Extensions[provenanceExtensionEffectiveConfigDigest] = sample.Provenance.ConfigDigest
			sample.Provenance.Extensions[provenanceExtensionToolchainsDigest] = spec.ExecutableClosure.Digest()
			sample.Usage.Parent.CalculatedCostUSD = sample.Usage.Parent.ProviderCostUSD
			sample.Usage.Tree.CalculatedCostUSD = sample.Usage.Tree.ProviderCostUSD
			sample.Usage.Parent.ProviderCostUSD = nil
			sample.Usage.Tree.ProviderCostUSD = nil
			if testCase.ID == holdoutCase.ID {
				sample.Checks[0].Summary = holdoutCheckSecret
				sample.Checks[0].Error = &contracts.RunError{Kind: "secret", Message: holdoutErrorSecret}
				sample.Evidence.BeforeTree = holdoutTextSecret
			}
			return modelRunResult{
				Result: runner.ContractResult{
					Suite: spec.Suite, Samples: []contracts.RunResult{sample},
					Started: time.Unix(1, 0).UTC(), Ended: time.Unix(2, 0).UTC(), Complete: true,
				},
				BundleDigest: spec.VerifiedBundleDigest, HarnessDigest: spec.HarnessDigest,
				ManifestDigest: spec.ManifestDigest, EvaluatorBinaryDigest: evaluatorSHA,
				OpenCodeBinaryDigest: binarySHA, CostEvidenceComplete: false,
				EffectiveCases: []contracts.Case{testCase},
			}, nil
		},
	}
	_, overlapErr := commandAB(context.Background(), []string{
		"--allow-model-calls", "--require-holdout", "--manifest", manifestPath, "--openai-oauth", openAIOAuthFile,
		"--cases-dir", filepath.Join(harnessRoot, "cases"),
		"--fixtures-dir", filepath.Join(harnessRoot, "fixtures"),
		"--binary", binary, "--output-prefix", filepath.Join(harnessRoot, "results", "ab-overlap"),
	}, deps)
	if overlapErr == nil || !strings.Contains(overlapErr.Error(), "outside frozen harness") {
		t.Fatalf("A/B output inside the frozen harness was not rejected: %v", overlapErr)
	}
	if probeCalls != 0 || modelCalls != 0 {
		t.Fatalf("invalid output reached external work: probes=%d models=%d", probeCalls, modelCalls)
	}
	populationManifest := manifest
	populationManifest.HoldoutCaseCount++
	populationManifestPath := filepath.Join(directory, "manifest-private-population.json")
	if err := baseline.SaveJSON(populationManifestPath, populationManifest, baseline.IOOptions{}); err != nil {
		t.Fatal(err)
	}
	_, populationErr := commandAB(context.Background(), []string{
		"--allow-model-calls", "--require-holdout", "--manifest", populationManifestPath, "--openai-oauth", openAIOAuthFile,
		"--cases-dir", filepath.Join(harnessRoot, "cases"),
		"--fixtures-dir", filepath.Join(harnessRoot, "fixtures"),
		"--binary", binary, "--output-prefix", filepath.Join(directory, "results", "ab-private-population"),
	}, deps)
	if populationErr == nil || !strings.Contains(populationErr.Error(), privateHoldoutError().Error()) ||
		strings.Contains(populationErr.Error(), "holdout suite contains") {
		t.Fatalf("holdout population diagnostic was not redacted: %v", populationErr)
	}
	if probeCalls != 0 || modelCalls != 0 {
		t.Fatalf("invalid holdout population reached external work: probes=%d models=%d", probeCalls, modelCalls)
	}

	const privatePreflightSentinel = "HOLDOUT_PRIVATE_MODEL_PREFLIGHT_SENTINEL"
	privateProbeDeps := deps
	privateProbeDeps.probeRuntime = func(context.Context, doctorOptions) (doctorResult, error) {
		return doctorResult{}, errors.New(privatePreflightSentinel + ": " + holdoutCase.ID)
	}
	_, privateProbeErr := commandAB(context.Background(), []string{
		"--allow-model-calls", "--require-holdout", "--manifest", manifestPath, "--openai-oauth", openAIOAuthFile,
		"--cases-dir", filepath.Join(harnessRoot, "cases"),
		"--fixtures-dir", filepath.Join(harnessRoot, "fixtures"),
		"--binary", binary, "--output-prefix", filepath.Join(directory, "results", "ab-private-preflight"),
	}, privateProbeDeps)
	if privateProbeErr == nil || !strings.Contains(privateProbeErr.Error(), privateHoldoutError().Error()) ||
		strings.Contains(privateProbeErr.Error(), privatePreflightSentinel) || strings.Contains(privateProbeErr.Error(), holdoutCase.ID) {
		t.Fatalf("holdout model preflight diagnostic leaked private data: %v", privateProbeErr)
	}
	if modelCalls != 0 {
		t.Fatalf("failed private model preflight reached model work: calls=%d", modelCalls)
	}
	outputPrefix := filepath.Join(directory, "results", "ab")
	result, err := commandAB(context.Background(), []string{
		"--allow-model-calls", "--require-holdout", "--manifest", manifestPath, "--openai-oauth", openAIOAuthFile,
		"--cases-dir", filepath.Join(harnessRoot, "cases"),
		"--fixtures-dir", filepath.Join(harnessRoot, "fixtures"),
		"--binary", binary, "--output-prefix", outputPrefix,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.CLIExitCode() != contracts.ExitSuccess {
		t.Fatalf("A/B exit=%d result=%+v comparison=%+v", result.CLIExitCode(), result, result.Comparison)
	}
	if probeCalls != 1 {
		t.Fatalf("preflight probes=%d, want 1 after the valid campaign", probeCalls)
	}
	if result.Intent != experiment.IntentDevelopment || result.Authority != evaluationAuthorityDevelopment ||
		result.Comparison == nil || result.Comparison.Authority != evaluationAuthorityDevelopment {
		t.Fatalf("development evidence could be confused with release authority: %+v", result)
	}
	if modelCalls != 8 || holdoutCalls != 4 {
		t.Fatalf("model calls=%d holdout calls=%d, want 8/4", modelCalls, holdoutCalls)
	}
	if result.HoldoutDigest != holdoutDigest || result.HoldoutCases != 1 || result.Comparison == nil || result.Comparison.Holdout == nil {
		t.Fatalf("missing holdout result metadata: %+v", result)
	}
	if result.Comparison.Holdout.BundleDigest != holdoutDigest || result.Comparison.Holdout.Cases != 1 {
		t.Fatalf("unexpected holdout comparison: %+v", result.Comparison.Holdout)
	}
	if !result.Comparison.Report.Compatibility.Compatible {
		t.Fatalf("model-assigned fingerprints are incompatible: %+v", result.Comparison.Report.Compatibility)
	}
	foundFirstInputMetric := false
	foundSubscriptionCostMetric := false
	for _, metric := range result.Comparison.Report.Metrics {
		if metric.Name == "parent_first_input_tokens" {
			foundFirstInputMetric = true
		}
		if metric.Name == "tree_cost_usd" {
			foundSubscriptionCostMetric = true
			if metric.Control.Eligible != 0 || metric.Candidate.Eligible != 0 || len(metric.Paired.Pairs) != 0 {
				t.Fatalf("counterfactual calculated cost entered subscription USD metric: %+v", metric)
			}
			if metric.Paired.CI.Reason != subscriptionCostNotApplicableReason {
				t.Fatalf("subscription USD metric is not explicitly unavailable: %+v", metric.Paired)
			}
		}
	}
	if !foundFirstInputMetric {
		t.Fatal("comparison report omitted parent_first_input_tokens")
	}
	if !foundSubscriptionCostMetric {
		t.Fatal("comparison report omitted the explicit not-applicable subscription cost metric")
	}
	foundSubscriptionCostGate := false
	foundTreatmentGate := false
	for _, gate := range result.Comparison.Report.Decision.Results {
		if gate.Name == "treatment_realized" {
			foundTreatmentGate = true
			if gate.Status != gates.StatusPass {
				t.Fatalf("frozen treatment was not realized: %+v", gate)
			}
		}
		if gate.Name != "tree_cost_usd" {
			continue
		}
		foundSubscriptionCostGate = true
		if gate.Status != gates.StatusNotApplicable || gate.Reason != gates.ReasonNotApplicable || !strings.Contains(gate.Detail, "counterfactual") {
			t.Fatalf("subscription cost gate has false price authority: %+v", gate)
		}
	}
	if !foundSubscriptionCostGate {
		t.Fatal("comparison decision omitted the not-applicable subscription cost gate")
	}
	if !foundTreatmentGate {
		t.Fatal("comparison decision omitted treatment_realized")
	}
	allowedMismatches := map[baseline.Field]bool{}
	for _, mismatch := range result.Comparison.Report.Compatibility.Mismatches {
		if !mismatch.Allowed {
			t.Fatalf("unexpected incompatible fingerprint mismatch: %+v", mismatch)
		}
		allowedMismatches[mismatch.Field] = true
	}
	if !allowedMismatches[baseline.FieldModel] || allowedMismatches[baseline.FieldProvider] || allowedMismatches[baseline.FieldCaseDigest] {
		t.Fatalf("unexpected model assignment mismatches: %+v", result.Comparison.Report.Compatibility.Mismatches)
	}
	comparisonJSON, err := json.Marshal(result.Comparison)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(comparisonJSON, []byte(holdoutCase.ID)) {
		t.Fatalf("comparison report exposes holdout case identity: %s", comparisonJSON)
	}
	abResultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(abResultJSON, []byte(holdoutCase.ID)) {
		t.Fatalf("A/B stdout payload exposes holdout case identity: %s", abResultJSON)
	}
	if bytes.Contains(abResultJSON, []byte(`"observed_cost_usd"`)) {
		t.Fatalf("subscription output invented an observed zero-dollar cost: %s", abResultJSON)
	}

	controlArtifact, err := baseline.Load(result.ControlPath, baseline.IOOptions{})
	if err != nil {
		t.Fatal(err)
	}
	candidateArtifact, err := baseline.Load(result.CandidatePath, baseline.IOOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, artifactPath := range []string{result.ControlPath, result.CandidatePath} {
		raw, readErr := os.ReadFile(artifactPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, secret := range []string{holdoutCase.ID, holdoutCheckSecret, holdoutErrorSecret, holdoutTextSecret} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("public baseline %s exposes holdout secret %q", artifactPath, secret)
			}
		}
		for _, guessedID := range []string{holdoutCase.ID, "holdout_1", "secret_holdout_case", "skx_external_holdout_002"} {
			for _, dictionaryDigest := range []string{
				digestBytes([]byte(guessedID)),
				digestBytes([]byte("skynex-eval-holdout-case-v1\x00" + guessedID)),
			} {
				if bytes.Contains(raw, []byte(dictionaryDigest)) || bytes.Contains(raw, []byte(strings.TrimPrefix(dictionaryDigest, "sha256:"))) {
					t.Fatalf("public baseline %s exposes dictionary-checkable holdout ID digest for %q", artifactPath, guessedID)
				}
			}
		}
	}
	if controlArtifact.Fingerprint.Model != manifest.ModelAssignment.Control || controlArtifact.Fingerprint.Provider != "openai" ||
		candidateArtifact.Fingerprint.Model != manifest.ModelAssignment.Candidate || candidateArtifact.Fingerprint.Provider != "openai" {
		t.Fatalf("artifact model assignment mismatch: control=%s/%s candidate=%s/%s",
			controlArtifact.Fingerprint.Provider, controlArtifact.Fingerprint.Model,
			candidateArtifact.Fingerprint.Provider, candidateArtifact.Fingerprint.Model)
	}
	if controlArtifact.Fingerprint.CaseDigest != candidateArtifact.Fingerprint.CaseDigest {
		t.Fatalf("model assignment changed model-neutral case digest: %s != %s", controlArtifact.Fingerprint.CaseDigest, candidateArtifact.Fingerprint.CaseDigest)
	}
	if observed := controlArtifact.Fingerprint.ToolsetDigest; observed != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("fingerprint ignored runtime-observed toolset digest: %s", observed)
	}
	if gate := manifestConformanceGate(manifest, controlArtifact, candidateArtifact); gate.Status != gates.StatusPass {
		t.Fatalf("valid model assignment failed manifest conformance: %+v", gate)
	}
	tamperedRuns, err := decodeArtifactRuns(candidateArtifact)
	if err != nil {
		t.Fatal(err)
	}
	for index := range tamperedRuns {
		tamperedRuns[index].Provenance.OpenCodeVersion = "1.18.15"
	}
	tamperedArtifact, err := baseline.NewRunArtifact(
		candidateArtifact.Label,
		candidateArtifact.Suite,
		time.Now().UTC(),
		candidateArtifact.Fingerprint,
		tamperedRuns,
		candidateArtifact.Aggregates,
	)
	if err != nil {
		t.Fatal(err)
	}
	if gate := manifestConformanceGate(manifest, controlArtifact, tamperedArtifact); gate.Status != gates.StatusPass {
		t.Fatalf("test fixture must remain fingerprint/manifest-conformant before sample binding: %+v", gate)
	}
	tamperedPath := filepath.Join(directory, "candidate-fingerprint-sample-divergence.json")
	if err := tamperedArtifact.Save(tamperedPath, baseline.IOOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := commandCompare([]string{
		"--control", result.ControlPath,
		"--candidate", tamperedPath,
		"--manifest", manifestPath,
	}); err == nil {
		t.Fatal("sealed, manifest-conformant artifact with divergent public samples was accepted")
	} else if exit, kind := classifyCommandError(err); exit != contracts.ExitInvalid || kind != "invalid_candidate_fingerprint_binding" {
		t.Fatalf("fingerprint/sample divergence classification = exit %d kind %q error %v", exit, kind, err)
	}
	if _, err := commandReport([]string{"--input", tamperedPath}); err == nil {
		t.Fatal("single-arm report accepted a sealed artifact with divergent fingerprint/sample provenance")
	} else if exit, kind := classifyCommandError(err); exit != contracts.ExitInvalid || kind != "invalid_fingerprint_binding" {
		t.Fatalf("single-arm fingerprint divergence classification = exit %d kind %q error %v", exit, kind, err)
	}
	tamperedCandidate := *candidateArtifact
	tamperedCandidate.Fingerprint.Model = "openai/uncommitted-model"
	if gate := manifestConformanceGate(manifest, controlArtifact, &tamperedCandidate); gate.Status != gates.StatusInvalid {
		t.Fatalf("uncommitted model assignment was accepted: %+v", gate)
	}
	tamperedEvaluator := *candidateArtifact
	tamperedEvaluator.Fingerprint.EvaluatorBinaryDigest = "sha256:" + strings.Repeat("0", 64)
	if gate := manifestConformanceGate(manifest, controlArtifact, &tamperedEvaluator); gate.Status != gates.StatusInvalid {
		t.Fatalf("uncommitted evaluator binary was accepted: %+v", gate)
	}
	rawMetadata, exists := candidateArtifact.Aggregates[holdoutAggregateKey]
	if !exists {
		t.Fatal("candidate artifact lacks holdout aggregate")
	}
	metadata, err := decodeHoldoutMetadata(candidateArtifact.Label, rawMetadata, holdoutDigest)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.CaseCount != 1 || len(metadata.Samples) != manifest.Runs || bytes.Contains(rawMetadata, []byte(holdoutCase.ID)) {
		t.Fatalf("holdout metadata exposes identity or has wrong count: %s", rawMetadata)
	}
	reported, err := commandReport([]string{"--input", result.CandidatePath})
	if err != nil {
		t.Fatal(err)
	}
	artifactSummary, ok := reported.(artifactReport)
	if !ok || artifactSummary.Holdout == nil || artifactSummary.Holdout.BundleDigest != holdoutDigest {
		t.Fatalf("artifact report lacks holdout aggregate: %#v", reported)
	}
	if artifactSummary.Authority != evaluationAuthorityDevelopment+"-evidence-only" {
		t.Fatalf("single A/B arm was presented as a decision authority: %q", artifactSummary.Authority)
	}
	artifactSummaryJSON, err := json.Marshal(artifactSummary)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(artifactSummaryJSON, []byte(holdoutCase.ID)) {
		t.Fatalf("artifact report exposes holdout case identity: %s", artifactSummaryJSON)
	}
	if _, err := commandReport([]string{"--input", outputPrefix + ".comparison.json"}); err == nil || !strings.Contains(err.Error(), "--control, --candidate and --manifest") {
		t.Fatalf("unverified comparison report was accepted: %v", err)
	}
	reportArgs := []string{
		"--input", outputPrefix + ".comparison.json",
		"--control", result.ControlPath,
		"--candidate", result.CandidatePath,
		"--manifest", manifestPath,
	}
	reportedComparison, err := commandReport(reportArgs)
	if err != nil {
		t.Fatal(err)
	}
	comparisonSummary, ok := reportedComparison.(savedComparisonReport)
	if !ok || comparisonSummary.Holdout == nil || comparisonSummary.Holdout.BundleDigest != holdoutDigest {
		t.Fatalf("comparison report lacks holdout aggregate: %#v", reportedComparison)
	}
	var forged comparisonCommandResult
	if err := baseline.LoadJSON(outputPrefix+".comparison.json", &forged, baseline.IOOptions{Strict: true}); err != nil {
		t.Fatal(err)
	}
	forged.Report.ExperimentID = "forged-release-pass"
	forgedPath := filepath.Join(directory, "forged-comparison.json")
	forgedBytes, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forgedPath, forgedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	reportArgs[1] = forgedPath
	if _, err := commandReport(reportArgs); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("forged comparison report was accepted: %v", err)
	}

	// A late storage failure must not discard the complete, already-paid
	// population. Replace the reserved control target after the last fake model
	// call and require commandAB to preserve a sanitized partial artifact.
	storageFailurePrefix := filepath.Join(directory, "results", "ab-storage-failure")
	storageFailureCalls := 0
	storageFailureDeps := deps
	originalRunModel := deps.runModel
	storageFailureDeps.runModel = func(runContext context.Context, spec modelRunSpec) (modelRunResult, error) {
		run, runErr := originalRunModel(runContext, spec)
		storageFailureCalls++
		if runErr == nil && storageFailureCalls == 8 {
			controlTarget := storageFailurePrefix + ".control.json"
			if err := os.Remove(controlTarget); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(controlTarget, []byte("external evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return run, runErr
	}
	storagePartial, storageErr := commandAB(context.Background(), []string{
		"--allow-model-calls", "--require-holdout", "--manifest", manifestPath, "--openai-oauth", openAIOAuthFile,
		"--cases-dir", filepath.Join(harnessRoot, "cases"),
		"--fixtures-dir", filepath.Join(harnessRoot, "fixtures"),
		"--binary", binary, "--output-prefix", storageFailurePrefix,
	}, storageFailureDeps)
	if storageErr == nil || storagePartial.PartialPath == "" || storagePartial.ExitCode != contracts.ExitInfrastructure {
		t.Fatalf("late storage failure lost partial evidence: result=%+v error=%v", storagePartial, storageErr)
	}
	if storageFailureCalls != 8 {
		t.Fatalf("storage failure occurred after %d fake model calls, want 8", storageFailureCalls)
	}
	if contents, err := os.ReadFile(storageFailurePrefix + ".control.json"); err != nil || string(contents) != "external evidence" {
		t.Fatalf("external result target was overwritten: contents=%q error=%v", contents, err)
	}
	storagePartialBytes, err := os.ReadFile(storagePartial.PartialPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{holdoutCase.ID, holdoutCheckSecret, holdoutErrorSecret, holdoutTextSecret} {
		if bytes.Contains(storagePartialBytes, []byte(secret)) {
			t.Fatalf("storage-failure partial exposes holdout secret %q", secret)
		}
	}

	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	canceledPrefix := filepath.Join(directory, "results", "ab-canceled")
	var canceledStdout, canceledStderr bytes.Buffer
	exit := runCLI(canceledContext, []string{
		"ab", "--allow-model-calls", "--require-holdout", "--manifest", manifestPath, "--openai-oauth", openAIOAuthFile,
		"--cases-dir", filepath.Join(harnessRoot, "cases"),
		"--fixtures-dir", filepath.Join(harnessRoot, "fixtures"),
		"--binary", binary, "--output-prefix", canceledPrefix,
	}, deps, &canceledStdout, &canceledStderr)
	if exit != contracts.ExitAborted {
		t.Fatalf("canceled A/B exit=%d stdout=%s stderr=%s", exit, canceledStdout.String(), canceledStderr.String())
	}
	var canceledEnvelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
		Error  *errorBody      `json:"error"`
	}
	if err := json.Unmarshal(canceledStdout.Bytes(), &canceledEnvelope); err != nil {
		t.Fatal(err)
	}
	var partialResult abCommandResult
	if err := json.Unmarshal(canceledEnvelope.Data, &partialResult); err != nil {
		t.Fatalf("canceled envelope lacks structured data: %v; payload=%s", err, canceledStdout.String())
	}
	if canceledEnvelope.Status != string(contracts.RunStatusAborted) || canceledEnvelope.Error == nil ||
		canceledEnvelope.Error.Kind != "aborted" || partialResult.PartialPath == "" || partialResult.ExitCode != contracts.ExitAborted {
		t.Fatalf("canceled envelope lost partial result: envelope=%+v data=%+v", canceledEnvelope, partialResult)
	}
	partialBytes, err := os.ReadFile(partialResult.PartialPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{holdoutCase.ID, holdoutCheckSecret, holdoutErrorSecret, holdoutTextSecret} {
		if bytes.Contains(partialBytes, []byte(secret)) {
			t.Fatalf("partial A/B artifact exposes holdout secret %q", secret)
		}
	}

	const privateLoadSentinel = "HOLDOUT_PRIVATE_LOAD_SENTINEL"
	privateFilename := privateLoadSentinel + ".yaml"
	if err := os.WriteFile(filepath.Join(holdoutRoot, "cases", privateFilename), []byte("private_prompt: ["+privateLoadSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	leakManifest := manifest
	leakHoldout := *manifest.Holdout
	leakHoldout.Digest = snapshotDigest(t, holdoutRoot)
	leakManifest.Holdout = &leakHoldout
	leakManifestPath := filepath.Join(directory, "manifest-private-load.json")
	leakManifestJSON, err := json.Marshal(leakManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leakManifestPath, leakManifestJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	var leakStdout, leakStderr bytes.Buffer
	leakExit := runCLI(context.Background(), []string{
		"ab", "--allow-model-calls", "--require-holdout", "--manifest", leakManifestPath, "--openai-oauth", openAIOAuthFile,
		"--cases-dir", filepath.Join(harnessRoot, "cases"),
		"--fixtures-dir", filepath.Join(harnessRoot, "fixtures"),
		"--binary", binary, "--output-prefix", filepath.Join(directory, "results", "ab-private-load"),
	}, deps, &leakStdout, &leakStderr)
	if leakExit != contracts.ExitInvalid {
		t.Fatalf("private holdout load exit=%d stdout=%s stderr=%s", leakExit, leakStdout.String(), leakStderr.String())
	}
	for _, output := range []string{leakStdout.String(), leakStderr.String()} {
		if strings.Contains(output, privateLoadSentinel) || strings.Contains(output, privateFilename) {
			t.Fatalf("holdout load diagnostic leaked private data: %s", output)
		}
	}
}

type runtimeFactoryFunc func(context.Context, runner.RuntimeRequest) (runner.Runtime, error)

func (f runtimeFactoryFunc) Start(ctx context.Context, request runner.RuntimeRequest) (runner.Runtime, error) {
	return f(ctx, request)
}

func writeOpenAIOAuthFile(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "opencode-auth.json")
	contents := []byte(`{"openai":{"type":"oauth","access":"test-access","refresh":"test-refresh","expires":4102444800000}}`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func writeFrozenCaseBundle(t *testing.T, repository, bundleRoot string, testCase contracts.Case) {
	t.Helper()
	casesRoot := filepath.Join(bundleRoot, "cases")
	fixturesRoot := filepath.Join(bundleRoot, "fixtures")
	if err := os.MkdirAll(casesRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceFixture := filepath.Join(repository, "eval", "fixtures", filepath.FromSlash(testCase.Fixture.Source))
	destinationFixture := filepath.Join(fixturesRoot, filepath.FromSlash(testCase.Fixture.Source))
	if err := os.MkdirAll(destinationFixture, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.CopyVerifiedTree(sourceFixture, destinationFixture, sandbox.DefaultSnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(testCase)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(casesRoot, testCase.ID+".yaml"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func snapshotDigest(t *testing.T, root string) string {
	t.Helper()
	snapshot, err := sandbox.DigestTree(root, sandbox.DefaultSnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.Digest
}

func projectRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writeArtifact(t *testing.T, path, label string, fingerprint baseline.Fingerprint, run contracts.RunResult, created time.Time) {
	t.Helper()
	run.Provenance.PromptDigest = fingerprint.PromptDigest
	run.Provenance.ConfigDigest = fingerprint.EffectiveConfigDigest
	run.Provenance.OpenCodeVersion = fingerprint.OpenCodeVersion
	run.Provenance.OpenCodeAPIDigest = fingerprint.OpenCodeOpenAPIDigest
	run.Provenance.Model = fingerprint.Model
	run.Provenance.Provider = fingerprint.Provider
	run.Provenance.ToolsetDigest = fingerprint.ToolsetDigest
	run.Provenance.JudgeDigest = fingerprint.JudgesDigest
	if fingerprint.PricingTableDigest != "" {
		run.Provenance.PricingTableDigest = fingerprint.PricingTableDigest
	}
	run.Provenance.ExecutionMode = contracts.ExecutionMode(fingerprint.ExecutionMode)
	run.Provenance.Network = contracts.NetworkPolicy(fingerprint.NetworkPolicy)
	run.Provenance.Host.OS = fingerprint.HostOS
	run.Provenance.Host.Arch = fingerprint.HostArch
	if run.Provenance.Extensions == nil {
		run.Provenance.Extensions = make(map[string]string)
	}
	run.Provenance.Extensions[provenanceExtensionAgentBundleDigest] = fingerprint.AgentBundleDigest
	run.Provenance.Extensions[provenanceExtensionHarnessBundleDigest] = fingerprint.HarnessBundleDigest
	run.Provenance.Extensions[provenanceExtensionManifestDigest] = fingerprint.ExperimentManifestDigest
	run.Provenance.Extensions[provenanceExtensionEffectiveAgentsDigest] = fingerprint.EffectiveAgentsDigest
	run.Provenance.Extensions[provenanceExtensionEffectiveConfigDigest] = fingerprint.EffectiveConfigDigest
	run.Provenance.Extensions[provenanceExtensionToolchainsDigest] = fingerprint.ToolchainsDigest
	if fingerprint.ProviderAuthMode == contracts.ProviderAuthModeOpenAIOAuthCleanProfileV1 {
		run.Provenance.Extensions[contracts.ProvenanceExtensionProviderAuthMode] = fingerprint.ProviderAuthMode
		run.Provenance.Extensions[contracts.ProvenanceExtensionBillingMode] = fingerprint.BillingMode
		run.Provenance.Extensions[contracts.ProvenanceExtensionCredentialBoundary] = fingerprint.CredentialBoundary
		run.Provenance.Extensions[contracts.ProvenanceExtensionAuthIsolation] = fingerprint.AuthIsolation
	}
	run.Provenance.Extensions[contracts.ProvenanceExtensionProviderCatalogDigest] = fingerprint.ProviderCatalogDigest
	critical, err := baseline.CanonicalJSON(map[string]any{"case_ids": []string{"case"}})
	if err != nil {
		t.Fatal(err)
	}
	authority := encodeEvaluationAuthority(evaluationAuthorityMetadata{
		Mode: evaluationAuthorityExploratory, Reason: "test artifact has no frozen manifest",
	})
	publicCasesDigest, err := baseline.CanonicalJSON(map[string]string{"digest": fingerprint.CaseDigest})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := baseline.NewRunArtifact(label, "suite", created, fingerprint, []contracts.RunResult{run}, map[string]json.RawMessage{
		"critical_case_ids": critical, publicCasesDigestAggregateKey: publicCasesDigest,
		evaluationAuthorityAggregateKey: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.Save(path, baseline.IOOptions{}); err != nil {
		t.Fatal(err)
	}
}

func validRun(runID, variant string, status contracts.RunStatus) contracts.RunResult {
	digest := "sha256:" + strings.Repeat("a", 64)
	cost := 0.01
	checkStatus := contracts.CheckStatusPass
	var runError *contracts.RunError
	if status == contracts.RunStatusFail {
		checkStatus = contracts.CheckStatusFail
		runError = &contracts.RunError{Kind: "hard_check", Message: "failed"}
	}
	return contracts.RunResult{
		SchemaVersion: contracts.ResultSchemaVersion,
		RunID:         runID, CaseID: "case", Variant: variant, Repetition: 1, Status: status,
		Provenance: contracts.Provenance{
			GitSHA: strings.Repeat("f", 40), CaseDigest: digest, PromptDigest: digest,
			ConfigDigest: digest, FixtureDigest: digest, OpenCodeVersion: defaultOpenCodeVersion,
			OpenCodeAPIDigest: digest, Model: "openai/test", Provider: "openai",
			ToolsetDigest: digest, JudgeDigest: digest, PricingTableDigest: digest,
			ExecutionMode: contracts.ExecutionTrustedLocal, Network: contracts.NetworkHostUnisolated,
			Host: contracts.HostProvenance{OS: "linux", Arch: "amd64"},
			Extensions: map[string]string{
				provenanceExtensionEffectiveAgentsDigest: digest,
				provenanceExtensionEffectiveConfigDigest: digest,
				provenanceExtensionToolchainsDigest:      digest,
			},
		},
		Checks: []contracts.CheckResult{{
			ID: "check", Type: "behavior", Status: checkStatus, Hard: true, Summary: "checked",
			RequirementIDs: []string{"REQ-001"}, EvidenceIDs: []string{"evidence"},
		}},
		Usage: contracts.Usage{
			Parent: contracts.TokenUsage{FirstInputTokens: 10, PeakInputTokens: 10, SumInputTokens: 10, OutputTokens: 2, ProviderCostUSD: &cost},
			Tree:   contracts.TreeUsage{SumInputTokens: 10, OutputTokens: 2, ProviderCostUSD: &cost, Sessions: 1},
		},
		Timing: contracts.Timing{WallMS: 10, ModelMS: 5},
		Evidence: contracts.Evidence{Items: []contracts.EvidenceItem{{
			ID: "evidence", Kind: "behavior", Source: contracts.EvidenceEvaluator, Digest: digest, Complete: true,
		}}},
		TelemetryComplete: true, Error: runError,
	}
}

func validFingerprint() baseline.Fingerprint {
	digest := "sha256:" + strings.Repeat("b", 64)
	return baseline.Fingerprint{
		PromptDigest: digest, AgentBundleDigest: digest, HarnessBundleDigest: digest,
		EvaluatorBinaryDigest: digest, ExperimentManifestDigest: digest,
		CaseSchemaVersion: contracts.CaseSchemaVersion, CaseDigest: digest, FixtureDigest: digest,
		SetupPolicyDigest: digest, OpenCodeVersion: defaultOpenCodeVersion,
		OpenCodeBinaryDigest: digest, OpenCodeOpenAPIDigest: digest,
		EffectiveConfigDigest: digest, EffectiveAgentsDigest: digest,
		Model: "openai/test", Provider: "openai", ToolsetDigest: digest,
		PermissionPolicyDigest: digest, ExecutionMode: string(contracts.ExecutionTrustedLocal),
		NetworkPolicy: string(contracts.NetworkHostUnisolated), JudgesDigest: digest,
		ProviderAuthMode: "provider-environment", BillingMode: "api-usage",
		CredentialBoundary: "environment", AuthIsolation: "none",
		ProviderCatalogDigest: digest,
		LLMJudgeUsed:          false, CalculatedCostUsed: false, PricingTableDigest: digest,
		HostOS: "linux", HostArch: "amd64", ToolchainsDigest: digest,
	}
}
