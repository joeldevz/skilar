package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/experiment"
	"github.com/joeldevz/skynex/internal/eval/runner"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/stats"
	"github.com/joeldevz/skynex/internal/safefs"
)

const defaultFreezeRuns = 5

// freezeResult is also the durable receipt when stdout is redirected. Source
// paths are deliberately omitted: a holdout location is private, while the
// materialized roots and their content digests are sufficient to consume the
// capsule. Source Git state is recorded here and in the manifest's source_git_*
// receipt fields rather than copied into git_sha, which describes Git metadata
// owned by a frozen root and is verified again by Manifest.VerifyBundles.
type freezeResult struct {
	CapsuleDir       string               `json:"capsule_dir"`
	ManifestPath     string               `json:"manifest_path"`
	ManifestDigest   string               `json:"manifest_digest"`
	Intent           string               `json:"intent"`
	PublicCaseCount  int                  `json:"public_case_count"`
	HoldoutCaseCount int                  `json:"holdout_case_count"`
	CriticalCaseIDs  []string             `json:"critical_case_ids"`
	Bundles          []freezeBundleResult `json:"bundles"`
	ModelCalls       int                  `json:"model_calls"`
}

type freezeBundleResult struct {
	Name                string                     `json:"name"`
	Root                string                     `json:"root"`
	Digest              string                     `json:"digest"`
	FileCount           *int                       `json:"file_count,omitempty"`
	TotalBytes          *int64                     `json:"total_bytes,omitempty"`
	SourceGitProvenance *freezeSourceGitProvenance `json:"source_git_provenance,omitempty"`
}

type freezeSourceGitProvenance struct {
	GitSHA           string `json:"git_sha"`
	DirtyPatchDigest string `json:"dirty_patch_digest,omitempty"`
	Dirty            bool   `json:"dirty"`
}

type freezeInput struct {
	name       string
	source     string
	relative   string
	snapshot   sandbox.Snapshot
	provenance *freezeSourceGitProvenance
}

type frozenDoctorEnvelope struct {
	SchemaVersion int          `json:"schema_version"`
	Command       string       `json:"command"`
	Status        string       `json:"status"`
	Data          doctorResult `json:"data"`
	Error         *errorBody   `json:"error,omitempty"`
}

