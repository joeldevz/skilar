package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/experiment"
	"github.com/joeldevz/skynex/internal/eval/gates"
	"github.com/joeldevz/skynex/internal/eval/reporter"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/stats"
)

const publicCasesDigestAggregateKey = "public_cases_digest"

type comparisonCommandResult struct {
	Kind                    string                    `json:"kind"`
	Intent                  string                    `json:"intent"`
	Authority               string                    `json:"authority"`
	ManifestDigest          string                    `json:"manifest_digest"`
	ControlArtifactDigest   string                    `json:"control_artifact_digest"`
	CandidateArtifactDigest string                    `json:"candidate_artifact_digest"`
	Report                  reporter.ComparisonReport `json:"report"`
	Holdout                 *holdoutComparisonSummary `json:"holdout,omitempty"`
	OutputPath              string                    `json:"output_path,omitempty"`
}

type holdoutComparisonSummary struct {
	BundleDigest string               `json:"bundle_digest"`
	Cases        int                  `json:"cases"`
	Control      reliabilityAggregate `json:"control"`
	Candidate    reliabilityAggregate `json:"candidate"`
}

type reliabilityAggregate struct {
	Runs               int                  `json:"runs"`
	Counts             map[stats.Status]int `json:"counts"`
	PassRate           stats.Rate           `json:"pass_rate"`
	FailureRate        stats.Rate           `json:"failure_rate"`
	InvalidRate        stats.Rate           `json:"invalid_rate"`
	InconclusiveRate   stats.Rate           `json:"inconclusive_rate"`
	InfrastructureRate stats.Rate           `json:"infrastructure_rate"`
	FlakyRate          stats.Rate           `json:"flaky_rate"`
}

func (r comparisonCommandResult) CLIExitCode() int { return r.Report.Decision.ExitCode }

