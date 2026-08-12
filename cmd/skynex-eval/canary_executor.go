package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/stats"
	"github.com/joeldevz/skynex/internal/eval/toolpolicy"
)

type canaryModelRunner func(context.Context, modelRunSpec) (modelRunResult, error)

type canaryExecutorDependencies struct {
	runModel        canaryModelRunner
	verifyAuthority func() error
	now             func() time.Time
}

func executeWorkflowV2Canary(ctx context.Context, request canaryExecutionRequest) (canaryExecutionResult, error) {
	return executeWorkflowV2CanaryWithDependencies(ctx, request, canaryExecutorDependencies{
		runModel: executeModelRuns,
		verifyAuthority: func() error {
			return verifyCanaryExecutionAuthority(request)
		},
		now: func() time.Time { return time.Now().UTC() },
	})
}

func executeWorkflowV2CanaryWithDependencies(
	ctx context.Context,
	request canaryExecutionRequest,
	deps canaryExecutorDependencies,
) (result canaryExecutionResult, returnErr error) {
	if ctx == nil {
		return result, errors.New("canary execution context is required")
	}
	if err := validateCanaryExecutionRequest(request); err != nil {
		return result, err
	}
	if deps.runModel == nil || deps.verifyAuthority == nil {
		return result, errors.New("canary executor dependencies are incomplete")
	}
	if deps.now == nil {
		deps.now = func() time.Time { return time.Now().UTC() }
	}
	result.StartedAt = deps.now()
	result.CleanupComplete = true
	defer func() { result.EndedAt = deps.now() }()

	executionCtx, executionCancel := context.WithDeadline(ctx, request.ExecutionDeadline)
	defer executionCancel()
	budgetCtx, budgetCancel := context.WithTimeout(executionCtx, request.SampleBudget)
	defer budgetCancel()

	caseByID := make(map[string]contracts.Case, len(request.Cases))
	for _, testCase := range request.Cases {
		caseByID[testCase.ID] = testCase
	}
	coordinates := flattenCanaryPlan(request.Plan)
	for _, coordinate := range coordinates {
		if err := budgetCtx.Err(); err != nil {
			return result, err
		}
		testCase, exists := caseByID[coordinate.CaseID]
		if !exists {
			return result, fmt.Errorf("canary plan refers to unknown case %q", coordinate.CaseID)
		}
		caseTimeout, err := time.ParseDuration(testCase.Completion.Timeout)
		if err != nil || caseTimeout <= 0 || caseTimeout > request.SampleTimeout {
			return result, fmt.Errorf("case %q has an invalid canary timeout", testCase.ID)
		}
		now := deps.now()
		if !now.Before(request.SchedulingDeadline) || now.Add(caseTimeout).After(request.SchedulingDeadline) {
			return result, context.DeadlineExceeded
		}
		if err := deps.verifyAuthority(); err != nil {
			return result, fmt.Errorf("revalidate canary authority before sample: %w", err)
		}

		bundle := request.Control
		if coordinate.Variant == stats.VariantCandidate {
			bundle = request.Candidate
		}
		resolvedBinary := request.OpenCodeBinary
		spec := modelRunSpec{
			Cases: []contracts.Case{testCase}, Suite: workflowV2CanarySuite,
			Variant: string(coordinate.Variant), Repetitions: 1, RepetitionStart: coordinate.Repetition,
			FixtureRoot: request.FixturesDir, AgentBundleRoot: bundle.AbsoluteRoot,
			Binary: resolvedBinary.Path, ExpectedVersion: request.Manifest.Execution.OpenCodeVersion,
			ExpectedOpenCodeBinaryDigest: request.Manifest.Execution.OpenCodeBinaryDigest,
			ExpectedOpenCodeAPIDigest:    request.Manifest.Execution.OpenCodeOpenAPIDigest,
			ExpectedToolchainsDigest:     request.Manifest.Execution.ToolchainsDigest,
			ResolvedBinary:               &resolvedBinary, ExecutableClosure: request.ExecutableClosure,
			WorkflowPlugin: request.WorkflowPlugin, SkynexBinary: request.SkynexBinary,
			OpenAIOAuthSession: request.OpenAIOAuthSession,
			RetainTrace:        false, AllowImpure: false,
			HarnessDigest: request.Manifest.Harness.Digest, ManifestDigest: request.ManifestDigest,
			VerifiedBundleDigest: bundle.Snapshot.Digest, RequireExactBundle: true,
		}
		sampleCtx, sampleCancel := context.WithTimeout(budgetCtx, caseTimeout)
		run, runErr := deps.runModel(sampleCtx, spec)
		sampleContextErr := sampleCtx.Err()
		sampleCancel()
		postErr := deps.verifyAuthority()
		if runErr != nil {
			// executeModelRuns owns runtime/process/workspace cleanup. An error
			// means it could not attest that complete boundary, so no returned
			// sample is published and cleanup fails closed.
			result.CleanupComplete = false
			if sampleContextErr != nil {
				return result, sampleContextErr
			}
			return result, errors.Join(runErr, postErr)
		}
		if postErr != nil {
			return result, fmt.Errorf("revalidate canary authority after sample: %w", postErr)
		}
		if len(run.Result.Samples) != 1 {
			return result, fmt.Errorf("model runner returned %d samples for one canary coordinate", len(run.Result.Samples))
		}
		sample := run.Result.Samples[0]
		if sample.CaseID != coordinate.CaseID || sample.Variant != string(coordinate.Variant) || sample.Repetition != coordinate.Repetition {
			return result, errors.New("model runner returned a sample outside the committed canary coordinate")
		}
		if err := sample.Validate(); err != nil {
			return result, fmt.Errorf("model runner returned an invalid canary sample: %w", err)
		}
		cleanupAttested := workflowCanarySampleCleanupAttested(sample)
		if !cleanupAttested {
			result.CleanupComplete = false
		}
		if sample.Status == contracts.RunStatusPass && !cleanupAttested {
			return result, errors.New("passing canary sample lacks runtime cleanup attestation")
		}
		result.Samples = append(result.Samples, sample)
		if sampleContextErr != nil {
			return result, sampleContextErr
		}
		if sample.Status != contracts.RunStatusPass {
			return result, nil
		}
	}
	return result, nil
}

