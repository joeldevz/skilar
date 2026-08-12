package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/experiment"
	"github.com/joeldevz/skynex/internal/eval/lifecycle"
	"github.com/joeldevz/skynex/internal/eval/runner"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/stats"
	"github.com/joeldevz/skynex/internal/eval/toolpolicy"
)

const (
	workflowV2CanaryProfile                = "workflow-v2-canary-v1"
	workflowV2CanarySuite                  = "workflow-v2-canary"
	workflowV2CanaryKind                   = "skynex-workflow-v2-canary"
	workflowV2CanaryAuthority              = "screening-non-release"
	workflowV2CanaryPlanMethod             = "single-pair-screen-v1"
	workflowV2CanaryDefaultPlugin          = "/usr/local/share/skynex-eval/skynex-workflow.ts"
	workflowV2CanaryToolCatalogExtension   = "x-effective-tool-catalog-digest"
	workflowV2CanaryAuthorizationExtension = "x-effective-authorization-digest"
	workflowV2CanaryPublicCasesDigest      = "sha256:45f8c7a75bd83669262aee39d84881e1a3dd3555353d7cbdd49adf405e865e81"
	workflowV2CanaryFixtureSetDigest       = "sha256:d9249b98e931e2856c911a3e5ddff25ff4eb0fa7bbd1eb5d442a8d12d9e38aa2"

	workflowV2CanaryCaseCount  = 3
	workflowV2CanaryRunsPerArm = 1
	workflowV2CanaryMaxSamples = workflowV2CanaryCaseCount * 2
)

const (
	workflowV2CanaryGlobalLimit    = 30 * time.Minute
	workflowV2CanaryPreflightLimit = time.Minute
	workflowV2CanarySampleLimit    = 4 * time.Minute
	workflowV2CanarySampleBudget   = 24 * time.Minute
	workflowV2CanaryCleanupReserve = time.Minute
	workflowV2CanarySealReserve    = 4 * time.Minute
	workflowV2CanaryMaxInputRatio  = 1.15
)

type canaryDecision string

const (
	canaryDecisionPromote      canaryDecision = "promote"
	canaryDecisionReject       canaryDecision = "reject"
	canaryDecisionInconclusive canaryDecision = "inconclusive"
)

// canaryExecutionRequest is the executor seam for the Workflow V2 runtime.
// The command owns population, provenance, timing and promotion policy; an
// executor may only execute this already-frozen plan and return its samples.
type canaryExecutionRequest struct {
	Profile             string
	Manifest            experiment.Manifest
	ManifestDigest      string
	Cases               []contracts.Case
	CasesDir            string
	FixturesDir         string
	Control             experiment.VerifiedBundle
	Candidate           experiment.VerifiedBundle
	Frozen              *experiment.FrozenSet
	Plan                stats.ExperimentPlan
	OpenCodeBinary      resolvedOpenCodeBinary
	SkynexBinary        *runner.ExecutableSnapshot
	WorkflowPlugin      *toolpolicy.ControlledPluginIdentity
	ExecutableClosure   *runner.ExecutableClosure
	OpenAIOAuthSession  *lifecycle.OpenAIOAuthSession
	SampleTimeout       time.Duration
	SampleBudget        time.Duration
	CleanupReserve      time.Duration
	SchedulingDeadline  time.Time
	ExecutionDeadline   time.Time
	FailFast            bool
	RunsPerArm          int
	MaximumSampleCount  int
	RetainTrace         bool
	AllowAmbientPlugins bool
}

// canaryExecutionResult deliberately contains only evaluator contracts. The
// CLI recomputes counts, gates and integrity rather than trusting an executor's
// conclusion.
type canaryExecutionResult struct {
	Samples         []contracts.RunResult
	StartedAt       time.Time
	EndedAt         time.Time
	CleanupComplete bool
}

type canaryLimits struct {
	GlobalWallClock   string  `json:"global_wall_clock"`
	Preflight         string  `json:"preflight"`
	PerSample         string  `json:"per_sample"`
	SampleBudget      string  `json:"sample_budget"`
	CleanupReserve    string  `json:"cleanup_reserve"`
	SealReserve       string  `json:"seal_reserve"`
	RunsPerArm        int     `json:"runs_per_arm"`
	MaximumSamples    int     `json:"maximum_samples"`
	MaximumInputRatio float64 `json:"maximum_tree_input_ratio"`
	FailFast          bool    `json:"fail_fast"`
}

type canaryGateSummary struct {
	PassedSamples            int      `json:"passed_samples"`
	FailedSamples            int      `json:"failed_samples"`
	Timeouts                 int      `json:"timeouts"`
	PassToFailRegressions    int      `json:"pass_to_fail_regressions"`
	FailedHardChecks         int      `json:"failed_hard_checks"`
	FalseSuccesses           int      `json:"false_successes"`
	RuntimeCompatible        bool     `json:"runtime_compatible"`
	TelemetryComplete        bool     `json:"telemetry_complete"`
	ControlTreeInputTokens   int64    `json:"control_tree_input_tokens"`
	CandidateTreeInputTokens int64    `json:"candidate_tree_input_tokens"`
	TreeInputRatio           *float64 `json:"tree_input_ratio,omitempty"`
}

type canaryPromotionContract struct {
	FullPublicSuiteRequired bool   `json:"full_public_suite_required"`
	CandidateBundleDigest   string `json:"candidate_bundle_digest"`
	DigestReuseRequired     bool   `json:"digest_reuse_required"`
	ReleaseEvidence         bool   `json:"release_evidence"`
}

type canaryArtifact struct {
	SchemaVersion         int                     `json:"schema_version"`
	Kind                  string                  `json:"kind"`
	IntegrityDigest       string                  `json:"integrity_digest"`
	Profile               string                  `json:"profile"`
	Authority             string                  `json:"authority"`
	ExperimentID          string                  `json:"experiment_id"`
	ManifestDigest        string                  `json:"manifest_digest"`
	Suite                 string                  `json:"suite"`
	HarnessBundleDigest   string                  `json:"harness_bundle_digest"`
	ControlBundleDigest   string                  `json:"control_bundle_digest"`
	CandidateBundleDigest string                  `json:"candidate_bundle_digest"`
	WorkflowPluginDigest  string                  `json:"workflow_plugin_digest"`
	HoldoutUsed           bool                    `json:"holdout_used"`
	Plan                  stats.ExperimentPlan    `json:"plan"`
	Limits                canaryLimits            `json:"limits"`
	StartedAt             time.Time               `json:"started_at"`
	EndedAt               time.Time               `json:"ended_at"`
	Samples               []contracts.RunResult   `json:"samples"`
	PlannedSamples        int                     `json:"planned_samples"`
	CompletedSamples      int                     `json:"completed_samples"`
	SkippedSamples        int                     `json:"skipped_samples"`
	CleanupComplete       bool                    `json:"cleanup_complete"`
	Decision              canaryDecision          `json:"decision"`
	Reasons               []string                `json:"reasons"`
	Gates                 canaryGateSummary       `json:"gates"`
	Promotion             canaryPromotionContract `json:"promotion"`
	ExitCode              int                     `json:"exit_code"`
}

