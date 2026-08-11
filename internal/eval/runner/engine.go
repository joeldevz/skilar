package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/lifecycle"
	"github.com/joeldevz/skynex/internal/eval/redact"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/trace"
)

const (
	provenanceExtensionAgentBundleDigest         = "x-agent-bundle-digest"
	provenanceExtensionHarnessBundleDigest       = "x-harness-bundle-digest"
	provenanceExtensionManifestDigest            = "x-experiment-manifest-digest"
	provenanceExtensionEffectiveConfigDigest     = ProvenanceExtensionEffectiveConfigDigest
	provenanceExtensionEffectiveAgentsDigest     = ProvenanceExtensionEffectiveAgentsDigest
	provenanceExtensionEffectiveToolPolicyDigest = "x-effective-tool-policy-digest"
	provenanceExtensionEffectiveToolCatalog      = "x-effective-tool-catalog-digest"
	provenanceExtensionEffectiveToolStatus       = "x-effective-tool-catalog-status"
	provenanceExtensionEffectiveProviderCatalog  = contracts.ProvenanceExtensionProviderCatalogDigest
	provenanceExtensionObservedProvider          = "x-observed-provider"
	provenanceExtensionObservedModel             = "x-observed-model"
	provenanceExtensionRedaction                 = "x-redaction"
	durableResponsePollInterval                  = 50 * time.Millisecond
	durableResponseMaxWait                       = 2 * time.Second
	defaultEventReadinessTimeout                 = 15 * time.Second
)

type evaluationErrorCode string

const (
	evaluationErrorPostResponseInvalid      evaluationErrorCode = "post_response_invalid"
	evaluationErrorDurableResponseMissing   evaluationErrorCode = "durable_response_missing"
	evaluationErrorDurableResponseGetFailed evaluationErrorCode = "durable_response_get_failed"
	evaluationErrorDurableResponseInvalid   evaluationErrorCode = "durable_response_invalid"
	evaluationErrorMessageListGetFailed     evaluationErrorCode = "message_list_get_failed"
	evaluationErrorMessageListEmpty         evaluationErrorCode = "message_list_empty"
	evaluationErrorMessageListInvalid       evaluationErrorCode = "message_list_invalid"
	evaluationErrorMessageListInconsistent  evaluationErrorCode = "message_list_inconsistent"
)

type codedEvaluationError struct {
	code evaluationErrorCode
}

func (e *codedEvaluationError) Error() string { return string(e.code) }

func newCodedEvaluationError(code evaluationErrorCode) error {
	return &codedEvaluationError{code: code}
}

type Engine struct {
	config        EngineConfig
	pricingDigest string
}