func commandFreeze(ctx context.Context, args []string) (freezeResult, error) {
	set := newFlagSet("freeze")
	outputDir := set.String("output-dir", "", "new capsule directory")
	harnessSource := set.String("harness", "", "source harness tree containing cases/ and fixtures/")
	controlSource := set.String("control", "", "source control OpenCode bundle")
	candidateSource := set.String("candidate", "", "source candidate OpenCode bundle")
	holdoutSource := set.String("holdout", "", "optional external holdout tree containing cases/ and fixtures/")
	experimentID := set.String("id", "", "experiment identifier")
	suite := set.String("suite", "", "public suite identifier")
	runs := set.Int("runs", defaultFreezeRuns, "paired runs per case")
	seed := set.String("seed", "1", "decimal balanced-block randomization seed")
	differencesText := set.String("intentional-differences", string(baseline.FieldAgentBundleDigest), "comma-separated predeclared fingerprint differences")
	controlModel := set.String("control-model", "", "optional exact provider/model override for the control arm")
	candidateModel := set.String("candidate-model", "", "optional exact provider/model override for the candidate arm")
	binary := set.String("binary", "opencode", "OpenCode binary to fingerprint without executing it")
	openCodeVersion := set.String("opencode-version", defaultOpenCodeVersion, "exact OpenCode version pin")
	openAPIDigest := set.String("opencode-openapi-digest", "", "sha256 digest previously observed for OpenCode /doc")
	doctorResultPath := set.String("doctor-result", "", "saved successful skynex-eval doctor JSON envelope")
	if err := parseFlagSet(set, args); err != nil {
		return freezeResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return freezeResult{}, err
	}
	for _, required := range []struct{ name, value string }{
		{name: "--output-dir", value: *outputDir},
		{name: "--harness", value: *harnessSource},
		{name: "--control", value: *controlSource},
		{name: "--candidate", value: *candidateSource},
		{name: "--id", value: *experimentID},
		{name: "--suite", value: *suite},
	} {
		if strings.TrimSpace(required.value) == "" {
			return freezeResult{}, invalidf("invalid_arguments", "%s is required", required.name)
		}
	}
	if *runs < 2 || *runs > contracts.MaxRuns {
		return freezeResult{}, invalidf("invalid_arguments", "--runs must be between 2 and %d", contracts.MaxRuns)
	}
	differences, err := parseFreezeDifferences(*differencesText)
	if err != nil {
		return freezeResult{}, invalidf("invalid_arguments", "%v", err)
	}
	modelAssignment, differences, err := freezeModelAssignment(*controlModel, *candidateModel, differences)
	if err != nil {
		return freezeResult{}, invalidf("invalid_arguments", "%v", err)
	}

	absoluteOutput, err := cleanAbsolutePath(*outputDir)
	if err != nil {
		return freezeResult{}, invalidf("invalid_output", "%v", err)
	}
	inputs := []freezeInput{
		{name: "harness", source: *harnessSource, relative: "bundles/harness"},
		{name: "control", source: *controlSource, relative: "bundles/control"},
		{name: "candidate", source: *candidateSource, relative: "bundles/candidate"},
	}
	if strings.TrimSpace(*holdoutSource) != "" {
		inputs = append(inputs, freezeInput{name: "holdout", source: *holdoutSource, relative: "bundles/holdout"})
	}
	for index := range inputs {
		input := &inputs[index]
		input.source, err = cleanAbsoluteDirectory(input.source)
		if err != nil {
			return freezeResult{}, invalidFreezeInput(input.name, err)
		}
		if pathContains(input.source, absoluteOutput) {
			return freezeResult{}, invalidf("invalid_output", "--output-dir must not be inside the %s input tree", input.name)
		}
		input.snapshot, err = sandbox.DigestTree(input.source, sandbox.DefaultSnapshotLimits())
		if err != nil {
			return freezeResult{}, invalidFreezeInput(input.name, err)
		}
	}
	if err := rejectOverlappingFreezeInputs(inputs); err != nil {
		return freezeResult{}, invalidf("invalid_bundle_layout", "%v", err)
	}
	if inputs[1].snapshot.Digest == inputs[2].snapshot.Digest &&
		(modelAssignment == nil || declaresFrozenBundleDifference(differences)) {
		return freezeResult{}, invalidf(
			"treatment_not_realized",
			"control and candidate have the same canonical tree digest without a model-only treatment",
		)
	}
	publicCases, err := validateFreezeCaseBundle(inputs[0].source, *suite, false)
	if err != nil {
		return freezeResult{}, invalidf("invalid_harness", "harness cases/fixtures are invalid: %v", err)
	}
	publicDigest, err := publicCaseSetDigest(publicCases)
	if err != nil {
		return freezeResult{}, invalidf("invalid_harness", "digest public cases: %v", err)
	}
	criticalIDs := declaredCriticalCaseIDs(publicCases)
	if len(criticalIDs) == 0 {
		return freezeResult{}, invalidf("invalid_harness", "suite %q has no critical cases", *suite)
	}
	holdoutCount := 0
	toolchainCases := append([]contracts.Case(nil), publicCases...)
	if len(inputs) == 4 {
		holdoutCases, holdoutErr := validateFreezeCaseBundle(inputs[3].source, *suite, true)
		if holdoutErr != nil {
			return freezeResult{}, invalidf("invalid_holdout", "%v", privateHoldoutError())
		}
		publicIDs := make(map[string]struct{}, len(publicCases))
		for _, testCase := range publicCases {
			publicIDs[testCase.ID] = struct{}{}
		}
		for _, testCase := range holdoutCases {
			if _, duplicate := publicIDs[testCase.ID]; duplicate {
				return freezeResult{}, invalidf("invalid_holdout", "%v", privateHoldoutError())
			}
		}
		holdoutCount = len(holdoutCases)
		toolchainCases = append(toolchainCases, holdoutCases...)
	}
	executableClosure, err := runner.ResolveExecutableClosure(toolchainCases, "git")
	if err != nil {
		if len(inputs) == 4 {
			return freezeResult{}, invalidf("invalid_toolchains", "%v", privateHoldoutError())
		}
		return freezeResult{}, invalidf("invalid_toolchains", "resolve effective executable closure: %v", err)
	}
	if _, err := executableClosure.PathFor("git"); err != nil {
		if len(inputs) == 4 {
			return freezeResult{}, invalidf("invalid_toolchains", "%v", privateHoldoutError())
		}
		return freezeResult{}, invalidf("invalid_toolchains", "resolve Git from effective executable closure: %v", err)
	}
	for index := range inputs {
		input := &inputs[index]
		input.provenance, err = inspectFreezeSourceGit(input.source, executableClosure)
		if err != nil {
			return freezeResult{}, invalidFreezeInput(input.name, fmt.Errorf("inspect Git provenance: %w", err))
		}
		if input.name == "holdout" && input.provenance != nil && input.provenance.Dirty {
			return freezeResult{}, invalidf("invalid_holdout", "%v", privateHoldoutError())
		}
	}

	evaluatorDigest, err := executableDigest()
	if err != nil {
		return freezeResult{}, infraf("evaluator_provenance", fmt.Errorf("fingerprint evaluator binary: %w", err))
	}
	resolvedBinary, err := resolveOpenCodeBinary(*binary)
	if err != nil {
		return freezeResult{}, invalidf("invalid_opencode_binary", "%v", err)
	}
	openCodeBinaryDigest := resolvedBinary.Digest
	resolvedOpenAPIDigest, err := resolveFreezeRuntimePins(
		*doctorResultPath, *openAPIDigest, *openCodeVersion, evaluatorDigest, openCodeBinaryDigest,
	)
	if err != nil {
		return freezeResult{}, invalidf("invalid_runtime_pins", "%v", err)
	}
	if err := ctx.Err(); err != nil {
		return freezeResult{}, err
	}

	parentPath := filepath.Dir(absoluteOutput)
	outputName := filepath.Base(absoluteOutput)
	parent, err := safefs.Open(parentPath)
	if err != nil {
		return freezeResult{}, invalidf("invalid_output", "open existing output parent: %v", err)
	}
	defer parent.Close()
	if _, statErr := parent.Lstat(outputName); statErr == nil {
		return freezeResult{}, invalidf("output_exists", "output directory already exists")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return freezeResult{}, infraf("freeze_output", fmt.Errorf("inspect output target: %w", statErr))
	}
	stageName, err := createFreezeStage(parent)
	if err != nil {
		return freezeResult{}, infraf("freeze_output", err)
	}
	published := false
	defer func() {
		if !published {
			_ = parent.RemoveAll(stageName)
		}
	}()
	stagePath := filepath.Join(parentPath, stageName)
	stage, err := safefs.Open(stagePath)
	if err != nil {
		return freezeResult{}, infraf("freeze_output", fmt.Errorf("open capsule staging directory: %w", err))
	}
	if err := stage.Mkdir("bundles", 0o700); err != nil {
		stage.Close()
		return freezeResult{}, infraf("freeze_output", fmt.Errorf("create bundle directory: %w", err))
	}
	for _, input := range inputs {
		if err := stage.Mkdir(filepath.FromSlash(input.relative), 0o700); err != nil {
			stage.Close()
			return freezeResult{}, infraf("freeze_output", fmt.Errorf("create %s destination: %w", input.name, err))
		}
	}
	if err := stage.Close(); err != nil {
		return freezeResult{}, infraf("freeze_output", fmt.Errorf("close capsule staging directory: %w", err))
	}

	results := make([]freezeBundleResult, 0, len(inputs))
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return freezeResult{}, err
		}
		destination := filepath.Join(stagePath, filepath.FromSlash(input.relative))
		copied, copyErr := sandbox.CopyVerifiedTree(input.source, destination, sandbox.DefaultSnapshotLimits())
		if copyErr != nil {
			return freezeResult{}, invalidFreezeInput(input.name, fmt.Errorf("materialize verified tree: %w", copyErr))
		}
		if copied.Digest != input.snapshot.Digest {
			return freezeResult{}, invalidFreezeInput(input.name, fmt.Errorf("source changed before materialization"))
		}
		publishedProvenance := cloneFreezeGitProvenance(input.provenance)
		if input.name == "holdout" {
			publishedProvenance = nil
		}
		result := freezeBundleResult{
			Name: input.name, Root: input.relative, Digest: copied.Digest,
			SourceGitProvenance: publishedProvenance,
		}
		if input.name != "holdout" {
			fileCount, totalBytes := copied.FileCount, copied.TotalBytes
			result.FileCount, result.TotalBytes = &fileCount, &totalBytes
		}
		results = append(results, result)
	}
	if err := recheckFreezeInputs(inputs, executableClosure); err != nil {
		return freezeResult{}, err
	}
	if err := executableClosure.Revalidate(); err != nil {
		return freezeResult{}, invalidf("toolchain_drift", "%v", err)
	}
	if err := resolvedBinary.Revalidate(); err != nil {
		return freezeResult{}, invalidf("opencode_binary_drift", "%v", err)
	}
	evaluatorDigestAfter, err := executableDigest()
	if err != nil || evaluatorDigestAfter != evaluatorDigest {
		if err == nil {
			err = fmt.Errorf("got %s, expected %s", evaluatorDigestAfter, evaluatorDigest)
		}
		return freezeResult{}, invalidf("evaluator_binary_drift", "%v", err)
	}

	manifest := experiment.Manifest{
		SchemaVersion:          experiment.SchemaVersion,
		ID:                     *experimentID,
		Suite:                  *suite,
		Intent:                 experiment.IntentDevelopment,
		Harness:                frozenBundleFromResult(results[0]),
		Control:                frozenBundleFromResult(results[1]),
		Candidate:              frozenBundleFromResult(results[2]),
		ModelAssignment:        modelAssignment,
		IntentionalDifferences: differences,
		PublicCaseCount:        len(publicCases),
		PublicCasesDigest:      publicDigest,
		CriticalCaseIDs:        append([]string(nil), criticalIDs...),
		HoldoutCaseCount:       holdoutCount,
		Runs:                   *runs,
		Randomization: experiment.Randomization{
			Method: stats.BalancedBlockedMethod, Seed: *seed, SerializeWithinBlock: true,
		},
		Execution: experiment.Execution{
			Mode: string(contracts.ExecutionTrustedLocal), Network: string(contracts.NetworkHostUnisolated),
			Concurrency: 1, ProviderAuth: experiment.ProviderAuthOpenAIOAuthCleanProfileV1,
			BillingMode:        experiment.BillingModeChatGPTSubscription,
			CredentialBoundary: experiment.CredentialBoundaryRuntimeReadable,
			OpenCodeVersion:    *openCodeVersion, EvaluatorBinaryDigest: evaluatorDigest,
			OpenCodeBinaryDigest: openCodeBinaryDigest, OpenCodeOpenAPIDigest: resolvedOpenAPIDigest,
			ToolchainsDigest: executableClosure.Digest(),
		},
		Gates: strictDevelopmentGates(*runs),
	}
	if len(results) == 4 {
		holdout := frozenBundleFromResult(results[3])
		manifest.Holdout = &holdout
	}
	if err := manifest.Validate(); err != nil {
		return freezeResult{}, invalidf("invalid_manifest", "generated manifest: %v", err)
	}
	manifestPath := filepath.Join(stagePath, "manifest.json")
	if err := baseline.SaveJSON(manifestPath, manifest, baseline.IOOptions{MaxBytes: 1 << 20}); err != nil {
		return freezeResult{}, infraf("freeze_output", fmt.Errorf("write manifest: %w", err))
	}
	loaded, err := experiment.Load(manifestPath)
	if err != nil {
		return freezeResult{}, invalidf("invalid_manifest", "reload generated manifest: %v", err)
	}
	if _, err := loaded.VerifyBundles(stagePath, sandbox.DefaultSnapshotLimits()); err != nil {
		return freezeResult{}, invalidf("invalid_manifest", "verify generated capsule: %v", err)
	}
	manifestDigest, err := contracts.CanonicalDigest(*loaded)
	if err != nil {
		return freezeResult{}, invalidf("invalid_manifest_digest", "%v", err)
	}
	if err := ctx.Err(); err != nil {
		return freezeResult{}, err
	}
	if _, statErr := parent.Lstat(outputName); statErr == nil {
		return freezeResult{}, invalidf("output_exists", "output directory appeared while freezing")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return freezeResult{}, infraf("freeze_output", fmt.Errorf("recheck output target: %w", statErr))
	}
	if err := parent.Rename(stageName, outputName); err != nil {
		return freezeResult{}, infraf("freeze_output", fmt.Errorf("publish capsule: %w", err))
	}
	published = true

	finalManifestPath := filepath.Join(absoluteOutput, "manifest.json")
	finalManifest, err := experiment.Load(finalManifestPath)
	if err != nil {
		return freezeResult{}, infraf("freeze_output", fmt.Errorf("reload published manifest: %w", err))
	}
	if _, err := finalManifest.VerifyBundles(absoluteOutput, sandbox.DefaultSnapshotLimits()); err != nil {
		return freezeResult{}, infraf("freeze_output", fmt.Errorf("verify published capsule: %w", err))
	}
	return freezeResult{
		CapsuleDir: absoluteOutput, ManifestPath: finalManifestPath, ManifestDigest: manifestDigest,
		Intent: experiment.IntentDevelopment, PublicCaseCount: len(publicCases), HoldoutCaseCount: holdoutCount,
		CriticalCaseIDs: append([]string(nil), criticalIDs...), Bundles: results, ModelCalls: 0,
	}, nil
}

