package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/experiment"
	"github.com/joeldevz/skynex/internal/eval/lifecycle"
	"github.com/joeldevz/skynex/internal/eval/runner"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
)

const (
	skynexOrchestratorCanaryProfile           = "skynex-orchestrator-canary-v1"
	skynexOrchestratorCanarySuite             = "skynex-orchestrator"
	skynexOrchestratorCanaryKind              = "skynex-orchestrator-canary"
	skynexOrchestratorCanaryPublicCaseCount   = 19
	skynexOrchestratorCanaryPublicCasesDigest = "sha256:f15ce851e7a999e5bd48a611b28c1dd3152effde734a2d22608d42192a243ccb"
	skynexOrchestratorCanaryFixtureSetDigest  = "sha256:31ec1f94f234a52d365227feb9b00659a8cd7c94194e2b080d953bc38234879a"
)

var skynexOrchestratorCanaryCaseIDs = []string{
	"skx_compaction",
	"skx_low_direct",
	"skx_no_workflow",
}

type skynexOrchestratorCanaryCasePin struct {
	CaseDigest    string
	FixtureDigest string
}

func skynexOrchestratorCanaryCasePinFor(id string) (skynexOrchestratorCanaryCasePin, bool) {
	switch id {
	case "skx_compaction":
		return skynexOrchestratorCanaryCasePin{
			CaseDigest:    "sha256:7bd302eb91b653e43a5c782f2bd2cb9274d39dd9571dc9f2a1dda194e9efd169",
			FixtureDigest: "sha256:0473cd1d2357ff6b75b8c5ba73280a55540ebea53cb6683b335c59a6f593687b",
		}, true
	case "skx_low_direct":
		return skynexOrchestratorCanaryCasePin{
			CaseDigest:    "sha256:d16ac8fa29d84bdfa9fdeb65a0b941bb33422e6176276fd53a902da5d54046ed",
			FixtureDigest: "sha256:6f2a3154b848016b768f415e55aecac00fdbb63ac3c226f3246c7146d616641e",
		}, true
	case "skx_no_workflow":
		return skynexOrchestratorCanaryCasePin{
			CaseDigest:    "sha256:699075c68ff3a90bc9d15311d60cf74d0f2e9049f099998fc11b7a9f6bbe5fb5",
			FixtureDigest: "sha256:0473cd1d2357ff6b75b8c5ba73280a55540ebea53cb6683b335c59a6f593687b",
		}, true
	default:
		return skynexOrchestratorCanaryCasePin{}, false
	}
}

func isSupportedCanaryProfile(profile string) bool {
	return profile == workflowV2CanaryProfile || profile == skynexOrchestratorCanaryProfile
}

func canaryArtifactKind(profile string) (string, bool) {
	switch profile {
	case workflowV2CanaryProfile:
		return workflowV2CanaryKind, true
	case skynexOrchestratorCanaryProfile:
		return skynexOrchestratorCanaryKind, true
	default:
		return "", false
	}
}

func prepareCanaryProfile(
	ctx context.Context,
	profile, manifestPath, oauthPath, binary, workflowPluginPath, output string,
) (preparedCanary, error) {
	switch profile {
	case workflowV2CanaryProfile:
		return prepareWorkflowV2Canary(ctx, manifestPath, oauthPath, binary, workflowPluginPath, output)
	case skynexOrchestratorCanaryProfile:
		return prepareSkynexOrchestratorCanary(ctx, manifestPath, oauthPath, binary, output)
	default:
		return preparedCanary{}, invalidf("invalid_canary_profile", "unsupported canary profile %q", profile)
	}
}