func NewEngine(config EngineConfig) (*Engine, error) {
	if config.Factory == nil {
		return nil, fmt.Errorf("runtime factory is required")
	}
	var err error
	if config.RunParent, err = absoluteDirectory(config.RunParent); err != nil {
		return nil, fmt.Errorf("run parent: %w", err)
	}
	if config.FixtureRoot, err = absoluteDirectory(config.FixtureRoot); err != nil {
		return nil, fmt.Errorf("fixture root: %w", err)
	}
	if config.AgentBundleRoot != "" {
		if config.AgentBundleRoot, err = absoluteDirectory(config.AgentBundleRoot); err != nil {
			return nil, fmt.Errorf("agent bundle root: %w", err)
		}
		if config.BundleDigest == "" {
			return nil, fmt.Errorf("agent bundle digest is required")
		}
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.NewRunID == nil {
		config.NewRunID = randomRunID
	}
	if config.EventReadinessTimeout <= 0 {
		config.EventReadinessTimeout = defaultEventReadinessTimeout
	}
	pricingDigest, err := config.Pricing.Digest()
	if err != nil {
		return nil, fmt.Errorf("pricing table: %w", err)
	}
	if config.Provenance.ConfigDigest == "" {
		config.Provenance.ConfigDigest = config.BundleDigest
	}
	if config.Provenance.BundleDigest == "" {
		config.Provenance.BundleDigest = config.BundleDigest
	}
	if config.ExecutableClosure != nil {
		if config.Provenance.ToolchainsDigest == "" {
			config.Provenance.ToolchainsDigest = config.ExecutableClosure.Digest()
		}
		if config.Provenance.ToolchainsDigest != config.ExecutableClosure.Digest() {
			return nil, fmt.Errorf("provenance toolchains digest does not match executable closure")
		}
	}
	if config.Provenance.ToolchainsDigest == "" {
		config.Provenance.ToolchainsDigest, _ = contracts.CanonicalDigest(map[string]string{
			"kind": "evaluator-runtime-only-v1", "go_runtime": runtime.Version(),
		})
	}
	if err := validateEngineProvenance(config); err != nil {
		return nil, err
	}
	return &Engine{config: config, pricingDigest: pricingDigest}, nil
}

var engineDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var engineGitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

func validateEngineProvenance(config EngineConfig) error {
	if config.AgentBundleRoot == "" {
		return fmt.Errorf("agent bundle root is required")
	}
	if !engineGitSHAPattern.MatchString(config.Provenance.GitSHA) {
		return fmt.Errorf("provenance git SHA must contain 40 to 64 lowercase hexadecimal characters")
	}
	if strings.TrimSpace(config.Provenance.OpenCodeVersion) == "" {
		return fmt.Errorf("provenance OpenCode version is required")
	}
	for name, digest := range map[string]string{
		"bundle": config.Provenance.BundleDigest, "config": config.Provenance.ConfigDigest,
		"prompt": config.Provenance.PromptDigest, "toolset": config.Provenance.ToolsetDigest,
		"toolchains": config.Provenance.ToolchainsDigest,
		"harness":    config.Provenance.HarnessDigest, "manifest": config.Provenance.ManifestDigest,
	} {
		if !engineDigestPattern.MatchString(digest) {
			return fmt.Errorf("provenance %s digest must be sha256", name)
		}
	}
	return nil
}

func (e *Engine) Run(ctx context.Context, testCase contracts.Case, request RunRequest) (runResult contracts.RunResult, returnErr error) {
	if ctx == nil {
		return contracts.RunResult{}, fmt.Errorf("run context is nil")
	}
	testCase.Normalize()
	if err := testCase.Validate(); err != nil {
		return contracts.RunResult{}, fmt.Errorf("case %s: %w", testCase.ID, err)
	}
	caseProvider, _, _ := contracts.ParseModelSelection(testCase.Agent.Model)
	if asserted := strings.TrimSpace(e.config.Provenance.Provider); asserted != "" && asserted != caseProvider {
		return contracts.RunResult{}, fmt.Errorf("case %s provider %q conflicts with configured provenance provider %q", testCase.ID, caseProvider, asserted)
	}
	if request.Variant == "" {
		request.Variant = "current"
	}
	if request.Repetition < 1 || request.Repetition > contracts.MaxRuns {
		return contracts.RunResult{}, fmt.Errorf("repetition must be between 1 and %d", contracts.MaxRuns)
	}
	runID, err := e.config.NewRunID()
	if err != nil {
		return contracts.RunResult{}, fmt.Errorf("create run id: %w", err)
	}
	caseDigest, err := testCase.Digest()
	if err != nil {
		return contracts.RunResult{}, err
	}
	result := e.baseResult(testCase, request, runID, caseDigest)
	started := e.config.Clock()
	runTimeout, _ := time.ParseDuration(testCase.Completion.Timeout)
	runCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	if err := e.verifyExecutableClosureForCase(testCase); err != nil {
		return e.earlyResult(result, started, contracts.RunStatusInvalid, "toolchain_closure", err)
	}

	sourcePath, err := resolveBelow(e.config.FixtureRoot, testCase.Fixture.Source)
	if err != nil {
		return e.earlyResult(result, started, contracts.RunStatusInvalid, "fixture_path", err)
	}
	workspace, err := sandbox.Materialize(runCtx, sandbox.Config{
		ParentDir: e.config.RunParent, SourceDir: sourcePath,
		ExpectedSourceDigest: testCase.Fixture.ExpectedDigest,
		InitialGit:           testCase.Fixture.InitialGit,
		GitSeed:              mapGitSeed(testCase.Fixture.GitSeed),
		Setup:                mapCommands(testCase.Setup.Commands, e.config.ExecutableClosure),
		Runner:               runnerConfig(testCase, e.config.ExecutableClosure), Snapshot: e.config.SnapshotLimits,
	})
	if err != nil {
		if closureErr := e.verifyExecutableClosure(); closureErr != nil {
			return e.earlyResult(result, started, contracts.RunStatusInvalid, "toolchain_drift", closureErr)
		}
		return e.earlyResultForContext(runCtx, result, started, contracts.RunStatusInfraError, "materialize", err)
	}
	defer func() {
		if closeErr := workspace.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove private run workspace: %w", closeErr))
		}
	}()
	if err := e.verifyExecutableClosure(); err != nil {
		return e.earlyResult(result, started, contracts.RunStatusInvalid, "toolchain_drift", err)
	}
	if err := rejectAmbientOpenCodeInputs(workspace.Path()); err != nil {
		return e.earlyResult(result, started, contracts.RunStatusInvalid, "fixture_runtime_authority", err)
	}
	result.Evidence.BeforeTree = workspace.Before.Digest
	result.Evidence.Items = append(result.Evidence.Items, evidenceItem("before", "filesystem-before", workspace.Before.Digest, true))
	if testCase.Fixture.InitialGit {
		result.Evidence.Items = append(result.Evidence.Items, gitStatusEvidenceItem("git_status_before", workspace.InitialGitStatus, workspace.InitialGitStatus.StateDigest != ""))
	}

	configRoot, bundleCopy, frozenBundleDigest, effectiveToolPolicy, err := e.freezeAgentBundle(workspace, testCase)
	if err != nil {
		return e.earlyResult(result, started, contracts.RunStatusInvalid, "bundle_freeze", err)
	}
	if bundleCopy != "" {
		defer thawTree(bundleCopy)
	}
	if err := e.verifyExecutableClosure(); err != nil {
		return e.earlyResult(result, started, contracts.RunStatusInvalid, "toolchain_drift", err)
	}
	// Record the exact per-case policy before starting OpenCode so an early
	// runtime failure remains auditable. The catalog cannot be known until the
	// read-only runtime probe succeeds, so ToolsetDigest is intentionally the
	// policy-only digest at this stage and is replaced with policy+catalog on
	// successful startup.
	result.Provenance.ConfigDigest = effectiveToolPolicy.Digest
	result.Provenance.ToolsetDigest = effectiveToolPolicy.Digest
	result.Provenance.Extensions[provenanceExtensionEffectiveToolPolicyDigest] = effectiveToolPolicy.Digest
	result.Provenance.Extensions[provenanceExtensionEffectiveToolStatus] = "unobserved"

	runtimeHandle, err := e.config.Factory.Start(runCtx, RuntimeRequest{
		WorkspacePath: workspace.Path(), RunPath: workspace.RunPath(), Case: testCase,
		ConfigRoot: configRoot, ToolPolicy: effectiveToolPolicy,
	})
	if err != nil {
		if runCtx.Err() != nil {
			return e.earlyResultForContext(runCtx, result, started, contracts.RunStatusInfraError, "runtime_start", err)
		}
		if errors.Is(err, ErrRuntimeContractIncompatible) || errors.Is(err, ErrRuntimeModelUnavailable) {
			return e.earlyResult(result, started, contracts.RunStatusInvalid, "runtime_contract", err)
		}
		return e.earlyResult(result, started, contracts.RunStatusInfraError, "runtime_start", err)
	}
	runtimeClosed := false
	defer func() {
		if !runtimeClosed {
			if closeErr := runtimeHandle.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("stop private runtime: %w", closeErr))
			}
		}
	}()
	info := runtimeHandle.Info()
	e.applyRuntimeInfo(&result, info)

	recorder, recorderErr := trace.StartRecorder(runCtx, runtimeHandle)
	if recorderErr != nil {
		closeErr := runtimeHandle.Close()
		runtimeClosed = true
		isolationErr := fmt.Errorf("%w: open global event stream before root session: %v", trace.ErrGlobalSessionIsolation, recorderErr)
		return e.earlyResultForContext(runCtx, result, started, contracts.RunStatusInvalid, "session_isolation", errors.Join(isolationErr, closeErr))
	}
	readyCtx, readyCancel := context.WithTimeout(runCtx, e.config.EventReadinessTimeout)
	readyErr := recorder.WaitForServerReady(readyCtx)
	readyCancel()
	if readyErr != nil {
		recorder.PrepareForRuntimeStop()
		closeErr := runtimeHandle.Close()
		runtimeClosed = true
		_, stopErr := recorder.Stop()
		isolationErr := fmt.Errorf("%w: global event readiness preflight: %v", trace.ErrGlobalSessionIsolation, readyErr)
		return e.earlyResultForContext(runCtx, result, started, contracts.RunStatusInvalid, "session_isolation", errors.Join(isolationErr, closeErr, stopErr))
	}
	isolationTimeout, _ := time.ParseDuration(testCase.Trace.Quiescence.Timeout)
	admissionCtx, admissionCancel := context.WithTimeout(runCtx, isolationTimeout)
	session, err := runtimeHandle.CreateSessionContext(admissionCtx, "skynex-eval:"+testCase.ID+":"+runID)
	if err != nil {
		admissionErr := admissionCtx.Err()
		admissionCancel()
		if recorder != nil {
			recorder.PrepareForRuntimeStop()
		}
		closeErr := runtimeHandle.Close()
		runtimeClosed = true
		var stopErr error
		if recorder != nil {
			_, stopErr = recorder.Stop()
		}
		if admissionErr != nil && runCtx.Err() == nil {
			isolationErr := fmt.Errorf("%w: root session creation exceeded the admission window", trace.ErrGlobalSessionIsolation)
			return e.earlyResult(result, started, contracts.RunStatusInvalid, "session_isolation", errors.Join(isolationErr, closeErr, stopErr))
		}
		return e.earlyResultForContext(runCtx, result, started, contracts.RunStatusInfraError, "session_create", errors.Join(err, recorderErr, closeErr, stopErr))
	}
	isolationErr := recorder.WaitForSessionCreated(admissionCtx, session.ID)
	admissionCancel()
	if isolationErr == nil {
		isolationErr = trace.ValidateRootSessionAdmission(session.ID, recorder.Snapshot())
	}
	if isolationErr != nil {
		recorder.PrepareForRuntimeStop()
		closeErr := runtimeHandle.Close()
		runtimeClosed = true
		_, stopErr := recorder.Stop()
		isolationErr = fmt.Errorf("%w: root session event preflight: %v", trace.ErrGlobalSessionIsolation, isolationErr)
		return e.earlyResultForContext(runCtx, result, started, contracts.RunStatusInvalid, "session_isolation", errors.Join(isolationErr, closeErr, stopErr))
	}
	response, conversationErr := executeConversation(runCtx, runtimeHandle, session.ID, testCase)

	anchorState := durableResponseAnchorNotAttempted
	var durableWaitErr error
	if response != nil && !hasResponseContractError(conversationErr) {
		// Anchor the synchronous POST through the directed durable endpoint.
		// A 404 may be a bounded visibility gap; transport failures and an
		// identity-invalid message fail closed immediately.
		anchorCtx, anchorCancel := context.WithTimeout(runCtx, durableResponseMaxWait)
		anchorState = waitForDurableResponse(anchorCtx, runtimeHandle, session.ID, response)
		durableWaitErr = durableResponseAnchorError(anchorState)
		anchorCancel()
	}
	reconcileTimeout, _ := time.ParseDuration(testCase.Trace.Quiescence.Timeout)
	reconcileCtx, reconcileCancel := context.WithTimeout(runCtx, reconcileTimeout)
	collector := trace.New(runtimeHandle, traceOptionsForCase(e.config.TraceOptions, testCase.Trace.Quiescence))
	if anchorState == durableResponseAnchorValid {
		collector.ExpectRootMessage(response.Info.ID)
	}
	var collectedTrace *trace.Trace
	var traceErr error
	var runtimeCloseErr error
	if recorder != nil {
		collectedTrace, traceErr = collector.ReconcileRecorded(reconcileCtx, session.ID, recorder)
	} else {
		collectedTrace, traceErr = collector.Reconcile(reconcileCtx, session.ID, nil)
	}
	reconcileCancel()
	var responseTraceErr error
	if anchorState == durableResponseAnchorValid {
		responseTraceErr = validateDurableResponse(collectedTrace, session.ID, response, conversationErr)
	}
	responseTraceErr = errors.Join(durableWaitErr, responseTraceErr)
	conversationErr = errors.Join(conversationErr, responseTraceErr)
	observedProvider, observedModel, modelObservationErr := validateObservedRootModel(collectedTrace, testCase.Agent.Model)
	// Observed identifiers are untrusted trace data. Publish them only after the
	// entire session tree has been proven to use the frozen model selection; a
	// mismatch must not become a channel for candidate-controlled artifact text.
	if modelObservationErr == nil {
		if observedProvider != "" {
			result.Provenance.Extensions[provenanceExtensionObservedProvider] = observedProvider
		}
		if observedModel != "" {
			result.Provenance.Extensions[provenanceExtensionObservedModel] = observedModel
		}
	}
	traceErr = errors.Join(traceErr, modelObservationErr)
	if recorder != nil {
		recorder.PrepareForRuntimeStop()
	}
	closeErr := runtimeHandle.Close()
	runtimeClosed = true
	if closeErr != nil {
		runtimeCloseErr = closeErr
		traceErr = errors.Join(traceErr, fmt.Errorf("stop runtime: %w", closeErr))
	}
	if recorder != nil {
		events, stopErr := recorder.Stop()
		recorderErr = errors.Join(recorderErr, stopErr)
		traceErr = errors.Join(traceErr, trace.FinalizeRunEvents(collectedTrace, events))
	}
	if recorderErr != nil {
		traceErr = errors.Join(traceErr, fmt.Errorf("%w: global event recorder: %v", trace.ErrGlobalSessionIsolation, recorderErr))
	}

	var afterGitStatus sandbox.GitStatusEvidence
	var gitStatusErr error
	if testCase.Fixture.InitialGit {
		afterGitStatus, gitStatusErr = workspace.CaptureGitStatus(runCtx)
		result.Evidence.Items = append(result.Evidence.Items, gitStatusEvidenceItem("git_status_after", afterGitStatus, gitStatusErr == nil))
	}
	after, snapshotErr := workspace.Snapshot()
	if snapshotErr == nil {
		result.Evidence.AfterTree = after.Digest
		result.Evidence.Items = append(result.Evidence.Items, evidenceItem("after", "filesystem-after", after.Digest, true))
	}
	commandResults := runOracleCommands(runCtx, workspace, testCase.Oracle.Commands, e.config.ExecutableClosure)
	sourceUnchanged, sourceErr := workspace.SourceUnchanged()
	bundleUnchanged, bundleErr := e.verifyFrozenBundle(bundleCopy, frozenBundleDigest)
	toolchainErr := e.verifyExecutableClosure()

	finalText := durableFinalResponseText(collectedTrace, session.ID, response, conversationErr)
	traceDigest, tracePath, redactionSummary, persistErr := e.processTrace(runID, collectedTrace, request.RetainTrace)
	tracePersistErr := persistErr
	if traceDigest != "" {
		result.Evidence.TraceDigest = traceDigest
		result.Evidence.TracePath = tracePath
		result.Evidence.Items = append(result.Evidence.Items, evidenceItem("trace", "sanitized-trace", traceDigest, collectedTrace != nil))
	}
	if redactionSummary != "" {
		result.Provenance.Extensions[provenanceExtensionRedaction] = redactionSummary
	}
	traceErr = errors.Join(traceErr, persistErr)

	if collectedTrace != nil {
		usage, coordination := usageFromTrace(collectedTrace, e.config.Pricing)
		if !result.Provenance.ProviderCostUSDAuthoritative() {
			usage.Parent.ProviderCostUSD = nil
			usage.Tree.ProviderCostUSD = nil
		}
		result.Usage = usage
		result.Coordination = coordination
		result.Timing.ModelMS = modelDurationMS(collectedTrace)
		result.TelemetryComplete = collectedTrace.TelemetryComplete && recorderErr == nil
	}

	evidence, policy, evidenceItems, policyDigest := buildDeterministicInputs(testCase, workspace.Before, after, workspace.SetupResults, commandResults, collectedTrace, finalText, info, sourceUnchanged, bundleUnchanged, conversationErr, traceErr, modelObservationErr, snapshotErr, sourceErr, bundleErr)
	result.Evidence.Items = append(result.Evidence.Items, evidenceItems...)
	result.Provenance.JudgeDigest = policyDigest
	verdict := evaluateDeterministically(evidence, policy)
	verdict = withGitStateCheck(verdict, testCase, workspace.InitialGitStatus, afterGitStatus, gitStatusErr)
	result.Checks = contractChecks(verdict.Checks, testCase.RequirementIDs, testCase.BehaviorChecks)
	result.Checks = append(result.Checks, evaluateCaseChecks(testCase, workspace.Before, after, workspace.SetupResults, commandResults, collectedTrace, finalText, verdict, result.Evidence.Items)...)
	result.Status = statusFromChecks(result.Checks, statusFromVerdict(verdict.Status))
	if errors.Is(traceErr, trace.ErrGlobalSessionIsolation) {
		result.Status = contracts.RunStatusInvalid
	}
	if runtimeCloseErr != nil {
		if errors.Is(runtimeCloseErr, lifecycle.ErrOpenAIOAuthSessionCredentialChanged) {
			result.Status = contracts.RunStatusInvalid
		} else {
			result.Status = contracts.RunStatusInfraError
		}
	}
	if tracePersistErr != nil {
		result.Status = contracts.RunStatusInfraError
	}
	if toolchainErr != nil {
		result.Status = contracts.RunStatusInvalid
	}
	if hasResponseContractError(conversationErr) && result.Status != contracts.RunStatusInfraError {
		result.Status = contracts.RunStatusInvalid
	} else if conversationErr != nil && result.Status == contracts.RunStatusPass {
		result.Status = contracts.RunStatusFail
	}
	if errors.Is(runCtx.Err(), context.Canceled) {
		result.Status = contracts.RunStatusAborted
		result.Error = runError("canceled", runCtx.Err(), false, "infrastructure")
	} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		result.Status = contracts.RunStatusBudgetExhausted
		result.Error = runError("timeout", runCtx.Err(), false, "infrastructure")
	} else if toolchainErr != nil {
		result.Error = runError("toolchain_drift", toolchainErr, false, "infrastructure")
	} else if tracePersistErr != nil {
		result.Error = runError("trace_persist", tracePersistErr, true, "infrastructure")
	} else if runtimeCloseErr != nil {
		if errors.Is(runtimeCloseErr, lifecycle.ErrOpenAIOAuthSessionCredentialChanged) {
			result.Error = runError("credential_integrity", runtimeCloseErr, false, "infrastructure")
		} else {
			result.Error = runError("runtime_close", runtimeCloseErr, true, "infrastructure")
		}
	} else if result.Status != contracts.RunStatusPass {
		result.Error = summarizedRunError(result, conversationErr, traceErr, snapshotErr, sourceErr, bundleErr)
	} else {
		result.Error = nil
	}
	changes := sandbox.Diff(workspace.Before, after)
	if digest, digestErr := contracts.CanonicalDigest(changes); digestErr == nil {
		result.Evidence.DiffDigest = digest
	}
	result.Timing.WallMS = nonNegativeMilliseconds(e.config.Clock().Sub(started))
	sanitizePersistableRunResult(&result)
	if err := result.Validate(); err != nil {
		return result, fmt.Errorf("constructed run result is invalid: %w", err)
	}
	return result, nil
}

