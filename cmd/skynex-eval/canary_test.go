package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/experiment"
	"github.com/joeldevz/skynex/internal/eval/lifecycle"
	"github.com/joeldevz/skynex/internal/eval/runner"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/stats"
	"github.com/joeldevz/skynex/internal/eval/toolpolicy"
)

func TestCanaryWithoutExecutorFailsClosedBeforeAnyRuntimeCall(t *testing.T) {
	probeCalls, modelCalls := 0, 0
	deps := dependencies{
		probeRuntime: func(context.Context, doctorOptions) (doctorResult, error) {
			probeCalls++
			return doctorResult{}, nil
		},
		runModel: func(context.Context, modelRunSpec) (modelRunResult, error) {
			modelCalls++
			return modelRunResult{}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	exit := runCLI(context.Background(), []string{
		"canary", "--allow-model-calls", "--profile", workflowV2CanaryProfile,
		"--manifest", "not-read-without-executor.json", "--openai-oauth", "not-read-without-executor-auth.json",
	}, deps, &stdout, &stderr)
	if exit != contracts.ExitInfrastructure {
		t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", exit, contracts.ExitInfrastructure, stdout.String(), stderr.String())
	}
	if probeCalls != 0 || modelCalls != 0 {
		t.Fatalf("unexpected runtime calls: probe=%d model=%d", probeCalls, modelCalls)
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Kind != "canary_executor_unavailable" {
		t.Fatalf("error = %#v", response.Error)
	}
}

func TestCanaryProfileIsClosedAndRequiresExplicitModelOptIn(t *testing.T) {
	tests := []struct {
		name string
		args []string
		kind string
	}{
		{
			name: "opt in",
			args: []string{"canary", "--profile", workflowV2CanaryProfile, "--manifest", "x", "--openai-oauth", "y"},
			kind: "model_calls_not_allowed",
		},
		{
			name: "unknown profile",
			args: []string{"canary", "--allow-model-calls", "--profile", "custom", "--manifest", "x", "--openai-oauth", "y"},
			kind: "invalid_canary_profile",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			exit := runCLI(context.Background(), test.args, dependencies{}, &stdout, &bytes.Buffer{})
			if exit != contracts.ExitInvalid {
				t.Fatalf("exit = %d", exit)
			}
			var response envelope
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Error == nil || response.Error.Kind != test.kind {
				t.Fatalf("error = %#v, want %q", response.Error, test.kind)
			}
		})
	}
}

func TestSkynexOrchestratorCanaryProfileIsSupportedButStillFailsClosed(t *testing.T) {
	var stdout bytes.Buffer
	exit := runCLI(context.Background(), []string{
		"canary", "--allow-model-calls", "--profile", skynexOrchestratorCanaryProfile,
		"--manifest", "not-read-without-executor.json", "--openai-oauth", "not-read-without-executor-auth.json",
	}, dependencies{}, &stdout, &bytes.Buffer{})
	if exit != contracts.ExitInfrastructure {
		t.Fatalf("exit = %d, want %d", exit, contracts.ExitInfrastructure)
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Kind != "canary_executor_unavailable" {
		t.Fatalf("error = %#v", response.Error)
	}
}

func TestWorkflowCanaryFixedLimits(t *testing.T) {
	limits := workflowCanaryLimits()
	if limits.GlobalWallClock != "30m0s" || limits.Preflight != "1m0s" || limits.PerSample != "4m0s" {
		t.Fatalf("unexpected wall-clock limits: %#v", limits)
	}
	if limits.SampleBudget != "24m0s" || limits.CleanupReserve != "1m0s" || limits.SealReserve != "4m0s" {
		t.Fatalf("unexpected budget allocation: %#v", limits)
	}
	if limits.RunsPerArm != 1 || limits.MaximumSamples != 6 || !limits.FailFast || limits.MaximumInputRatio != 1.15 {
		t.Fatalf("unexpected immutable profile: %#v", limits)
	}
	if workflowV2CanaryPreflightLimit+workflowV2CanarySampleBudget+workflowV2CanaryCleanupReserve+workflowV2CanarySealReserve != workflowV2CanaryGlobalLimit {
		t.Fatal("canary budget allocation does not equal the hard global limit")
	}
}

func TestCanaryTimeoutIsAStableScreeningFailure(t *testing.T) {
	err := canaryTimeoutFailure()
	code, kind := classifyCommandError(err)
	if code != contracts.ExitFailed || kind != "canary_timeout" {
		t.Fatalf("timeout classification = (%d, %q), want (%d, canary_timeout)", code, kind, contracts.ExitFailed)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("screening timeout must not be reclassified as generic budget exhaustion")
	}
}

func TestResolveWorkflowCanaryPluginMatchesEmbeddedBytes(t *testing.T) {
	root, err := assets.OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	content, err := fs.ReadFile(root, "plugins/skynex-workflow.ts")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "skynex-workflow.ts")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := resolveWorkflowCanaryPluginContent(path)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Path != path || !contracts.IsDigest(identity.ContentDigest) {
		t.Fatalf("identity = %#v", identity)
	}
	if err := os.WriteFile(path, append(content, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWorkflowCanaryPluginContent(path); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("modified plugin accepted: %v", err)
	}
	if _, err := resolveWorkflowCanaryPlugin(path); err == nil || !strings.Contains(err.Error(), "installation path") {
		t.Fatalf("non-installation plugin path accepted: %v", err)
	}
}

func TestValidateWorkflowCanaryCases(t *testing.T) {
	cases := workflowCanaryCaseFixtures(t)
	if err := validateWorkflowCanaryCases(cases); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func([]contracts.Case)
		want   string
	}{
		{name: "not critical", mutate: func(c []contracts.Case) { c[0].Critical = false }, want: "critical"},
		{name: "wrong agent", mutate: func(c []contracts.Case) { c[0].Agent.Name = "skynex-orchestrator" }, want: "workflow-orchestrator"},
		{name: "slow", mutate: func(c []contracts.Case) { c[0].Completion.Timeout = "4m1s" }, want: "at most"},
		{name: "runs", mutate: func(c []contracts.Case) { c[0].Runs.Count = 2 }, want: "runs.count"},
		{name: "holdout metadata", mutate: func(c []contracts.Case) { c[0].Extensions["x-visibility"] = "secret" }, want: "public"},
		{name: "neurox", mutate: func(c []contracts.Case) { c[0].ToolPolicy.AllowedTools = []string{"Read", "neurox_search"} }, want: "Neurox"},
		{name: "fake mcp", mutate: func(c []contracts.Case) { c[0].ToolPolicy.FakeMCPs = []contracts.FakeMCP{{Name: "ambient"}} }, want: "MCP"},
		{name: "arbitrary self pinned harness", mutate: func(c []contracts.Case) {
			c[0].ID = "trivial_self_pinned_case"
		}, want: "evaluator-owned"},
		{name: "fixture drift", mutate: func(c []contracts.Case) {
			c[0].Fixture.ExpectedDigest = "sha256:" + strings.Repeat("f", 64)
		}, want: "fixture digest"},
		{name: "missing mandatory check", mutate: func(c []contracts.Case) {
			c[0].BehaviorChecks = c[0].BehaviorChecks[:len(c[0].BehaviorChecks)-1]
		}, want: "exactly one hard"},
		{name: "case content drift", mutate: func(c []contracts.Case) {
			c[0].Input += " harmless-looking drift"
		}, want: "digest differs"},
		{name: "driver extra", mutate: func(c []contracts.Case) {
			c[0].Extensions["x-workflow-driver-v1"] = map[string]any{
				"mode": "managed-detach", "workflow_id": c[0].ID, "terminal_state": "candidate_frozen", "autonomous_turns": 2, "extra": true,
			}
		}, want: "exactly"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyCases := workflowCanaryCaseFixtures(t)
			test.mutate(copyCases)
			if err := validateWorkflowCanaryCases(copyCases); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestSkynexOrchestratorCanarySelectsExactPinnedCasesFromFullSuite(t *testing.T) {
	fullSuite, err := loadSelectedCases(filepath.Join(projectRoot(t), "eval", "cases"), skynexOrchestratorCanarySuite, "")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectSkynexOrchestratorCanaryCases(fullSuite)
	if err != nil {
		t.Fatal(err)
	}
	if len(fullSuite) != skynexOrchestratorCanaryPublicCaseCount || len(selected) != workflowV2CanaryCaseCount {
		t.Fatalf("full=%d selected=%d", len(fullSuite), len(selected))
	}
	ids := make([]string, len(selected))
	for index, testCase := range selected {
		ids[index] = testCase.ID
		pin, pinned := skynexOrchestratorCanaryCasePinFor(testCase.ID)
		if !pinned {
			t.Fatalf("selected unpinned case %q", testCase.ID)
		}
		digest, digestErr := testCase.Digest()
		if digestErr != nil || digest != pin.CaseDigest || testCase.Fixture.ExpectedDigest != pin.FixtureDigest {
			t.Fatalf("case %q does not match its pin", testCase.ID)
		}
		if _, managedWorkflow := testCase.Extensions["x-workflow-driver-v1"]; managedWorkflow {
			t.Fatalf("standalone case %q declares Workflow V2 authority", testCase.ID)
		}
	}
	if got, want := strings.Join(ids, ","), "skx_compaction,skx_low_direct,skx_no_workflow"; got != want {
		t.Fatalf("selected ids = %q, want %q", got, want)
	}
	publicDigest, err := publicCaseSetDigest(fullSuite)
	if err != nil || publicDigest != skynexOrchestratorCanaryPublicCasesDigest {
		t.Fatalf("public digest = %q, err=%v", publicDigest, err)
	}
	_, fixtureDigest, err := validateFixtures(filepath.Join(projectRoot(t), "eval", "fixtures"), fullSuite)
	if err != nil || fixtureDigest != skynexOrchestratorCanaryFixtureSetDigest {
		t.Fatalf("fixture digest = %q, err=%v", fixtureDigest, err)
	}
	closure, err := runner.ResolveExecutableClosure(selected, "git")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := closure.PathFor("skynex"); err == nil {
		t.Fatal("standalone executable closure unexpectedly contains skynex")
	}

	mutant := append([]contracts.Case(nil), fullSuite...)
	for index := range mutant {
		if mutant[index].ID == "skx_low_direct" {
			mutant[index].Input += " changed"
			break
		}
	}
	if _, err := selectSkynexOrchestratorCanaryCases(mutant); err == nil || !strings.Contains(err.Error(), "digest differs") {
		t.Fatalf("mutated pinned case accepted: %v", err)
	}
}

func TestSkynexOrchestratorCanaryManifestFixesTheScreeningPopulation(t *testing.T) {
	manifest := &experiment.Manifest{
		Suite:           skynexOrchestratorCanarySuite,
		Intent:          experiment.IntentDevelopment,
		PublicCaseCount: skynexOrchestratorCanaryPublicCaseCount,
		Runs:            5,
	}
	if err := validateSkynexOrchestratorCanaryManifest(manifest); err == nil || !strings.Contains(err.Error(), "runs must equal 2") {
		t.Fatalf("mutable run count accepted: %v", err)
	}
}

func TestValidateWorkflowCanarySampleChecksRequiresFullOwnedProfile(t *testing.T) {
	testCase := workflowCanaryCaseFixtures(t)[0]
	sample := contracts.RunResult{}
	for _, declared := range testCase.BehaviorChecks {
		sample.Checks = append(sample.Checks, contracts.CheckResult{
			ID: declared.ID, Type: declared.Type, Hard: true, Status: contracts.CheckStatusPass,
		})
	}
	for _, category := range []string{"infrastructure", "filesystem", "acceptance", "behavior", "claim-consistency", "security"} {
		sample.Checks = append(sample.Checks, contracts.CheckResult{
			ID: "judge_" + strings.ReplaceAll(category, "-", "_"), Type: category,
			Hard: true, Status: contracts.CheckStatusPass,
		})
	}
	if err := validateWorkflowCanarySampleChecks(sample, testCase); err != nil {
		t.Fatalf("complete evaluator-owned checks rejected: %v", err)
	}
	sample.Checks = sample.Checks[1:]
	if err := validateWorkflowCanarySampleChecks(sample, testCase); err == nil || !strings.Contains(err.Error(), "required hard check") {
		t.Fatalf("missing profile check accepted: %v", err)
	}
}

func TestEvaluateWorkflowCanaryPromotion(t *testing.T) {
	plan := workflowCanaryTestPlan()
	result := passingCanaryExecution(plan, 100, 110)
	evaluation := evaluateWorkflowCanary(result, plan, nil)
	if evaluation.Decision != canaryDecisionPromote || evaluation.ExitCode != contracts.ExitSuccess {
		t.Fatalf("evaluation = %#v", evaluation)
	}
	if len(evaluation.Reasons) != 1 || evaluation.Reasons[0] != "all_gates_passed" {
		t.Fatalf("reasons = %v", evaluation.Reasons)
	}
	if evaluation.Summary.TreeInputRatio == nil || *evaluation.Summary.TreeInputRatio != 1.1 {
		t.Fatalf("ratio = %v", evaluation.Summary.TreeInputRatio)
	}
}

func TestEvaluateWorkflowCanaryRejectsTimeoutAndStopsPromotion(t *testing.T) {
	plan := workflowCanaryTestPlan()
	result := passingCanaryExecution(plan, 100, 100)
	result.Samples = result.Samples[:2]
	result.Samples[1].Status = contracts.RunStatusBudgetExhausted
	evaluation := evaluateWorkflowCanary(result, plan, nil)
	if evaluation.Decision != canaryDecisionReject || evaluation.ExitCode != contracts.ExitFailed {
		t.Fatalf("evaluation = %#v", evaluation)
	}
	if len(evaluation.Reasons) != 1 || evaluation.Reasons[0] != "timeout_is_failure" {
		t.Fatalf("reasons = %v", evaluation.Reasons)
	}
}

func TestEvaluateWorkflowCanaryDecisionTable(t *testing.T) {
	plan := workflowCanaryTestPlan()
	tests := []struct {
		name   string
		result canaryExecutionResult
		err    error
		want   canaryDecision
		exit   int
		reason string
	}{
		{
			name: "behavioral failure",
			result: func() canaryExecutionResult {
				r := passingCanaryExecution(plan, 100, 100)
				r.Samples = r.Samples[:1]
				r.Samples[0].Status = contracts.RunStatusFail
				return r
			}(),
			want: canaryDecisionReject, exit: contracts.ExitFailed, reason: "sample_failed",
		},
		{
			name:   "context regression",
			result: passingCanaryExecution(plan, 100, 116),
			want:   canaryDecisionReject, exit: contracts.ExitFailed, reason: "tree_input_ratio_exceeded",
		},
		{
			name: "missing telemetry",
			result: func() canaryExecutionResult {
				r := passingCanaryExecution(plan, 100, 100)
				r.Samples[0].TelemetryComplete = false
				return r
			}(),
			want: canaryDecisionInconclusive, exit: contracts.ExitInconclusive, reason: "telemetry_incomplete",
		},
		{
			name: "incomplete",
			result: func() canaryExecutionResult {
				r := passingCanaryExecution(plan, 100, 100)
				r.Samples = r.Samples[:2]
				return r
			}(),
			want: canaryDecisionInconclusive, exit: contracts.ExitInconclusive, reason: "incomplete_population",
		},
		{
			name: "runtime compatibility",
			result: func() canaryExecutionResult {
				r := passingCanaryExecution(plan, 100, 100)
				for index := range r.Samples {
					if r.Samples[index].Variant == string(stats.VariantCandidate) {
						r.Samples[index].Provenance.ToolsetDigest = "sha256:" + strings.Repeat("e", 64)
						break
					}
				}
				return r
			}(),
			want: canaryDecisionInconclusive, exit: contracts.ExitInconclusive, reason: "runtime_compatibility_mismatch",
		},
		{
			name: "timeout outranks compatibility mismatch",
			result: func() canaryExecutionResult {
				r := passingCanaryExecution(plan, 100, 100)
				r.Samples[1].Status = contracts.RunStatusBudgetExhausted
				r.Samples[1].Provenance.ToolsetDigest = "sha256:" + strings.Repeat("e", 64)
				return r
			}(),
			want: canaryDecisionReject, exit: contracts.ExitFailed, reason: "timeout_is_failure",
		},
		{
			name: "behavior failure outranks compatibility mismatch",
			result: func() canaryExecutionResult {
				r := passingCanaryExecution(plan, 100, 100)
				r.Samples[1].Status = contracts.RunStatusFail
				r.Samples[1].Provenance.ToolsetDigest = "sha256:" + strings.Repeat("e", 64)
				return r
			}(),
			want: canaryDecisionReject, exit: contracts.ExitFailed, reason: "sample_failed",
		},
		{
			name:   "zero candidate telemetry",
			result: passingCanaryExecution(plan, 100, 0),
			want:   canaryDecisionInconclusive, exit: contracts.ExitInconclusive, reason: "telemetry_incomplete",
		},
		{
			name: "zero candidate sessions",
			result: func() canaryExecutionResult {
				r := passingCanaryExecution(plan, 100, 100)
				for index := range r.Samples {
					if r.Samples[index].Variant == string(stats.VariantCandidate) {
						r.Samples[index].Usage.Tree.Sessions = 0
						break
					}
				}
				return r
			}(),
			want: canaryDecisionInconclusive, exit: contracts.ExitInconclusive, reason: "telemetry_incomplete",
		},
		{
			name:   "executor infrastructure",
			result: canaryExecutionResult{CleanupComplete: true}, err: errors.New("provider unavailable"),
			want: canaryDecisionInconclusive, exit: contracts.ExitInfrastructure, reason: "executor_infrastructure",
		},
		{
			name:   "global deadline is failure",
			result: canaryExecutionResult{CleanupComplete: true}, err: context.DeadlineExceeded,
			want: canaryDecisionReject, exit: contracts.ExitFailed, reason: "timeout_is_failure",
		},
		{
			name: "cleanup",
			result: func() canaryExecutionResult {
				r := passingCanaryExecution(plan, 100, 100)
				r.CleanupComplete = false
				return r
			}(),
			want: canaryDecisionInconclusive, exit: contracts.ExitInfrastructure, reason: "cleanup_incomplete",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evaluation := evaluateWorkflowCanary(test.result, plan, test.err)
			if evaluation.Decision != test.want || evaluation.ExitCode != test.exit {
				t.Fatalf("evaluation = %#v", evaluation)
			}
			if !containsString(evaluation.Reasons, test.reason) {
				t.Fatalf("reasons = %v, want %q", evaluation.Reasons, test.reason)
			}
		})
	}
}

func TestWorkflowCanaryRuntimeCompatibilityGateChecksEveryCoordinateField(t *testing.T) {
	plan := workflowCanaryTestPlan()
	differentDigest := "sha256:" + strings.Repeat("e", 64)
	tests := []struct {
		name   string
		mutate func(*contracts.Provenance)
	}{
		{name: "toolset", mutate: func(p *contracts.Provenance) { p.ToolsetDigest = differentDigest }},
		{name: "provider", mutate: func(p *contracts.Provenance) { p.Provider = "other" }},
		{name: "model", mutate: func(p *contracts.Provenance) { p.Model = "openai/other" }},
		{name: "execution mode", mutate: func(p *contracts.Provenance) { p.ExecutionMode = contracts.ExecutionIsolatedContainer }},
		{name: "network", mutate: func(p *contracts.Provenance) { p.Network = contracts.NetworkLoopback }},
		{name: "tool catalog", mutate: func(p *contracts.Provenance) {
			p.Extensions[workflowV2CanaryToolCatalogExtension] = differentDigest
		}},
		{name: "authorization", mutate: func(p *contracts.Provenance) {
			p.Extensions[workflowV2CanaryAuthorizationExtension] = differentDigest
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := passingCanaryExecution(plan, 100, 100)
			for index := range result.Samples {
				if result.Samples[index].Variant == string(stats.VariantCandidate) {
					test.mutate(&result.Samples[index].Provenance)
					break
				}
			}
			evaluation := evaluateWorkflowCanary(result, plan, nil)
			if evaluation.Decision != canaryDecisionInconclusive || evaluation.ExitCode != contracts.ExitInconclusive ||
				!containsString(evaluation.Reasons, "runtime_compatibility_mismatch") {
				t.Fatalf("evaluation = %#v", evaluation)
			}
		})
	}
}

func TestWorkflowCanaryGateRequiresEveryCleanupAttestation(t *testing.T) {
	plan := workflowCanaryTestPlan()
	for _, value := range []string{"", "false"} {
		result := passingCanaryExecution(plan, 100, 100)
		if value == "" {
			delete(result.Samples[0].Provenance.Extensions, runner.ProvenanceExtensionRuntimeCleanupAttested)
		} else {
			result.Samples[0].Provenance.Extensions[runner.ProvenanceExtensionRuntimeCleanupAttested] = value
		}
		evaluation := evaluateWorkflowCanary(result, plan, nil)
		if evaluation.ExitCode != contracts.ExitInfrastructure || !containsString(evaluation.Reasons, "cleanup_incomplete") {
			t.Fatalf("value=%q evaluation=%#v", value, evaluation)
		}
	}
}

func TestCanaryArtifactIntegrity(t *testing.T) {
	artifact := canaryArtifact{
		SchemaVersion: 1, Kind: workflowV2CanaryKind, Profile: workflowV2CanaryProfile,
		Authority: workflowV2CanaryAuthority, ExperimentID: "canary", Suite: workflowV2CanarySuite,
		Samples: []contracts.RunResult{}, Reasons: []string{"incomplete_population"},
	}
	if err := sealCanaryArtifact(&artifact); err != nil {
		t.Fatal(err)
	}
	if err := verifyCanaryArtifactIntegrity(artifact); err != nil {
		t.Fatal(err)
	}
	artifact.Decision = canaryDecisionPromote
	if err := verifyCanaryArtifactIntegrity(artifact); err == nil {
		t.Fatal("tampered artifact passed integrity validation")
	}
}

func TestWorkflowCanaryExecutorRunsCommittedCoordinatesSerially(t *testing.T) {
	now := time.Now().UTC()
	request := workflowCanaryExecutorRequest(now)
	coordinates := flattenCanaryPlan(request.Plan)
	var calls int
	var specs []modelRunSpec
	verifications := 0
	result, err := executeWorkflowV2CanaryWithDependencies(context.Background(), request, canaryExecutorDependencies{
		now: func() time.Time { return now },
		verifyAuthority: func() error {
			verifications++
			return nil
		},
		runModel: func(_ context.Context, spec modelRunSpec) (modelRunResult, error) {
			specs = append(specs, spec)
			coordinate := coordinates[calls]
			calls++
			sample := validRun("canary-run-"+string(rune('a'+calls)), string(coordinate.Variant), contracts.RunStatusPass)
			sample.CaseID = coordinate.CaseID
			sample.Provenance.Extensions[runner.ProvenanceExtensionRuntimeCleanupAttested] = "true"
			return modelRunResult{Result: runner.ContractResult{Samples: []contracts.RunResult{sample}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != workflowV2CanaryMaxSamples || len(result.Samples) != workflowV2CanaryMaxSamples || !result.CleanupComplete {
		t.Fatalf("calls=%d samples=%d cleanup=%t", calls, len(result.Samples), result.CleanupComplete)
	}
	if verifications != workflowV2CanaryMaxSamples*2 {
		t.Fatalf("authority verifications = %d", verifications)
	}
	for index, spec := range specs {
		coordinate := coordinates[index]
		wantBundle := request.Control
		if coordinate.Variant == stats.VariantCandidate {
			wantBundle = request.Candidate
		}
		if spec.AgentBundleRoot != wantBundle.AbsoluteRoot || spec.VerifiedBundleDigest != wantBundle.Snapshot.Digest || !spec.RequireExactBundle {
			t.Fatalf("spec %d did not bind arm bundle: %#v", index, spec)
		}
		if spec.WorkflowPlugin != request.WorkflowPlugin || spec.SkynexBinary != request.SkynexBinary || spec.OpenAIOAuthSession != request.OpenAIOAuthSession {
			t.Fatalf("spec %d lost frozen Workflow authority", index)
		}
		if spec.AllowImpure || spec.RetainTrace || spec.Repetitions != 1 || spec.RepetitionStart != 1 {
			t.Fatalf("spec %d violates canary execution controls", index)
		}
	}
}

func TestSkynexOrchestratorCanaryExecutorOmitsWorkflowAuthorities(t *testing.T) {
	now := time.Now().UTC()
	request := skynexOrchestratorCanaryExecutorRequest(t, now)
	coordinates := flattenCanaryPlan(request.Plan)
	calls := 0
	result, err := executeWorkflowV2CanaryWithDependencies(context.Background(), request, canaryExecutorDependencies{
		now:             func() time.Time { return now },
		verifyAuthority: func() error { return nil },
		runModel: func(_ context.Context, spec modelRunSpec) (modelRunResult, error) {
			if spec.Suite != skynexOrchestratorCanarySuite || spec.WorkflowPlugin != nil || spec.SkynexBinary != nil {
				t.Fatalf("standalone spec leaked Workflow V2 authority: %#v", spec)
			}
			coordinate := coordinates[calls]
			calls++
			sample := validRun("standalone-canary-"+string(rune('a'+calls)), string(coordinate.Variant), contracts.RunStatusPass)
			sample.CaseID = coordinate.CaseID
			sample.Provenance.Extensions[runner.ProvenanceExtensionRuntimeCleanupAttested] = "true"
			return modelRunResult{Result: runner.ContractResult{Samples: []contracts.RunResult{sample}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != workflowV2CanaryMaxSamples || len(result.Samples) != workflowV2CanaryMaxSamples {
		t.Fatalf("calls=%d samples=%d", calls, len(result.Samples))
	}
}

func TestCanaryRuntimeAuthorityIsProfileExact(t *testing.T) {
	now := time.Now().UTC()
	standalone := skynexOrchestratorCanaryExecutorRequest(t, now)
	if err := validateCanaryExecutionRequest(standalone); err != nil {
		t.Fatalf("valid standalone request: %v", err)
	}

	withPlugin := standalone
	withPlugin.WorkflowPlugin = &toolpolicy.ControlledPluginIdentity{}
	if err := validateCanaryExecutionRequest(withPlugin); err == nil || !strings.Contains(err.Error(), "forbids Workflow V2") {
		t.Fatalf("standalone plugin authority error = %v", err)
	}
	withSkynex := standalone
	withSkynex.SkynexBinary = &runner.ExecutableSnapshot{}
	if err := validateCanaryExecutionRequest(withSkynex); err == nil || !strings.Contains(err.Error(), "forbids Workflow V2") {
		t.Fatalf("standalone skynex authority error = %v", err)
	}
	withDriver := standalone
	withDriver.Cases = append([]contracts.Case(nil), standalone.Cases...)
	withDriver.Cases[0].Extensions = map[string]any{"x-workflow-driver-v1": map[string]any{}}
	if err := validateCanaryExecutionRequest(withDriver); err == nil || !strings.Contains(err.Error(), "managed Workflow V2") {
		t.Fatalf("standalone workflow driver error = %v", err)
	}

	workflow := workflowCanaryExecutorRequest(now)
	workflow.WorkflowPlugin = nil
	if err := validateCanaryExecutionRequest(workflow); err == nil || !strings.Contains(err.Error(), "requires frozen skynex and plugin") {
		t.Fatalf("Workflow V2 missing authority error = %v", err)
	}
	unsupported := standalone
	unsupported.Profile = "custom-canary"
	if err := validateCanaryExecutionRequest(unsupported); err == nil || !strings.Contains(err.Error(), "unsupported profile") {
		t.Fatalf("unsupported profile error = %v", err)
	}
}

func TestSkynexOrchestratorCanaryExecutorRejectsCaseAuthorityDriftBeforeModelCall(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name   string
		mutate func(*canaryExecutionRequest)
		want   string
	}{
		{
			name: "case digest",
			mutate: func(request *canaryExecutionRequest) {
				request.Cases = append([]contracts.Case(nil), request.Cases...)
				request.Cases[0].Input += " drift"
			},
			want: "digest differs",
		},
		{
			name: "missing",
			mutate: func(request *canaryExecutionRequest) {
				request.Cases = append([]contracts.Case(nil), request.Cases[:2]...)
			},
			want: "exactly 3",
		},
		{
			name: "extra",
			mutate: func(request *canaryExecutionRequest) {
				request.Cases = append(append([]contracts.Case(nil), request.Cases...), contracts.Case{ID: "skx_extra"})
			},
			want: "exactly 3",
		},
		{
			name: "duplicate",
			mutate: func(request *canaryExecutionRequest) {
				request.Cases = append([]contracts.Case(nil), request.Cases...)
				request.Cases[2] = request.Cases[0]
			},
			want: "duplicate standalone canary case",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := skynexOrchestratorCanaryExecutorRequest(t, now)
			test.mutate(&request)
			modelCalls := 0
			_, err := executeWorkflowV2CanaryWithDependencies(context.Background(), request, canaryExecutorDependencies{
				now:             func() time.Time { return now },
				verifyAuthority: func() error { return nil },
				runModel: func(context.Context, modelRunSpec) (modelRunResult, error) {
					modelCalls++
					return modelRunResult{}, nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if modelCalls != 0 {
				t.Fatalf("invalid authority made %d model calls", modelCalls)
			}
		})
	}
}

func TestWorkflowCanaryExecutorFailsFast(t *testing.T) {
	now := time.Now().UTC()
	request := workflowCanaryExecutorRequest(now)
	coordinates := flattenCanaryPlan(request.Plan)
	calls := 0
	result, err := executeWorkflowV2CanaryWithDependencies(context.Background(), request, canaryExecutorDependencies{
		now:             func() time.Time { return now },
		verifyAuthority: func() error { return nil },
		runModel: func(_ context.Context, spec modelRunSpec) (modelRunResult, error) {
			coordinate := coordinates[calls]
			calls++
			status := contracts.RunStatusPass
			if calls == 2 {
				status = contracts.RunStatusFail
			}
			sample := validRun("fail-fast-"+string(rune('a'+calls)), string(coordinate.Variant), status)
			sample.CaseID = coordinate.CaseID
			sample.Provenance.Extensions[runner.ProvenanceExtensionRuntimeCleanupAttested] = "true"
			return modelRunResult{Result: runner.ContractResult{Samples: []contracts.RunResult{sample}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(result.Samples) != 2 || result.Samples[1].Status != contracts.RunStatusFail {
		t.Fatalf("calls=%d samples=%#v", calls, result.Samples)
	}
}

func TestWorkflowCanaryExecutorRejectsUnattestedPassingSample(t *testing.T) {
	for _, value := range []string{"", "false"} {
		t.Run("value_"+value, func(t *testing.T) {
			now := time.Now().UTC()
			request := workflowCanaryExecutorRequest(now)
			coordinate := flattenCanaryPlan(request.Plan)[0]
			result, err := executeWorkflowV2CanaryWithDependencies(context.Background(), request, canaryExecutorDependencies{
				now:             func() time.Time { return now },
				verifyAuthority: func() error { return nil },
				runModel: func(context.Context, modelRunSpec) (modelRunResult, error) {
					sample := validRun("cleanup-unattested", string(coordinate.Variant), contracts.RunStatusPass)
					sample.CaseID = coordinate.CaseID
					if value != "" {
						sample.Provenance.Extensions[runner.ProvenanceExtensionRuntimeCleanupAttested] = value
					}
					return modelRunResult{Result: runner.ContractResult{Samples: []contracts.RunResult{sample}}}, nil
				},
			})
			if err == nil || result.CleanupComplete || len(result.Samples) != 0 {
				t.Fatalf("err=%v cleanup=%t samples=%d", err, result.CleanupComplete, len(result.Samples))
			}
		})
	}
}

func TestWorkflowCanaryExecutorDoesNotPublishRunnerErrorSample(t *testing.T) {
	now := time.Now().UTC()
	request := workflowCanaryExecutorRequest(now)
	calls := 0
	result, err := executeWorkflowV2CanaryWithDependencies(context.Background(), request, canaryExecutorDependencies{
		now:             func() time.Time { return now },
		verifyAuthority: func() error { return nil },
		runModel: func(_ context.Context, spec modelRunSpec) (modelRunResult, error) {
			calls++
			if calls == 1 {
				sample := validRun("first-pass", spec.Variant, contracts.RunStatusPass)
				sample.CaseID = spec.Cases[0].ID
				sample.Provenance.Extensions[runner.ProvenanceExtensionRuntimeCleanupAttested] = "true"
				return modelRunResult{Result: runner.ContractResult{Samples: []contracts.RunResult{sample}}}, nil
			}
			sample := validRun("untrusted-error-sample", spec.Variant, contracts.RunStatusPass)
			sample.CaseID = spec.Cases[0].ID
			sample.Provenance.Extensions[runner.ProvenanceExtensionRuntimeCleanupAttested] = "true"
			return modelRunResult{Result: runner.ContractResult{Samples: []contracts.RunResult{sample}}}, errors.New("cleanup could not be attested")
		},
	})
	if err == nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if len(result.Samples) != 1 || result.CleanupComplete {
		t.Fatalf("samples=%d cleanup=%t", len(result.Samples), result.CleanupComplete)
	}
}

func TestWorkflowCanaryExecutorRefusesLateScheduling(t *testing.T) {
	now := time.Now().UTC()
	request := workflowCanaryExecutorRequest(now)
	request.SchedulingDeadline = now.Add(-time.Second)
	request.ExecutionDeadline = now.Add(time.Minute)
	calls := 0
	result, err := executeWorkflowV2CanaryWithDependencies(context.Background(), request, canaryExecutorDependencies{
		now:             func() time.Time { return now },
		verifyAuthority: func() error { return nil },
		runModel: func(context.Context, modelRunSpec) (modelRunResult, error) {
			calls++
			return modelRunResult{}, nil
		},
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 0 || len(result.Samples) != 0 {
		t.Fatalf("err=%v calls=%d samples=%d", err, calls, len(result.Samples))
	}
}

func workflowCanaryCaseFixtures(t *testing.T) []contracts.Case {
	t.Helper()
	loaded, err := loadSelectedCases(filepath.Join(projectRoot(t), "eval", "cases"), workflowV2CanarySuite, "")
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func workflowCanaryTestPlan() stats.ExperimentPlan {
	return stats.ExperimentPlan{
		Method: workflowV2CanaryPlanMethod, RunsPerCase: 1, SerializeWithinBlock: true,
		Blocks: []stats.BlockPlan{
			{ID: "case-a-0001", CaseID: "case-a", Repetition: 1, Order: []stats.Variant{stats.VariantControl, stats.VariantCandidate}},
			{ID: "case-b-0001", CaseID: "case-b", Repetition: 1, Order: []stats.Variant{stats.VariantCandidate, stats.VariantControl}},
			{ID: "case-c-0001", CaseID: "case-c", Repetition: 1, Order: []stats.Variant{stats.VariantControl, stats.VariantCandidate}},
		},
	}
}

func workflowCanaryExecutorRequest(now time.Time) canaryExecutionRequest {
	plan := workflowCanaryTestPlan()
	cases := make([]contracts.Case, 0, len(plan.Blocks))
	for _, block := range plan.Blocks {
		cases = append(cases, contracts.Case{ID: block.CaseID, Completion: contracts.CompletionConfig{Timeout: "4m"}})
	}
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	return canaryExecutionRequest{
		Profile: workflowV2CanaryProfile,
		Manifest: experiment.Manifest{
			Suite:   workflowV2CanarySuite,
			Harness: experiment.FrozenBundle{Digest: digestA},
			Execution: experiment.Execution{
				OpenCodeVersion: defaultOpenCodeVersion, OpenCodeBinaryDigest: digestA,
				OpenCodeOpenAPIDigest: digestA, ToolchainsDigest: digestA,
			},
		},
		ManifestDigest: digestA, Cases: cases, FixturesDir: "/frozen/fixtures",
		Control:   experiment.VerifiedBundle{AbsoluteRoot: "/frozen/control", Snapshot: sandbox.Snapshot{Digest: digestA}},
		Candidate: experiment.VerifiedBundle{AbsoluteRoot: "/frozen/candidate", Snapshot: sandbox.Snapshot{Digest: digestB}},
		Frozen:    &experiment.FrozenSet{}, Plan: plan,
		SkynexBinary: &runner.ExecutableSnapshot{}, ExecutableClosure: &runner.ExecutableClosure{},
		WorkflowPlugin: &toolpolicy.ControlledPluginIdentity{}, OpenAIOAuthSession: &lifecycle.OpenAIOAuthSession{},
		SampleTimeout: workflowV2CanarySampleLimit, SampleBudget: workflowV2CanarySampleBudget,
		CleanupReserve:     workflowV2CanaryCleanupReserve,
		SchedulingDeadline: now.Add(25 * time.Minute), ExecutionDeadline: now.Add(26 * time.Minute),
		FailFast: true, RunsPerArm: 1, MaximumSampleCount: 6,
	}
}

func skynexOrchestratorCanaryExecutorRequest(t *testing.T, now time.Time) canaryExecutionRequest {
	t.Helper()
	fullSuite, err := loadSelectedCases(filepath.Join(projectRoot(t), "eval", "cases"), skynexOrchestratorCanarySuite, "")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectSkynexOrchestratorCanaryCases(fullSuite)
	if err != nil {
		t.Fatal(err)
	}
	request := workflowCanaryExecutorRequest(now)
	request.Profile = skynexOrchestratorCanaryProfile
	request.Manifest.Suite = skynexOrchestratorCanarySuite
	request.Cases = selected
	for index := range request.Plan.Blocks {
		request.Plan.Blocks[index].ID = selected[index].ID + "-0001"
		request.Plan.Blocks[index].CaseID = selected[index].ID
	}
	request.SkynexBinary = nil
	request.WorkflowPlugin = nil
	return request
}

func passingCanaryExecution(plan stats.ExperimentPlan, controlTokens, candidateTokens int64) canaryExecutionResult {
	coordinates := flattenCanaryPlan(plan)
	samples := make([]contracts.RunResult, 0, len(coordinates))
	digest := "sha256:" + strings.Repeat("d", 64)
	for index, coordinate := range coordinates {
		tokens := controlTokens
		if coordinate.Variant == stats.VariantCandidate {
			tokens = candidateTokens
		}
		samples = append(samples, contracts.RunResult{
			RunID: "run-" + string(rune('a'+index)), CaseID: coordinate.CaseID,
			Variant: string(coordinate.Variant), Repetition: coordinate.Repetition,
			Status: contracts.RunStatusPass, TelemetryComplete: true,
			Provenance: contracts.Provenance{
				Model: "openai/gpt-test", Provider: "openai", ToolsetDigest: digest,
				ExecutionMode: contracts.ExecutionTrustedLocal, Network: contracts.NetworkHostUnisolated,
				Extensions: map[string]string{
					workflowV2CanaryToolCatalogExtension:             digest,
					runner.ProvenanceExtensionRuntimeCleanupAttested: "true",
				},
			},
			Usage: contracts.Usage{Tree: contracts.TreeUsage{SumInputTokens: tokens, Sessions: 1}},
		})
	}
	now := time.Now().UTC()
	return canaryExecutionResult{Samples: samples, StartedAt: now, EndedAt: now.Add(time.Second), CleanupComplete: true}
}