func strictDevelopmentGates(runs int) experiment.Gates {
	minimumPairs := defaultFreezeRuns
	if runs < minimumPairs {
		minimumPairs = runs
	}
	return experiment.Gates{
		CriticalCasePassRate: 1, PassToFailRegressions: 0, ScopeViolations: 0, FalseSuccesses: 0,
		MaxParentPeakInputRatio: 0.70, MaxTreeInputRatio: 1, MaxCostRatio: 1,
		MaxWallTimeRatio: 1.10, MaxRetryRateRatio: 1, Confidence: 0.95, MinimumPairs: minimumPairs,
	}
}

func parseFreezeDifferences(value string) ([]baseline.Field, error) {
	allowed := map[baseline.Field]bool{
		baseline.FieldPromptDigest: true, baseline.FieldAgentBundleDigest: true,
		baseline.FieldModel: true, baseline.FieldProvider: true,
	}
	seen := make(map[baseline.Field]struct{})
	result := make([]baseline.Field, 0)
	for _, raw := range strings.Split(value, ",") {
		field := baseline.Field(strings.TrimSpace(raw))
		if field == "" || !allowed[field] {
			return nil, fmt.Errorf("unsupported intentional difference %q", raw)
		}
		if _, duplicate := seen[field]; duplicate {
			return nil, fmt.Errorf("duplicate intentional difference %q", field)
		}
		seen[field] = struct{}{}
		result = append(result, field)
	}
	// The current artifact producer binds both prompt_digest and
	// agent_bundle_digest to the verified arm tree. Declaring either side alone
	// would make the inevitable companion mismatch an undeclared incompatibility.
	if _, prompt := seen[baseline.FieldPromptDigest]; prompt {
		if _, agent := seen[baseline.FieldAgentBundleDigest]; !agent {
			result = append(result, baseline.FieldAgentBundleDigest)
		}
	} else if _, agent := seen[baseline.FieldAgentBundleDigest]; agent {
		result = append(result, baseline.FieldPromptDigest)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	if len(result) == 0 {
		return nil, fmt.Errorf("at least one intentional difference is required")
	}
	return result, nil
}

func declaresFrozenBundleDifference(differences []baseline.Field) bool {
	for _, field := range differences {
		if field == baseline.FieldPromptDigest || field == baseline.FieldAgentBundleDigest {
			return true
		}
	}
	return false
}

func freezeModelAssignment(control, candidate string, differences []baseline.Field) (*experiment.ModelAssignment, []baseline.Field, error) {
	control = strings.TrimSpace(control)
	candidate = strings.TrimSpace(candidate)
	if control == "" && candidate == "" {
		for _, field := range differences {
			if field == baseline.FieldModel || field == baseline.FieldProvider {
				return nil, nil, fmt.Errorf("--control-model and --candidate-model are required for model/provider intentional differences")
			}
		}
		return nil, differences, nil
	}
	if control == "" || candidate == "" {
		return nil, nil, fmt.Errorf("--control-model and --candidate-model must be supplied together")
	}
	controlProvider, _, err := contracts.ParseModelSelection(control)
	if err != nil {
		return nil, nil, fmt.Errorf("--control-model: %w", err)
	}
	candidateProvider, _, err := contracts.ParseModelSelection(candidate)
	if err != nil {
		return nil, nil, fmt.Errorf("--candidate-model: %w", err)
	}
	if control == candidate {
		return nil, nil, fmt.Errorf("control and candidate model assignments must differ")
	}
	seen := make(map[baseline.Field]struct{}, len(differences)+2)
	for _, field := range differences {
		seen[field] = struct{}{}
	}
	seen[baseline.FieldModel] = struct{}{}
	if controlProvider != candidateProvider {
		seen[baseline.FieldProvider] = struct{}{}
	} else if _, declared := seen[baseline.FieldProvider]; declared {
		return nil, nil, fmt.Errorf("provider is declared as a difference but both model assignments use provider %q", controlProvider)
	}
	resolved := make([]baseline.Field, 0, len(seen))
	for field := range seen {
		resolved = append(resolved, field)
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i] < resolved[j] })
	return &experiment.ModelAssignment{Control: control, Candidate: candidate}, resolved, nil
}