func validateCanaryExecutionRequest(request canaryExecutionRequest) error {
	if request.Profile != workflowV2CanaryProfile || request.Manifest.Suite != workflowV2CanarySuite {
		return errors.New("canary executor received an unsupported profile")
	}
	if request.RunsPerArm != workflowV2CanaryRunsPerArm || request.MaximumSampleCount != workflowV2CanaryMaxSamples || !request.FailFast {
		return errors.New("canary executor received mutable population controls")
	}
	if request.RetainTrace || request.AllowAmbientPlugins {
		return errors.New("canary executor forbids trace retention and ambient plugins")
	}
	if len(request.Cases) != workflowV2CanaryCaseCount || len(flattenCanaryPlan(request.Plan)) != workflowV2CanaryMaxSamples {
		return errors.New("canary executor requires exactly six committed coordinates")
	}
	if request.SampleTimeout <= 0 || request.SampleTimeout > workflowV2CanarySampleLimit ||
		request.SampleBudget <= 0 || request.SampleBudget > workflowV2CanarySampleBudget {
		return errors.New("canary executor received an invalid time budget")
	}
	if request.SchedulingDeadline.IsZero() || request.ExecutionDeadline.IsZero() ||
		!request.SchedulingDeadline.Before(request.ExecutionDeadline) ||
		request.ExecutionDeadline.Sub(request.SchedulingDeadline) < request.CleanupReserve {
		return errors.New("canary executor received invalid scheduling/cleanup deadlines")
	}
	if request.Frozen == nil || request.ExecutableClosure == nil || request.SkynexBinary == nil || request.WorkflowPlugin == nil || request.OpenAIOAuthSession == nil {
		return errors.New("canary executor is missing frozen runtime authority")
	}
	if request.Control.AbsoluteRoot == "" || request.Candidate.AbsoluteRoot == "" || request.FixturesDir == "" {
		return errors.New("canary executor is missing frozen bundle roots")
	}
	return nil
}

func verifyCanaryExecutionAuthority(request canaryExecutionRequest) error {
	var result error
	if request.Frozen == nil {
		result = errors.Join(result, errors.New("frozen bundle set is required"))
	} else {
		result = errors.Join(result, request.Frozen.VerifyUnchanged())
	}
	if request.ExecutableClosure == nil {
		result = errors.Join(result, errors.New("executable closure is required"))
	} else {
		result = errors.Join(result, request.ExecutableClosure.Revalidate())
	}
	if request.SkynexBinary == nil {
		result = errors.Join(result, errors.New("skynex executable identity is required"))
	} else {
		result = errors.Join(result, request.SkynexBinary.Revalidate())
	}
	result = errors.Join(result, toolpolicy.VerifyControlledPluginIdentity(request.WorkflowPlugin))
	result = errors.Join(result, request.OpenCodeBinary.Revalidate())
	return result
}