type canaryCommandResult struct {
	ExperimentID          string         `json:"experiment_id"`
	Profile               string         `json:"profile"`
	Authority             string         `json:"authority"`
	Decision              canaryDecision `json:"decision"`
	Reasons               []string       `json:"reasons"`
	CandidateBundleDigest string         `json:"candidate_bundle_digest"`
	PlannedSamples        int            `json:"planned_samples"`
	CompletedSamples      int            `json:"completed_samples"`
	SkippedSamples        int            `json:"skipped_samples"`
	ArtifactPath          string         `json:"artifact_path"`
	ExitCode              int            `json:"exit_code"`
}

func (r canaryCommandResult) CLIExitCode() int { return r.ExitCode }

type workflowCanaryDriver struct {
	Mode            string `json:"mode"`
	WorkflowID      string `json:"workflow_id"`
	TerminalState   string `json:"terminal_state"`
	AutonomousTurns int    `json:"autonomous_turns"`
}

type workflowCanaryCasePin struct {
	CaseDigest    string
	FixtureDigest string
}

func workflowV2CanaryCasePinFor(id string) (workflowCanaryCasePin, bool) {
	switch id {
	case "wfv2_canary_detach_wake":
		return workflowCanaryCasePin{
			CaseDigest:    "sha256:c070d06460ee1987eb8d3747ad28d03f036238d0359c76e1fe576afdeef19150",
			FixtureDigest: "sha256:a32b85f2158ba18435d022992d59894ed8cfd565a5f5dd428ebaefe5f61c256d",
		}, true
	case "wfv2_canary_low_complete":
		return workflowCanaryCasePin{
			CaseDigest:    "sha256:ce3b4fcac27702fb536bf8a75b2abdd607d49ca59e3dce0d24eef1e787fbd262",
			FixtureDigest: "sha256:e63a5fbcd62320401f68881a9a5c74061843ee628e80c408dbd80efce3e57d4d",
		}, true
	case "wfv2_canary_recovery_safety":
		return workflowCanaryCasePin{
			CaseDigest:    "sha256:e030ff606ed6e3fc19a1a27d5e1e031b149adf47162431dbe5070a0bfc3c2387",
			FixtureDigest: "sha256:7d1ec76ab2c7446f9f53bfda5b82f41c0cc87e99edcc2a364e0ab893bfa3ee47",
		}, true
	default:
		return workflowCanaryCasePin{}, false
	}
}

type preparedCanary struct {
	request         canaryExecutionRequest
	frozen          *experiment.FrozenSet
	outputPath      string
	evaluatorDigest string
}

type canaryGateEvaluation struct {
	Decision canaryDecision
	Reasons  []string
	Summary  canaryGateSummary
	ExitCode int
}

func commandCanary(ctx context.Context, args []string, deps dependencies) (canaryCommandResult, error) {
	set := newFlagSet("canary")
	allow := set.Bool("allow-model-calls", false, "authorize model calls that may consume quota or incur provider charges")
	profile := set.String("profile", "", "immutable screening profile")
	manifestPath := set.String("manifest", "", "frozen experiment manifest")
	openAIOAuth := set.String("openai-oauth", "", "OpenCode auth.json containing an OpenAI OAuth login")
	binary := set.String("binary", "opencode", "OpenCode binary")
	workflowPlugin := set.String("workflow-plugin", workflowV2CanaryDefaultPlugin, "installed evaluator-owned Workflow V2 plugin")
	output := set.String("output", "", "canary artifact path")
	if err := parseFlagSet(set, args); err != nil {
		return canaryCommandResult{}, err
	}
	if err := requireModelOptIn(*allow); err != nil {
		return canaryCommandResult{}, err
	}
	if !isSupportedCanaryProfile(*profile) {
		return canaryCommandResult{}, invalidf(
			"invalid_canary_profile",
			"--profile must equal %q or %q",
			workflowV2CanaryProfile,
			skynexOrchestratorCanaryProfile,
		)
	}
	if strings.TrimSpace(*manifestPath) == "" {
		return canaryCommandResult{}, invalidf("invalid_arguments", "--manifest is required")
	}
	if strings.TrimSpace(*openAIOAuth) == "" {
		return canaryCommandResult{}, invalidf("openai_oauth_required", "canary requires --openai-oauth PATH for its clean OpenCode profile")
	}
	if deps.runCanary == nil {
		return canaryCommandResult{}, infraf("canary_executor_unavailable", errors.New("canary executor is not configured"))
	}
	if deps.probeRuntime == nil {
		return canaryCommandResult{}, infraf("canary_preflight_unavailable", errors.New("canary runtime probe is not configured"))
	}

	hardCtx, hardCancel := context.WithTimeout(ctx, workflowV2CanaryGlobalLimit)
	defer hardCancel()
	startedAt := time.Now().UTC()
	preflightCtx, preflightCancel := context.WithTimeout(hardCtx, workflowV2CanaryPreflightLimit)
	prepared, err := prepareCanaryProfile(preflightCtx, *profile, *manifestPath, *openAIOAuth, *binary, *workflowPlugin, *output)
	if err == nil {
		err = probeWorkflowV2Canary(preflightCtx, prepared.request, deps.probeRuntime)
	}
	preflightCancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return canaryCommandResult{}, canaryTimeoutFailure()
		}
		return canaryCommandResult{}, err
	}

	reservations, err := reserveABOutputs(prepared.outputPath)
	if err != nil {
		return canaryCommandResult{}, invalidf("output_exists", "%v", err)
	}
	defer func() { _ = reservations.Close() }()

	hardDeadline, ok := hardCtx.Deadline()
	if !ok {
		return canaryCommandResult{}, infraf("canary_deadline", errors.New("hard canary deadline is unavailable"))
	}
	executionDeadline := hardDeadline.Add(-workflowV2CanarySealReserve)
	schedulingDeadline := executionDeadline.Add(-workflowV2CanaryCleanupReserve)
	if !time.Now().Before(schedulingDeadline) {
		return canaryCommandResult{}, canaryTimeoutFailure()
	}
	prepared.request.ExecutionDeadline = executionDeadline
	prepared.request.SchedulingDeadline = schedulingDeadline
	executionCtx, executionCancel := context.WithDeadline(hardCtx, executionDeadline)
	execution, executionErr := deps.runCanary(executionCtx, prepared.request)
	executionCancel()

	validationErr := validateWorkflowCanaryExecution(execution, prepared.request)
	driftErr := verifyPreparedCanaryUnchanged(prepared)
	terminalErr := executionErr
	if validationErr != nil || driftErr != nil {
		terminalErr = errors.Join(terminalErr, validationErr, driftErr)
	}
	evaluation := evaluateWorkflowCanary(execution, prepared.request.Plan, terminalErr)
	if validationErr != nil || driftErr != nil {
		evaluation.Decision = canaryDecisionInconclusive
		evaluation.Reasons = []string{"invalid_execution_result"}
		evaluation.ExitCode = contracts.ExitInvalid
	}

	endedAt := execution.EndedAt.UTC()
	if endedAt.IsZero() || endedAt.Before(startedAt) {
		endedAt = time.Now().UTC()
	}
	publishedSamples := append([]contracts.RunResult(nil), execution.Samples...)
	if validationErr != nil {
		// Structurally invalid results are untrusted runtime data. Retain only a
		// stable reason code; never turn malformed sample text into an artifact.
		publishedSamples = []contracts.RunResult{}
	}
	kind, supported := canaryArtifactKind(prepared.request.Profile)
	if !supported {
		return canaryCommandResult{}, invalidf("invalid_canary_profile", "prepared canary has an unsupported profile")
	}
	workflowPluginDigest := ""
	if prepared.request.WorkflowPlugin != nil {
		workflowPluginDigest = prepared.request.WorkflowPlugin.ContentDigest
	}
	artifact := canaryArtifact{
		SchemaVersion: 1, Kind: kind,
		Profile: prepared.request.Profile, Authority: workflowV2CanaryAuthority,
		ExperimentID: prepared.request.Manifest.ID, ManifestDigest: prepared.request.ManifestDigest,
		Suite:                 prepared.request.Manifest.Suite,
		HarnessBundleDigest:   prepared.request.Manifest.Harness.Digest,
		ControlBundleDigest:   prepared.request.Control.Snapshot.Digest,
		CandidateBundleDigest: prepared.request.Candidate.Snapshot.Digest,
		WorkflowPluginDigest:  workflowPluginDigest,
		HoldoutUsed:           false, Plan: prepared.request.Plan, Limits: workflowCanaryLimits(),
		StartedAt: startedAt, EndedAt: endedAt,
		Samples:          publishedSamples,
		PlannedSamples:   prepared.request.MaximumSampleCount,
		CompletedSamples: len(publishedSamples), SkippedSamples: prepared.request.MaximumSampleCount - len(publishedSamples),
		CleanupComplete: execution.CleanupComplete,
		Decision:        evaluation.Decision, Reasons: append([]string(nil), evaluation.Reasons...), Gates: evaluation.Summary,
		Promotion: canaryPromotionContract{
			FullPublicSuiteRequired: true, CandidateBundleDigest: prepared.request.Candidate.Snapshot.Digest,
			DigestReuseRequired: true, ReleaseEvidence: false,
		},
		ExitCode: evaluation.ExitCode,
	}
	if err := sealCanaryArtifact(&artifact); err != nil {
		return canaryCommandResult{}, infraf("seal_canary", err)
	}
	staged, err := stageABJSON(prepared.outputPath, artifact)
	if err != nil {
		return canaryCommandResult{}, infraf("save_canary", err)
	}
	defer func() { _ = os.Remove(staged) }()
	if err := reservations.PublishStaged(prepared.outputPath, staged); err != nil {
		return canaryCommandResult{}, infraf("save_canary", err)
	}

	return canaryCommandResult{
		ExperimentID: artifact.ExperimentID, Profile: artifact.Profile, Authority: artifact.Authority,
		Decision: artifact.Decision, Reasons: append([]string(nil), artifact.Reasons...),
		CandidateBundleDigest: artifact.CandidateBundleDigest,
		PlannedSamples:        artifact.PlannedSamples, CompletedSamples: artifact.CompletedSamples,
		SkippedSamples: artifact.SkippedSamples, ArtifactPath: prepared.outputPath, ExitCode: artifact.ExitCode,
	}, nil
}