func commandCompare(args []string) (comparisonCommandResult, error) {
	set := newFlagSet("compare")
	controlPath := set.String("control", "", "control baseline artifact")
	baselineAlias := set.String("baseline", "", "compatibility alias for --control")
	candidatePath := set.String("candidate", "", "candidate baseline artifact")
	manifestPath := set.String("manifest", "", "required frozen experiment manifest")
	allowedText := set.String("allow-difference", "", "deprecated; intentional differences must be frozen in the manifest")
	output := set.String("output", "", "optional comparison report path")
	if err := parseFlagSet(set, args); err != nil {
		return comparisonCommandResult{}, err
	}
	if *controlPath == "" {
		*controlPath = *baselineAlias
	} else if *baselineAlias != "" && *baselineAlias != *controlPath {
		return comparisonCommandResult{}, invalidf("invalid_arguments", "--control and --baseline identify different files")
	}
	if *controlPath == "" || *candidatePath == "" {
		return comparisonCommandResult{}, invalidf("invalid_arguments", "--control/--baseline and --candidate are required")
	}
	if *manifestPath == "" {
		return comparisonCommandResult{}, invalidf("manifest_required", "--manifest is required; comparisons without a frozen manifest are non-authoritative and cannot pass")
	}
	control, err := baseline.Load(*controlPath, baseline.IOOptions{})
	if err != nil {
		return comparisonCommandResult{}, invalidf("invalid_control_artifact", "%v", err)
	}
	candidate, err := baseline.Load(*candidatePath, baseline.IOOptions{})
	if err != nil {
		return comparisonCommandResult{}, invalidf("invalid_candidate_artifact", "%v", err)
	}
	if control.Suite != candidate.Suite {
		return comparisonCommandResult{}, invalidf("incompatible_suite", "control suite %q differs from candidate suite %q", control.Suite, candidate.Suite)
	}
	controlRuns, err := decodeArtifactRuns(control)
	if err != nil {
		return comparisonCommandResult{}, invalidf("invalid_control_samples", "%v", err)
	}
	candidateRuns, err := decodeArtifactRuns(candidate)
	if err != nil {
		return comparisonCommandResult{}, invalidf("invalid_candidate_samples", "%v", err)
	}
	if err := validateArtifactFingerprintBinding(control, controlRuns); err != nil {
		return comparisonCommandResult{}, invalidf("invalid_control_fingerprint_binding", "%v", err)
	}
	if err := validateArtifactFingerprintBinding(candidate, candidateRuns); err != nil {
		return comparisonCommandResult{}, invalidf("invalid_candidate_fingerprint_binding", "%v", err)
	}

	if strings.TrimSpace(*allowedText) != "" {
		return comparisonCommandResult{}, invalidf("invalid_arguments", "--allow-difference cannot override a frozen manifest")
	}
	loadedManifest, loadErr := experiment.Load(*manifestPath)
	if loadErr != nil {
		return comparisonCommandResult{}, invalidf("invalid_manifest", "%v", loadErr)
	}
	if loadedManifest.Intent == experiment.IntentRelease ||
		loadedManifest.Execution.Mode == string(contracts.ExecutionIsolatedContainer) ||
		loadedManifest.Execution.CredentialBoundary == experiment.CredentialBoundaryProviderProxy {
		return comparisonCommandResult{}, invalidf(
			"release_attestation_unavailable",
			"provider-proxy/container decisions are disabled until that backend emits a verifiable external attestation; self-sealed artifacts are trusted-local development evidence only",
		)
	}
	if loadedManifest.Suite != control.Suite {
		return comparisonCommandResult{}, invalidf("incompatible_suite", "manifest suite %q differs from artifact suite %q", loadedManifest.Suite, control.Suite)
	}
	frozenCases, frozenBundles, frozenCasesErr := loadFrozenPublicCasesForComparison(*loadedManifest, *manifestPath)
	if frozenCasesErr != nil {
		if loadedManifest.Holdout != nil {
			return comparisonCommandResult{}, invalidf("invalid_frozen_bundles", "frozen experiment inputs could not be reverified; holdout diagnostics redacted")
		}
		return comparisonCommandResult{}, invalidf("invalid_frozen_bundles", "%v", frozenCasesErr)
	}
	if err := validateArtifactCaseContracts("control", controlRuns, frozenCases, loadedManifest.ModelAssignment, true); err != nil {
		return comparisonCommandResult{}, invalidf("invalid_control_case_binding", "%v", err)
	}
	if err := validateArtifactCaseContracts("candidate", candidateRuns, frozenCases, loadedManifest.ModelAssignment, false); err != nil {
		return comparisonCommandResult{}, invalidf("invalid_candidate_case_binding", "%v", err)
	}
	manifestDigest, digestErr := contracts.CanonicalDigest(*loadedManifest)
	if digestErr != nil {
		return comparisonCommandResult{}, invalidf("invalid_manifest_digest", "%v", digestErr)
	}
	intentional := append([]baseline.Field(nil), loadedManifest.IntentionalDifferences...)
	thresholds := gates.HardThresholds{
		CriticalCasePassRate: loadedManifest.Gates.CriticalCasePassRate,
		PassToFailMaximum:    loadedManifest.Gates.PassToFailRegressions,
		ScopeViolationMax:    loadedManifest.Gates.ScopeViolations,
		FalseSuccessMax:      loadedManifest.Gates.FalseSuccesses,
	}
	metricRatios := map[string]float64{
		"parent_peak_input_tokens": loadedManifest.Gates.MaxParentPeakInputRatio,
		"tree_sum_input_tokens":    loadedManifest.Gates.MaxTreeInputRatio,
		"tree_cost_usd":            loadedManifest.Gates.MaxCostRatio,
		"wall_time_ms":             loadedManifest.Gates.MaxWallTimeRatio,
		"retry_rate":               loadedManifest.Gates.MaxRetryRateRatio,
	}
	confidence, minimumPairs := loadedManifest.Gates.Confidence, loadedManifest.Gates.MinimumPairs
	if confidence == 0 {
		confidence = 0.95
	}
	if minimumPairs == 0 {
		minimumPairs = 5
	}
	experimentID := loadedManifest.ID

	compatibility := baseline.CompareFingerprints(control.Fingerprint, candidate.Fingerprint, intentional)
	controlOutcomes, _ := stats.OutcomesFromRunResults(controlRuns)
	candidateOutcomes, _ := stats.OutcomesFromRunResults(candidateRuns)
	reliability := map[string]stats.ReliabilitySummary{
		"control":   stats.SummarizeReliability(controlOutcomes),
		"candidate": stats.SummarizeReliability(candidateOutcomes),
	}
	holdoutEvidence, holdoutErr := matchingHoldoutMetadata(control, candidate, loadedManifest.Holdout, loadedManifest.HoldoutCaseCount)
	holdoutHashes := holdoutEvidence.References
	allControlRuns := append(append([]contracts.RunResult(nil), controlRuns...), holdoutEvidence.ControlRuns...)
	allCandidateRuns := append(append([]contracts.RunResult(nil), candidateRuns...), holdoutEvidence.CandidateRuns...)
	var candidateHoldoutOutcomes []stats.Outcome
	var holdoutSummary *holdoutComparisonSummary
	if holdoutErr == nil && loadedManifest.Holdout != nil {
		controlHoldoutOutcomes, _ := stats.OutcomesFromRunResults(holdoutEvidence.ControlRuns)
		candidateHoldoutOutcomes, _ = stats.OutcomesFromRunResults(holdoutEvidence.CandidateRuns)
		controlHoldoutSummary := stats.SummarizeReliability(controlHoldoutOutcomes)
		candidateHoldoutSummary := stats.SummarizeReliability(candidateHoldoutOutcomes)
		holdoutSummary = &holdoutComparisonSummary{
			BundleDigest: loadedManifest.Holdout.Digest, Cases: len(holdoutHashes),
			Control: aggregateReliability(controlHoldoutSummary), Candidate: aggregateReliability(candidateHoldoutSummary),
		}
	}
	criticalIDs, criticalErr := matchingCriticalCases(control, candidate)
	if criticalErr == nil {
		criticalErr = validateCriticalCoverage(criticalIDs, controlOutcomes, candidateOutcomes)
	}
	criticalOutcomes := filterOutcomes(candidateOutcomes, criticalIDs)
	passRate := stats.SummarizeReliability(criticalOutcomes).PassRate
	var passRateValue *float64
	if passRate.Available {
		value := passRate.Value
		passRateValue = &value
	}
	passToFail := countPassToFail(allControlRuns, allCandidateRuns)
	scopeViolations := countFailedChecks(candidateRuns, "scope")
	falseSuccesses := countFailedChecks(candidateRuns, "false_success")
	hardEvidence := gates.HardEvidence{
		CriticalCasePassRate: passRateValue, PassToFail: &passToFail,
		ScopeViolations: &scopeViolations, FalseSuccesses: &falseSuccesses,
	}
	gateResults := []gates.Result{
		gates.EvaluateCompatibility(compatibility),
		treatmentRealizedGate(compatibility, intentional),
		evidenceValidityGate("control_evidence", allControlRuns),
		evidenceValidityGate("candidate_evidence", allCandidateRuns),
	}
	gateResults = append(gateResults, manifestConformanceGate(*loadedManifest, control, candidate))
	gateResults = append(gateResults, experimentPopulationGate(allControlRuns, allCandidateRuns, loadedManifest.Runs, loadedManifest.PublicCaseCount+loadedManifest.HoldoutCaseCount))
	if loadedManifest.Holdout != nil && holdoutErr == nil {
		gateResults = append(gateResults, holdoutPassRateGate(candidateHoldoutOutcomes))
	}
	if criticalErr != nil {
		gateResults = append(gateResults, gates.Result{Name: "critical_case_metadata", Status: gates.StatusInvalid, Reason: gates.ReasonEvidenceInvalid, Detail: criticalErr.Error()})
	} else {
		gateResults = append(gateResults, gates.Result{Name: "critical_case_metadata", Status: gates.StatusPass, Reason: gates.ReasonGateSatisfied})
	}
	gateResults = append(gateResults, gates.EvaluateHardGates(thresholds, hardEvidence)...)

	subscriptionBilling := loadedManifest.Execution.BillingMode == experiment.BillingModeChatGPTSubscription
	treeCostExtractor := preferredTreeCost
	if subscriptionBilling {
		// A ChatGPT subscription does not expose authoritative per-request USD.
		// Keep any calculated API-price estimate in the source evidence as an
		// explicitly counterfactual field, but never project it into a metric
		// named tree_cost_usd.
		treeCostExtractor = func(contracts.RunResult) (float64, bool) { return 0, false }
	}
	metricDefinitions := []metricDefinition{
		{name: "parent_peak_input_tokens", unit: "tokens", extractor: stats.MetricParentPeakInput, requireTelemetry: true, scope: stats.ScopeSuccessful},
		{name: "tree_sum_input_tokens", unit: "tokens", extractor: stats.MetricTreeInput, requireTelemetry: true, scope: stats.ScopeSuccessful},
		{name: "tree_cost_usd", unit: "USD", extractor: treeCostExtractor, requireTelemetry: true, scope: stats.ScopeSuccessful},
		{name: "wall_time_ms", unit: "ms", extractor: stats.MetricWallMS, requireTelemetry: false, scope: stats.ScopeAllCompleted},
		{name: "retry_rate", unit: "retries/run", extractor: stats.MetricRetries, requireTelemetry: true, scope: stats.ScopeAllCompleted, estimator: stats.EstimatorMean},
		// Append new descriptive metrics so bootstrap seeds for the v1 gate
		// metrics above remain stable across evaluator upgrades.
		{name: "parent_first_input_tokens", unit: "tokens", extractor: stats.MetricParentFirstInput, requireTelemetry: true, scope: stats.ScopeSuccessful},
	}
	metricReports := make([]reporter.MetricComparison, 0, len(metricDefinitions))
	for index, definition := range metricDefinitions {
		controlSamples, sampleErr := stats.SamplesFromRunResults(allControlRuns, definition.extractor, definition.requireTelemetry)
		if sampleErr != nil {
			return comparisonCommandResult{}, invalidf("invalid_metric_samples", "%s: %s", definition.name, redactHoldoutText(sampleErr.Error(), allControlRuns, allCandidateRuns, holdoutHashes))
		}
		candidateSamples, sampleErr := stats.SamplesFromRunResults(allCandidateRuns, definition.extractor, definition.requireTelemetry)
		if sampleErr != nil {
			return comparisonCommandResult{}, invalidf("invalid_metric_samples", "%s: %s", definition.name, redactHoldoutText(sampleErr.Error(), allControlRuns, allCandidateRuns, holdoutHashes))
		}
		pairs, reportPairs, pairErr := buildMetricPairs(allControlRuns, allCandidateRuns, definition)
		if pairErr != nil {
			return comparisonCommandResult{}, invalidf("invalid_pairs", "%s: %s", definition.name, redactHoldoutText(pairErr.Error(), allControlRuns, allCandidateRuns, holdoutHashes))
		}
		bootstrap := stats.BootstrapConfig{Confidence: confidence, MinimumPairs: minimumPairs, Iterations: 10_000, Seed: uint64(index + 1)}
		pairedReport := stats.SummarizePaired(reportPairs, bootstrap)
		if definition.estimator == stats.EstimatorMean {
			pairedReport = stats.SummarizePairedMean(reportPairs, bootstrap)
		}
		costNotApplicable := subscriptionBilling && definition.name == "tree_cost_usd"
		if costNotApplicable {
			markSubscriptionCostNotApplicable(&pairedReport)
		}
		metricReport := reporter.MetricComparison{
			Name: definition.name, Unit: definition.unit, Scope: definition.scope,
			Control:   stats.Summarize(controlSamples, stats.SummaryConfig{Scope: definition.scope}),
			Candidate: stats.Summarize(candidateSamples, stats.SummaryConfig{Scope: definition.scope}),
			Paired:    pairedReport,
		}
		if costNotApplicable {
			markSubscriptionCostSummaryNotApplicable(&metricReport.Control)
			markSubscriptionCostSummaryNotApplicable(&metricReport.Candidate)
		}
		if holdoutSummary != nil {
			metricReport.Control.Samples = nil
			metricReport.Control.EligibleValues = nil
			metricReport.Candidate.Samples = nil
			metricReport.Candidate.EligibleValues = nil
			metricReport.Paired.Pairs = nil
		}
		metricReports = append(metricReports, metricReport)
		if ratio, gated := metricRatios[definition.name]; gated {
			if costNotApplicable {
				gateResults = append(gateResults, gates.Result{
					Name:   definition.name,
					Status: gates.StatusNotApplicable,
					Reason: gates.ReasonNotApplicable,
					Detail: "ChatGPT subscription billing has no authoritative per-request USD; max_cost_ratio was not evaluated and calculated API-price estimates are counterfactual only",
				})
			} else {
				gateResults = append(gateResults, gates.EvaluateMetric(gates.MetricRule{
					Name: definition.name, Direction: gates.LowerOrEqual, Ratio: ratio,
					Scope: definition.scope, Estimator: definition.estimator, Bootstrap: bootstrap,
				}, pairs))
			}
		}
	}

	decision := gates.Combine(gateResults...)
	if holdoutSummary != nil {
		redactHoldoutDecision(&decision, append(append([]contracts.RunResult(nil), allControlRuns...), allCandidateRuns...), holdoutHashes)
	}
	report := reporter.ComparisonReport{
		SchemaVersion: 1, ExperimentID: experimentID, Compatibility: compatibility,
		Reliability: reliability, Metrics: metricReports, Decision: decision,
	}
	result := comparisonCommandResult{
		Kind: "skynex-eval-comparison", Intent: loadedManifest.Intent,
		Authority: authorityForIntent(loadedManifest.Intent), ManifestDigest: manifestDigest,
		ControlArtifactDigest: control.Integrity.Digest, CandidateArtifactDigest: candidate.Integrity.Digest,
		Report: report, Holdout: holdoutSummary, OutputPath: *output,
	}
	if *output != "" {
		resolvedOutput, locationErr := validateCompareOutputLocation(
			*output, *manifestPath, *controlPath, *candidatePath, frozenBundles.Bundles,
		)
		if locationErr != nil {
			return comparisonCommandResult{}, invalidf("invalid_output", "%v", locationErr)
		}
		if err := saveCompareOutputNoClobber(resolvedOutput, result); err != nil {
			if errors.Is(err, os.ErrExist) {
				return comparisonCommandResult{}, invalidf("invalid_output", "%v", err)
			}
			return comparisonCommandResult{}, infraf("save_comparison", err)
		}
	}
	return result, nil
}

