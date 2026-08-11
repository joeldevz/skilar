package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/experiment"
	"github.com/joeldevz/skynex/internal/eval/lifecycle"
	"github.com/joeldevz/skynex/internal/eval/reporter"
	"github.com/joeldevz/skynex/internal/eval/runner"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/stats"
)

type abCommandResult struct {
	ExperimentID  string                   `json:"experiment_id"`
	Intent        string                   `json:"intent"`
	Authority     string                   `json:"authority"`
	Plan          stats.ExperimentPlan     `json:"plan"`
	ControlPath   string                   `json:"control_artifact,omitempty"`
	CandidatePath string                   `json:"candidate_artifact,omitempty"`
	PartialPath   string                   `json:"partial_artifact,omitempty"`
	Comparison    *comparisonCommandResult `json:"comparison,omitempty"`
	ControlRuns   int                      `json:"control_runs"`
	CandidateRuns int                      `json:"candidate_runs"`
	ObservedCost  *float64                 `json:"observed_cost_usd,omitempty"`
	CostComplete  bool                     `json:"cost_evidence_complete"`
	HoldoutDigest string                   `json:"holdout_bundle_digest,omitempty"`
	HoldoutCases  int                      `json:"holdout_cases"`
	ExitCode      int                      `json:"exit_code"`
}

func (r abCommandResult) CLIExitCode() int { return r.ExitCode }

type partialABArtifact struct {
	SchemaVersion   int                   `json:"schema_version"`
	Kind            string                `json:"kind"`
	IntegrityDigest string                `json:"integrity_digest"`
	ExperimentID    string                `json:"experiment_id"`
	Intent          string                `json:"intent"`
	Authority       string                `json:"authority"`
	ManifestDigest  string                `json:"manifest_digest"`
	Plan            stats.ExperimentPlan  `json:"plan"`
	Control         runner.ContractResult `json:"control"`
	Candidate       runner.ContractResult `json:"candidate"`
	HoldoutDigest   string                `json:"holdout_bundle_digest,omitempty"`
	HoldoutCases    int                   `json:"holdout_cases"`
	ExitCode        int                   `json:"exit_code"`
}

type holdoutArtifactMetadata struct {
	BundleDigest string                `json:"bundle_digest"`
	CaseCount    int                   `json:"case_count"`
	Samples      []contracts.RunResult `json:"sanitized_samples"`
}

const (
	holdoutAggregateKey             = "holdout"
	evaluationAuthorityAggregateKey = "evaluation_authority"
	evaluationAuthorityExploratory  = "exploratory"
	evaluationAuthorityDevelopment  = "development-non-release"
	evaluationAuthorityRelease      = "release"
)