func canaryTimeoutFailure() error {
	// Do not wrap context.DeadlineExceeded here. classifyCommandError gives raw
	// context deadlines the generic budget-exhausted exit, while this screening
	// contract deliberately treats every timeout as a candidate rejection.
	return &commandError{
		exitCode: contracts.ExitFailed,
		kind:     "canary_timeout",
		err:      errors.New("canary exceeded its screening deadline"),
	}
}

func probeWorkflowV2Canary(
	ctx context.Context,
	request canaryExecutionRequest,
	probe func(context.Context, doctorOptions) (doctorResult, error),
) error {
	models, err := effectiveOpenAIModels(&request.Manifest, request.Cases, "openai/gpt-5.6-terra")
	if err != nil {
		return err
	}
	resolvedBinary := request.OpenCodeBinary
	result, err := probe(ctx, doctorOptions{
		Binary: request.OpenCodeBinary.Path, ResolvedBinary: &resolvedBinary,
		ExpectedVersion: request.Manifest.Execution.OpenCodeVersion,
		Timeout:         30 * time.Second, OpenAIOAuthSession: request.OpenAIOAuthSession, Models: models,
	})
	if err != nil {
		var mismatch *lifecycle.VersionMismatchError
		if errors.As(err, &mismatch) {
			return invalidf("opencode_version_mismatch", "%v", err)
		}
		var incompatible *openCodeCompatibilityError
		if errors.As(err, &incompatible) || errors.Is(err, client.ErrIncompatibleAPI) ||
			errors.Is(err, client.ErrInvalidProviderCatalog) || errors.Is(err, client.ErrInvalidMCPStatusCatalog) {
			return invalidf("opencode_api_incompatible", "%v", err)
		}
		return infraf("opencode_preflight", err)
	}
	if !result.Healthy || result.Version != request.Manifest.Execution.OpenCodeVersion {
		return invalidf("opencode_version_mismatch", "canary preflight observed an unexpected OpenCode version")
	}
	openAPIDigest := ""
	for _, endpoint := range result.Endpoints {
		if endpoint.Name == "/doc" {
			openAPIDigest = endpoint.Digest
			break
		}
	}
	if openAPIDigest != request.Manifest.Execution.OpenCodeOpenAPIDigest {
		return invalidf("opencode_api_mismatch", "canary preflight OpenAPI digest differs from the frozen manifest")
	}
	if err := verifyCanaryExecutionAuthority(request); err != nil {
		return invalidf("canary_authority_drift", "%v", err)
	}
	return nil
}