func validateObservedRootModel(collected *trace.Trace, expected string) (string, string, error) {
	if collected == nil {
		return "", "", fmt.Errorf("root model identity is unavailable because trace collection failed")
	}
	expectedProvider, expectedModel, err := contracts.ParseModelSelection(expected)
	if err != nil {
		return "", "", fmt.Errorf("expected model: %w", err)
	}
	observedProvider := ""
	observedModel := ""
	observations := 0
	for _, sessionTrace := range collected.Sessions {
		if sessionTrace.Session.ParentID != "" && !sessionHasCompletedAssistantResponse(sessionTrace) {
			return "", "", fmt.Errorf("trace child session %s has no completed assistant response", sessionTrace.Session.ID)
		}
		for _, message := range sessionTrace.Messages {
			if message.Info.Role != "assistant" {
				continue
			}
			providerID := message.Info.ProviderID
			modelID := message.Info.ModelID
			if providerID == "" || modelID == "" {
				return providerID, modelID, fmt.Errorf("trace session %s contains an incomplete provider/model identity", sessionTrace.Session.ID)
			}
			if strings.TrimSpace(providerID) != providerID || strings.TrimSpace(modelID) != modelID {
				return providerID, modelID, fmt.Errorf("trace session %s contains provider/model identity with surrounding whitespace", sessionTrace.Session.ID)
			}
			if _, _, err := contracts.ParseModelSelection(providerID + "/" + modelID); err != nil {
				return providerID, modelID, fmt.Errorf("trace session %s contains an invalid provider/model identity: %w", sessionTrace.Session.ID, err)
			}
			observations++
			observedProvider, observedModel = providerID, modelID
			if providerID != expectedProvider || (modelID != expectedModel && modelID != expected) {
				return providerID, modelID, fmt.Errorf("trace session %s used %s/%s, expected %s", sessionTrace.Session.ID, providerID, modelID, expected)
			}
		}
	}
	if observations == 0 {
		return "", "", fmt.Errorf("trace tree contains no observed provider/model identity")
	}
	return observedProvider, observedModel, nil
}