func validateFreezeCaseBundle(root, suite string, holdout bool) ([]contracts.Case, error) {
	casesRoot, err := frozenBundleDirectory(root, "cases")
	if err != nil {
		return nil, err
	}
	fixturesRoot, err := frozenBundleDirectory(root, "fixtures")
	if err != nil {
		return nil, err
	}
	loaded, err := loadSelectedCases(casesRoot, suite, "")
	if err != nil {
		return nil, err
	}
	if _, _, err := validateFixtures(fixturesRoot, loaded); err != nil {
		return nil, err
	}
	if holdout {
		for _, testCase := range loaded {
			if testCase.Migration != nil {
				return nil, fmt.Errorf("external holdouts must use native v1 cases")
			}
		}
	}
	return loaded, nil
}

func resolveFreezeRuntimePins(doctorPath, explicitOpenAPI, expectedVersion, evaluatorDigest, binaryDigest string) (string, error) {
	if strings.TrimSpace(doctorPath) != "" && strings.TrimSpace(explicitOpenAPI) != "" {
		return "", fmt.Errorf("--doctor-result and --opencode-openapi-digest are mutually exclusive")
	}
	if strings.TrimSpace(doctorPath) == "" {
		if !contracts.IsDigest(explicitOpenAPI) {
			return "", fmt.Errorf("--opencode-openapi-digest must be a canonical sha256 digest when --doctor-result is absent")
		}
		return explicitOpenAPI, nil
	}
	absolute, err := cleanAbsolutePath(doctorPath)
	if err != nil {
		return "", fmt.Errorf("resolve doctor result: %w", err)
	}
	var envelope frozenDoctorEnvelope
	if err := baseline.LoadJSON(absolute, &envelope, baseline.IOOptions{Strict: true, MaxBytes: 4 << 20}); err != nil {
		return "", fmt.Errorf("load doctor result: %w", err)
	}
	if envelope.SchemaVersion != cliSchemaVersion || envelope.Command != "doctor" || envelope.Status != string(contracts.RunStatusPass) || envelope.Error != nil {
		return "", fmt.Errorf("doctor result is not a successful v%d doctor envelope", cliSchemaVersion)
	}
	if !envelope.Data.Healthy || envelope.Data.ModelCalls != 0 {
		return "", fmt.Errorf("doctor result must attest healthy=true and model_calls=0")
	}
	if envelope.Data.Version != expectedVersion || envelope.Data.ExpectedVersion != expectedVersion {
		return "", fmt.Errorf("doctor OpenCode version pin does not match --opencode-version")
	}
	if envelope.Data.EvaluatorBinaryDigest != evaluatorDigest {
		return "", fmt.Errorf("doctor evaluator binary digest does not match this evaluator")
	}
	if envelope.Data.OpenCodeBinaryDigest != binaryDigest {
		return "", fmt.Errorf("doctor OpenCode binary digest does not match --binary")
	}
	for name, value := range map[string]string{
		"toolchains_digest":       envelope.Data.ToolchainsDigest,
		"effective_config_digest": envelope.Data.EffectiveConfigDigest,
		"effective_agents_digest": envelope.Data.EffectiveAgentsDigest,
	} {
		if !contracts.IsDigest(value) {
			return "", fmt.Errorf("doctor result lacks canonical %s evidence", name)
		}
	}
	openAPI := ""
	for _, endpoint := range envelope.Data.Endpoints {
		if endpoint.Name != "/doc" {
			continue
		}
		if openAPI != "" {
			return "", fmt.Errorf("doctor result contains duplicate /doc evidence")
		}
		openAPI = endpoint.Digest
	}
	if !contracts.IsDigest(openAPI) {
		return "", fmt.Errorf("doctor result lacks a canonical /doc digest")
	}
	return openAPI, nil
}

func cleanAbsolutePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') {
		return "", fmt.Errorf("path is required and must not contain NUL")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func cleanAbsoluteDirectory(path string) (string, error) {
	absolute, err := cleanAbsolutePath(path)
	if err != nil {
		return "", err
	}
	root, err := safefs.Open(absolute)
	if err != nil {
		return "", err
	}
	if err := root.Close(); err != nil {
		return "", err
	}
	return absolute, nil
}

func pathContains(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

func rejectOverlappingFreezeInputs(inputs []freezeInput) error {
	for left := 0; left < len(inputs); left++ {
		for right := left + 1; right < len(inputs); right++ {
			if pathContains(inputs[left].source, inputs[right].source) || pathContains(inputs[right].source, inputs[left].source) {
				return fmt.Errorf("%s and %s input trees must be disjoint", inputs[left].name, inputs[right].name)
			}
		}
	}
	return nil
}

func inspectFreezeSourceGit(root string, executableClosure *runner.ExecutableClosure) (*freezeSourceGitProvenance, error) {
	if executableClosure == nil {
		return nil, fmt.Errorf("pinned executable closure is required")
	}
	gitPath, err := executableClosure.PathFor("git")
	if err != nil {
		return nil, fmt.Errorf("resolve Git from pinned executable closure: %w", err)
	}
	if err := executableClosure.Revalidate(); err != nil {
		return nil, fmt.Errorf("revalidate pinned executable closure before Git inspection: %w", err)
	}
	present, err := hasAncestorGitMetadata(root)
	var result *freezeSourceGitProvenance
	if err == nil && present {
		var observed experiment.GitProvenance
		observed, err = experiment.InspectGitBundleWithExecutable(root, gitPath)
		if err == nil {
			result = &freezeSourceGitProvenance{
				GitSHA: observed.GitSHA, DirtyPatchDigest: observed.DirtyPatchDigest, Dirty: observed.Dirty,
			}
		}
	}
	if revalidateErr := executableClosure.Revalidate(); revalidateErr != nil {
		return nil, fmt.Errorf("revalidate pinned executable closure after Git inspection: %w", revalidateErr)
	}
	return result, err
}

func hasAncestorGitMetadata(root string) (bool, error) {
	for current := root; ; current = filepath.Dir(current) {
		info, err := os.Lstat(filepath.Join(current, ".git"))
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
				return false, fmt.Errorf("Git metadata must be a directory or regular gitfile, not a link or special file")
			}
			if info.IsDir() {
				head, headErr := os.Lstat(filepath.Join(current, ".git", "HEAD"))
				if headErr != nil || head.Mode()&os.ModeSymlink != 0 || !head.Mode().IsRegular() {
					if current == root {
						return false, fmt.Errorf("bundle-owned .git directory has no regular HEAD")
					}
					// An unrelated malformed ancestor .git directory is not source
					// provenance and also prevents Git's own upward discovery.
					return false, nil
				}
			}
			return true, nil
		case errors.Is(err, os.ErrNotExist):
			// Continue to the filesystem root.
		case err != nil:
			return false, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
	}
}