// treatmentRealizedGate prevents a declared A/B experiment from passing when
// the two artifacts only differ in frozen source material that had no observed
// runtime effect. Compatibility answers whether every observed difference was
// permitted; this gate separately proves that every declared treatment was
// present and that at least one behavior-effective field actually changed.
func treatmentRealizedGate(report baseline.CompatibilityReport, declared []baseline.Field) gates.Result {
	realized := make(map[baseline.Field]bool, len(report.Mismatches))
	behaviorEffective := false
	for _, mismatch := range report.Mismatches {
		if !mismatch.Allowed || mismatch.Control == mismatch.Current {
			continue
		}
		realized[mismatch.Field] = true
		switch mismatch.Field {
		case baseline.FieldEffectiveConfigDigest,
			baseline.FieldEffectiveAgentsDigest,
			baseline.FieldModel,
			baseline.FieldProvider,
			baseline.FieldToolsetDigest,
			baseline.FieldPermissionPolicyDigest:
			behaviorEffective = true
		}
	}

	missing := make([]string, 0)
	for _, field := range declared {
		if !realized[field] {
			missing = append(missing, string(field))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return gates.Result{
			Name: "treatment_realized", Status: gates.StatusInvalid, Reason: gates.ReasonEvidenceInvalid,
			Detail: "declared treatment fields were not observed as real mismatches: " + strings.Join(missing, ", "),
		}
	}
	if !behaviorEffective {
		return gates.Result{
			Name: "treatment_realized", Status: gates.StatusInvalid, Reason: gates.ReasonEvidenceInvalid,
			Detail: "frozen inputs differ, but effective config, agents, model, provider, toolset, and permission policy are unchanged",
		}
	}
	return gates.Result{Name: "treatment_realized", Status: gates.StatusPass, Reason: gates.ReasonGateSatisfied}
}

func loadFrozenPublicCasesForComparison(manifest experiment.Manifest, manifestPath string) ([]contracts.Case, *experiment.FrozenSet, error) {
	manifestDirectory, err := filepath.Abs(filepath.Dir(manifestPath))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve manifest directory: %w", err)
	}
	frozen, err := manifest.VerifyBundles(manifestDirectory, sandbox.DefaultSnapshotLimits())
	if err != nil {
		return nil, nil, err
	}
	harnessRoot := ""
	for _, bundle := range frozen.Bundles {
		if bundle.Name == "harness" {
			harnessRoot = bundle.AbsoluteRoot
			break
		}
	}
	if harnessRoot == "" {
		return nil, nil, fmt.Errorf("verified manifest has no harness bundle")
	}
	casesDirectory, err := frozenBundleDirectory(harnessRoot, "cases")
	if err != nil {
		return nil, nil, fmt.Errorf("frozen harness cases: %w", err)
	}
	publicCases, err := loadSelectedCases(casesDirectory, manifest.Suite, "")
	if err != nil {
		return nil, nil, fmt.Errorf("load frozen public cases: %w", err)
	}
	if len(publicCases) != manifest.PublicCaseCount {
		return nil, nil, fmt.Errorf("frozen public case count is %d, expected %d", len(publicCases), manifest.PublicCaseCount)
	}
	digest, err := publicCaseSetDigest(publicCases)
	if err != nil {
		return nil, nil, fmt.Errorf("digest frozen public cases: %w", err)
	}
	if digest != manifest.PublicCasesDigest {
		return nil, nil, fmt.Errorf("frozen public case catalog does not match manifest")
	}
	if strings.Join(declaredCriticalCaseIDs(publicCases), "\x00") != strings.Join(manifest.CriticalCaseIDs, "\x00") {
		return nil, nil, fmt.Errorf("frozen critical case ids do not match manifest")
	}
	return publicCases, frozen, nil
}

func validateArtifactCaseContracts(label string, runs []contracts.RunResult, publicCases []contracts.Case, assignment *experiment.ModelAssignment, control bool) error {
	type expectedCase struct {
		digest   string
		model    string
		provider string
		fixture  string
	}
	expected := make(map[string]expectedCase, len(publicCases))
	for _, original := range publicCases {
		testCase := original
		if assignment != nil {
			if control {
				testCase.Agent.Model = assignment.Control
			} else {
				testCase.Agent.Model = assignment.Candidate
			}
		}
		digest, err := testCase.Digest()
		if err != nil {
			return fmt.Errorf("%s case %q: %w", label, testCase.ID, err)
		}
		provider, _, err := contracts.ParseModelSelection(testCase.Agent.Model)
		if err != nil {
			return fmt.Errorf("%s case %q model: %w", label, testCase.ID, err)
		}
		expected[testCase.ID] = expectedCase{digest: digest, model: testCase.Agent.Model, provider: provider, fixture: testCase.Fixture.ExpectedDigest}
	}
	seen := make(map[string]bool, len(expected))
	for _, run := range runs {
		contract, exists := expected[run.CaseID]
		if !exists {
			return fmt.Errorf("%s sample %q references case %q outside the frozen public catalog", label, run.RunID, run.CaseID)
		}
		if run.Provenance.CaseDigest != contract.digest {
			return fmt.Errorf("%s sample %q case digest does not match frozen case %q", label, run.RunID, run.CaseID)
		}
		if run.Provenance.FixtureDigest != contract.fixture {
			return fmt.Errorf("%s sample %q fixture digest does not match frozen case %q", label, run.RunID, run.CaseID)
		}
		if run.Provenance.Model != contract.model || run.Provenance.Provider != contract.provider {
			return fmt.Errorf("%s sample %q model/provider does not match frozen case %q", label, run.RunID, run.CaseID)
		}
		if run.Status == contracts.RunStatusPass || run.Status == contracts.RunStatusFail {
			observedModel := run.Provenance.Extensions["x-observed-model"]
			expectedModel := strings.TrimPrefix(contract.model, contract.provider+"/")
			if run.Provenance.Extensions["x-observed-provider"] != contract.provider || (observedModel != expectedModel && observedModel != contract.model) {
				return fmt.Errorf("%s sample %q lacks an exact observed model identity for frozen case %q", label, run.RunID, run.CaseID)
			}
		}
		seen[run.CaseID] = true
	}
	for id := range expected {
		if !seen[id] {
			return fmt.Errorf("%s artifact has no sample for frozen case %q", label, id)
		}
	}
	return nil
}