func (e *Engine) RunCases(ctx context.Context, suite string, testCases []contracts.Case, variant string, repetitions int) (ContractResult, error) {
	if repetitions < 1 || repetitions > contracts.MaxRuns {
		return ContractResult{}, fmt.Errorf("repetitions must be between 1 and %d", contracts.MaxRuns)
	}
	result := ContractResult{Suite: suite, Started: e.config.Clock(), Complete: true}
	for _, testCase := range testCases {
		if suite != "" && testCase.Suite != suite {
			continue
		}
		for repetition := 1; repetition <= repetitions; repetition++ {
			sample, err := e.Run(ctx, testCase, RunRequest{Variant: variant, Repetition: repetition})
			if err != nil {
				return result, err
			}
			result.Samples = append(result.Samples, sample)
			if sample.Status == contracts.RunStatusAborted || sample.Status == contracts.RunStatusBudgetExhausted || sample.Status == contracts.RunStatusInfraError {
				result.Complete = false
			}
		}
	}
	result.Ended = e.config.Clock()
	if len(result.Samples) == 0 {
		return result, fmt.Errorf("suite %q selected no cases", suite)
	}
	return result, nil
}

func (e *Engine) baseResult(testCase contracts.Case, request RunRequest, runID, caseDigest string) contracts.RunResult {
	provider, _, _ := contracts.ParseModelSelection(testCase.Agent.Model)
	provenance := contracts.Provenance{
		GitSHA: e.config.Provenance.GitSHA, CaseDigest: caseDigest,
		PromptDigest: e.config.Provenance.PromptDigest, ConfigDigest: e.config.Provenance.ConfigDigest,
		FixtureDigest:   testCase.Fixture.ExpectedDigest,
		OpenCodeVersion: e.config.Provenance.OpenCodeVersion,
		Model:           testCase.Agent.Model, Provider: provider,
		ToolsetDigest:      e.config.Provenance.ToolsetDigest,
		JudgeDigest:        e.config.Provenance.JudgeDigest,
		PricingTableDigest: e.pricingDigest,
		ExecutionMode:      testCase.Security.ExecutionMode, Network: testCase.Security.Network,
		Host: contracts.HostProvenance{OS: runtime.GOOS, Arch: runtime.GOARCH, Runtime: runtime.Version()},
		Extensions: map[string]string{
			provenanceExtensionAgentBundleDigest:         e.config.Provenance.BundleDigest,
			provenanceExtensionHarnessBundleDigest:       e.config.Provenance.HarnessDigest,
			provenanceExtensionManifestDigest:            e.config.Provenance.ManifestDigest,
			ProvenanceExtensionEffectiveToolchainsDigest: e.config.Provenance.ToolchainsDigest,
		},
	}
	return contracts.RunResult{
		SchemaVersion: contracts.ResultSchemaVersion, RunID: runID,
		CaseID: testCase.ID, Variant: request.Variant, Repetition: request.Repetition,
		Status: contracts.RunStatusInfraError, Provenance: provenance,
		Checks: []contracts.CheckResult{}, Evidence: contracts.Evidence{Items: []contracts.EvidenceItem{}},
		Error: &contracts.RunError{Kind: "not_started", Message: "run did not start"},
	}
}