func cloneFreezeGitProvenance(value *freezeSourceGitProvenance) *freezeSourceGitProvenance {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func equalFreezeGitProvenance(left, right *freezeSourceGitProvenance) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func recheckFreezeInputs(inputs []freezeInput, executableClosure *runner.ExecutableClosure) error {
	for _, input := range inputs {
		current, err := sandbox.DigestTree(input.source, sandbox.DefaultSnapshotLimits())
		if err != nil {
			return invalidFreezeInput(input.name, fmt.Errorf("recheck source tree: %w", err))
		}
		if current.Digest != input.snapshot.Digest {
			return invalidFreezeInput(input.name, fmt.Errorf("source tree changed while freezing"))
		}
		provenance, err := inspectFreezeSourceGit(input.source, executableClosure)
		if err != nil {
			return invalidFreezeInput(input.name, fmt.Errorf("recheck Git provenance: %w", err))
		}
		if !equalFreezeGitProvenance(provenance, input.provenance) {
			return invalidFreezeInput(input.name, fmt.Errorf("source Git provenance changed while freezing"))
		}
	}
	return nil
}

func invalidFreezeInput(name string, err error) error {
	if name == "holdout" {
		return invalidf("invalid_holdout", "%v", privateHoldoutError())
	}
	return invalidf("invalid_frozen_input", "%s bundle: %v", name, err)
}

func createFreezeStage(parent *os.Root) (string, error) {
	for attempt := 0; attempt < 32; attempt++ {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", fmt.Errorf("generate staging name: %w", err)
		}
		name := ".skynex-eval-freeze-" + hex.EncodeToString(nonce[:])
		if err := parent.Mkdir(name, 0o700); err == nil {
			return name, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create staging directory: %w", err)
		}
	}
	return "", fmt.Errorf("could not allocate a unique staging directory")
}

func frozenBundleFromResult(result freezeBundleResult) experiment.FrozenBundle {
	bundle := experiment.FrozenBundle{Root: result.Root, Digest: result.Digest}
	if result.SourceGitProvenance != nil {
		bundle.SourceGitSHA = result.SourceGitProvenance.GitSHA
		bundle.SourceDirtyPatchDigest = result.SourceGitProvenance.DirtyPatchDigest
	}
	return bundle
}