type evaluationAuthorityMetadata struct {
	Mode           string `json:"mode"`
	Intent         string `json:"intent,omitempty"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

func commandAB(ctx context.Context, args []string, deps dependencies) (abCommandResult, error) {
	set := newFlagSet("ab")
	allow := set.Bool("allow-model-calls", false, "authorize model calls that may consume quota or incur provider charges")
	manifestPath := set.String("manifest", "", "frozen experiment manifest")
	casesDir := set.String("cases-dir", "eval/cases", "case catalog owned by the harness")
	fixturesDir := set.String("fixtures-dir", "eval/fixtures", "fixtures owned by the harness")
	binary := set.String("binary", "opencode", "OpenCode binary")
	providerEnv := set.String("provider-env", "", "comma-separated provider environment names")
	openAIOAuth := set.String("openai-oauth", "", "OpenCode auth.json containing an OpenAI OAuth login")
	traceDir := set.String("trace-dir", "eval/results/traces", "sanitized trace directory")
	retainTrace := set.Bool("retain-trace", false, "persist sanitized traces")
	allowImpure := set.Bool("allow-impure", false, "explicitly disable OpenCode --pure")
	requireHoldout := set.Bool("require-holdout", false, "fail unless the frozen manifest includes an external holdout")
	costCap := set.Float64(
		"cost-cap", 0,
		"maximum observed USD before stopping; unsupported for this OAuth-only A/B because ChatGPT subscription billing has no authoritative per-request USD",
	)
	legacyModel := set.String("legacy-model-label", "openai/gpt-5.6-terra", "exact provider/model for migrated legacy cases")
	outputPrefix := set.String("output-prefix", "", "artifact path prefix")
	resumePartial := set.String("resume-partial", "", "explicit partial A/B artifact to validate and resume without repeating completed samples")
	if err := parseFlagSet(set, args); err != nil {
		return abCommandResult{}, err
	}
	if err := requireModelOptIn(*allow); err != nil {
		return abCommandResult{}, err
	}
	if *manifestPath == "" {
		return abCommandResult{}, invalidf("invalid_arguments", "--manifest is required")
	}
	if *costCap < 0 {
		return abCommandResult{}, invalidf("invalid_arguments", "--cost-cap must not be negative")
	}
	if *costCap > 0 {
		return abCommandResult{}, invalidf(
			"subscription_cost_cap_unsupported",
			"--cost-cap cannot enforce ChatGPT subscription quota in OAuth A/B; frozen counts/timeouts bound scheduled samples only, not provider calls, tokens, or quota",
		)
	}
	if *allowImpure {
		return abCommandResult{}, invalidf("impure_ab_forbidden", "frozen A/B requires OpenCode --pure; ambient configuration would invalidate bundle provenance")
	}
	envNames, err := parseEnvNames(*providerEnv)
	if err != nil {
		return abCommandResult{}, invalidf("invalid_arguments", "%v", err)
	}
	openAIOAuthFile, err := resolveOpenAIOAuthFile(*openAIOAuth, envNames)
	if err != nil {
		return abCommandResult{}, err
	}
	if openAIOAuthFile == "" {
		return abCommandResult{}, invalidf("openai_oauth_required", "frozen A/B requires --openai-oauth PATH for its clean OpenCode profile")
	}
	if *retainTrace {
		return abCommandResult{}, invalidf("oauth_trace_retention_forbidden", "runtime-readable OAuth forbids persisted traces")
	}
	oauthSession, err := lifecycle.NewOpenAIOAuthSession(openAIOAuthFile)
	if err != nil {
		return abCommandResult{}, invalidf("invalid_openai_oauth", "%v", err)
	}
	manifest, err := experiment.Load(*manifestPath)
	if err != nil {
		return abCommandResult{}, invalidf("invalid_manifest", "%v", err)
	}
	if manifest.Runs > contracts.MaxRuns {
		return abCommandResult{}, invalidf("invalid_manifest", "manifest runs %d exceeds %d", manifest.Runs, contracts.MaxRuns)
	}
	if manifest.Execution.ProviderAuth != experiment.ProviderAuthOpenAIOAuthCleanProfileV1 {
		return abCommandResult{}, invalidf("unsupported_provider_auth", "A/B requires execution.provider_auth=%q", experiment.ProviderAuthOpenAIOAuthCleanProfileV1)
	}
	if *requireHoldout && manifest.Holdout == nil {
		return abCommandResult{}, invalidf("holdout_required", "--require-holdout was set but the frozen manifest has no external holdout bundle")
	}
	if manifest.Execution.Mode != string(contracts.ExecutionTrustedLocal) || manifest.Execution.Network != string(contracts.NetworkHostUnisolated) {
		return abCommandResult{}, invalidf("unsupported_execution", "this CLI build only wires trusted-local/host-unisolated execution")
	}
	if err := requireLocalABCredentialBoundary(manifest.Execution); err != nil {
		return abCommandResult{}, err
	}
	if manifest.Execution.Concurrency != 1 {
		return abCommandResult{}, invalidf("unsupported_execution", "this CLI build requires execution.concurrency=1 while preserving serialized A/B blocks")
	}
	observedEvaluatorDigest, err := executableDigest()
	if err != nil {
		return abCommandResult{}, infraf("evaluator_provenance", err)
	}
	if observedEvaluatorDigest != manifest.Execution.EvaluatorBinaryDigest {
		return abCommandResult{}, invalidf("evaluator_binary_mismatch", "got %s, expected %s", observedEvaluatorDigest, manifest.Execution.EvaluatorBinaryDigest)
	}
	resolvedBinary, err := resolveOpenCodeBinary(*binary)
	if err != nil {
		return abCommandResult{}, infraf("opencode_provenance", err)
	}
	observedBinaryDigest := resolvedBinary.Digest
	if observedBinaryDigest != manifest.Execution.OpenCodeBinaryDigest {
		return abCommandResult{}, invalidf("opencode_binary_mismatch", "got %s, expected %s", observedBinaryDigest, manifest.Execution.OpenCodeBinaryDigest)
	}
	manifestDirectory, err := filepath.Abs(filepath.Dir(*manifestPath))
	if err != nil {
		return abCommandResult{}, invalidf("invalid_manifest", "%v", err)
	}
	frozen, err := manifest.VerifyBundles(manifestDirectory, sandbox.DefaultSnapshotLimits())
	if err != nil {
		if manifest.Holdout != nil {
			return abCommandResult{}, invalidf("invalid_frozen_bundles", "%v", privateHoldoutError())
		}
		return abCommandResult{}, invalidf("invalid_frozen_bundles", "%v", err)
	}
	bundles := make(map[string]experiment.VerifiedBundle, len(frozen.Bundles))
	for _, bundle := range frozen.Bundles {
		bundles[bundle.Name] = bundle
	}
	harness := bundles["harness"]
	control := bundles["control"]
	candidate := bundles["candidate"]
	if harness.AbsoluteRoot == "" || control.AbsoluteRoot == "" || candidate.AbsoluteRoot == "" {
		return abCommandResult{}, invalidf("invalid_frozen_bundles", "harness, control and candidate bundles are required")
	}
	prefix := *outputPrefix
	if *resumePartial != "" && prefix == "" {
		if !strings.HasSuffix(*resumePartial, ".partial.json") {
			return abCommandResult{}, invalidf("invalid_arguments", "--resume-partial must name a .partial.json artifact")
		}
		prefix = strings.TrimSuffix(*resumePartial, ".partial.json")
	}
	if prefix == "" {
		prefix = filepath.Join(manifestDirectory, "results", "ab-"+manifest.ID)
	}
	outputPaths, err := validateABOutputLocations(prefix, frozen.Bundles)
	if err != nil {
		return abCommandResult{}, invalidf("invalid_output_location", "%v", err)
	}
	if *resumePartial != "" && !sameABOutputPath(*resumePartial, outputPaths.Partial) {
		return abCommandResult{}, invalidf("invalid_arguments", "--resume-partial must match the partial path selected by --output-prefix")
	}
	expectedCasesDir, err := frozenBundleDirectory(harness.AbsoluteRoot, "cases")
	if err != nil {
		return abCommandResult{}, invalidf("invalid_harness_layout", "%v", err)
	}
	expectedFixturesDir, err := frozenBundleDirectory(harness.AbsoluteRoot, "fixtures")
	if err != nil {
		return abCommandResult{}, invalidf("invalid_harness_layout", "%v", err)
	}
	for name, observed := range map[string]string{"cases-dir": *casesDir, "fixtures-dir": *fixturesDir} {
		expected := expectedCasesDir
		if name == "fixtures-dir" {
			expected = expectedFixturesDir
		}
		absoluteObserved, absoluteErr := filepath.Abs(observed)
		if absoluteErr != nil || filepath.Clean(absoluteObserved) != filepath.Clean(expected) {
			return abCommandResult{}, invalidf("invalid_harness_layout", "%s must equal the frozen harness /%s directory", name, strings.TrimSuffix(name, "-dir"))
		}
	}
	selected, err := loadSelectedCases(*casesDir, manifest.Suite, "")
	if err != nil {
		return abCommandResult{}, err
	}
	if len(selected) != manifest.PublicCaseCount {
		return abCommandResult{}, invalidf("experiment_population", "public suite contains %d cases, manifest committed %d", len(selected), manifest.PublicCaseCount)
	}
	publicCasesDigest, err := publicCaseSetDigest(selected)
	if err != nil {
		return abCommandResult{}, invalidf("experiment_population", "digest public case catalog: %v", err)
	}
	if publicCasesDigest != manifest.PublicCasesDigest {
		return abCommandResult{}, invalidf("experiment_population", "public case catalog does not match the frozen manifest")
	}
	if strings.Join(declaredCriticalCaseIDs(selected), "\x00") != strings.Join(manifest.CriticalCaseIDs, "\x00") {
		return abCommandResult{}, invalidf("experiment_population", "critical case ids do not match the frozen manifest")
	}
	fixtureRootByCaseID := make(map[string]string, len(selected))
	caseIsHoldout := make(map[string]bool)
	holdoutReferenceByCaseID := make(map[string]string)
	for _, testCase := range selected {
		fixtureRootByCaseID[testCase.ID] = *fixturesDir
	}
	var holdoutCases []contracts.Case
	var holdoutDigest string
	if manifest.Holdout != nil {
		holdout := bundles["holdout"]
		if holdout.AbsoluteRoot == "" {
			return abCommandResult{}, invalidf("invalid_frozen_bundles", "manifest declares a holdout bundle but none was verified")
		}
		holdoutCasesDir, directoryErr := frozenBundleDirectory(holdout.AbsoluteRoot, "cases")
		if directoryErr != nil {
			return abCommandResult{}, invalidf("invalid_holdout_layout", "%v", privateHoldoutError())
		}
		holdoutFixturesDir, directoryErr := frozenBundleDirectory(holdout.AbsoluteRoot, "fixtures")
		if directoryErr != nil {
			return abCommandResult{}, invalidf("invalid_holdout_layout", "%v", privateHoldoutError())
		}
		holdoutCases, err = loadSelectedCases(holdoutCasesDir, manifest.Suite, "")
		if err != nil {
			return abCommandResult{}, invalidf("invalid_holdout", "%v", privateHoldoutError())
		}
		if len(holdoutCases) != manifest.HoldoutCaseCount {
			return abCommandResult{}, invalidf("experiment_population", "%v", privateHoldoutError())
		}
		known := make(map[string]struct{}, len(selected)+len(holdoutCases))
		for _, testCase := range selected {
			known[testCase.ID] = struct{}{}
		}
		for _, testCase := range holdoutCases {
			if testCase.Migration != nil {
				return abCommandResult{}, invalidf("invalid_holdout", "%v", privateHoldoutError())
			}
			if _, duplicate := known[testCase.ID]; duplicate {
				return abCommandResult{}, invalidf("invalid_holdout", "%v", privateHoldoutError())
			}
			known[testCase.ID] = struct{}{}
			fixtureRootByCaseID[testCase.ID] = holdoutFixturesDir
			caseIsHoldout[testCase.ID] = true
		}
		holdoutReferenceByCaseID = buildHoldoutReferences(holdoutCases)
		selected = append(selected, holdoutCases...)
		holdoutDigest = holdout.Snapshot.Digest
	}
	executableClosure, err := runner.ResolveExecutableClosure(selected, "git")
	if err != nil {
		if manifest.Holdout != nil {
			return abCommandResult{}, invalidf("invalid_toolchain_closure", "%v", privateHoldoutError())
		}
		return abCommandResult{}, invalidf("invalid_toolchain_closure", "%v", err)
	}
	if executableClosure.Digest() != manifest.Execution.ToolchainsDigest {
		if manifest.Holdout != nil {
			return abCommandResult{}, invalidf("toolchains_mismatch", "%v", privateHoldoutError())
		}
		return abCommandResult{}, invalidf("toolchains_mismatch", "got %s, expected %s", executableClosure.Digest(), manifest.Execution.ToolchainsDigest)
	}
	modelsToProbe, err := effectiveOpenAIModels(manifest, selected, *legacyModel)
	if err != nil {
		if manifest.Holdout != nil {
			return abCommandResult{}, invalidf("invalid_holdout", "%v", privateHoldoutError())
		}
		return abCommandResult{}, err
	}
	caseIDs := make([]string, len(selected))
	for i := range selected {
		caseIDs[i] = selected[i].ID
	}
	plan, err := manifest.Plan(caseIDs)
	if err != nil {
		if manifest.Holdout != nil {
			return abCommandResult{}, invalidf("invalid_plan", "%v", privateHoldoutError())
		}
		return abCommandResult{}, invalidf("invalid_plan", "%v", err)
	}
	manifestDigest, err := contracts.CanonicalDigest(*manifest)
	if err != nil {
		return abCommandResult{}, invalidf("invalid_manifest_digest", "%v", err)
	}
	partialPath := outputPaths.Partial
	controlPath := outputPaths.Control
	candidatePath := outputPaths.Candidate
	comparisonPath := outputPaths.Comparison
	publishedPlan := redactHoldoutPlan(plan, holdoutReferenceByCaseID)

	caseByID := make(map[string]contracts.Case, len(selected))
	for _, testCase := range selected {
		caseByID[testCase.ID] = testCase
	}
	controlAggregate := emptyModelAggregate(manifest.Suite, manifest.Harness.Digest, manifestDigest)
	candidateAggregate := emptyModelAggregate(manifest.Suite, manifest.Harness.Digest, manifestDigest)
	started := time.Now().UTC()
	completedSamples := make(map[abSampleKey]struct{}, len(plan.Blocks)*2)
	completedRunIDs := make(map[string]struct{}, len(plan.Blocks)*2)
	var resumeSession *abResumeSession
	if *resumePartial != "" {
		holdoutByReference := make(map[string]string, len(holdoutReferenceByCaseID))
		for caseID, reference := range holdoutReferenceByCaseID {
			if _, conflictsWithPublic := caseByID[reference]; conflictsWithPublic {
				return abCommandResult{}, invalidf("invalid_holdout", "%v", privateHoldoutError())
			}
			holdoutByReference[reference] = caseID
		}
		expected := abResumeExpectation{
			ExperimentID: manifest.ID, Intent: manifest.Intent, Authority: authorityForIntent(manifest.Intent),
			ManifestDigest: manifestDigest, Suite: manifest.Suite, PublishedPlan: publishedPlan, ExecutionPlan: plan,
			HoldoutDigest: holdoutDigest, HoldoutCases: len(holdoutCases), HoldoutByRef: holdoutByReference,
			CasesByID: caseByID, ModelAssignment: manifest.ModelAssignment, Execution: manifest.Execution,
			HarnessDigest: manifest.Harness.Digest, ControlDigest: control.Snapshot.Digest, CandidateDigest: candidate.Snapshot.Digest,
		}
		var resumed abResumeState
		resumeSession, resumed, err = openABResume(*resumePartial, expected)
		if err != nil {
			return abCommandResult{}, invalidf("invalid_resume_partial", "partial artifact failed strict integrity, locking, or frozen-experiment validation")
		}
		defer func() { _ = resumeSession.Close() }()
		controlAggregate.Result = resumed.Control
		candidateAggregate.Result = resumed.Candidate
		completedSamples = resumed.Completed
		completedRunIDs = resumed.RunIDs
		started = resumed.Started
		for aggregate, bundleDigest := range map[*modelRunResult]string{
			&controlAggregate: control.Snapshot.Digest, &candidateAggregate: candidate.Snapshot.Digest,
		} {
			aggregate.BundleDigest = bundleDigest
			aggregate.HarnessDigest = manifest.Harness.Digest
			aggregate.ManifestDigest = manifestDigest
			aggregate.EvaluatorBinaryDigest = observedEvaluatorDigest
			aggregate.OpenCodeBinaryDigest = observedBinaryDigest
		}
		controlAggregate.EffectiveCases = effectiveABCases(selected, manifest.ModelAssignment, stats.VariantControl)
		candidateAggregate.EffectiveCases = effectiveABCases(selected, manifest.ModelAssignment, stats.VariantCandidate)
	} else {
		controlAggregate.Result.Started = started
		candidateAggregate.Result.Started = started
	}

	var reservations *outputReservations
	if resumeSession != nil {
		// A process can die after atomically publishing one final but before it
		// publishes the other two. Existing finals are eligible for exact reuse
		// only when the validated partial already contains the whole population;
		// an incomplete campaign must still reserve every final from scratch.
		allowExistingFinals := validateCompleteABPopulation(completedSamples, plan) == nil
		reservations, err = reserveABOutputsAllowExisting(allowExistingFinals, controlPath, candidatePath, comparisonPath)
	} else {
		reservations, err = reserveABOutputs(partialPath, controlPath, candidatePath, comparisonPath)
	}
	if err != nil {
		return abCommandResult{}, invalidf("output_exists", "%v", err)
	}
	defer func() { _ = reservations.Close() }()

	if deps.runModel == nil {
		return abCommandResult{}, infraf("runner_unavailable", fmt.Errorf("model runner is not configured"))
	}
	if deps.probeRuntime == nil {
		return abCommandResult{}, infraf("doctor_unavailable", fmt.Errorf("runtime probe is not configured"))
	}
	probe, probeErr := deps.probeRuntime(ctx, doctorOptions{
		Binary: resolvedBinary.Path, ResolvedBinary: &resolvedBinary,
		ExpectedVersion: manifest.Execution.OpenCodeVersion, Timeout: 30 * time.Second,
		OpenAIOAuthFile: openAIOAuthFile, OpenAIOAuthSession: oauthSession, Models: modelsToProbe,
	})
	if binaryErr := resolvedBinary.Revalidate(); binaryErr != nil {
		return abCommandResult{}, invalidf("opencode_binary_mismatch", "OpenCode executable drifted during preflight: %v", binaryErr)
	}
	if closureErr := executableClosure.Revalidate(); closureErr != nil {
		if manifest.Holdout != nil {
			return abCommandResult{}, invalidf("invalid_toolchain_closure", "%v", privateHoldoutError())
		}
		return abCommandResult{}, invalidf("invalid_toolchain_closure", "effective executable closure drifted during preflight: %v", closureErr)
	}
	if probeErr != nil {
		publishedProbeErr := probeErr
		if manifest.Holdout != nil {
			publishedProbeErr = privateHoldoutError()
		}
		var mismatch *lifecycle.VersionMismatchError
		if errors.As(probeErr, &mismatch) {
			return abCommandResult{}, invalidf("opencode_version_mismatch", "%v", publishedProbeErr)
		}
		var incompatible *openCodeCompatibilityError
		if errors.As(probeErr, &incompatible) || errors.Is(probeErr, client.ErrIncompatibleAPI) || errors.Is(probeErr, client.ErrInvalidProviderCatalog) || errors.Is(probeErr, client.ErrInvalidMCPStatusCatalog) {
			return abCommandResult{}, invalidf("opencode_api_incompatible", "%v", publishedProbeErr)
		}
		return abCommandResult{}, infraf("opencode_preflight", publishedProbeErr)
	}
	if !probe.Healthy || probe.Version != manifest.Execution.OpenCodeVersion {
		return abCommandResult{}, invalidf("opencode_version_mismatch", "preflight got healthy=%t version=%q", probe.Healthy, probe.Version)
	}
	openAPIDigest := ""
	for _, endpoint := range probe.Endpoints {
		if endpoint.Name == "/doc" {
			openAPIDigest = endpoint.Digest
			break
		}
	}
	if openAPIDigest != manifest.Execution.OpenCodeOpenAPIDigest {
		return abCommandResult{}, invalidf("opencode_api_mismatch", "got %s, expected %s", openAPIDigest, manifest.Execution.OpenCodeOpenAPIDigest)
	}

	costComplete := manifest.Execution.BillingMode != experiment.BillingModeChatGPTSubscription
	controlAggregate.CostEvidenceComplete = costComplete
	candidateAggregate.CostEvidenceComplete = costComplete
	totalCost := 0.0
	forcedExit := contracts.ExitSuccess
	savePartial := func(exitCode int) (string, error) {
		ended := time.Now().UTC()
		controlAggregate.Result.Ended, candidateAggregate.Result.Ended = ended, ended
		controlAggregate.Result.Complete, candidateAggregate.Result.Complete = false, false
		partial := partialABArtifact{
			SchemaVersion: partialABSchemaVersion, Kind: partialABKind, ExperimentID: manifest.ID,
			Intent: manifest.Intent, Authority: authorityForIntent(manifest.Intent),
			ManifestDigest: manifestDigest, Plan: publishedPlan,
			Control:       sanitizeHoldoutContractResult(controlAggregate.Result, holdoutReferenceByCaseID),
			Candidate:     sanitizeHoldoutContractResult(candidateAggregate.Result, holdoutReferenceByCaseID),
			HoldoutDigest: holdoutDigest, HoldoutCases: len(holdoutCases), ExitCode: exitCode,
		}
		if resumeSession != nil {
			if err := resumeSession.Save(partial); err != nil {
				return "", err
			}
			return partialPath, nil
		}
		if err := sealPartialABArtifact(&partial); err != nil {
			return "", err
		}
		stagedPartial, stageErr := stageABJSON(partialPath, partial)
		if stageErr != nil {
			return "", stageErr
		}
		stagedInfo, stageErr := os.Lstat(stagedPartial)
		if stageErr != nil {
			return "", fmt.Errorf("inspect staged A/B checkpoint: %w", stageErr)
		}
		defer removeABStageIfOwned(stagedPartial, stagedInfo)
		if target := reservations.targets[partialPath]; target != nil && target.consumed {
			updated, replaceErr := replaceOwnedABFile(partialPath, stagedPartial, target.publishedInfo)
			if replaceErr != nil {
				return "", replaceErr
			}
			target.publishedInfo = updated
		} else {
			if err := reservations.PublishStaged(partialPath, stagedPartial); err != nil {
				return "", err
			}
		}
		return partialPath, nil
	}
	finalizeFailure := func(cause error, controlSaved, candidateSaved bool) (abCommandResult, error) {
		exitCode, kind := classifyCommandError(cause)
		partialArtifact, saveErr := savePartial(exitCode)
		if saveErr != nil {
			return abCommandResult{}, infraf("save_partial_ab", errors.Join(cause, saveErr))
		}
		partialResult := abCommandResult{
			ExperimentID: manifest.ID, Intent: manifest.Intent, Authority: authorityForIntent(manifest.Intent),
			Plan: publishedPlan, PartialPath: partialArtifact,
			ControlRuns: len(controlAggregate.Result.Samples), CandidateRuns: len(candidateAggregate.Result.Samples),
			ObservedCost: observedCostPointer(totalCost, costComplete), CostComplete: costComplete,
			HoldoutDigest: holdoutDigest, HoldoutCases: len(holdoutCases), ExitCode: exitCode,
		}
		if controlSaved {
			partialResult.ControlPath = controlPath
		}
		if candidateSaved {
			partialResult.CandidatePath = candidatePath
		}
		return partialResult, &commandError{
			exitCode: exitCode, kind: kind,
			err:  fmt.Errorf("%w (partial artifact saved to %s)", cause, partialArtifact),
			data: partialResult,
		}
	}
	rollbackBundleDrift := func(checkpoint abBlockCheckpoint, driftErr error) (abCommandResult, error) {
		checkpoint.Restore(
			&controlAggregate, &candidateAggregate, &completedSamples, &completedRunIDs,
			&totalCost, &costComplete,
		)
		publishedDriftErr := driftErr
		if manifest.Holdout != nil {
			publishedDriftErr = privateHoldoutError()
		}
		partialArtifact, saveErr := savePartial(contracts.ExitInvalid)
		if saveErr != nil {
			return abCommandResult{}, infraf("save_partial_ab", errors.Join(publishedDriftErr, saveErr))
		}
		return abCommandResult{}, invalidf("bundle_drift", "%v (partial artifact saved to %s)", publishedDriftErr, partialArtifact)
	}
	if _, err := savePartial(contracts.ExitSuccess); err != nil {
		return abCommandResult{}, infraf("save_partial_ab", err)
	}

	for _, block := range plan.Blocks {
		if abBlockComplete(completedSamples, block) {
			continue
		}
		blockCheckpoint := checkpointABBlock(
			controlAggregate, candidateAggregate, completedSamples, completedRunIDs,
			totalCost, costComplete,
		)
		for _, variant := range block.Order {
			sampleKey := abSampleKey{CaseID: block.CaseID, Variant: variant, Repetition: block.Repetition}
			if _, complete := completedSamples[sampleKey]; complete {
				continue
			}
			if contextErr := ctx.Err(); contextErr != nil {
				forcedExit, _ = classifyCommandError(contextErr)
				break
			}
			bundle := control
			aggregate := &controlAggregate
			if variant == stats.VariantCandidate {
				bundle = candidate
				aggregate = &candidateAggregate
			}
			remaining := 0.0
			if *costCap > 0 {
				remaining = *costCap - totalCost
				if remaining <= 0 {
					forcedExit = contracts.ExitBudgetExhausted
					break
				}
			}
			testCase := caseByID[block.CaseID]
			if manifest.ModelAssignment != nil {
				testCase.Agent.Model = manifest.ModelAssignment.Control
				if variant == stats.VariantCandidate {
					testCase.Agent.Model = manifest.ModelAssignment.Candidate
				}
			}
			spec := modelRunSpec{
				Cases: []contracts.Case{testCase}, Suite: manifest.Suite,
				Variant: string(variant), Repetitions: 1, RepetitionStart: block.Repetition,
				FixtureRoot: fixtureRootByCaseID[block.CaseID], AgentBundleRoot: bundle.AbsoluteRoot,
				Binary: resolvedBinary.Path, ExpectedVersion: manifest.Execution.OpenCodeVersion,
				ExpectedOpenCodeBinaryDigest: manifest.Execution.OpenCodeBinaryDigest,
				ExpectedOpenCodeAPIDigest:    manifest.Execution.OpenCodeOpenAPIDigest,
				ExpectedToolchainsDigest:     manifest.Execution.ToolchainsDigest,
				ResolvedBinary:               &resolvedBinary,
				ExecutableClosure:            executableClosure,
				OpenAIOAuthFile:              openAIOAuthFile,
				OpenAIOAuthSession:           oauthSession,
				TraceDir:                     *traceDir, RetainTrace: *retainTrace,
				AllowImpure: *allowImpure, CostCapUSD: remaining, LegacyModelLabel: *legacyModel,
				HarnessDigest: manifest.Harness.Digest, ManifestDigest: manifestDigest,
				VerifiedBundleDigest: bundle.Snapshot.Digest, RequireExactBundle: true,
			}
			if caseIsHoldout[block.CaseID] {
				// Holdout prompts and responses must not leave a persisted trace,
				// even when public-case trace retention was requested.
				spec.RetainTrace = false
			}
			if closureErr := executableClosure.Revalidate(); closureErr != nil {
				publishedClosureErr := error(closureErr)
				if manifest.Holdout != nil {
					publishedClosureErr = privateHoldoutError()
				}
				return finalizeFailure(invalidf("invalid_toolchain_closure", "effective executable closure drifted before an A/B sample: %v", publishedClosureErr), false, false)
			}
			run, runErr := deps.runModel(ctx, spec)
			closureErr := executableClosure.Revalidate()
			if driftErr := rejectABSamplesAfterClosureDrift(&run, closureErr); driftErr != nil {
				if manifest.Holdout != nil {
					runErr = invalidf("invalid_toolchain_closure", "%v", privateHoldoutError())
				} else {
					runErr = driftErr
				}
			}
			if runErr != nil {
				markDeferredABSampleFailure(run.Result.Samples)
			}
			sampleErr := validateNewABSamples(
				run.Result.Samples, sampleKey, completedSamples, completedRunIDs, runErr == nil,
				holdoutReferenceByCaseID[block.CaseID],
			)
			if sampleErr != nil {
				run.Result.Samples = nil
				runErr = invalidf("invalid_model_result", "%v", sampleErr)
			}
			if len(run.Result.Samples) != 0 {
				mergeModelAggregate(aggregate, run)
				recordABSamples(run.Result.Samples, completedSamples, completedRunIDs)
				totalCost += run.ObservedCostUSD
				costComplete = costComplete && run.CostEvidenceComplete
			}
			if runErr != nil {
				if driftErr := frozen.VerifyUnchanged(); driftErr != nil {
					return rollbackBundleDrift(blockCheckpoint, driftErr)
				}
				costComplete = false
				aggregate.CostEvidenceComplete = false
				exitCode, kind := classifyCommandError(runErr)
				publishedRunErr := runErr
				if caseIsHoldout[block.CaseID] || manifest.Holdout != nil &&
					(kind == "invalid_toolchain_closure" || kind == "toolchains_mismatch") {
					publishedRunErr = fmt.Errorf("holdout run failed; diagnostic details redacted")
				}
				partialPath, saveErr := savePartial(exitCode)
				if saveErr != nil {
					return abCommandResult{}, infraf("save_partial_ab", errors.Join(publishedRunErr, saveErr))
				}
				partialResult := abCommandResult{
					ExperimentID: manifest.ID, Intent: manifest.Intent, Authority: authorityForIntent(manifest.Intent),
					Plan: publishedPlan, PartialPath: partialPath,
					ControlRuns: len(controlAggregate.Result.Samples), CandidateRuns: len(candidateAggregate.Result.Samples),
					ObservedCost: observedCostPointer(totalCost, costComplete), CostComplete: costComplete,
					HoldoutDigest: holdoutDigest, HoldoutCases: len(holdoutCases), ExitCode: exitCode,
				}
				return partialResult, &commandError{
					exitCode: exitCode, kind: kind,
					err:  fmt.Errorf("%w (partial artifact saved to %s)", publishedRunErr, partialPath),
					data: partialResult,
				}
			}
			if _, checkpointErr := savePartial(contracts.ExitSuccess); checkpointErr != nil {
				return finalizeFailure(infraf("save_partial_ab", checkpointErr), false, false)
			}
			code := run.CLIExitCode()
			if code == contracts.ExitBudgetExhausted || code == contracts.ExitInfrastructure || code == contracts.ExitInvalid || code == contracts.ExitAborted {
				forcedExit = code
				break
			}
		}
		if driftErr := frozen.VerifyUnchanged(); driftErr != nil {
			return rollbackBundleDrift(blockCheckpoint, driftErr)
		}
		if forcedExit != contracts.ExitSuccess {
			break
		}
		if *costCap > 0 && !costComplete {
			forcedExit = contracts.ExitInvalid
			break
		}
	}
	if forcedExit == contracts.ExitSuccess {
		if populationErr := validateCompleteABPopulation(completedSamples, plan); populationErr != nil {
			return finalizeFailure(invalidf("incomplete_ab_population", "%v", populationErr), false, false)
		}
	}
	ended := time.Now().UTC()
	controlAggregate.Result.Ended = ended
	candidateAggregate.Result.Ended = ended
	controlAggregate.Result.Complete = forcedExit == contracts.ExitSuccess
	candidateAggregate.Result.Complete = forcedExit == contracts.ExitSuccess
	if err := frozen.VerifyUnchanged(); err != nil {
		publishedDriftErr := err
		if manifest.Holdout != nil {
			publishedDriftErr = privateHoldoutError()
		}
		partialPath, saveErr := savePartial(contracts.ExitInvalid)
		if saveErr != nil {
			return abCommandResult{}, infraf("save_partial_ab", errors.Join(publishedDriftErr, saveErr))
		}
		return abCommandResult{}, invalidf("bundle_drift", "%v (partial artifact saved to %s)", publishedDriftErr, partialPath)
	}
	result := abCommandResult{
		ExperimentID: manifest.ID, Intent: manifest.Intent, Authority: authorityForIntent(manifest.Intent), Plan: publishedPlan,
		ControlRuns: len(controlAggregate.Result.Samples), CandidateRuns: len(candidateAggregate.Result.Samples),
		ObservedCost: observedCostPointer(totalCost, costComplete), CostComplete: costComplete,
		HoldoutDigest: holdoutDigest, HoldoutCases: len(holdoutCases), ExitCode: forcedExit,
	}
	if forcedExit != contracts.ExitSuccess {
		partialPath, err := savePartial(forcedExit)
		if err != nil {
			return abCommandResult{}, infraf("save_partial_ab", err)
		}
		result.PartialPath = partialPath
		return result, nil
	}

	stagingDirectory, err := os.MkdirTemp(filepath.Dir(controlPath), ".skynex-ab-finals-")
	if err != nil {
		return finalizeFailure(infraf("stage_ab_outputs", err), false, false)
	}
	stagedControl := filepath.Join(stagingDirectory, "control.json")
	stagedCandidate := filepath.Join(stagingDirectory, "candidate.json")
	stagedComparison := filepath.Join(stagingDirectory, "comparison.json")
	defer func() {
		_ = os.Remove(stagedControl)
		_ = os.Remove(stagedCandidate)
		_ = os.Remove(stagedComparison)
		_ = os.Remove(stagingDirectory)
	}()
	if err := saveABBaseline(stagedControl, "control", manifest.Suite, manifest.Intent, controlAggregate, selected, holdoutCases, holdoutDigest, started); err != nil {
		return finalizeFailure(err, false, false)
	}
	if err := saveABBaseline(stagedCandidate, "candidate", manifest.Suite, manifest.Intent, candidateAggregate, selected, holdoutCases, holdoutDigest, started); err != nil {
		return finalizeFailure(err, false, false)
	}
	var comparison comparisonCommandResult
	comparison, err = commandCompare([]string{
		"--control", stagedControl, "--candidate", stagedCandidate,
		"--manifest", *manifestPath,
	})
	if err != nil {
		return finalizeFailure(err, false, false)
	}
	comparison.OutputPath = comparisonPath
	if err := reporter.Save(comparison, stagedComparison); err != nil {
		return finalizeFailure(err, false, false)
	}
	published := make([]string, 0, 3)
	for _, output := range []struct{ final, staged string }{
		{controlPath, stagedControl}, {candidatePath, stagedCandidate}, {comparisonPath, stagedComparison},
	} {
		if err := reservations.PublishOrReuse(output.final, output.staged); err != nil {
			rollbackErr := reservations.RollbackPublished(published...)
			return finalizeFailure(errors.Join(err, rollbackErr), false, false)
		}
		published = append(published, output.final)
	}
	if err := syncABDirectory(filepath.Dir(controlPath)); err != nil {
		rollbackErr := reservations.RollbackPublished(published...)
		return finalizeFailure(errors.Join(err, rollbackErr), false, false)
	}
	result.ControlPath, result.CandidatePath = controlPath, candidatePath
	result.Comparison = &comparison
	result.ExitCode = comparison.CLIExitCode()
	if resumeSession != nil {
		if removeErr := resumeSession.RemoveAfterSuccess(); removeErr != nil {
			result.PartialPath = partialPath
			result.ExitCode = contracts.ExitInfrastructure
			return result, &commandError{
				exitCode: contracts.ExitInfrastructure, kind: "remove_completed_partial",
				err: removeErr, data: result,
			}
		}
	} else if removeErr := reservations.RemoveOwned(partialPath); removeErr != nil {
		result.PartialPath = partialPath
		result.ExitCode = contracts.ExitInfrastructure
		return result, &commandError{
			exitCode: contracts.ExitInfrastructure, kind: "remove_completed_partial",
			err: removeErr, data: result,
		}
	}
	return result, nil
}

func effectiveOpenAIModels(manifest *experiment.Manifest, testCases []contracts.Case, legacyModel string) ([]string, error) {
	models := make([]string, 0, len(testCases))
	if manifest.ModelAssignment != nil {
		models = append(models, manifest.ModelAssignment.Control, manifest.ModelAssignment.Candidate)
	} else {
		for _, testCase := range testCases {
			model := testCase.Agent.Model
			if model == "" && testCase.Migration != nil {
				model = legacyModel
			}
			models = append(models, model)
		}
	}
	seen := make(map[string]struct{}, len(models))
	unique := models[:0]
	for _, model := range models {
		provider, _, err := contracts.ParseModelSelection(model)
		if err != nil {
			return nil, invalidf("invalid_model_selection", "one or more effective A/B model selections are invalid")
		}
		if provider != "openai" {
			return nil, invalidf("unsupported_provider", "clean OAuth A/B requires every effective model to select provider openai")
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		unique = append(unique, model)
	}
	if len(unique) == 0 {
		return nil, invalidf("empty_selection", "A/B selected no effective models")
	}
	return unique, nil
}

func requireLocalABCredentialBoundary(execution experiment.Execution) error {
	if execution.CredentialBoundary != experiment.CredentialBoundaryRuntimeReadable {
		return invalidf(
			"unsupported_execution",
			"this local OAuth backend is runtime-readable and cannot satisfy execution.credential_boundary=%q; release requires a provider-proxy backend",
			execution.CredentialBoundary,
		)
	}
	return nil
}

func privateHoldoutError() error {
	return errors.New("external holdout validation failed; diagnostic details redacted")
}

func emptyModelAggregate(suite, harnessDigest, manifestDigest string) modelRunResult {
	return modelRunResult{
		Result:        runner.ContractResult{Suite: suite, Complete: true},
		HarnessDigest: harnessDigest, ManifestDigest: manifestDigest, CostEvidenceComplete: true,
	}
}

func mergeModelAggregate(target *modelRunResult, source modelRunResult) {
	target.Result.Samples = append(target.Result.Samples, source.Result.Samples...)
	if target.BundleDigest == "" {
		target.BundleDigest = source.BundleDigest
		target.EvaluatorBinaryDigest = source.EvaluatorBinaryDigest
		target.OpenCodeBinaryDigest = source.OpenCodeBinaryDigest
	}
	target.ObservedCostUSD += source.ObservedCostUSD
	target.CostEvidenceComplete = target.CostEvidenceComplete && source.CostEvidenceComplete
	target.PublishedObservedCost = observedCostPointer(target.ObservedCostUSD, target.CostEvidenceComplete)
	target.BudgetExhausted = target.BudgetExhausted || source.BudgetExhausted
	known := make(map[string]bool, len(target.EffectiveCases))
	for _, testCase := range target.EffectiveCases {
		known[testCase.ID] = true
	}
	for _, testCase := range source.EffectiveCases {
		if !known[testCase.ID] {
			target.EffectiveCases = append(target.EffectiveCases, testCase)
			known[testCase.ID] = true
		}
	}
}

func saveABBaseline(path, label, suite, intent string, run modelRunResult, testCases, holdoutCases []contracts.Case, holdoutDigest string, createdAt time.Time) error {
	holdoutIDs := make(map[string]bool, len(holdoutCases))
	for _, testCase := range holdoutCases {
		holdoutIDs[testCase.ID] = true
	}
	holdoutReferences := buildHoldoutReferences(holdoutCases)
	effectiveCases := testCases
	if len(run.EffectiveCases) != 0 {
		effectiveCases = run.EffectiveCases
	}
	publicCases := make([]contracts.Case, 0, len(effectiveCases)-len(holdoutCases))
	for _, testCase := range effectiveCases {
		if !holdoutIDs[testCase.ID] {
			publicCases = append(publicCases, testCase)
		}
	}
	publicRun := run
	publicRun.Result.Samples = make([]contracts.RunResult, 0, len(run.Result.Samples))
	holdoutRuns := make([]contracts.RunResult, 0)
	for _, sample := range run.Result.Samples {
		if holdoutIDs[sample.CaseID] {
			holdoutRuns = append(holdoutRuns, sanitizeHoldoutRun(sample, holdoutReferences[sample.CaseID]))
			continue
		}
		publicRun.Result.Samples = append(publicRun.Result.Samples, sample)
	}
	publicRun.EffectiveCases = publicCases
	fingerprint, err := fingerprintForRun(publicRun, publicCases)
	if err != nil {
		return invalidf("invalid_fingerprint", "%s: %v", label, err)
	}
	aggregates, err := abArtifactAggregates(publicCases, holdoutCases, holdoutRuns, holdoutDigest, run.ManifestDigest, intent)
	if err != nil {
		return invalidf("invalid_baseline", "%s aggregates: %v", label, err)
	}
	artifact, err := baseline.NewRunArtifact(label, suite, createdAt, fingerprint, publicRun.Result.Samples, aggregates)
	if err != nil {
		return invalidf("invalid_baseline", "%s: %v", label, err)
	}
	if err := artifact.Save(path, baseline.IOOptions{}); err != nil {
		return infraf("save_baseline", err)
	}
	return nil
}

func abArtifactAggregates(testCases, holdoutCases []contracts.Case, holdoutRuns []contracts.RunResult, holdoutDigest, manifestDigest, intent string) (map[string]json.RawMessage, error) {
	result, err := artifactAggregates(testCases)
	if err != nil {
		return nil, err
	}
	result[evaluationAuthorityAggregateKey] = encodeEvaluationAuthority(evaluationAuthorityMetadata{
		Mode: authorityForIntent(intent), Intent: intent, ManifestDigest: manifestDigest,
	})
	if len(holdoutCases) == 0 || holdoutDigest == "" {
		return result, nil
	}
	encoded, err := baseline.CanonicalJSON(holdoutArtifactMetadata{
		BundleDigest: holdoutDigest, CaseCount: len(holdoutCases),
		Samples: append([]contracts.RunResult(nil), holdoutRuns...),
	})
	if err != nil {
		return nil, err
	}
	result[holdoutAggregateKey] = encoded
	return result, nil
}

func sanitizeHoldoutContractResult(result runner.ContractResult, references map[string]string) runner.ContractResult {
	sanitized := result
	sanitized.Samples = make([]contracts.RunResult, len(result.Samples))
	for index, sample := range result.Samples {
		if reference := references[sample.CaseID]; reference != "" {
			sample = sanitizeHoldoutRun(sample, reference)
		}
		sanitized.Samples[index] = sample
	}
	return sanitized
}

func sanitizeHoldoutRun(run contracts.RunResult, caseReference string) contracts.RunResult {
	redactedDigest := digestBytes([]byte("skynex-eval-holdout-redacted-v1"))
	runReference := digestBytes([]byte(fmt.Sprintf("%s\x00%s\x00%d", caseReference, run.Variant, run.Repetition)))
	var runError *contracts.RunError
	checkStatus := contracts.CheckStatusPass
	if run.Status != contracts.RunStatusPass {
		runError = &contracts.RunError{Kind: "holdout_redacted", Message: "holdout outcome details redacted"}
		checkStatus = contracts.CheckStatusFail
	}
	return contracts.RunResult{
		SchemaVersion: contracts.ResultSchemaVersion,
		RunID:         "holdout_run_" + strings.TrimPrefix(runReference, "sha256:"),
		CaseID:        caseReference,
		Variant:       run.Variant,
		Repetition:    run.Repetition,
		Status:        run.Status,
		Provenance: contracts.Provenance{
			GitSHA: strings.Repeat("0", 40), CaseDigest: redactedDigest,
			PromptDigest: redactedDigest, ConfigDigest: redactedDigest, FixtureDigest: redactedDigest,
			OpenCodeVersion: "redacted", OpenCodeAPIDigest: redactedDigest,
			Model: "redacted/model", Provider: "redacted", ToolsetDigest: redactedDigest,
			JudgeDigest: redactedDigest, PricingTableDigest: redactedDigest,
			ExecutionMode: run.Provenance.ExecutionMode, Network: run.Provenance.Network,
			Host: contracts.HostProvenance{OS: "redacted", Arch: "redacted"},
		},
		Checks: []contracts.CheckResult{{
			ID: "holdout_redacted_check", Type: "redacted", Status: checkStatus, Hard: true,
			Summary: "holdout behavior details redacted", RequirementIDs: []string{"REQ-001"},
			EvidenceIDs: []string{"holdout_redacted_evidence"},
		}},
		Usage: run.Usage, Coordination: run.Coordination,
		Timing: run.Timing, Evidence: contracts.Evidence{Items: []contracts.EvidenceItem{{
			ID: "holdout_redacted_evidence", Kind: "redacted", Source: contracts.EvidenceEvaluator,
			Digest: redactedDigest, Summary: "holdout evidence redacted", Complete: true,
		}}},
		TelemetryComplete: run.TelemetryComplete, Error: runError,
	}
}

func encodeEvaluationAuthority(metadata evaluationAuthorityMetadata) json.RawMessage {
	encoded, _ := baseline.CanonicalJSON(metadata)
	return encoded
}

func authorityForIntent(intent string) string {
	if intent == experiment.IntentRelease {
		return evaluationAuthorityRelease
	}
	return evaluationAuthorityDevelopment
}

func buildHoldoutReferences(testCases []contracts.Case) map[string]string {
	ids := make([]string, len(testCases))
	for index := range testCases {
		ids[index] = testCases[index].ID
	}
	sort.Strings(ids)
	references := make(map[string]string, len(ids))
	for index, id := range ids {
		references[id] = fmt.Sprintf("holdout_%04d", index+1)
	}
	return references
}

func redactHoldoutPlan(plan stats.ExperimentPlan, references map[string]string) stats.ExperimentPlan {
	redacted := plan
	redacted.Blocks = append([]stats.BlockPlan(nil), plan.Blocks...)
	for index := range redacted.Blocks {
		block := &redacted.Blocks[index]
		token := references[block.CaseID]
		if token == "" {
			block.Order = append([]stats.Variant(nil), block.Order...)
			continue
		}
		block.CaseID = token
		block.ID = fmt.Sprintf("%s-%04d", token, block.Repetition)
		block.Order = append([]stats.Variant(nil), block.Order...)
	}
	return redacted
}

func frozenBundleDirectory(root, relative string) (string, error) {
	resolved, err := resolveWithin(root, relative)
	if err != nil {
		return "", err
	}
	if !isWithin(root, resolved) {
		return "", fmt.Errorf("%q is outside bundle root", relative)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", relative)
	}
	return resolved, nil
}

// outputReservations claims every A/B result path before the first model call.
// Keeping the opened inode lets us detect a target replaced during a long
// campaign and refuse to overwrite it. The writer may atomically replace only
// the placeholder inode owned by this reservation.
type outputReservations struct {
	targets map[string]*reservedOutput
}

type reservedOutput struct {
	file          *os.File
	info          os.FileInfo
	publishedInfo os.FileInfo
	preexisting   bool
	consumed      bool
}

type abOutputPaths struct {
	Partial    string
	Control    string
	Candidate  string
	Comparison string
}

func validateABOutputLocations(prefix string, bundles []experiment.VerifiedBundle) (abOutputPaths, error) {
	if strings.TrimSpace(prefix) == "" {
		return abOutputPaths{}, fmt.Errorf("A/B output prefix must not be empty")
	}
	paths := abOutputPaths{
		Partial: prefix + ".partial.json", Control: prefix + ".control.json",
		Candidate: prefix + ".candidate.json", Comparison: prefix + ".comparison.json",
	}
	for label, path := range map[string]string{
		"partial": paths.Partial, "control": paths.Control,
		"candidate": paths.Candidate, "comparison": paths.Comparison,
	} {
		resolved, err := resolveFuturePath(path)
		if err != nil {
			return abOutputPaths{}, fmt.Errorf("resolve %s output: %w", label, err)
		}
		for _, bundle := range bundles {
			root, err := filepath.EvalSymlinks(bundle.AbsoluteRoot)
			if err != nil {
				return abOutputPaths{}, fmt.Errorf("resolve frozen %s bundle: %w", bundle.Name, err)
			}
			inside, err := pathWithinOrEqual(root, resolved)
			if err != nil {
				return abOutputPaths{}, fmt.Errorf("compare %s output with %s bundle: %w", label, bundle.Name, err)
			}
			if inside {
				return abOutputPaths{}, fmt.Errorf("%s output %q must be outside frozen %s bundle %q", label, path, bundle.Name, bundle.AbsoluteRoot)
			}
		}
	}
	return paths, nil
}

// resolveFuturePath resolves every existing symlink in the nearest ancestor
// while preserving the not-yet-created suffix. It lets preflight reject an
// output alias into a frozen bundle without creating the target first.
func resolveFuturePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	probe := absolute
	missing := make([]string, 0, 4)
	for {
		if _, err := os.Lstat(probe); err == nil {
			resolved, err := filepath.EvalSymlinks(probe)
			if err != nil {
				return "", err
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("no existing ancestor for %q", path)
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

func pathWithinOrEqual(root, candidate string) (bool, error) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil {
		return false, err
	}
	return relative == "." || relative != ".." && !filepath.IsAbs(relative) && !startsWithParent(relative), nil
}

func reserveABOutputs(paths ...string) (*outputReservations, error) {
	return reserveABOutputsAllowExisting(false, paths...)
}

// reserveABOutputsAllowExisting keeps the ordinary no-clobber behavior unless
// allowExisting is true. The latter is used only for a fully completed,
// integrity-validated resume, where a prior process may have died between
// publishing the three finals. Such files are opened and inode-pinned here;
// PublishOrReuse later accepts them only if they exactly equal the regenerated
// canonical artifact.
func reserveABOutputsAllowExisting(allowExisting bool, paths ...string) (*outputReservations, error) {
	reservations := &outputReservations{targets: make(map[string]*reservedOutput, len(paths))}
	for _, path := range paths {
		if _, duplicate := reservations.targets[path]; duplicate {
			_ = reservations.Close()
			return nil, fmt.Errorf("duplicate result target %q", path)
		}
		directory := filepath.Dir(path)
		if directory == "" {
			directory = "."
		}
		if err := os.MkdirAll(directory, 0o700); err != nil {
			_ = reservations.Close()
			return nil, fmt.Errorf("create result directory for %q: %w", path, err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) && allowExisting {
				existing, existingInfo, openErr := openExistingABOutput(path)
				if openErr == nil {
					reservations.targets[path] = &reservedOutput{
						file: existing, info: existingInfo, preexisting: true, consumed: true,
					}
					continue
				}
				_ = reservations.Close()
				return nil, fmt.Errorf("inspect resumable existing result %q: %w", path, openErr)
			}
			_ = reservations.Close()
			if errors.Is(err, os.ErrExist) {
				return nil, fmt.Errorf("refusing to replace existing result %q", path)
			}
			return nil, fmt.Errorf("reserve result target %q: %w", path, err)
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			// The opened descriptor did not yield an inode identity. Preserve the
			// fail-closed placeholder rather than risk unlinking a concurrent
			// pathname replacement that we cannot prove belongs to us.
			_ = reservations.Close()
			return nil, fmt.Errorf("stat reserved result target %q: %w", path, err)
		}
		reservations.targets[path] = &reservedOutput{file: file, info: info}
	}
	return reservations, nil
}

func openExistingABOutput(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("existing result must be a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("existing result changed while opening")
	}
	return file, opened, nil
}

// PublishOrReuse publishes into an owned empty placeholder or, for a
// fully-completed resume, proves that an inode-pinned preexisting final is
// byte-for-byte identical to the canonical artifact regenerated from the
// partial. Mismatches are preserved and never overwritten.
func (reservations *outputReservations) PublishOrReuse(path, staged string) error {
	if reservations == nil {
		return fmt.Errorf("output reservation is required")
	}
	target := reservations.targets[path]
	if target == nil {
		return fmt.Errorf("result target %q is not reserved", path)
	}
	if !target.preexisting {
		return reservations.PublishStaged(path, staged)
	}
	if target.file == nil || target.info == nil {
		return fmt.Errorf("preexisting result %q has no pinned identity", path)
	}
	current, err := os.Lstat(path)
	if err != nil || !stableABFileIdentity(target.info, current) {
		return fmt.Errorf("preexisting result %q changed during resume", path)
	}
	if target.info.Size() > baseline.DefaultMaxJSONBytes {
		return fmt.Errorf("preexisting result %q exceeds %d bytes", path, baseline.DefaultMaxJSONBytes)
	}
	expected, err := os.ReadFile(staged)
	if err != nil {
		return fmt.Errorf("read regenerated result %q: %w", path, err)
	}
	if int64(len(expected)) > baseline.DefaultMaxJSONBytes {
		return fmt.Errorf("regenerated result %q exceeds %d bytes", path, baseline.DefaultMaxJSONBytes)
	}
	if _, err := target.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind preexisting result %q: %w", path, err)
	}
	observed, err := io.ReadAll(io.LimitReader(target.file, baseline.DefaultMaxJSONBytes+1))
	if err != nil {
		return fmt.Errorf("read preexisting result %q: %w", path, err)
	}
	if int64(len(observed)) > baseline.DefaultMaxJSONBytes {
		return fmt.Errorf("preexisting result %q exceeds %d bytes", path, baseline.DefaultMaxJSONBytes)
	}
	afterFD, statErr := target.file.Stat()
	afterPath, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !stableABFileIdentity(target.info, afterFD) || !stableABFileIdentity(target.info, afterPath) {
		return fmt.Errorf("preexisting result %q changed while validating", path)
	}
	if !bytes.Equal(observed, expected) {
		return fmt.Errorf("refusing to replace non-matching existing result %q", path)
	}
	if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("retire regenerated duplicate for %q: %w", path, err)
	}
	return nil
}

// PublishStaged removes only the inode reserved by this process and then uses
// hard-link publication. Link is atomic and has no replace mode: if any path
// appears after the owned placeholder is retired, publication fails with
// EEXIST and preserves that external file.
func (reservations *outputReservations) PublishStaged(path, staged string) error {
	if reservations == nil {
		return fmt.Errorf("output reservation is required")
	}
	target := reservations.targets[path]
	if target == nil || target.consumed || target.preexisting || target.info == nil {
		return fmt.Errorf("result target %q has no fresh owned reservation", path)
	}
	stagedBefore, err := os.Lstat(staged)
	if err != nil || stagedBefore.Mode()&os.ModeSymlink != 0 || !stagedBefore.Mode().IsRegular() {
		if err == nil {
			err = fmt.Errorf("staged result is not a regular non-symlink file")
		}
		return fmt.Errorf("inspect staged result %q: %w", staged, err)
	}
	if err := removeOwnedABFile(path, target.info); err != nil {
		return fmt.Errorf("retire owned result placeholder %q: %w", path, err)
	}
	target.consumed = true
	if err := os.Link(staged, path); err != nil {
		return fmt.Errorf("publish result %q without clobbering: %w", path, err)
	}
	stagedAfter, stagedErr := os.Lstat(staged)
	published, publishedErr := os.Lstat(path)
	if stagedErr != nil || publishedErr != nil || !stableABFileIdentity(stagedBefore, stagedAfter) ||
		!stableABFileIdentity(stagedBefore, published) {
		return fmt.Errorf("published result %q has an unexpected identity", path)
	}
	target.publishedInfo = published
	if err := os.Remove(staged); err != nil {
		return fmt.Errorf("remove linked staged result %q: %w", staged, err)
	}
	if target.file != nil {
		_ = target.file.Close()
	}
	return syncABDirectory(filepath.Dir(path))
}

func stageABJSON(target string, value any) (string, error) {
	directory := filepath.Dir(target)
	if directory == "" {
		directory = "."
	}
	file, err := os.CreateTemp(directory, ".skynex-ab-checkpoint-")
	if err != nil {
		return "", fmt.Errorf("create staged A/B checkpoint: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close staged A/B checkpoint: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("prepare staged A/B checkpoint: %w", err)
	}
	if err := reporter.Save(value, path); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func stableABFileIdentity(expected, observed os.FileInfo) bool {
	return expected != nil && observed != nil && observed.Mode().IsRegular() && os.SameFile(expected, observed) &&
		expected.Size() == observed.Size() && expected.ModTime().Equal(observed.ModTime())
}

func (reservations *outputReservations) RemoveOwned(path string) error {
	if reservations == nil {
		return fmt.Errorf("output reservation is required")
	}
	target, exists := reservations.targets[path]
	if !exists || target.publishedInfo == nil {
		return fmt.Errorf("result target %q has no owned published file", path)
	}
	if err := removeOwnedABFile(path, target.publishedInfo); err != nil {
		return err
	}
	target.publishedInfo = nil
	target.consumed = true
	return nil
}

func (reservations *outputReservations) RollbackPublished(paths ...string) error {
	if reservations == nil {
		return nil
	}
	var rollbackErr error
	for _, path := range paths {
		target := reservations.targets[path]
		if target == nil || target.preexisting || target.publishedInfo == nil {
			continue
		}
		if err := removeOwnedABFile(path, target.publishedInfo); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
			continue
		}
		target.publishedInfo = nil
		target.consumed = true
	}
	return rollbackErr
}

func (reservations *outputReservations) Close() error {
	if reservations == nil {
		return nil
	}
	var cleanupErr error
	for path, target := range reservations.targets {
		if target == nil {
			continue
		}
		if err := target.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close reserved result %q: %w", path, err))
		}
		if !target.consumed {
			current, statErr := os.Lstat(path)
			// A path already replaced or removed by somebody else is not ours to
			// clean up. removeOwnedABFile fences the remaining race after this
			// identity check by quarantining and re-checking the moved inode.
			if statErr != nil || !current.Mode().IsRegular() || !os.SameFile(target.info, current) {
				continue
			}
			if err := removeOwnedABFile(path, target.info); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove reserved result %q: %w", path, err))
			}
		}
	}
	return cleanupErr
}

func isWithin(root, candidate string) bool {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absoluteCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	if evaluated, evalErr := filepath.EvalSymlinks(absoluteRoot); evalErr == nil {
		absoluteRoot = evaluated
	} else {
		return false
	}
	if evaluated, evalErr := filepath.EvalSymlinks(absoluteCandidate); evalErr == nil {
		absoluteCandidate = evaluated
	} else {
		return false
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteCandidate)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && relative != "" && relative[0] != os.PathSeparator && !startsWithParent(relative)
}

func startsWithParent(relative string) bool {
	return relative == ".." || len(relative) > 3 && relative[:3] == ".."+string(os.PathSeparator)
}