func (e *Engine) applyRuntimeInfo(result *contracts.RunResult, info RuntimeInfo) {
	if info.OpenCodeVersion != "" {
		result.Provenance.OpenCodeVersion = info.OpenCodeVersion
	}
	result.Provenance.OpenCodeAPIDigest = info.OpenCodeAPI
	if info.ConfigDigest != "" {
		result.Provenance.ConfigDigest = info.ConfigDigest
	}
	result.Provenance.Extensions[provenanceExtensionEffectiveConfigDigest] = info.ConfigDigest
	result.Provenance.Extensions[provenanceExtensionEffectiveAgentsDigest] = info.AgentsDigest
	if info.ToolsetDigest != "" {
		result.Provenance.ToolsetDigest = info.ToolsetDigest
	} else if info.ToolPolicyDigest != "" {
		result.Provenance.ToolsetDigest = info.ToolPolicyDigest
	}
	if info.ToolPolicyDigest != "" {
		result.Provenance.Extensions[provenanceExtensionEffectiveToolPolicyDigest] = info.ToolPolicyDigest
	}
	if info.ToolCatalogDigest != "" {
		result.Provenance.Extensions[provenanceExtensionEffectiveToolCatalog] = info.ToolCatalogDigest
		delete(result.Provenance.Extensions, provenanceExtensionEffectiveToolStatus)
	}
	if info.ProviderCatalogDigest != "" {
		result.Provenance.Extensions[provenanceExtensionEffectiveProviderCatalog] = info.ProviderCatalogDigest
	}
	if info.ProviderAuthMode != "" {
		result.Provenance.Extensions[contracts.ProvenanceExtensionProviderAuthMode] = info.ProviderAuthMode
	}
	if info.BillingMode != "" {
		result.Provenance.Extensions[contracts.ProvenanceExtensionBillingMode] = info.BillingMode
	}
	if info.CredentialBoundary != "" {
		result.Provenance.Extensions[contracts.ProvenanceExtensionCredentialBoundary] = info.CredentialBoundary
	}
	if info.AuthIsolation != "" {
		result.Provenance.Extensions[contracts.ProvenanceExtensionAuthIsolation] = info.AuthIsolation
	}
	result.Provenance.ExecutionMode = info.ExecutionMode
	result.Provenance.Network = info.Network
}

func (e *Engine) earlyResult(result contracts.RunResult, started time.Time, status contracts.RunStatus, kind string, cause error) (contracts.RunResult, error) {
	result.Status = status
	result.Error = runError(kind, cause, status == contracts.RunStatusInfraError)
	result.Timing.WallMS = nonNegativeMilliseconds(e.config.Clock().Sub(started))
	sanitizePersistableRunResult(&result)
	if err := result.Validate(); err != nil {
		return result, fmt.Errorf("constructed early run result is invalid: %w", err)
	}
	return result, nil
}

// earlyResultForContext preserves the public run taxonomy when an operation
// returns because the caller canceled the run or its contractual time budget
// expired. Those outcomes are neither product failures nor infrastructure
// failures, even when a lower layer wraps the context error.
func (e *Engine) earlyResultForContext(ctx context.Context, result contracts.RunResult, started time.Time, fallback contracts.RunStatus, kind string, cause error) (contracts.RunResult, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return e.earlyResult(result, started, contracts.RunStatusAborted, "canceled", ctx.Err())
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return e.earlyResult(result, started, contracts.RunStatusBudgetExhausted, "timeout", ctx.Err())
	}
	return e.earlyResult(result, started, fallback, kind, cause)
}

func absoluteDirectory(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("directory is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", abs)
	}
	return filepath.Clean(abs), nil
}

func resolveBelow(root, relative string) (string, error) {
	if err := contracts.ValidateRelativePath(relative); err != nil {
		return "", err
	}
	resolved := filepath.Join(root, filepath.FromSlash(relative))
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes root")
	}
	return resolved, nil
}

func randomRunID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return "run_" + hex.EncodeToString(bytes[:]), nil
}

func nonNegativeMilliseconds(duration time.Duration) int64 {
	if duration < 0 {
		return 0
	}
	return duration.Milliseconds()
}

func evidenceItem(id, kind, digest string, complete bool) contracts.EvidenceItem {
	return contracts.EvidenceItem{ID: id, Kind: kind, Source: contracts.EvidenceEvaluator, Digest: digest, Complete: complete}
}