const subscriptionCostNotApplicableReason = "not_applicable_chatgpt_subscription_no_authoritative_per_request_usd"

func markSubscriptionCostNotApplicable(summary *stats.PairedSummary) {
	if summary == nil {
		return
	}
	summary.Pairs = nil
	summary.Estimate = stats.Estimate{Reason: subscriptionCostNotApplicableReason}
	summary.MedianDelta = stats.Estimate{Reason: subscriptionCostNotApplicableReason}
	summary.CI.Available = false
	summary.CI.Lower = 0
	summary.CI.Upper = 0
	summary.CI.Confidence = 0
	summary.CI.N = 0
	summary.CI.Reason = subscriptionCostNotApplicableReason
}

func markSubscriptionCostSummaryNotApplicable(summary *stats.Summary) {
	if summary == nil {
		return
	}
	summary.Median.Available = false
	summary.Median.Value = 0
	summary.Median.N = 0
	summary.Median.Reason = subscriptionCostNotApplicableReason
	for quantile, estimate := range summary.Quantiles {
		estimate.Available = false
		estimate.Value = 0
		estimate.N = 0
		estimate.Reason = subscriptionCostNotApplicableReason
		summary.Quantiles[quantile] = estimate
	}
}

func manifestConformanceGate(manifest experiment.Manifest, controlArtifact, candidateArtifact *baseline.Artifact) gates.Result {
	control := controlArtifact.Fingerprint
	candidate := candidateArtifact.Fingerprint
	manifestDigest, err := contracts.CanonicalDigest(manifest)
	if err != nil {
		return gates.Result{Name: "manifest_conformance", Status: gates.StatusInvalid, Reason: gates.ReasonEvidenceInvalid, Detail: err.Error()}
	}
	for _, artifact := range []*baseline.Artifact{controlArtifact, candidateArtifact} {
		if err := requireFrozenManifestAuthority(artifact, manifestDigest, manifest.Intent); err != nil {
			return gates.Result{Name: "manifest_conformance", Status: gates.StatusInvalid, Reason: gates.ReasonEvidenceInvalid, Detail: err.Error()}
		}
		catalogDigest, err := artifactPublicCasesDigest(artifact)
		if err != nil {
			return gates.Result{Name: "manifest_conformance", Status: gates.StatusInvalid, Reason: gates.ReasonEvidenceInvalid, Detail: err.Error()}
		}
		if catalogDigest != manifest.PublicCasesDigest || catalogDigest != artifact.Fingerprint.CaseDigest {
			return gates.Result{Name: "manifest_conformance", Status: gates.StatusInvalid, Reason: gates.ReasonCompatibility, Detail: "public case catalog does not match manifest and fingerprint"}
		}
	}
	type expectedField struct {
		name             string
		control, current string
		expectedControl  string
		expectedCurrent  string
	}
	fields := []expectedField{
		{name: "harness_bundle_digest", control: control.HarnessBundleDigest, current: candidate.HarnessBundleDigest, expectedControl: manifest.Harness.Digest, expectedCurrent: manifest.Harness.Digest},
		{name: "evaluator_binary_digest", control: control.EvaluatorBinaryDigest, current: candidate.EvaluatorBinaryDigest, expectedControl: manifest.Execution.EvaluatorBinaryDigest, expectedCurrent: manifest.Execution.EvaluatorBinaryDigest},
		{name: "experiment_manifest_digest", control: control.ExperimentManifestDigest, current: candidate.ExperimentManifestDigest, expectedControl: manifestDigest, expectedCurrent: manifestDigest},
		{name: "case_digest", control: control.CaseDigest, current: candidate.CaseDigest, expectedControl: manifest.PublicCasesDigest, expectedCurrent: manifest.PublicCasesDigest},
		{name: "agent_bundle_digest", control: control.AgentBundleDigest, current: candidate.AgentBundleDigest, expectedControl: manifest.Control.Digest, expectedCurrent: manifest.Candidate.Digest},
		{name: "opencode_version", control: control.OpenCodeVersion, current: candidate.OpenCodeVersion, expectedControl: manifest.Execution.OpenCodeVersion, expectedCurrent: manifest.Execution.OpenCodeVersion},
		{name: "execution_mode", control: control.ExecutionMode, current: candidate.ExecutionMode, expectedControl: manifest.Execution.Mode, expectedCurrent: manifest.Execution.Mode},
		{name: "network_policy", control: control.NetworkPolicy, current: candidate.NetworkPolicy, expectedControl: manifest.Execution.Network, expectedCurrent: manifest.Execution.Network},
		{name: "provider_auth_mode", control: control.ProviderAuthMode, current: candidate.ProviderAuthMode, expectedControl: manifest.Execution.ProviderAuth, expectedCurrent: manifest.Execution.ProviderAuth},
		{name: "billing_mode", control: control.BillingMode, current: candidate.BillingMode, expectedControl: manifest.Execution.BillingMode, expectedCurrent: manifest.Execution.BillingMode},
		{name: "credential_boundary", control: control.CredentialBoundary, current: candidate.CredentialBoundary, expectedControl: manifest.Execution.CredentialBoundary, expectedCurrent: manifest.Execution.CredentialBoundary},
		{name: "auth_isolation", control: control.AuthIsolation, current: candidate.AuthIsolation, expectedControl: contracts.AuthIsolationDedicatedFreshTokenFailStopV1, expectedCurrent: contracts.AuthIsolationDedicatedFreshTokenFailStopV1},
		{name: "toolchains_digest", control: control.ToolchainsDigest, current: candidate.ToolchainsDigest, expectedControl: manifest.Execution.ToolchainsDigest, expectedCurrent: manifest.Execution.ToolchainsDigest},
	}
	if manifest.Execution.OpenCodeBinaryDigest != "" {
		fields = append(fields, expectedField{name: "opencode_binary_digest", control: control.OpenCodeBinaryDigest, current: candidate.OpenCodeBinaryDigest, expectedControl: manifest.Execution.OpenCodeBinaryDigest, expectedCurrent: manifest.Execution.OpenCodeBinaryDigest})
	}
	if manifest.Execution.OpenCodeOpenAPIDigest != "" {
		fields = append(fields, expectedField{name: "opencode_openapi_digest", control: control.OpenCodeOpenAPIDigest, current: candidate.OpenCodeOpenAPIDigest, expectedControl: manifest.Execution.OpenCodeOpenAPIDigest, expectedCurrent: manifest.Execution.OpenCodeOpenAPIDigest})
	}
	if manifest.ModelAssignment != nil {
		controlProvider, _, _ := contracts.ParseModelSelection(manifest.ModelAssignment.Control)
		candidateProvider, _, _ := contracts.ParseModelSelection(manifest.ModelAssignment.Candidate)
		fields = append(fields,
			expectedField{name: "model", control: control.Model, current: candidate.Model, expectedControl: manifest.ModelAssignment.Control, expectedCurrent: manifest.ModelAssignment.Candidate},
			expectedField{name: "provider", control: control.Provider, current: candidate.Provider, expectedControl: controlProvider, expectedCurrent: candidateProvider},
		)
	}
	for _, field := range fields {
		if field.control != field.expectedControl || field.current != field.expectedCurrent {
			return gates.Result{
				Name: "manifest_conformance", Status: gates.StatusInvalid, Reason: gates.ReasonCompatibility,
				Detail: fmt.Sprintf("%s does not match frozen manifest", field.name),
			}
		}
	}
	if _, err := matchingHoldoutMetadata(controlArtifact, candidateArtifact, manifest.Holdout, manifest.HoldoutCaseCount); err != nil {
		return gates.Result{
			Name: "manifest_conformance", Status: gates.StatusInvalid, Reason: gates.ReasonEvidenceInvalid,
			Detail: err.Error(),
		}
	}
	criticalSet, err := matchingCriticalCases(controlArtifact, candidateArtifact)
	if err != nil {
		return gates.Result{Name: "manifest_conformance", Status: gates.StatusInvalid, Reason: gates.ReasonEvidenceInvalid, Detail: err.Error()}
	}
	criticalIDs := make([]string, 0, len(criticalSet))
	for id := range criticalSet {
		criticalIDs = append(criticalIDs, id)
	}
	sort.Strings(criticalIDs)
	if strings.Join(criticalIDs, "\x00") != strings.Join(manifest.CriticalCaseIDs, "\x00") {
		return gates.Result{Name: "manifest_conformance", Status: gates.StatusInvalid, Reason: gates.ReasonCompatibility, Detail: "critical case ids do not match frozen manifest"}
	}
	return gates.Result{Name: "manifest_conformance", Status: gates.StatusPass, Reason: gates.ReasonGateSatisfied}
}