func prepareSkynexOrchestratorCanary(
	ctx context.Context,
	manifestPath, oauthPath, binary, output string,
) (preparedCanary, error) {
	var prepared preparedCanary
	if err := ctx.Err(); err != nil {
		return prepared, err
	}
	manifest, err := experiment.Load(manifestPath)
	if err != nil {
		return prepared, invalidf("invalid_manifest", "%v", err)
	}
	if err := validateSkynexOrchestratorCanaryManifest(manifest); err != nil {
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
	fullSuite, err := loadSelectedCases(casesDir, skynexOrchestratorCanarySuite, "")
	if err != nil {
		return prepared, err
	}
	selected, err := selectSkynexOrchestratorCanaryCases(fullSuite)
	if err != nil {
		return prepared, invalidf("invalid_canary_cases", "%v", err)
	}
	caseDigest, err := publicCaseSetDigest(fullSuite)
	if err != nil || caseDigest != skynexOrchestratorCanaryPublicCasesDigest || manifest.PublicCasesDigest != skynexOrchestratorCanaryPublicCasesDigest {
		return prepared, invalidf("experiment_population", "public skynex-orchestrator cases differ from the frozen profile")
	}
	_, observedFixtureSetDigest, err := validateFixtures(fixturesDir, fullSuite)
	if err != nil || observedFixtureSetDigest != skynexOrchestratorCanaryFixtureSetDigest {
		return prepared, invalidf("experiment_population", "skynex-orchestrator fixture set differs from the frozen profile")
	}
	if strings.Join(declaredCriticalCaseIDs(fullSuite), "\x00") != strings.Join(manifest.CriticalCaseIDs, "\x00") {
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
	if _, err := executableClosure.PathFor("skynex"); err == nil {
		return prepared, invalidf("invalid_toolchain_closure", "standalone canary executable closure must not contain skynex")
	}
	if executableClosure.Digest() != manifest.Execution.ToolchainsDigest {
		return prepared, invalidf("toolchains_mismatch", "effective executable closure differs from the frozen manifest")
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
			Profile: skynexOrchestratorCanaryProfile, Manifest: *manifest, ManifestDigest: manifestDigest,
			Cases: append([]contracts.Case(nil), selected...), CasesDir: casesDir, FixturesDir: fixturesDir,
			Control: control, Candidate: candidate, Frozen: frozen, Plan: plan, OpenCodeBinary: resolvedBinary,
			SkynexBinary: nil, WorkflowPlugin: nil,
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

func validateSkynexOrchestratorCanaryManifest(manifest *experiment.Manifest) error {
	if manifest == nil {
		return errors.New("manifest is required")
	}
	if manifest.Suite != skynexOrchestratorCanarySuite {
		return fmt.Errorf("suite must equal %q", skynexOrchestratorCanarySuite)
	}
	if manifest.Intent != experiment.IntentDevelopment {
		return errors.New("canary intent must be development")
	}
	if manifest.PublicCaseCount != skynexOrchestratorCanaryPublicCaseCount {
		return fmt.Errorf("public_case_count must equal %d", skynexOrchestratorCanaryPublicCaseCount)
	}
	if manifest.Holdout != nil || manifest.HoldoutCaseCount != 0 {
		return errors.New("canary must not contain or consume a holdout")
	}
	if manifest.Runs != 2 {
		return errors.New("canary manifest runs must equal 2; the screening profile consumes only repetition 1")
	}
	if len(manifest.CriticalCaseIDs) != skynexOrchestratorCanaryPublicCaseCount {
		return fmt.Errorf("all %d public cases must be critical", skynexOrchestratorCanaryPublicCaseCount)
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

func selectSkynexOrchestratorCanaryCases(fullSuite []contracts.Case) ([]contracts.Case, error) {
	if len(fullSuite) != skynexOrchestratorCanaryPublicCaseCount {
		return nil, fmt.Errorf("profile requires the complete %d-case public suite", skynexOrchestratorCanaryPublicCaseCount)
	}
	byID := make(map[string]contracts.Case, len(fullSuite))
	for _, testCase := range fullSuite {
		if testCase.Suite != skynexOrchestratorCanarySuite {
			return nil, fmt.Errorf("case %q has a different suite", testCase.ID)
		}
		if !testCase.Critical {
			return nil, fmt.Errorf("case %q must be critical", testCase.ID)
		}
		if testCase.Agent.Name != "skynex-orchestrator" {
			return nil, fmt.Errorf("case %q must use skynex-orchestrator", testCase.ID)
		}
		provider, _, err := contracts.ParseModelSelection(testCase.Agent.Model)
		if err != nil || provider != "openai" {
			return nil, fmt.Errorf("case %q must use an exact OpenAI provider/model", testCase.ID)
		}
		if value, ok := testCase.Extensions["x-visibility"].(string); !ok || value != "public" {
			return nil, fmt.Errorf("case %q must be public", testCase.ID)
		}
		if _, managedWorkflow := testCase.Extensions["x-workflow-driver-v1"]; managedWorkflow {
			return nil, fmt.Errorf("case %q must not declare managed Workflow V2 authority", testCase.ID)
		}
		if _, duplicate := byID[testCase.ID]; duplicate {
			return nil, fmt.Errorf("duplicate public case %q", testCase.ID)
		}
		byID[testCase.ID] = testCase
	}

	selected := make([]contracts.Case, 0, len(skynexOrchestratorCanaryCaseIDs))
	for _, id := range skynexOrchestratorCanaryCaseIDs {
		testCase, exists := byID[id]
		if !exists {
			return nil, fmt.Errorf("required canary case %q is missing", id)
		}
		selected = append(selected, testCase)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	if err := validateSkynexOrchestratorCanarySelectedCases(selected); err != nil {
		return nil, err
	}
	return selected, nil
}

func validateSkynexOrchestratorCanarySelectedCases(selected []contracts.Case) error {
	if len(selected) != len(skynexOrchestratorCanaryCaseIDs) {
		return fmt.Errorf("standalone canary requires exactly %d selected cases", len(skynexOrchestratorCanaryCaseIDs))
	}
	seen := make(map[string]struct{}, len(selected))
	for _, testCase := range selected {
		pin, pinned := skynexOrchestratorCanaryCasePinFor(testCase.ID)
		if !pinned {
			return fmt.Errorf("case %q is not selected by the standalone canary profile", testCase.ID)
		}
		if _, duplicate := seen[testCase.ID]; duplicate {
			return fmt.Errorf("duplicate standalone canary case %q", testCase.ID)
		}
		seen[testCase.ID] = struct{}{}
		if testCase.Suite != skynexOrchestratorCanarySuite {
			return fmt.Errorf("case %q has a different suite", testCase.ID)
		}
		if testCase.Agent.Name != "skynex-orchestrator" {
			return fmt.Errorf("case %q must use skynex-orchestrator", testCase.ID)
		}
		if _, managedWorkflow := testCase.Extensions["x-workflow-driver-v1"]; managedWorkflow {
			return fmt.Errorf("case %q must not declare managed Workflow V2 authority", testCase.ID)
		}
		if testCase.Fixture.ExpectedDigest != pin.FixtureDigest {
			return fmt.Errorf("case %q fixture digest differs from the evaluator-owned profile", testCase.ID)
		}
		caseDigest, err := testCase.Digest()
		if err != nil || caseDigest != pin.CaseDigest {
			return fmt.Errorf("case %q digest differs from the evaluator-owned profile", testCase.ID)
		}
	}
	for _, id := range skynexOrchestratorCanaryCaseIDs {
		if _, exists := seen[id]; !exists {
			return fmt.Errorf("required standalone canary case %q is missing", id)
		}
	}
	return nil
}