func runError(kind string, err error, retryable bool, evidenceIDs ...string) *contracts.RunError {
	message := "unknown error"
	if err != nil {
		message = err.Error()
	}
	return &contracts.RunError{Kind: kind, Message: message, Retryable: retryable, EvidenceIDs: evidenceIDs}
}

// sanitizePersistableRunResult is the final trust boundary before a result can
// leave the engine. Candidate-controlled prose, tool output, runtime errors and
// filesystem names may contain a complete secret or an arbitrary substring of
// one, which no pattern-based redactor can reliably recognize. Persisted
// results therefore retain structured status/lineage/digests while replacing
// free-form diagnostics with bounded evaluator-authored summaries.
func sanitizePersistableRunResult(result *contracts.RunResult) {
	if result == nil {
		return
	}
	for index := range result.Checks {
		check := &result.Checks[index]
		switch check.Status {
		case contracts.CheckStatusPass:
			check.Summary = "deterministic check passed"
		case contracts.CheckStatusFail:
			check.Summary = "deterministic check failed"
		case contracts.CheckStatusSkipped:
			check.Summary = "deterministic check skipped"
		default:
			check.Summary = "deterministic check evidence is invalid"
		}
		if check.Error != nil {
			check.Error.Message = "deterministic check error details withheld"
		}
	}
	for index := range result.Evidence.Items {
		result.Evidence.Items[index].Path = ""
		result.Evidence.Items[index].Summary = ""
	}
	if result.Error != nil {
		result.Error.Message = fmt.Sprintf("run ended with status %s during %s", result.Status, result.Error.Kind)
	}
}

func summarizedRunError(result contracts.RunResult, errs ...error) *contracts.RunError {
	joined := errors.Join(errs...)
	if joined != nil {
		if code := evaluationCode(joined); code != "" {
			return runError(code, newCodedEvaluationError(evaluationErrorCode(code)), false)
		}
		return runError("evaluation", joined, false)
	}
	for _, check := range result.Checks {
		if check.Hard && check.Status != contracts.CheckStatusPass {
			return &contracts.RunError{Kind: "hard_check", Message: check.Summary, EvidenceIDs: append([]string(nil), check.EvidenceIDs...)}
		}
	}
	return &contracts.RunError{Kind: "evaluation", Message: "run did not pass deterministic gates"}
}

func executeConversation(ctx context.Context, api Runtime, sessionID string, testCase contracts.Case) (*client.Response, error) {
	tools := api.PromptTools()
	if len(tools) == 0 {
		return nil, fmt.Errorf("runtime did not provide a fail-closed prompt tool map")
	}
	providerID, modelID, err := contracts.ParseModelSelection(testCase.Agent.Model)
	if err != nil {
		return nil, fmt.Errorf("agent model: %w", err)
	}
	model := &client.ModelSelection{ProviderID: providerID, ModelID: modelID}
	send := func(text string) (*client.Response, error) {
		messageID, messageIDErr := randomMessageID()
		if messageIDErr != nil {
			return nil, fmt.Errorf("create message id: %w", messageIDErr)
		}
		response, sendErr := api.SendMessageWithRequestContext(ctx, sessionID, client.SendMessageRequest{
			MessageID: messageID, Agent: testCase.Agent.Name, Model: model, Tools: tools,
			Parts: []client.Part{{Type: "text", Text: text}},
		})
		if sendErr != nil {
			return nil, newCodedEvaluationError(evaluationErrorPostResponseInvalid)
		}
		if responseErr := validatePostedResponse(response, sessionID, messageID, providerID, modelID); responseErr != nil {
			return response, responseErr
		}
		return response, nil
	}
	response, err := send(testCase.Input)
	if err != nil {
		return response, err
	}
	turn := 0
	for turn < len(testCase.Turns) && turn+1 < testCase.Completion.MaxTurns && responseLooksLikeQuestion(response) {
		response, err = send(testCase.Turns[turn].Answer)
		if err != nil {
			return response, err
		}
		turn++
	}
	if responseLooksLikeQuestion(response) {
		switch testCase.Completion.UnexpectedQuestion {
		case contracts.UnexpectedQuestionFail:
			return response, fmt.Errorf("agent asked an unexpected question")
		case contracts.UnexpectedQuestionContinue:
			if turn+1 < testCase.Completion.MaxTurns {
				return send("Use the safest reversible default consistent with the stated scope, then verify it.")
			}
		}
	}
	return response, nil
}

func responseLooksLikeQuestion(response *client.Response) bool {
	if response == nil {
		return false
	}
	text := strings.TrimSpace(client.ExtractText(response.Parts))
	return strings.HasSuffix(text, "?")
}

func validatePostedResponse(response *client.Response, sessionID, parentID, providerID, modelID string) error {
	if response == nil {
		return newCodedEvaluationError(evaluationErrorPostResponseInvalid)
	}
	info := response.Info
	if info.Role != "assistant" {
		return newCodedEvaluationError(evaluationErrorPostResponseInvalid)
	}
	if info.ID == "" {
		return newCodedEvaluationError(evaluationErrorPostResponseInvalid)
	}
	if info.SessionID != sessionID {
		return newCodedEvaluationError(evaluationErrorPostResponseInvalid)
	}
	if info.ParentID == "" || info.ParentID != parentID {
		return newCodedEvaluationError(evaluationErrorPostResponseInvalid)
	}
	if info.ProviderID != providerID {
		return newCodedEvaluationError(evaluationErrorPostResponseInvalid)
	}
	if info.ModelID != modelID {
		return newCodedEvaluationError(evaluationErrorPostResponseInvalid)
	}
	if info.Error != nil {
		return newCodedEvaluationError(evaluationErrorPostResponseInvalid)
	}
	if info.Finish != "stop" || info.Time.Completed == 0 {
		return newCodedEvaluationError(evaluationErrorPostResponseInvalid)
	}
	for _, part := range response.Parts {
		if part.ID == "" || part.Type == "" || part.SessionID != sessionID || part.MessageID != info.ID {
			return newCodedEvaluationError(evaluationErrorPostResponseInvalid)
		}
	}
	return nil
}

type durableMessageAPI interface {
	GetMessageContext(context.Context, string, string) (*client.Message, error)
}

type durableResponseAnchorState uint8

const (
	durableResponseAnchorNotAttempted durableResponseAnchorState = iota
	durableResponseAnchorValid
	durableResponseAnchorAbsent
	durableResponseAnchorGetFailed
	durableResponseAnchorInvalid
)