func artifactPublicCasesDigest(artifact *baseline.Artifact) (string, error) {
	if artifact == nil {
		return "", fmt.Errorf("artifact is required")
	}
	raw, exists := artifact.Aggregates[publicCasesDigestAggregateKey]
	if !exists {
		return "", fmt.Errorf("artifact %q lacks %s", artifact.Label, publicCasesDigestAggregateKey)
	}
	var metadata struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return "", fmt.Errorf("artifact %q %s: %w", artifact.Label, publicCasesDigestAggregateKey, err)
	}
	if !contracts.IsDigest(metadata.Digest) {
		return "", fmt.Errorf("artifact %q has invalid %s", artifact.Label, publicCasesDigestAggregateKey)
	}
	return metadata.Digest, nil
}

func requireFrozenManifestAuthority(artifact *baseline.Artifact, manifestDigest, intent string) error {
	raw, exists := artifact.Aggregates[evaluationAuthorityAggregateKey]
	if !exists {
		return fmt.Errorf("artifact %q lacks frozen-manifest authority metadata", artifact.Label)
	}
	var metadata evaluationAuthorityMetadata
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return fmt.Errorf("artifact %q authority metadata: %w", artifact.Label, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return fmt.Errorf("artifact %q authority metadata: %w", artifact.Label, err)
	}
	if metadata.Mode != authorityForIntent(intent) || metadata.Intent != intent || metadata.ManifestDigest != manifestDigest || metadata.Reason != "" {
		return fmt.Errorf("artifact %q is exploratory or does not match the frozen manifest", artifact.Label)
	}
	return nil
}

type matchedHoldoutEvidence struct {
	References    map[string]struct{}
	ControlRuns   []contracts.RunResult
	CandidateRuns []contracts.RunResult
}

func matchingHoldoutMetadata(control, candidate *baseline.Artifact, expected *experiment.FrozenBundle, expectedCaseCount int) (matchedHoldoutEvidence, error) {
	controlRaw, controlPresent := control.Aggregates[holdoutAggregateKey]
	candidateRaw, candidatePresent := candidate.Aggregates[holdoutAggregateKey]
	if expected == nil {
		if controlPresent || candidatePresent {
			return matchedHoldoutEvidence{}, fmt.Errorf("artifacts declare holdout evidence absent from the frozen manifest")
		}
		return matchedHoldoutEvidence{References: map[string]struct{}{}}, nil
	}
	if !controlPresent || !candidatePresent {
		return matchedHoldoutEvidence{}, fmt.Errorf("both artifacts must contain holdout evidence")
	}
	controlMetadata, err := decodeHoldoutMetadata(control.Label, controlRaw, expected.Digest)
	if err != nil {
		return matchedHoldoutEvidence{}, err
	}
	candidateMetadata, err := decodeHoldoutMetadata(candidate.Label, candidateRaw, expected.Digest)
	if err != nil {
		return matchedHoldoutEvidence{}, err
	}
	if controlMetadata.BundleDigest != candidateMetadata.BundleDigest || controlMetadata.CaseCount != candidateMetadata.CaseCount {
		return matchedHoldoutEvidence{}, fmt.Errorf("control and candidate holdout populations differ")
	}
	if controlMetadata.CaseCount != expectedCaseCount {
		return matchedHoldoutEvidence{}, fmt.Errorf("holdout population contains %d cases, manifest committed %d", controlMetadata.CaseCount, expectedCaseCount)
	}
	references := make(map[string]struct{}, controlMetadata.CaseCount)
	for index := 1; index <= controlMetadata.CaseCount; index++ {
		references[fmt.Sprintf("holdout_%04d", index)] = struct{}{}
	}
	controlKeys, err := indexRuns(controlMetadata.Samples)
	if err != nil {
		return matchedHoldoutEvidence{}, fmt.Errorf("artifact %q holdout samples: %w", control.Label, err)
	}
	candidateKeys, err := indexRuns(candidateMetadata.Samples)
	if err != nil {
		return matchedHoldoutEvidence{}, fmt.Errorf("artifact %q holdout samples: %w", candidate.Label, err)
	}
	for key := range controlKeys {
		if _, exists := candidateKeys[key]; !exists {
			return matchedHoldoutEvidence{}, fmt.Errorf("candidate holdout evidence is missing block %s", key)
		}
	}
	for key := range candidateKeys {
		if _, exists := controlKeys[key]; !exists {
			return matchedHoldoutEvidence{}, fmt.Errorf("control holdout evidence is missing block %s", key)
		}
	}
	return matchedHoldoutEvidence{
		References: references, ControlRuns: controlMetadata.Samples, CandidateRuns: candidateMetadata.Samples,
	}, nil
}

func decodeHoldoutMetadata(label string, raw json.RawMessage, expectedDigest string) (holdoutArtifactMetadata, error) {
	var metadata holdoutArtifactMetadata
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return metadata, fmt.Errorf("artifact %q holdout metadata: %w", label, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return metadata, fmt.Errorf("artifact %q holdout metadata: %w", label, err)
	}
	if expectedDigest != "" && metadata.BundleDigest != expectedDigest {
		return metadata, fmt.Errorf("artifact %q holdout digest does not match frozen manifest", label)
	}
	if !contracts.IsDigest(metadata.BundleDigest) {
		return metadata, fmt.Errorf("artifact %q holdout digest is invalid", label)
	}
	if metadata.CaseCount < 1 {
		return metadata, fmt.Errorf("artifact %q holdout case count is invalid", label)
	}
	references := make(map[string]struct{}, metadata.CaseCount)
	for index := 1; index <= metadata.CaseCount; index++ {
		references[fmt.Sprintf("holdout_%04d", index)] = struct{}{}
	}
	if len(metadata.Samples) < metadata.CaseCount {
		return metadata, fmt.Errorf("artifact %q has incomplete holdout samples", label)
	}
	seenCases := make(map[string]struct{}, metadata.CaseCount)
	for index := range metadata.Samples {
		run := metadata.Samples[index]
		if err := validateSanitizedHoldoutRun(run, label, references); err != nil {
			return metadata, fmt.Errorf("artifact %q sanitized holdout sample %d: %w", label, index, err)
		}
		seenCases[run.CaseID] = struct{}{}
	}
	if len(seenCases) != metadata.CaseCount {
		return metadata, fmt.Errorf("artifact %q is missing a holdout case population", label)
	}
	return metadata, nil
}