func prepareWorkflowV2Canary(ctx context.Context, manifestPath, oauthPath, binary, workflowPluginPath, output string) (preparedCanary, error) {
	var prepared preparedCanary
	if err := ctx.Err(); err != nil {
		return prepared, err
	}
	manifest, err := experiment.Load(manifestPath)
	if err != nil {
		return prepared, invalidf("invalid_manifest", "%v", err)
	}
	if err := validateWorkflowCanaryManifest(manifest); err != nil {
		return prepared, invalidf("invalid_canary_manifest", "%v", err)
	}
	manifestDigest, err := contracts.CanonicalDigest(*manifest)
	if err != nil {
		return prepared, invalidf("invalid_manifest_digest", "%v", err)
	}
	manifestDir, err := filepath.Abs(filepath.Dir(manifestPath))
	if err != nil {
		return prepared, invalidf("invalid_manifest", "%v", err)
	}
	observedEvaluatorDigest, err := executableDigest()
	if err != nil {
		return prepared, infraf("evaluator_provenance", err)
	}
	if observedEvaluatorDigest != manifest.Execution.EvaluatorBinaryDigest {
		return prepared, invalidf("evaluator_binary_mismatch", "frozen evaluator digest does not match this binary")
	}
	resolvedBinary, err := resolveOpenCodeBinary(binary)
	if err != nil {
		return prepared, infraf("opencode_provenance", err)
	}
	if resolvedBinary.Digest != manifest.Execution.OpenCodeBinaryDigest {
		return prepared, invalidf("opencode_binary_mismatch", "frozen OpenCode digest does not match --binary")
	}
	workflowPlugin, err := resolveWorkflowCanaryPlugin(workflowPluginPath)
	if err != nil {
		return prepared, invalidf("invalid_workflow_plugin", "%v", err)
	}

	frozen, err := manifest.VerifyBundles(manifestDir, sandbox.DefaultSnapshotLimits())
	if err != nil {
		return prepared, invalidf("invalid_frozen_bundles", "%v", err)
	}
	bundles := make(map[string]experiment.VerifiedBundle, len(frozen.Bundles))
	for _, bundle := range frozen.Bundles {
		bundles[bundle.Name] = bundle
	}
	harness, control, candidate := bundles["harness"], bundles["control"], bundles["candidate"]
	if harness.AbsoluteRoot == "" || control.AbsoluteRoot == "" || candidate.AbsoluteRoot == "" {
		return prepared, invalidf("invalid_frozen_bundles", "harness, control and candidate bundles are required")
	}
	casesDir, err := frozenBundleDirectory(harness.AbsoluteRoot, "cases")
	if err != nil {
		return prepared, invalidf("invalid_harness_layout", "%v", err)
	}
	fixturesDir, err := frozenBundleDirectory(harness.AbsoluteRoot, "fixtures")
	if err != nil {
		return prepared, invalidf("invalid_harness_layout", "%v", err)
	}
	selected, err := loadSelectedCases(casesDir, workflowV2CanarySuite, "")
	if err != nil {
		return prepared, err
	}
	if err := validateWorkflowCanaryCases(selected); err != nil {
		return prepared, invalidf("invalid_canary_cases", "%v", err)
	}
	caseDigest, err := publicCaseSetDigest(selected)
	if err != nil || caseDigest != workflowV2CanaryPublicCasesDigest || manifest.PublicCasesDigest != workflowV2CanaryPublicCasesDigest {
		return prepared, invalidf("experiment_population", "public canary cases differ from the frozen manifest")
	}
	_, observedFixtureSetDigest, err := validateFixtures(fixturesDir, selected)
	if err != nil || observedFixtureSetDigest != workflowV2CanaryFixtureSetDigest {
		return prepared, invalidf("experiment_population", "canary fixture set differs from the evaluator-owned profile")
	}
	if strings.Join(declaredCriticalCaseIDs(selected), "\x00") != strings.Join(manifest.CriticalCaseIDs, "\x00") {
		return prepared, invalidf("experiment_population", "critical case ids differ from the frozen manifest")
	}

	caseIDs := make([]string, len(selected))
	for index := range selected {
		caseIDs[index] = selected[index].ID
	}
	plan, err := buildWorkflowCanaryPlan(*manifest, caseIDs)
	if err != nil {
		return prepared, invalidf("invalid_canary_plan", "%v", err)
	}
	executableClosure, err := runner.ResolveExecutableClosure(selected, "git")
	if err != nil {
		return prepared, invalidf("invalid_toolchain_closure", "%v", err)
	}
	if executableClosure.Digest() != manifest.Execution.ToolchainsDigest {
		return prepared, invalidf("toolchains_mismatch", "effective executable closure differs from the frozen manifest")
	}
	skynexPath, err := executableClosure.PathFor("skynex")
	if err != nil {
		return prepared, invalidf("invalid_toolchain_closure", "Workflow V2 canary requires a frozen skynex executable")
	}
	skynexBinary, err := runner.ResolveExecutableSnapshot(skynexPath)
	if err != nil {
		return prepared, invalidf("invalid_skynex_binary", "%v", err)
	}
	resolvedOAuth, err := resolveOpenAIOAuthFile(oauthPath, nil)
	if err != nil {
		return prepared, err
	}
	oauthSession, err := lifecycle.NewOpenAIOAuthSession(resolvedOAuth)
	if err != nil {
		return prepared, invalidf("invalid_openai_oauth", "%v", err)
	}
	if err := ctx.Err(); err != nil {
		return prepared, err
	}

	outputPath := output
	if strings.TrimSpace(outputPath) == "" {
		outputPath = filepath.Join(manifestDir, "results", "canary-"+manifest.ID+".json")
	}
	outputPath, err = validateCanaryOutputLocation(outputPath, frozen.Bundles)
	if err != nil {
		return prepared, invalidf("invalid_output_location", "%v", err)
	}

	prepared = preparedCanary{
		request: canaryExecutionRequest{
			Profile: workflowV2CanaryProfile, Manifest: *manifest, ManifestDigest: manifestDigest,
			Cases: append([]contracts.Case(nil), selected...), CasesDir: casesDir, FixturesDir: fixturesDir,
			Control: control, Candidate: candidate, Frozen: frozen, Plan: plan, OpenCodeBinary: resolvedBinary,
			SkynexBinary: skynexBinary, WorkflowPlugin: workflowPlugin,
			ExecutableClosure: executableClosure, OpenAIOAuthSession: oauthSession,
			SampleTimeout: workflowV2CanarySampleLimit, SampleBudget: workflowV2CanarySampleBudget,
			CleanupReserve: workflowV2CanaryCleanupReserve, FailFast: true,
			RunsPerArm: workflowV2CanaryRunsPerArm, MaximumSampleCount: workflowV2CanaryMaxSamples,
			RetainTrace: false, AllowAmbientPlugins: false,
		},
		frozen: frozen, outputPath: outputPath,
		evaluatorDigest: observedEvaluatorDigest,
	}
	return prepared, nil
}

func validateWorkflowCanaryManifest(manifest *experiment.Manifest) error {
	if manifest == nil {
		return errors.New("manifest is required")
	}
	if manifest.Suite != workflowV2CanarySuite {
		return fmt.Errorf("suite must equal %q", workflowV2CanarySuite)
	}
	if manifest.Intent != experiment.IntentDevelopment {
		return errors.New("canary intent must be development")
	}
	if manifest.PublicCaseCount != workflowV2CanaryCaseCount {
		return fmt.Errorf("public_case_count must equal %d", workflowV2CanaryCaseCount)
	}
	if manifest.Holdout != nil || manifest.HoldoutCaseCount != 0 {
		return errors.New("canary must not contain or consume a holdout")
	}
	if len(manifest.CriticalCaseIDs) != workflowV2CanaryCaseCount {
		return fmt.Errorf("all %d canary cases must be critical", workflowV2CanaryCaseCount)
	}
	if manifest.ModelAssignment != nil {
		return errors.New("canary arms must use the same case-pinned model")
	}
	if manifest.Execution.Concurrency != 1 {
		return errors.New("canary execution.concurrency must equal 1")
	}
	if manifest.Execution.ProviderAuth != experiment.ProviderAuthOpenAIOAuthCleanProfileV1 {
		return fmt.Errorf("canary requires execution.provider_auth=%q", experiment.ProviderAuthOpenAIOAuthCleanProfileV1)
	}
	if manifest.Execution.Mode != string(contracts.ExecutionTrustedLocal) || manifest.Execution.Network != string(contracts.NetworkHostUnisolated) {
		return errors.New("this canary backend requires trusted-local/host-unisolated execution")
	}
	return requireLocalABCredentialBoundary(manifest.Execution)
}