func waitForDurableResponse(ctx context.Context, api durableMessageAPI, sessionID string, response *client.Response) durableResponseAnchorState {
	if ctx == nil || api == nil || response == nil || response.Info.ID == "" {
		return durableResponseAnchorInvalid
	}
	seenNotFound := false
	for {
		message, err := api.GetMessageContext(ctx, sessionID, response.Info.ID)
		if err != nil {
			if isDirectedMessageNotFound(err) {
				seenNotFound = true
			} else if seenNotFound && ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return durableResponseAnchorAbsent
			} else {
				return durableResponseAnchorGetFailed
			}
		} else {
			if directedDurableResponseValid(message, sessionID, response) {
				return durableResponseAnchorValid
			}
			return durableResponseAnchorInvalid
		}
		timer := time.NewTimer(durableResponsePollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return durableResponseAnchorAbsent
		case <-timer.C:
		}
	}
}

func isDirectedMessageNotFound(err error) bool {
	var httpErr *client.HTTPError
	return errors.As(err, &httpErr) && httpErr != nil && httpErr.StatusCode == http.StatusNotFound
}

func durableResponseAnchorError(state durableResponseAnchorState) error {
	switch state {
	case durableResponseAnchorValid, durableResponseAnchorNotAttempted:
		return nil
	case durableResponseAnchorAbsent:
		return newCodedEvaluationError(evaluationErrorDurableResponseMissing)
	case durableResponseAnchorGetFailed:
		return newCodedEvaluationError(evaluationErrorDurableResponseGetFailed)
	default:
		return newCodedEvaluationError(evaluationErrorDurableResponseInvalid)
	}
}

func directedDurableResponseValid(message *client.Message, sessionID string, response *client.Response) bool {
	if message == nil || response == nil {
		return false
	}
	return durableAssistantMatches(*message, sessionID, response)
}

type durableResponseState uint8

const (
	durableResponseAbsent durableResponseState = iota
	durableResponseValid
	durableResponseInvalid
)

func durableResponseStatus(messages []client.Message, sessionID string, response *client.Response) durableResponseState {
	if response == nil {
		return durableResponseInvalid
	}
	parentFound := false
	for _, message := range messages {
		if message.Info.ID != response.Info.ParentID {
			continue
		}
		if message.Info.Role != "user" || message.Info.SessionID != sessionID {
			return durableResponseInvalid
		}
		parentFound = true
		break
	}
	for _, message := range messages {
		if message.Info.ID != response.Info.ID {
			continue
		}
		if !durableAssistantMatches(message, sessionID, response) {
			return durableResponseInvalid
		}
		if !parentFound {
			return durableResponseInvalid
		}
		return durableResponseValid
	}
	return durableResponseAbsent
}

func durableAssistantMatches(message client.Message, sessionID string, response *client.Response) bool {
	if response == nil || message.Info.ID != response.Info.ID {
		return false
	}
	info := message.Info
	if info.Role != "assistant" || info.SessionID != sessionID ||
		info.ParentID == "" || info.ParentID != response.Info.ParentID ||
		info.ProviderID != response.Info.ProviderID || info.ModelID != response.Info.ModelID ||
		info.Error != nil || info.Finish != response.Info.Finish || info.Finish != "stop" || info.Time.Completed == 0 {
		return false
	}
	for _, part := range message.Parts {
		if part.ID == "" || part.Type == "" || part.SessionID != sessionID || part.MessageID != info.ID {
			return false
		}
	}
	return sameStablePartIdentities(response.Parts, message.Parts)
}

type stablePartIdentity struct {
	ID        string
	Type      string
	SessionID string
	MessageID string
}