func validateSanitizedHoldoutRun(run contracts.RunResult, label string, references map[string]struct{}) error {
	if err := run.Validate(); err != nil {
		return err
	}
	if run.Variant != label {
		return fmt.Errorf("variant %q does not match artifact label", run.Variant)
	}
	if _, exists := references[run.CaseID]; !exists {
		return fmt.Errorf("case reference is absent from holdout population")
	}
	expectedRunID := "holdout_run_" + strings.TrimPrefix(digestBytes([]byte(fmt.Sprintf("%s\x00%s\x00%d", run.CaseID, run.Variant, run.Repetition))), "sha256:")
	if run.RunID != expectedRunID {
		return fmt.Errorf("run reference is not canonical")
	}
	redactedDigest := digestBytes([]byte("skynex-eval-holdout-redacted-v1"))
	provenance := run.Provenance
	if provenance.GitSHA != strings.Repeat("0", 40) || provenance.CaseDigest != redactedDigest ||
		provenance.PromptDigest != redactedDigest || provenance.ConfigDigest != redactedDigest || provenance.FixtureDigest != redactedDigest ||
		provenance.OpenCodeVersion != "redacted" || provenance.OpenCodeAPIDigest != redactedDigest ||
		provenance.Model != "redacted/model" || provenance.Provider != "redacted" ||
		provenance.ToolsetDigest != redactedDigest || provenance.JudgeDigest != redactedDigest || provenance.PricingTableDigest != redactedDigest ||
		provenance.Host.OS != "redacted" || provenance.Host.Arch != "redacted" || provenance.Host.Runtime != "" || len(provenance.Extensions) != 0 {
		return fmt.Errorf("provenance is not fully redacted")
	}
	if len(run.Checks) != 1 || run.Checks[0].ID != "holdout_redacted_check" || run.Checks[0].Type != "redacted" || !run.Checks[0].Hard ||
		run.Checks[0].Summary != "holdout behavior details redacted" || strings.Join(run.Checks[0].RequirementIDs, "\x00") != "REQ-001" ||
		strings.Join(run.Checks[0].EvidenceIDs, "\x00") != "holdout_redacted_evidence" || run.Checks[0].Error != nil ||
		len(run.Evidence.Items) != 1 || run.Evidence.Items[0].ID != "holdout_redacted_evidence" || run.Evidence.Items[0].Kind != "redacted" ||
		run.Evidence.Items[0].Source != contracts.EvidenceEvaluator || run.Evidence.Items[0].Digest != redactedDigest ||
		run.Evidence.Items[0].Path != "" || run.Evidence.Items[0].Summary != "holdout evidence redacted" || !run.Evidence.Items[0].Complete ||
		run.Evidence.BeforeTree != "" || run.Evidence.AfterTree != "" ||
		run.Evidence.DiffDigest != "" || run.Evidence.TraceDigest != "" || run.Evidence.TracePath != "" {
		return fmt.Errorf("behavior evidence was retained in sanitized holdout sample")
	}
	if run.Status == contracts.RunStatusPass {
		if run.Error != nil || run.Checks[0].Status != contracts.CheckStatusPass {
			return fmt.Errorf("passing sample retained an error")
		}
	} else if run.Checks[0].Status != contracts.CheckStatusFail || run.Error == nil || run.Error.Kind != "holdout_redacted" || run.Error.Message != "holdout outcome details redacted" || run.Error.Retryable || len(run.Error.EvidenceIDs) != 0 {
		return fmt.Errorf("non-passing sample error is not redacted")
	}
	return nil
}

func aggregateReliability(summary stats.ReliabilitySummary) reliabilityAggregate {
	counts := make(map[stats.Status]int, len(summary.Counts))
	for status, count := range summary.Counts {
		counts[status] = count
	}
	return reliabilityAggregate{
		Runs: len(summary.Outcomes), Counts: counts,
		PassRate: summary.PassRate, FailureRate: summary.FailureRate,
		InvalidRate: summary.InvalidRate, InconclusiveRate: summary.InconclusiveRate,
		InfrastructureRate: summary.InfrastructureRate, FlakyRate: summary.FlakyRate,
	}
}

func redactHoldoutDecision(decision *gates.Decision, runs []contracts.RunResult, hashes map[string]struct{}) {
	if decision == nil || len(hashes) == 0 {
		return
	}
	for index := range decision.Results {
		decision.Results[index].Detail = redactHoldoutText(decision.Results[index].Detail, runs, nil, hashes)
		// The aggregate interval remains authoritative; raw secret block IDs
		// and per-pair values are intentionally omitted from the report.
		decision.Results[index].Paired.Pairs = nil
	}
	for index := range decision.Reasons {
		decision.Reasons[index].Detail = redactHoldoutText(decision.Reasons[index].Detail, runs, nil, hashes)
	}
}

func redactHoldoutText(value string, control, candidate []contracts.RunResult, hashes map[string]struct{}) string {
	if len(hashes) == 0 || value == "" {
		return value
	}
	for _, population := range [][]contracts.RunResult{control, candidate} {
		for _, run := range population {
			if holdoutCaseIncluded(run.CaseID, hashes) {
				value = strings.ReplaceAll(value, fmt.Sprintf("%s-%04d", run.CaseID, run.Repetition), "[holdout-block]")
				value = strings.ReplaceAll(value, run.CaseID, "[holdout]")
			}
		}
	}
	return value
}

func holdoutCaseIncluded(caseID string, references map[string]struct{}) bool {
	_, exists := references[caseID]
	return exists
}

func holdoutPassRateGate(outcomes []stats.Outcome) gates.Result {
	passRate := stats.SummarizeReliability(outcomes).PassRate
	if !passRate.Available {
		return gates.Result{Name: "holdout_pass_rate", Status: gates.StatusInvalid, Reason: gates.ReasonMetricMissing, Detail: "holdout pass-rate evidence is unavailable"}
	}
	if passRate.Value < 1 {
		return gates.Result{Name: "holdout_pass_rate", Status: gates.StatusRegression, Reason: gates.ReasonHardGateFailed, Detail: fmt.Sprintf("observed %.6g is below minimum 1", passRate.Value)}
	}
	return gates.Result{Name: "holdout_pass_rate", Status: gates.StatusPass, Reason: gates.ReasonGateSatisfied}
}

func matchingCriticalCases(control, candidate *baseline.Artifact) (map[string]struct{}, error) {
	decode := func(artifact *baseline.Artifact) ([]string, error) {
		raw, exists := artifact.Aggregates["critical_case_ids"]
		if !exists {
			return nil, fmt.Errorf("artifact %q lacks critical_case_ids", artifact.Label)
		}
		var metadata struct {
			CaseIDs []string `json:"case_ids"`
		}
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return nil, fmt.Errorf("artifact %q critical_case_ids: %w", artifact.Label, err)
		}
		values := metadata.CaseIDs
		if len(values) == 0 {
			return nil, fmt.Errorf("artifact %q has no critical cases", artifact.Label)
		}
		seen := make(map[string]struct{}, len(values))
		for _, value := range values {
			if value == "" {
				return nil, fmt.Errorf("artifact %q contains an empty critical case id", artifact.Label)
			}
			if _, duplicate := seen[value]; duplicate {
				return nil, fmt.Errorf("artifact %q contains duplicate critical case id %q", artifact.Label, value)
			}
			seen[value] = struct{}{}
		}
		sort.Strings(values)
		return values, nil
	}
	controlIDs, err := decode(control)
	if err != nil {
		return nil, err
	}
	candidateIDs, err := decode(candidate)
	if err != nil {
		return nil, err
	}
	if strings.Join(controlIDs, "\x00") != strings.Join(candidateIDs, "\x00") {
		return nil, fmt.Errorf("control and candidate critical_case_ids differ")
	}
	result := make(map[string]struct{}, len(controlIDs))
	for _, id := range controlIDs {
		result[id] = struct{}{}
	}
	return result, nil
}

func filterOutcomes(outcomes []stats.Outcome, caseIDs map[string]struct{}) []stats.Outcome {
	if len(caseIDs) == 0 {
		return nil
	}
	result := make([]stats.Outcome, 0, len(outcomes))
	for _, outcome := range outcomes {
		if _, included := caseIDs[outcome.CaseID]; included {
			result = append(result, outcome)
		}
	}
	return result
}

func validateCriticalCoverage(caseIDs map[string]struct{}, populations ...[]stats.Outcome) error {
	for populationIndex, outcomes := range populations {
		seen := make(map[string]bool, len(caseIDs))
		for _, outcome := range outcomes {
			if _, critical := caseIDs[outcome.CaseID]; critical {
				seen[outcome.CaseID] = true
			}
		}
		for caseID := range caseIDs {
			if !seen[caseID] {
				return fmt.Errorf("critical case %q is missing from artifact population %d", caseID, populationIndex)
			}
		}
	}
	return nil
}