func resolveWorkflowCanaryPlugin(path string) (*toolpolicy.ControlledPluginIdentity, error) {
	if path != workflowV2CanaryDefaultPlugin {
		return nil, fmt.Errorf("--workflow-plugin must equal the evaluator installation path %q", workflowV2CanaryDefaultPlugin)
	}
	if err := verifyRootOwnedReadOnlyPluginPath(path); err != nil {
		return nil, err
	}
	return resolveWorkflowCanaryPluginContent(path)
}

func verifyRootOwnedReadOnlyPluginPath(path string) error {
	if os.Geteuid() == 0 {
		return errors.New("Workflow V2 canary must run as an unprivileged user so its root-owned plugin is outside evaluator write authority")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect installed Workflow V2 plugin authority: %w", err)
		}
		rootOwned, err := fileInfoOwnedByRoot(info)
		if err != nil || !rootOwned {
			return errors.New("installed Workflow V2 plugin and all its ancestors must be owned by root")
		}
		if info.Mode().Perm()&0o022 != 0 {
			return errors.New("installed Workflow V2 plugin and all its ancestors must not be group- or world-writable")
		}
		if current == path {
			if !info.Mode().IsRegular() {
				return errors.New("installed Workflow V2 plugin must be a regular file")
			}
		} else if !info.IsDir() {
			return errors.New("installed Workflow V2 plugin ancestors must be directories")
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}

// resolveWorkflowCanaryPluginContent performs the bounded content identity
// check after production installation authority has been established. It is
// split out so tests can exercise the byte contract without writing /usr/local.
func resolveWorkflowCanaryPluginContent(path string) (*toolpolicy.ControlledPluginIdentity, error) {
	const maximumPluginBytes = 4 << 20
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("--workflow-plugin must be an exact clean absolute path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Clean(resolved) != path {
		return nil, errors.New("--workflow-plugin must not contain symlink components")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect installed Workflow V2 plugin: %w", err)
	}
	if !before.Mode().IsRegular() || before.Size() > maximumPluginBytes {
		return nil, fmt.Errorf("installed Workflow V2 plugin must be a regular file no larger than %d bytes", maximumPluginBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open installed Workflow V2 plugin: %w", err)
	}
	opened, statErr := file.Stat()
	content, readErr := io.ReadAll(io.LimitReader(file, maximumPluginBytes+1))
	closeErr := file.Close()
	after, err := os.Lstat(path)
	if statErr != nil || readErr != nil || closeErr != nil {
		return nil, fmt.Errorf("read installed Workflow V2 plugin: %w", errors.Join(statErr, readErr, closeErr))
	}
	if len(content) > maximumPluginBytes {
		return nil, fmt.Errorf("installed Workflow V2 plugin must be no larger than %d bytes", maximumPluginBytes)
	}
	if err != nil || !opened.Mode().IsRegular() || !after.Mode().IsRegular() ||
		!os.SameFile(before, opened) || !os.SameFile(before, after) ||
		before.Size() != opened.Size() || before.Size() != after.Size() || before.Size() != int64(len(content)) {
		return nil, errors.New("installed Workflow V2 plugin changed while it was read")
	}
	embeddedRoot, err := assets.OpencodeFS()
	if err != nil {
		return nil, fmt.Errorf("open embedded OpenCode assets: %w", err)
	}
	embedded, err := fs.ReadFile(embeddedRoot, "plugins/skynex-workflow.ts")
	if err != nil {
		return nil, fmt.Errorf("read embedded Workflow V2 plugin: %w", err)
	}
	if !bytes.Equal(content, embedded) {
		return nil, errors.New("installed Workflow V2 plugin differs from the evaluator-embedded plugin")
	}
	sum := sha256.Sum256(content)
	identity := &toolpolicy.ControlledPluginIdentity{
		Path: path, ContentDigest: "sha256:" + hex.EncodeToString(sum[:]),
	}
	if err := toolpolicy.VerifyControlledPluginIdentity(identity); err != nil {
		return nil, err
	}
	return identity, nil
}

func validateWorkflowCanaryCases(testCases []contracts.Case) error {
	if len(testCases) != workflowV2CanaryCaseCount {
		return fmt.Errorf("profile requires exactly %d cases", workflowV2CanaryCaseCount)
	}
	seenWorkflowIDs := make(map[string]struct{}, len(testCases))
	for _, testCase := range testCases {
		pin, pinned := workflowV2CanaryCasePinFor(testCase.ID)
		if !pinned {
			return fmt.Errorf("case %q is not one of the evaluator-owned Workflow V2 canary cases", testCase.ID)
		}
		if testCase.Suite != workflowV2CanarySuite {
			return fmt.Errorf("case %q has a different suite", testCase.ID)
		}
		if !testCase.Critical {
			return fmt.Errorf("case %q must be critical", testCase.ID)
		}
		if testCase.Agent.Name != "workflow-orchestrator" {
			return fmt.Errorf("case %q must use workflow-orchestrator", testCase.ID)
		}
		provider, _, err := contracts.ParseModelSelection(testCase.Agent.Model)
		if err != nil || provider != "openai" {
			return fmt.Errorf("case %q must use an exact OpenAI provider/model", testCase.ID)
		}
		timeout, err := time.ParseDuration(testCase.Completion.Timeout)
		if err != nil || timeout <= 0 || timeout > workflowV2CanarySampleLimit {
			return fmt.Errorf("case %q completion timeout must be positive and at most %s", testCase.ID, workflowV2CanarySampleLimit)
		}
		if testCase.Runs.Count != workflowV2CanaryRunsPerArm {
			return fmt.Errorf("case %q runs.count must equal %d", testCase.ID, workflowV2CanaryRunsPerArm)
		}
		if value, ok := testCase.Extensions["x-canary-profile"].(string); !ok || value != workflowV2CanaryProfile {
			return fmt.Errorf("case %q must declare x-canary-profile=%q", testCase.ID, workflowV2CanaryProfile)
		}
		if value, ok := testCase.Extensions["x-visibility"].(string); !ok || value != "public" {
			return fmt.Errorf("case %q must be public", testCase.ID)
		}
		driver, err := decodeWorkflowCanaryDriver(testCase.Extensions["x-workflow-driver-v1"])
		if err != nil {
			return fmt.Errorf("case %q workflow driver: %w", testCase.ID, err)
		}
		if driver.WorkflowID != testCase.ID {
			return fmt.Errorf("case %q workflow_id must equal the stable case id", testCase.ID)
		}
		if _, duplicate := seenWorkflowIDs[driver.WorkflowID]; duplicate {
			return fmt.Errorf("duplicate workflow_id %q", driver.WorkflowID)
		}
		seenWorkflowIDs[driver.WorkflowID] = struct{}{}
		if !containsString(testCase.Security.AllowedExecutables, "skynex") {
			return fmt.Errorf("case %q must allow the frozen skynex executable", testCase.ID)
		}
		if len(testCase.ToolPolicy.FakeMCPs) != 0 {
			return fmt.Errorf("case %q must not enable fake or ambient MCPs", testCase.ID)
		}
		for _, tool := range testCase.ToolPolicy.AllowedTools {
			if strings.Contains(strings.ToLower(tool), "neurox") {
				return fmt.Errorf("case %q must not enable Neurox", testCase.ID)
			}
		}
		if testCase.Fixture.ExpectedDigest != pin.FixtureDigest {
			return fmt.Errorf("case %q fixture digest differs from the evaluator-owned profile", testCase.ID)
		}
		if err := validateWorkflowCanaryDeclaredChecks(testCase); err != nil {
			return err
		}
		caseDigest, err := testCase.Digest()
		if err != nil || caseDigest != pin.CaseDigest {
			return fmt.Errorf("case %q digest differs from the evaluator-owned profile", testCase.ID)
		}
	}
	return nil
}

func validateWorkflowCanaryDeclaredChecks(testCase contracts.Case) error {
	seen := make(map[string]struct{}, len(testCase.BehaviorChecks))
	falseSuccesses, exactScopes := 0, 0
	for _, check := range testCase.BehaviorChecks {
		if _, duplicate := seen[check.ID]; duplicate {
			return fmt.Errorf("case %q has duplicate behavior check %q", testCase.ID, check.ID)
		}
		seen[check.ID] = struct{}{}
		if check.Hard == nil || !*check.Hard {
			return fmt.Errorf("case %q behavior check %q must be explicitly hard", testCase.ID, check.ID)
		}
		switch check.Type {
		case "no_false_success":
			falseSuccesses++
		case "expected_diff":
			exactScopes++
		}
	}
	if falseSuccesses != 1 || exactScopes != 1 {
		return fmt.Errorf("case %q must declare exactly one hard no_false_success and expected_diff check", testCase.ID)
	}
	return nil
}

func decodeWorkflowCanaryDriver(raw any) (workflowCanaryDriver, error) {
	var driver workflowCanaryDriver
	encoded, err := json.Marshal(raw)
	if err != nil {
		return driver, errors.New("x-workflow-driver-v1 must be an object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil || fields == nil {
		return driver, errors.New("x-workflow-driver-v1 must be an object")
	}
	required := []string{"mode", "workflow_id", "terminal_state", "autonomous_turns"}
	if len(fields) != len(required) {
		return driver, errors.New("driver must contain exactly mode, workflow_id, terminal_state and autonomous_turns")
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return driver, fmt.Errorf("driver field %q is required", name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&driver); err != nil {
		return driver, errors.New("driver fields have invalid types")
	}
	if driver.Mode != "foreground" && driver.Mode != "managed-detach" {
		return driver, errors.New("mode must be foreground or managed-detach")
	}
	if strings.TrimSpace(driver.WorkflowID) == "" || strings.TrimSpace(driver.TerminalState) == "" {
		return driver, errors.New("workflow_id and terminal_state are required")
	}
	if driver.AutonomousTurns < 0 || driver.AutonomousTurns > 2 {
		return driver, errors.New("autonomous_turns must be between 0 and 2")
	}
	return driver, nil
}

func buildWorkflowCanaryPlan(manifest experiment.Manifest, caseIDs []string) (stats.ExperimentPlan, error) {
	full, err := manifest.Plan(caseIDs)
	if err != nil {
		return stats.ExperimentPlan{}, err
	}
	plan := stats.ExperimentPlan{
		Method: workflowV2CanaryPlanMethod, Seed: full.Seed,
		RunsPerCase: workflowV2CanaryRunsPerArm, SerializeWithinBlock: true,
	}
	for _, block := range full.Blocks {
		if block.Repetition != 1 {
			continue
		}
		block.Order = append([]stats.Variant(nil), block.Order...)
		plan.Blocks = append(plan.Blocks, block)
	}
	if len(plan.Blocks) != workflowV2CanaryCaseCount {
		return stats.ExperimentPlan{}, fmt.Errorf("expected %d one-pair blocks, got %d", workflowV2CanaryCaseCount, len(plan.Blocks))
	}
	return plan, nil
}

func validateWorkflowCanaryExecution(result canaryExecutionResult, request canaryExecutionRequest) error {
	expected := flattenCanaryPlan(request.Plan)
	if len(expected) != workflowV2CanaryMaxSamples || len(result.Samples) > len(expected) {
		return errors.New("executor returned an invalid sample count")
	}
	seenRunIDs := make(map[string]struct{}, len(result.Samples))
	caseByID := make(map[string]contracts.Case, len(request.Cases))
	for _, testCase := range request.Cases {
		caseByID[testCase.ID] = testCase
	}
	for index, sample := range result.Samples {
		if err := sample.Validate(); err != nil {
			return fmt.Errorf("sample %d violates the result contract: %w", index+1, err)
		}
		coordinate := expected[index]
		if sample.CaseID != coordinate.CaseID || sample.Variant != string(coordinate.Variant) || sample.Repetition != coordinate.Repetition {
			return fmt.Errorf("sample %d is outside the committed canary plan", index+1)
		}
		if _, duplicate := seenRunIDs[sample.RunID]; duplicate {
			return errors.New("executor returned a duplicate run_id")
		}
		seenRunIDs[sample.RunID] = struct{}{}
		if index+1 < len(result.Samples) && sample.Status != contracts.RunStatusPass {
			return errors.New("executor continued after fail-fast became terminal")
		}
		expectedBundle := request.Control.Snapshot.Digest
		if coordinate.Variant == stats.VariantCandidate {
			expectedBundle = request.Candidate.Snapshot.Digest
		}
		extensions := sample.Provenance.Extensions
		if extensions["x-agent-bundle-digest"] != expectedBundle ||
			extensions["x-harness-bundle-digest"] != request.Manifest.Harness.Digest ||
			extensions["x-experiment-manifest-digest"] != request.ManifestDigest {
			return fmt.Errorf("sample %d provenance is not bound to the frozen canary", index+1)
		}
		testCase, ok := caseByID[sample.CaseID]
		if !ok {
			return fmt.Errorf("sample %d refers to an unknown case", index+1)
		}
		caseDigest, err := testCase.Digest()
		if err != nil || sample.Provenance.CaseDigest != caseDigest {
			return fmt.Errorf("sample %d case digest differs from the frozen case", index+1)
		}
		if sample.Status == contracts.RunStatusPass {
			if !workflowCanarySampleCleanupAttested(sample) {
				return fmt.Errorf("sample %d passed without runtime cleanup attestation", index+1)
			}
			if err := validateWorkflowCanarySampleChecks(sample, testCase); err != nil {
				return fmt.Errorf("sample %d does not prove the evaluator-owned canary checks: %w", index+1, err)
			}
		}
	}
	if result.CleanupComplete && !workflowCanarySamplesCleanupAttested(result.Samples) {
		return errors.New("executor cleanup aggregate contradicts sample attestations")
	}
	if !result.StartedAt.IsZero() && !result.EndedAt.IsZero() && result.EndedAt.Before(result.StartedAt) {
		return errors.New("executor timestamps are reversed")
	}
	return nil
}

func workflowCanarySampleCleanupAttested(sample contracts.RunResult) bool {
	return sample.Provenance.Extensions[runner.ProvenanceExtensionRuntimeCleanupAttested] == "true"
}

func workflowCanarySamplesCleanupAttested(samples []contracts.RunResult) bool {
	for _, sample := range samples {
		if !workflowCanarySampleCleanupAttested(sample) {
			return false
		}
	}
	return true
}

func validateWorkflowCanarySampleChecks(sample contracts.RunResult, testCase contracts.Case) error {
	declared := make(map[string]string, len(testCase.BehaviorChecks))
	for _, check := range testCase.BehaviorChecks {
		declared[check.ID] = check.Type
	}
	observed := make(map[string]contracts.CheckResult, len(sample.Checks))
	categoryPass := map[string]bool{
		"infrastructure":    false,
		"filesystem":        false,
		"acceptance":        false,
		"behavior":          false,
		"claim-consistency": false,
		"security":          false,
	}
	for _, check := range sample.Checks {
		if check.Hard && check.Status == contracts.CheckStatusPass {
			if _, wanted := categoryPass[check.Type]; wanted {
				categoryPass[check.Type] = true
			}
		}
		if _, required := declared[check.ID]; required {
			observed[check.ID] = check
		}
	}
	for id, checkType := range declared {
		check, exists := observed[id]
		if !exists || !check.Hard || check.Status != contracts.CheckStatusPass || check.Type != checkType {
			return fmt.Errorf("required hard check %q did not pass", id)
		}
	}
	for category, passed := range categoryPass {
		if !passed {
			return fmt.Errorf("required evaluator category %q has no passing hard check", category)
		}
	}
	return nil
}

type canaryCoordinate struct {
	CaseID     string
	Variant    stats.Variant
	Repetition int
}

func flattenCanaryPlan(plan stats.ExperimentPlan) []canaryCoordinate {
	coordinates := make([]canaryCoordinate, 0, len(plan.Blocks)*2)
	for _, block := range plan.Blocks {
		for _, variant := range block.Order {
			coordinates = append(coordinates, canaryCoordinate{CaseID: block.CaseID, Variant: variant, Repetition: block.Repetition})
		}
	}
	return coordinates
}

func evaluateWorkflowCanary(result canaryExecutionResult, plan stats.ExperimentPlan, executionErr error) canaryGateEvaluation {
	evaluation := canaryGateEvaluation{
		Decision: canaryDecisionInconclusive, Reasons: []string{"incomplete_population"},
		Summary: canaryGateSummary{TelemetryComplete: true}, ExitCode: contracts.ExitInconclusive,
	}
	statusByCase := make(map[string]map[string]contracts.RunStatus)
	for _, sample := range result.Samples {
		switch sample.Status {
		case contracts.RunStatusPass:
			evaluation.Summary.PassedSamples++
		case contracts.RunStatusFail:
			evaluation.Summary.FailedSamples++
		case contracts.RunStatusBudgetExhausted:
			evaluation.Summary.Timeouts++
		default:
			evaluation.Summary.FailedSamples++
		}
		if !sample.TelemetryComplete {
			evaluation.Summary.TelemetryComplete = false
		}
		if sample.Usage.Tree.SumInputTokens <= 0 || sample.Usage.Tree.Sessions <= 0 {
			evaluation.Summary.TelemetryComplete = false
		}
		if sample.Variant == string(stats.VariantControl) {
			evaluation.Summary.ControlTreeInputTokens += sample.Usage.Tree.SumInputTokens
		} else if sample.Variant == string(stats.VariantCandidate) {
			evaluation.Summary.CandidateTreeInputTokens += sample.Usage.Tree.SumInputTokens
		}
		if statusByCase[sample.CaseID] == nil {
			statusByCase[sample.CaseID] = make(map[string]contracts.RunStatus)
		}
		statusByCase[sample.CaseID][sample.Variant] = sample.Status
		for _, check := range sample.Checks {
			if check.Hard && check.Status != contracts.CheckStatusPass {
				evaluation.Summary.FailedHardChecks++
				if check.Type == "no_false_success" {
					evaluation.Summary.FalseSuccesses++
				}
			}
		}
	}
	for _, variants := range statusByCase {
		if variants[string(stats.VariantControl)] == contracts.RunStatusPass && variants[string(stats.VariantCandidate)] != "" && variants[string(stats.VariantCandidate)] != contracts.RunStatusPass {
			evaluation.Summary.PassToFailRegressions++
		}
	}
	if errors.Is(executionErr, context.DeadlineExceeded) || evaluation.Summary.Timeouts > 0 {
		evaluation.Decision, evaluation.Reasons, evaluation.ExitCode = canaryDecisionReject, []string{"timeout_is_failure"}, contracts.ExitFailed
		return evaluation
	}
	for _, sample := range result.Samples {
		if sample.Status == contracts.RunStatusFail {
			reasons := []string{"sample_failed"}
			if evaluation.Summary.PassToFailRegressions > 0 {
				reasons = append(reasons, "pass_to_fail_regression")
			}
			evaluation.Decision, evaluation.Reasons, evaluation.ExitCode = canaryDecisionReject, reasons, contracts.ExitFailed
			return evaluation
		}
	}
	compatible, completePairs := workflowCanaryRuntimeCompatible(result.Samples)
	evaluation.Summary.RuntimeCompatible = compatible && completePairs == workflowV2CanaryCaseCount
	if !compatible {
		evaluation.Decision = canaryDecisionInconclusive
		evaluation.Reasons = []string{"runtime_compatibility_mismatch"}
		evaluation.ExitCode = contracts.ExitInconclusive
		return evaluation
	}
	if executionErr != nil {
		switch {
		case errors.Is(executionErr, context.Canceled):
			evaluation.Reasons, evaluation.ExitCode = []string{"execution_aborted"}, contracts.ExitAborted
		default:
			evaluation.Reasons, evaluation.ExitCode = []string{"executor_infrastructure"}, contracts.ExitInfrastructure
		}
		return evaluation
	}
	for _, sample := range result.Samples {
		switch sample.Status {
		case contracts.RunStatusInvalid:
			evaluation.Reasons, evaluation.ExitCode = []string{"invalid_sample"}, contracts.ExitInvalid
			return evaluation
		case contracts.RunStatusInfraError:
			evaluation.Reasons, evaluation.ExitCode = []string{"sample_infrastructure"}, contracts.ExitInfrastructure
			return evaluation
		case contracts.RunStatusAborted:
			evaluation.Reasons, evaluation.ExitCode = []string{"execution_aborted"}, contracts.ExitAborted
			return evaluation
		case contracts.RunStatusInconclusive:
			evaluation.Reasons, evaluation.ExitCode = []string{"sample_inconclusive"}, contracts.ExitInconclusive
			return evaluation
		}
	}
	if !result.CleanupComplete || !workflowCanarySamplesCleanupAttested(result.Samples) {
		evaluation.Reasons, evaluation.ExitCode = []string{"cleanup_incomplete"}, contracts.ExitInfrastructure
		return evaluation
	}
	if len(result.Samples) != len(flattenCanaryPlan(plan)) || len(result.Samples) != workflowV2CanaryMaxSamples {
		return evaluation
	}
	if !evaluation.Summary.TelemetryComplete || evaluation.Summary.ControlTreeInputTokens <= 0 || evaluation.Summary.CandidateTreeInputTokens <= 0 {
		evaluation.Reasons = []string{"telemetry_incomplete"}
		return evaluation
	}
	ratio := float64(evaluation.Summary.CandidateTreeInputTokens) / float64(evaluation.Summary.ControlTreeInputTokens)
	evaluation.Summary.TreeInputRatio = &ratio
	if ratio > workflowV2CanaryMaxInputRatio {
		evaluation.Decision, evaluation.Reasons, evaluation.ExitCode = canaryDecisionReject, []string{"tree_input_ratio_exceeded"}, contracts.ExitFailed
		return evaluation
	}
	evaluation.Decision, evaluation.Reasons, evaluation.ExitCode = canaryDecisionPromote, []string{"all_gates_passed"}, contracts.ExitSuccess
	return evaluation
}

func workflowCanaryRuntimeCompatible(samples []contracts.RunResult) (bool, int) {
	type pair struct {
		control   *contracts.RunResult
		candidate *contracts.RunResult
	}
	pairs := make(map[string]*pair)
	for index := range samples {
		sample := &samples[index]
		current := pairs[sample.CaseID]
		if current == nil {
			current = &pair{}
			pairs[sample.CaseID] = current
		}
		switch sample.Variant {
		case string(stats.VariantControl):
			current.control = sample
		case string(stats.VariantCandidate):
			current.candidate = sample
		}
	}
	complete := 0
	for _, current := range pairs {
		if current.control == nil || current.candidate == nil {
			continue
		}
		complete++
		control, candidate := current.control.Provenance, current.candidate.Provenance
		controlCatalog := control.Extensions[workflowV2CanaryToolCatalogExtension]
		candidateCatalog := candidate.Extensions[workflowV2CanaryToolCatalogExtension]
		if !contracts.IsDigest(control.ToolsetDigest) || control.ToolsetDigest != candidate.ToolsetDigest ||
			control.Provider == "" || control.Provider != candidate.Provider ||
			control.Model == "" || control.Model != candidate.Model ||
			control.ExecutionMode != candidate.ExecutionMode || control.Network != candidate.Network ||
			!contracts.IsDigest(controlCatalog) || controlCatalog != candidateCatalog {
			return false, complete
		}
		controlAuthorization, controlHasAuthorization := control.Extensions[workflowV2CanaryAuthorizationExtension]
		candidateAuthorization, candidateHasAuthorization := candidate.Extensions[workflowV2CanaryAuthorizationExtension]
		if controlHasAuthorization || candidateHasAuthorization {
			if !controlHasAuthorization || !candidateHasAuthorization || !contracts.IsDigest(controlAuthorization) || controlAuthorization != candidateAuthorization {
				return false, complete
			}
		}
	}
	return true, complete
}

func verifyPreparedCanaryUnchanged(prepared preparedCanary) error {
	var result error
	result = errors.Join(result, validateCanaryRuntimeAuthority(prepared.request))
	if prepared.frozen != nil {
		result = errors.Join(result, prepared.frozen.VerifyUnchanged())
	}
	if prepared.request.ExecutableClosure != nil {
		result = errors.Join(result, prepared.request.ExecutableClosure.Revalidate())
	}
	if prepared.request.Profile == workflowV2CanaryProfile {
		if prepared.request.SkynexBinary != nil {
			result = errors.Join(result, prepared.request.SkynexBinary.Revalidate())
		}
		result = errors.Join(result, toolpolicy.VerifyControlledPluginIdentity(prepared.request.WorkflowPlugin))
	}
	result = errors.Join(result, prepared.request.OpenCodeBinary.Revalidate())
	if digest, err := executableDigest(); err != nil || digest != prepared.evaluatorDigest {
		if err == nil {
			err = errors.New("evaluator binary drifted")
		}
		result = errors.Join(result, err)
	}
	return result
}

func workflowCanaryLimits() canaryLimits {
	return canaryLimits{
		GlobalWallClock: workflowV2CanaryGlobalLimit.String(), Preflight: workflowV2CanaryPreflightLimit.String(),
		PerSample: workflowV2CanarySampleLimit.String(), SampleBudget: workflowV2CanarySampleBudget.String(),
		CleanupReserve: workflowV2CanaryCleanupReserve.String(), SealReserve: workflowV2CanarySealReserve.String(),
		RunsPerArm: workflowV2CanaryRunsPerArm, MaximumSamples: workflowV2CanaryMaxSamples,
		MaximumInputRatio: workflowV2CanaryMaxInputRatio, FailFast: true,
	}
}

func validateCanaryOutputLocation(path string, bundles []experiment.VerifiedBundle) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("canary output path must not be empty")
	}
	resolved, err := resolveFuturePath(path)
	if err != nil {
		return "", err
	}
	for _, bundle := range bundles {
		root, err := filepath.EvalSymlinks(bundle.AbsoluteRoot)
		if err != nil {
			return "", err
		}
		inside, err := pathWithinOrEqual(root, resolved)
		if err != nil {
			return "", err
		}
		if inside {
			return "", fmt.Errorf("output must be outside frozen %s bundle", bundle.Name)
		}
	}
	return filepath.Clean(resolved), nil
}

func sealCanaryArtifact(artifact *canaryArtifact) error {
	if artifact == nil {
		return errors.New("canary artifact is required")
	}
	copy := *artifact
	copy.IntegrityDigest = ""
	digest, err := contracts.CanonicalDigest(copy)
	if err != nil {
		return err
	}
	artifact.IntegrityDigest = digest
	return nil
}

func verifyCanaryArtifactIntegrity(artifact canaryArtifact) error {
	if !contracts.IsDigest(artifact.IntegrityDigest) {
		return errors.New("integrity_digest is not canonical")
	}
	want := artifact.IntegrityDigest
	if err := sealCanaryArtifact(&artifact); err != nil {
		return err
	}
	if artifact.IntegrityDigest != want {
		return errors.New("canary artifact integrity mismatch")
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