func sameStablePartIdentities(posted, durable []client.Part) bool {
	left := stablePartIdentities(posted)
	right := stablePartIdentities(durable)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func stablePartIdentities(parts []client.Part) []stablePartIdentity {
	result := make([]stablePartIdentity, 0, len(parts))
	for _, part := range parts {
		result = append(result, stablePartIdentity{
			ID: part.ID, Type: part.Type, SessionID: part.SessionID, MessageID: part.MessageID,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ID != result[j].ID {
			return result[i].ID < result[j].ID
		}
		if result[i].Type != result[j].Type {
			return result[i].Type < result[j].Type
		}
		if result[i].SessionID != result[j].SessionID {
			return result[i].SessionID < result[j].SessionID
		}
		return result[i].MessageID < result[j].MessageID
	})
	return result
}

func durableResponsePresent(messages []client.Message, sessionID string, response *client.Response) bool {
	// Compare only the durable envelope and part ownership. Text is consumed
	// from the durable message below, never from the synchronous response.
	return durableResponseStatus(messages, sessionID, response) == durableResponseValid
}

func validateDurableResponse(collected *trace.Trace, sessionID string, response *client.Response, conversationErr error) error {
	if hasResponseContractError(conversationErr) || (conversationErr != nil && response == nil) {
		return nil
	}
	if collected == nil || response == nil {
		return newCodedEvaluationError(evaluationErrorMessageListGetFailed)
	}
	for _, session := range collected.Sessions {
		if session.Session.ID != sessionID {
			continue
		}
		switch session.MessageCollection {
		case trace.MessageCollectionFailed:
			return newCodedEvaluationError(evaluationErrorMessageListGetFailed)
		case trace.MessageCollectionEmpty:
			return newCodedEvaluationError(evaluationErrorMessageListEmpty)
		case trace.MessageCollectionInvalid:
			return newCodedEvaluationError(evaluationErrorMessageListInvalid)
		case trace.MessageCollectionComplete:
			// Continue with the independently reconciled envelope below.
		default:
			return newCodedEvaluationError(evaluationErrorMessageListGetFailed)
		}
		switch durableResponseStatus(session.Messages, sessionID, response) {
		case durableResponseValid:
			return nil
		case durableResponseAbsent:
			return newCodedEvaluationError(evaluationErrorMessageListInconsistent)
		default:
			return newCodedEvaluationError(evaluationErrorMessageListInvalid)
		}
	}
	return newCodedEvaluationError(evaluationErrorMessageListGetFailed)
}

func durableFinalResponseText(collected *trace.Trace, sessionID string, response *client.Response, conversationErr error) string {
	if hasResponseContractError(conversationErr) || collected == nil || response == nil {
		return ""
	}
	for _, session := range collected.Sessions {
		if session.Session.ID != sessionID {
			continue
		}
		if !durableResponsePresent(session.Messages, sessionID, response) {
			return ""
		}
		for _, message := range session.Messages {
			if message.Info.ID != response.Info.ID {
				continue
			}
			return client.ExtractText(message.Parts)
		}
	}
	return ""
}

func evaluationCode(err error) string {
	var coded *codedEvaluationError
	if errors.As(err, &coded) && coded != nil {
		return string(coded.code)
	}
	return ""
}

func hasResponseContractError(err error) bool {
	code := evaluationCode(err)
	switch evaluationErrorCode(code) {
	case evaluationErrorPostResponseInvalid,
		evaluationErrorDurableResponseMissing,
		evaluationErrorDurableResponseGetFailed,
		evaluationErrorDurableResponseInvalid,
		evaluationErrorMessageListGetFailed,
		evaluationErrorMessageListEmpty,
		evaluationErrorMessageListInvalid,
		evaluationErrorMessageListInconsistent:
		return true
	default:
		return false
	}
}

func traceOptionsForCase(options trace.Options, quiescence contracts.QuiescenceConfig) trace.Options {
	if options.StablePasses <= 0 {
		options.StablePasses = 2
	}
	if options.PollInterval <= 0 {
		options.PollInterval, _ = time.ParseDuration(quiescence.QuietPeriod)
	}
	if options.MaxPasses <= 0 {
		timeout, _ := time.ParseDuration(quiescence.Timeout)
		intervals := int(timeout / options.PollInterval)
		if timeout%options.PollInterval != 0 {
			intervals++
		}
		options.MaxPasses = intervals + 1
		if options.MaxPasses < options.StablePasses {
			options.MaxPasses = options.StablePasses
		}
	}
	return options
}

func randomMessageID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return "msg_" + hex.EncodeToString(bytes[:]), nil
}

func runnerConfig(testCase contracts.Case, closure *ExecutableClosure) sandbox.RunnerConfig {
	config := sandbox.DefaultRunnerConfig()
	config.AllowedExecutables = append([]string(nil), testCase.Security.AllowedExecutables...)
	if testCase.Fixture.InitialGit {
		seenGit := false
		for _, declaration := range config.AllowedExecutables {
			seenGit = seenGit || declaration == "git"
		}
		if !seenGit {
			config.AllowedExecutables = append(config.AllowedExecutables, "git")
		}
	}
	if closure != nil {
		config.ExecutablePaths = make(map[string]string, len(config.AllowedExecutables)*2)
		seen := make(map[string]struct{}, len(config.AllowedExecutables)*2)
		for _, declaration := range config.AllowedExecutables {
			seen[declaration] = struct{}{}
		}
		for _, declaration := range append([]string(nil), config.AllowedExecutables...) {
			resolved, err := closure.PathFor(declaration)
			if err == nil {
				config.ExecutablePaths[declaration] = resolved
				if _, exists := seen[resolved]; !exists {
					config.AllowedExecutables = append(config.AllowedExecutables, resolved)
					seen[resolved] = struct{}{}
				}
				config.ExecutablePaths[resolved] = resolved
			}
		}
	}
	envNames := make(map[string]struct{})
	for _, command := range append(append([]contracts.Command(nil), testCase.Setup.Commands...), testCase.Oracle.Commands...) {
		for key := range command.Env {
			envNames[key] = struct{}{}
		}
	}
	for key := range envNames {
		config.AllowedEnv = append(config.AllowedEnv, key)
	}
	sort.Strings(config.AllowedEnv)
	return config
}

func mapCommands(commands []contracts.Command, closures ...*ExecutableClosure) []sandbox.Command {
	var closure *ExecutableClosure
	if len(closures) != 0 {
		closure = closures[0]
	}
	result := make([]sandbox.Command, 0, len(commands))
	for _, command := range commands {
		timeout, _ := time.ParseDuration(command.Timeout)
		argv := append([]string(nil), command.Argv...)
		if closure != nil && len(argv) != 0 {
			if executable, err := closure.PathFor(argv[0]); err == nil {
				argv[0] = executable
			}
		}
		result = append(result, sandbox.Command{
			ID: command.ID, Argv: argv, Dir: command.Cwd,
			Env: cloneStrings(command.Env), Timeout: timeout,
			ExpectedExit: append([]int(nil), command.ExpectedExit...), MaxOutputBytes: command.MaxOutputBytes,
		})
	}
	return result
}

func (e *Engine) verifyExecutableClosure() error {
	if e.config.ExecutableClosure == nil {
		return nil
	}
	if err := e.config.ExecutableClosure.Revalidate(); err != nil {
		return fmt.Errorf("effective executable closure: %w", err)
	}
	return nil
}

func (e *Engine) verifyExecutableClosureForCase(testCase contracts.Case) error {
	if e.config.ExecutableClosure == nil {
		return nil
	}
	if err := e.config.ExecutableClosure.validateCaseCoverage(testCase); err != nil {
		return fmt.Errorf("effective executable closure coverage: %w", err)
	}
	return e.verifyExecutableClosure()
}

func mapGitSeed(seed contracts.GitSeed) sandbox.GitSeed {
	mapFiles := func(files []contracts.GitSeedFile) []sandbox.SeedFile {
		result := make([]sandbox.SeedFile, 0, len(files))
		for _, file := range files {
			var content []byte
			if file.Content != nil {
				content = []byte(*file.Content)
			}
			mode := os.FileMode(0)
			if file.Mode == "0644" {
				mode = 0o644
			} else if file.Mode == "0755" {
				mode = 0o755
			}
			result = append(result, sandbox.SeedFile{Path: file.Path, Content: content, Digest: file.Digest, Mode: mode})
		}
		return result
	}
	return sandbox.GitSeed{
		Tracked: mapFiles(seed.Tracked), Staged: mapFiles(seed.Staged),
		Untracked: mapFiles(seed.Untracked), Ignored: mapFiles(seed.Ignored),
	}
}

func cloneStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneBoolMap(values map[string]bool) map[string]bool {
	if values == nil {
		return nil
	}
	result := make(map[string]bool, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func (e *Engine) processTrace(runID string, collected *trace.Trace, retain bool) (string, string, string, error) {
	if collected == nil {
		return "", "", "", nil
	}
	encoded, err := contracts.CanonicalJSON(collected)
	if err != nil {
		return "", "", "", err
	}
	sanitized, findings, err := redact.New(int(e.config.SnapshotLimits.MaxTotalBytes)).JSON(encoded)
	if err != nil {
		return "", "", "", err
	}
	digest, err := contracts.CanonicalDigest(jsonRaw(sanitized))
	if err != nil {
		return "", "", "", err
	}
	summary := findingsSummary(findings)
	if !retain {
		return digest, "", summary, nil
	}
	if e.config.TraceDir == "" {
		return "", "", summary, errors.New("trace retention requested without a trace directory")
	}
	traceDir, err := filepath.Abs(e.config.TraceDir)
	if err != nil {
		return "", "", summary, err
	}
	path := filepath.Join(traceDir, runID+".trace.json")
	if err := baseline.SaveJSON(path, jsonRaw(sanitized), baseline.IOOptions{}); err != nil {
		return "", "", summary, err
	}
	return digest, path, summary, nil
}

type jsonRaw []byte

func (r jsonRaw) MarshalJSON() ([]byte, error) { return append([]byte(nil), r...), nil }

func findingsSummary(findings []redact.Finding) string {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		parts = append(parts, fmt.Sprintf("%s:%d", finding.Kind, finding.Count))
	}
	return strings.Join(parts, ",")
}