func experimentPopulationGate(control, candidate []contracts.RunResult, runsPerCase, expectedCases int) gates.Result {
	invalid := func(detail string) gates.Result {
		return gates.Result{Name: "experiment_population", Status: gates.StatusInvalid, Reason: gates.ReasonEvidenceInvalid, Detail: detail}
	}
	if runsPerCase < 1 {
		return invalid("manifest runs_per_case is invalid")
	}
	if expectedCases < 1 {
		return invalid("manifest expected case count is invalid")
	}
	controlIndex, err := indexRuns(control)
	if err != nil {
		return invalid("control: " + err.Error())
	}
	candidateIndex, err := indexRuns(candidate)
	if err != nil {
		return invalid("candidate: " + err.Error())
	}
	if len(controlIndex) == 0 || len(controlIndex) != len(candidateIndex) {
		return invalid("control and candidate populations differ or are empty")
	}
	for key := range controlIndex {
		if _, exists := candidateIndex[key]; !exists {
			return invalid("candidate is missing block " + key)
		}
	}
	for key := range candidateIndex {
		if _, exists := controlIndex[key]; !exists {
			return invalid("control is missing block " + key)
		}
	}
	byCase := make(map[string]map[int]struct{})
	for _, run := range control {
		if run.Variant != string(stats.VariantControl) {
			return invalid(fmt.Sprintf("control block %s-%04d has variant %q", run.CaseID, run.Repetition, run.Variant))
		}
		if byCase[run.CaseID] == nil {
			byCase[run.CaseID] = make(map[int]struct{}, runsPerCase)
		}
		byCase[run.CaseID][run.Repetition] = struct{}{}
	}
	for _, run := range candidate {
		if run.Variant != string(stats.VariantCandidate) {
			return invalid(fmt.Sprintf("candidate block %s-%04d has variant %q", run.CaseID, run.Repetition, run.Variant))
		}
	}
	if len(byCase) != expectedCases {
		return invalid(fmt.Sprintf("population contains %d cases, manifest committed %d", len(byCase), expectedCases))
	}
	for caseID, repetitions := range byCase {
		if len(repetitions) != runsPerCase {
			return invalid(fmt.Sprintf("case %s has %d pairs, expected %d", caseID, len(repetitions), runsPerCase))
		}
		for repetition := 1; repetition <= runsPerCase; repetition++ {
			if _, exists := repetitions[repetition]; !exists {
				return invalid(fmt.Sprintf("case %s is missing repetition %d", caseID, repetition))
			}
		}
	}
	return gates.Result{Name: "experiment_population", Status: gates.StatusPass, Reason: gates.ReasonGateSatisfied}
}

func evidenceValidityGate(name string, runs []contracts.RunResult) gates.Result {
	for _, run := range runs {
		if err := run.Validate(); err != nil {
			return gates.Result{Name: name, Status: gates.StatusInvalid, Reason: gates.ReasonEvidenceInvalid, Detail: fmt.Sprintf("run %s violates the result contract: %v", run.RunID, err)}
		}
	}
	for _, run := range runs {
		if run.Status == contracts.RunStatusInfraError || run.Status == contracts.RunStatusAborted || run.Status == contracts.RunStatusBudgetExhausted {
			return gates.Result{Name: name, Status: gates.StatusInfraError, Reason: gates.ReasonInfrastructure, Detail: fmt.Sprintf("run %s has status %s", run.RunID, run.Status)}
		}
	}
	for _, run := range runs {
		if run.Status == contracts.RunStatusInvalid {
			return gates.Result{Name: name, Status: gates.StatusInvalid, Reason: gates.ReasonEvidenceInvalid, Detail: fmt.Sprintf("run %s is invalid", run.RunID)}
		}
	}
	for _, run := range runs {
		if run.Status == contracts.RunStatusInconclusive {
			return gates.Result{Name: name, Status: gates.StatusInconclusive, Reason: gates.ReasonEvidenceInconclusive, Detail: fmt.Sprintf("run %s is inconclusive", run.RunID)}
		}
	}
	return gates.Result{Name: name, Status: gates.StatusPass, Reason: gates.ReasonGateSatisfied}
}

type metricDefinition struct {
	name             string
	unit             string
	extractor        stats.RunMetric
	requireTelemetry bool
	scope            stats.Scope
	estimator        stats.PairedEstimator
}

func buildMetricPairs(control, candidate []contracts.RunResult, definition metricDefinition) ([]gates.Pair, []stats.PairedValue, error) {
	controlMap, err := indexRuns(control)
	if err != nil {
		return nil, nil, err
	}
	candidateMap, err := indexRuns(candidate)
	if err != nil {
		return nil, nil, err
	}
	keys := make([]string, 0, len(controlMap))
	for key := range controlMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]gates.Pair, 0, len(keys))
	plain := make([]stats.PairedValue, 0, len(keys))
	for _, key := range keys {
		controlRun := controlMap[key]
		candidateRun, exists := candidateMap[key]
		if !exists {
			return nil, nil, fmt.Errorf("candidate is missing block %s", key)
		}
		controlValue, controlAvailable := definition.extractor(controlRun)
		candidateValue, candidateAvailable := definition.extractor(candidateRun)
		var controlPointer, candidatePointer *float64
		if controlAvailable {
			value := controlValue
			controlPointer = &value
		}
		if candidateAvailable {
			value := candidateValue
			candidatePointer = &value
		}
		controlStatus, _ := contractStatsStatus(controlRun.Status)
		candidateStatus, _ := contractStatsStatus(candidateRun.Status)
		pair := gates.Pair{
			BlockID: key, CaseID: controlRun.CaseID,
			Control:   gates.Observation{Value: controlPointer, Status: controlStatus, TelemetryComplete: !definition.requireTelemetry || controlRun.TelemetryComplete},
			Candidate: gates.Observation{Value: candidatePointer, Status: candidateStatus, TelemetryComplete: !definition.requireTelemetry || candidateRun.TelemetryComplete},
		}
		result = append(result, pair)
		if controlPointer != nil && candidatePointer != nil && reportPairEligible(definition.scope, controlStatus, candidateStatus) && pair.Control.TelemetryComplete && pair.Candidate.TelemetryComplete {
			plain = append(plain, stats.PairedValue{BlockID: key, CaseID: controlRun.CaseID, Control: *controlPointer, Candidate: *candidatePointer})
		}
	}
	for key := range candidateMap {
		if _, exists := controlMap[key]; !exists {
			return nil, nil, fmt.Errorf("control is missing block %s", key)
		}
	}
	return result, plain, nil
}

func reportPairEligible(scope stats.Scope, control, candidate stats.Status) bool {
	if scope == stats.ScopeSuccessful {
		return control == stats.StatusPass && candidate == stats.StatusPass
	}
	completed := func(status stats.Status) bool { return status == stats.StatusPass || status == stats.StatusFail }
	return completed(control) && completed(candidate)
}

func indexRuns(runs []contracts.RunResult) (map[string]contracts.RunResult, error) {
	result := make(map[string]contracts.RunResult, len(runs))
	for _, run := range runs {
		key := fmt.Sprintf("%s-%04d", run.CaseID, run.Repetition)
		if _, duplicate := result[key]; duplicate {
			return nil, fmt.Errorf("duplicate block %s", key)
		}
		result[key] = run
	}
	return result, nil
}

func preferredTreeCost(result contracts.RunResult) (float64, bool) {
	if result.Provenance.ProviderCostUSDAuthoritative() && result.Usage.Tree.ProviderCostUSD != nil {
		return *result.Usage.Tree.ProviderCostUSD, true
	}
	return stats.MetricTreeCalculatedCost(result)
}

func contractStatsStatus(status contracts.RunStatus) (stats.Status, error) {
	switch status {
	case contracts.RunStatusPass:
		return stats.StatusPass, nil
	case contracts.RunStatusFail:
		return stats.StatusFail, nil
	case contracts.RunStatusInvalid:
		return stats.StatusInvalid, nil
	case contracts.RunStatusInconclusive:
		return stats.StatusInconclusive, nil
	case contracts.RunStatusAborted:
		return stats.StatusAborted, nil
	case contracts.RunStatusInfraError:
		return stats.StatusInfraError, nil
	case contracts.RunStatusBudgetExhausted:
		return stats.StatusBudgetExhausted, nil
	default:
		return "", fmt.Errorf("unsupported status %q", status)
	}
}

func decodeArtifactRuns(artifact *baseline.Artifact) ([]contracts.RunResult, error) {
	return baseline.DecodeRunResults(artifact.Samples)
}

func countPassToFail(control, candidate []contracts.RunResult) int {
	controlMap, err := indexRuns(control)
	if err != nil {
		return 0
	}
	count := 0
	for key, candidateRun := range mustIndexRuns(candidate) {
		if controlRun, exists := controlMap[key]; exists && controlRun.Status == contracts.RunStatusPass && candidateRun.Status != contracts.RunStatusPass {
			count++
		}
	}
	return count
}

func mustIndexRuns(runs []contracts.RunResult) map[string]contracts.RunResult {
	indexed, _ := indexRuns(runs)
	return indexed
}

func countFailedChecks(runs []contracts.RunResult, fragments ...string) int {
	count := 0
	for _, run := range runs {
		for _, check := range run.Checks {
			value := strings.ToLower(check.ID + " " + check.Type + " " + strings.Join(check.RequirementIDs, " "))
			matched := false
			for _, fragment := range fragments {
				if strings.Contains(value, fragment) {
					matched = true
					break
				}
			}
			if matched && check.Status == contracts.CheckStatusFail {
				count++
			}
		}
	}
	return count
}

type artifactReport struct {
	Kind        string                   `json:"kind"`
	Authority   string                   `json:"authority"`
	Label       string                   `json:"label"`
	Suite       string                   `json:"suite"`
	CreatedAt   string                   `json:"created_at"`
	Samples     int                      `json:"samples"`
	Integrity   baseline.Integrity       `json:"integrity"`
	Reliability stats.ReliabilitySummary `json:"reliability"`
	Holdout     *holdoutArtifactReport   `json:"holdout,omitempty"`
	ExitCode    int                      `json:"exit_code"`
}

type holdoutArtifactReport struct {
	BundleDigest string               `json:"bundle_digest"`
	Cases        int                  `json:"cases"`
	Reliability  reliabilityAggregate `json:"reliability"`
}

func (r artifactReport) CLIExitCode() int { return r.ExitCode }

type savedComparisonReport struct {
	Kind      string                    `json:"kind"`
	Path      string                    `json:"path"`
	Intent    string                    `json:"intent"`
	Authority string                    `json:"authority"`
	Report    reporter.ComparisonReport `json:"report"`
	Holdout   *holdoutComparisonSummary `json:"holdout,omitempty"`
	ExitCode  int                       `json:"exit_code"`
}

func (r savedComparisonReport) CLIExitCode() int { return r.ExitCode }

func commandReport(args []string) (any, error) {
	set := newFlagSet("report")
	input := set.String("input", "", "baseline artifact or comparison report")
	controlPath := set.String("control", "", "control artifact used to verify a comparison")
	candidatePath := set.String("candidate", "", "candidate artifact used to verify a comparison")
	manifestPath := set.String("manifest", "", "frozen manifest used to verify a comparison")
	if err := parseFlagSet(set, args); err != nil {
		return nil, err
	}
	if *input == "" {
		return nil, invalidf("invalid_arguments", "--input is required")
	}
	var header struct {
		Kind         string `json:"kind"`
		ExperimentID string `json:"experiment_id"`
	}
	if err := baseline.LoadJSON(*input, &header, baseline.IOOptions{}); err != nil {
		return nil, invalidf("invalid_artifact", "%v", err)
	}
	if header.Kind == baseline.ArtifactKind {
		artifact, err := baseline.Load(*input, baseline.IOOptions{})
		if err != nil {
			return nil, invalidf("invalid_artifact", "%v", err)
		}
		runs, err := decodeArtifactRuns(artifact)
		if err != nil {
			return nil, invalidf("invalid_samples", "%v", err)
		}
		if err := validateArtifactFingerprintBinding(artifact, runs); err != nil {
			return nil, invalidf("invalid_fingerprint_binding", "%v", err)
		}
		authority, err := reportedArtifactAuthority(artifact)
		if err != nil {
			return nil, invalidf("invalid_artifact", "%v", err)
		}
		outcomes, _ := stats.OutcomesFromRunResults(runs)
		var holdout *holdoutArtifactReport
		if raw, exists := artifact.Aggregates[holdoutAggregateKey]; exists {
			metadata, decodeErr := decodeHoldoutMetadata(artifact.Label, raw, "")
			if decodeErr != nil {
				return nil, invalidf("invalid_artifact", "%v", decodeErr)
			}
			holdoutOutcomes, outcomesErr := stats.OutcomesFromRunResults(metadata.Samples)
			if outcomesErr != nil {
				return nil, invalidf("invalid_artifact", "holdout outcomes: %v", outcomesErr)
			}
			holdout = &holdoutArtifactReport{
				BundleDigest: metadata.BundleDigest, Cases: metadata.CaseCount,
				Reliability: aggregateReliability(stats.SummarizeReliability(holdoutOutcomes)),
			}
			runs = append(runs, metadata.Samples...)
		}
		return artifactReport{
			Kind: artifact.Kind, Authority: authority, Label: artifact.Label, Suite: artifact.Suite, CreatedAt: artifact.CreatedAt,
			Samples: len(runs), Integrity: artifact.Integrity, Reliability: stats.SummarizeReliability(outcomes),
			Holdout: holdout, ExitCode: resultSetExit(runs),
		}, nil
	}
	if header.Kind == "skynex-eval-comparison" {
		if *controlPath == "" || *candidatePath == "" || *manifestPath == "" {
			return nil, invalidf("comparison_verification_required", "comparison reports require --control, --candidate and --manifest; an input JSON alone is not authoritative")
		}
		var comparison comparisonCommandResult
		if err := baseline.LoadJSON(*input, &comparison, baseline.IOOptions{Strict: true}); err != nil {
			return nil, invalidf("invalid_comparison", "%v", err)
		}
		if comparison.Report.SchemaVersion != 1 || comparison.Report.Decision.ExitCode != gates.ExitCode(comparison.Report.Decision.Status) ||
			(comparison.Intent != experiment.IntentDevelopment && comparison.Intent != experiment.IntentRelease) || comparison.Authority != authorityForIntent(comparison.Intent) {
			return nil, invalidf("invalid_comparison", "comparison report has an invalid schema or decision exit code")
		}
		verified, err := commandCompare([]string{"--control", *controlPath, "--candidate", *candidatePath, "--manifest", *manifestPath})
		if err != nil {
			return nil, invalidf("comparison_verification_failed", "%v", err)
		}
		comparison.OutputPath = ""
		verified.OutputPath = ""
		observedDigest, err := contracts.CanonicalDigest(comparison)
		if err != nil {
			return nil, invalidf("invalid_comparison", "%v", err)
		}
		verifiedDigest, err := contracts.CanonicalDigest(verified)
		if err != nil {
			return nil, invalidf("comparison_verification_failed", "%v", err)
		}
		if observedDigest != verifiedDigest {
			return nil, invalidf("comparison_verification_failed", "saved comparison does not match the frozen manifest and source artifacts")
		}
		return savedComparisonReport{
			Kind: verified.Kind, Path: *input, Intent: verified.Intent, Authority: verified.Authority, Report: verified.Report,
			Holdout: verified.Holdout, ExitCode: verified.Report.Decision.ExitCode,
		}, nil
	}
	if header.ExperimentID != "" {
		return nil, invalidf("invalid_comparison", "raw comparison reports lack frozen intent/authority; use the skynex-eval-comparison envelope")
	}
	return nil, invalidf("invalid_artifact", "unrecognized artifact kind")
}

func reportedArtifactAuthority(artifact *baseline.Artifact) (string, error) {
	raw, exists := artifact.Aggregates[evaluationAuthorityAggregateKey]
	if !exists {
		return "", fmt.Errorf("artifact %q lacks evaluation authority metadata", artifact.Label)
	}
	var metadata evaluationAuthorityMetadata
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return "", fmt.Errorf("artifact %q authority metadata: %w", artifact.Label, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return "", fmt.Errorf("artifact %q authority metadata: %w", artifact.Label, err)
	}
	switch metadata.Mode {
	case evaluationAuthorityExploratory:
		if metadata.Intent != "" || metadata.ManifestDigest != "" || metadata.Reason == "" {
			return "", fmt.Errorf("artifact %q has malformed exploratory authority", artifact.Label)
		}
		return evaluationAuthorityExploratory, nil
	case evaluationAuthorityRelease:
		return "", fmt.Errorf("artifact %q claims release authority without a verifiable external attestation", artifact.Label)
	case evaluationAuthorityDevelopment:
		if metadata.Mode != authorityForIntent(metadata.Intent) || !contracts.IsDigest(metadata.ManifestDigest) || metadata.Reason != "" {
			return "", fmt.Errorf("artifact %q has malformed frozen authority", artifact.Label)
		}
		// A single arm is evidence captured under a frozen experiment, not an
		// acceptance decision. Only a comparison reverified against both arms and
		// the manifest may publish development/release decision authority.
		return metadata.Mode + "-evidence-only", nil
	default:
		return "", fmt.Errorf("artifact %q has unknown authority %q", artifact.Label, metadata.Mode)
	}
}
