package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/cases"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/lifecycle"
	"github.com/joeldevz/skynex/internal/eval/metrics"
	"github.com/joeldevz/skynex/internal/eval/reporter"
	"github.com/joeldevz/skynex/internal/eval/runner"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/safefs"
)

type modelFlags struct {
	casesDir        *string
	fixturesDir     *string
	agentBundle     *string
	binary          *string
	expectedVersion *string
	envAllowlist    *string
	openAIOAuth     *string
	traceDir        *string
	retainTrace     *bool
	allowImpure     *bool
	costCap         *float64
	legacyModel     *string
	deprecatedPort  *int
	noLLMJudge      *bool
}

func bindModelFlags(set interface {
	String(string, string, string) *string
	Bool(string, bool, string) *bool
	Float64(string, float64, string) *float64
	Int(string, int, string) *int
}) modelFlags {
	return modelFlags{
		casesDir:        set.String("cases-dir", "eval/cases", "trusted case catalog"),
		fixturesDir:     set.String("fixtures-dir", "eval/fixtures", "trusted fixture root"),
		agentBundle:     set.String("agent-bundle", "opencode", "OpenCode configuration bundle"),
		binary:          set.String("binary", "opencode", "OpenCode binary"),
		expectedVersion: set.String("expected-version", defaultOpenCodeVersion, "exact supported OpenCode version"),
		envAllowlist:    set.String("provider-env", "", "comma-separated provider environment names"),
		openAIOAuth:     set.String("openai-oauth", "", "OpenCode auth.json containing an OpenAI OAuth login"),
		traceDir:        set.String("trace-dir", "eval/results/traces", "sanitized trace directory"),
		retainTrace:     set.Bool("retain-trace", false, "persist a sanitized trace"),
		allowImpure:     set.Bool("allow-impure", false, "explicitly disable OpenCode --pure"),
		costCap: set.Float64(
			"cost-cap", 0,
			"maximum observed USD before stopping; unsupported with --openai-oauth because ChatGPT subscription billing has no authoritative per-request USD",
		),
		legacyModel:    set.String("legacy-model-label", "openai/gpt-5.6-terra", "exact provider/model for migrated legacy cases"),
		deprecatedPort: set.Int("port", 0, "deprecated; evaluation always allocates a private port"),
		noLLMJudge:     set.Bool("no-llm-judge", true, "accepted compatibility flag; deterministic judges remain authoritative"),
	}
}

type modelRunSpec struct {
	Cases                        []contracts.Case
	Suite                        string
	Variant                      string
	Repetitions                  int
	RepetitionStart              int
	UseCaseRuns                  bool
	FixtureRoot                  string
	AgentBundleRoot              string
	Binary                       string
	ExpectedVersion              string
	ExpectedOpenCodeBinaryDigest string
	ExpectedOpenCodeAPIDigest    string
	ExpectedToolchainsDigest     string
	ResolvedBinary               *resolvedOpenCodeBinary
	ExecutableClosure            *runner.ExecutableClosure
	EnvAllowlist                 []string
	OpenAIOAuthFile              string
	OpenAIOAuthSession           *lifecycle.OpenAIOAuthSession
	TraceDir                     string
	RetainTrace                  bool
	AllowImpure                  bool
	CostCapUSD                   float64
	LegacyModelLabel             string
	HarnessDigest                string
	ManifestDigest               string
	VerifiedBundleDigest         string
	RequireExactBundle           bool
}

type modelRunResult struct {
	Result                runner.ContractResult `json:"result"`
	BundleDigest          string                `json:"agent_bundle_digest"`
	HarnessDigest         string                `json:"harness_bundle_digest"`
	ManifestDigest        string                `json:"experiment_manifest_digest"`
	EvaluatorBinaryDigest string                `json:"evaluator_binary_digest"`
	OpenCodeBinaryDigest  string                `json:"opencode_binary_digest"`
	ObservedCostUSD       float64               `json:"-"`
	PublishedObservedCost *float64              `json:"observed_cost_usd,omitempty"`
	CostEvidenceComplete  bool                  `json:"cost_evidence_complete"`
	CostCapUSD            float64               `json:"cost_cap_usd"`
	BudgetExhausted       bool                  `json:"budget_exhausted"`
	EffectiveCases        []contracts.Case      `json:"-"`
}

func (r modelRunResult) CLIExitCode() int {
	base := resultSetExit(r.Result.Samples)
	if base == contracts.ExitInfrastructure || base == contracts.ExitInvalid || base == contracts.ExitAborted {
		return base
	}
	if r.CostCapUSD > 0 && !r.CostEvidenceComplete {
		return contracts.ExitInvalid
	}
	if r.BudgetExhausted {
		return contracts.ExitBudgetExhausted
	}
	return base
}

type runCommandResult struct {
	modelRunResult
	OutputPath string `json:"output_path,omitempty"`
}

func (r runCommandResult) CLIExitCode() int { return r.modelRunResult.CLIExitCode() }

func commandRun(ctx context.Context, args []string, deps dependencies) (runCommandResult, error) {
	set := newFlagSet("run")
	allow := set.Bool("allow-model-calls", false, "authorize model calls that may consume quota or incur provider charges")
	caseID := set.String("case", "", "case identifier")
	repetitions := set.Int("n", 1, "repetitions")
	variant := set.String("variant", "current", "variant label")
	output := set.String("output", "", "optional result JSON path")
	flags := bindModelFlags(set)
	if err := parseFlagSet(set, args); err != nil {
		return runCommandResult{}, err
	}
	if err := requireModelOptIn(*allow); err != nil {
		return runCommandResult{}, err
	}
	if *caseID == "" {
		return runCommandResult{}, invalidf("invalid_arguments", "--case is required")
	}
	if *repetitions < 1 || *repetitions > contracts.MaxRuns {
		return runCommandResult{}, invalidf("invalid_arguments", "--n must be between 1 and %d", contracts.MaxRuns)
	}
	spec, err := buildModelSpec(flags, *variant, *repetitions, false)
	if err != nil {
		return runCommandResult{}, err
	}
	selected, err := loadSelectedCases(*flags.casesDir, "all", *caseID)
	if err != nil {
		return runCommandResult{}, err
	}
	spec.Cases = selected
	spec.Suite = selected[0].Suite
	if deps.runModel == nil {
		return runCommandResult{}, infraf("runner_unavailable", fmt.Errorf("model runner is not configured"))
	}
	result, err := deps.runModel(ctx, spec)
	if err != nil {
		return runCommandResult{}, err
	}
	commandResult := runCommandResult{modelRunResult: result, OutputPath: *output}
	if *output != "" {
		if err := reporter.Save(result.Result, *output); err != nil {
			return runCommandResult{}, infraf("save_result", err)
		}
	}
	return commandResult, nil
}

type baselineCommandResult struct {
	ArtifactPath string `json:"artifact_path"`
	Label        string `json:"label"`
	Suite        string `json:"suite"`
	Samples      int    `json:"samples"`
	Digest       string `json:"integrity_digest"`
	Authority    string `json:"comparison_authority"`
	ExitCode     int    `json:"run_exit_code"`
}

func (r baselineCommandResult) CLIExitCode() int { return r.ExitCode }

func commandBaseline(ctx context.Context, args []string, deps dependencies) (baselineCommandResult, error) {
	set := newFlagSet("baseline")
	allow := set.Bool("allow-model-calls", false, "authorize model calls that may consume quota or incur provider charges")
	suite := set.String("suite", "", "suite identifier")
	repetitions := set.Int("n", 0, "uniform repetitions; zero uses each case contract")
	label := set.String("label", "control", "baseline label")
	output := set.String("output", "", "artifact JSON path")
	flags := bindModelFlags(set)
	if err := parseFlagSet(set, args); err != nil {
		return baselineCommandResult{}, err
	}
	if err := requireModelOptIn(*allow); err != nil {
		return baselineCommandResult{}, err
	}
	if *suite == "" {
		return baselineCommandResult{}, invalidf("invalid_arguments", "--suite is required for a model baseline")
	}
	if *repetitions < 0 || *repetitions > contracts.MaxRuns {
		return baselineCommandResult{}, invalidf("invalid_arguments", "--n must be zero or between 1 and %d", contracts.MaxRuns)
	}
	spec, err := buildModelSpec(flags, "control", *repetitions, *repetitions == 0)
	if err != nil {
		return baselineCommandResult{}, err
	}
	selected, err := loadSelectedCases(*flags.casesDir, *suite, "")
	if err != nil {
		return baselineCommandResult{}, err
	}
	spec.Cases, spec.Suite = selected, *suite
	if deps.runModel == nil {
		return baselineCommandResult{}, infraf("runner_unavailable", fmt.Errorf("model runner is not configured"))
	}
	runResult, err := deps.runModel(ctx, spec)
	if err != nil {
		return baselineCommandResult{}, err
	}
	fingerprint, err := fingerprintForRun(runResult, selected)
	if err != nil {
		if exitCode := runResult.CLIExitCode(); exitCode != contracts.ExitSuccess {
			return baselineCommandResult{
				Label: *label, Suite: *suite, Samples: len(runResult.Result.Samples),
				Authority: evaluationAuthorityExploratory, ExitCode: exitCode,
			}, nil
		}
		return baselineCommandResult{}, invalidf("invalid_fingerprint", "%v", err)
	}
	aggregates, err := artifactAggregates(selected)
	if err != nil {
		return baselineCommandResult{}, invalidf("invalid_case_digest", "%v", err)
	}
	aggregates[evaluationAuthorityAggregateKey] = encodeEvaluationAuthority(evaluationAuthorityMetadata{
		Mode:   evaluationAuthorityExploratory,
		Reason: "standalone baseline has no frozen experiment manifest; use ab for authoritative paired evidence",
	})
	artifact, err := baseline.NewRunArtifact(*label, *suite, time.Now().UTC(), fingerprint, runResult.Result.Samples, aggregates)
	if err != nil {
		return baselineCommandResult{}, invalidf("invalid_baseline", "%v", err)
	}
	path := *output
	if path == "" {
		path = filepath.Join("eval", "results", "baseline-"+time.Now().UTC().Format("20060102T150405Z")+".json")
	}
	if err := artifact.Save(path, baseline.IOOptions{}); err != nil {
		return baselineCommandResult{}, infraf("save_baseline", err)
	}
	return baselineCommandResult{
		ArtifactPath: path, Label: artifact.Label, Suite: artifact.Suite,
		Samples: len(artifact.Samples), Digest: artifact.Integrity.Digest,
		Authority: evaluationAuthorityExploratory, ExitCode: runResult.CLIExitCode(),
	}, nil
}

func buildModelSpec(flags modelFlags, variant string, repetitions int, useCaseRuns bool) (modelRunSpec, error) {
	if *flags.deprecatedPort != 0 {
		return modelRunSpec{}, invalidf("invalid_arguments", "--port is no longer supported; each run uses a private ephemeral port")
	}
	if !*flags.noLLMJudge {
		return modelRunSpec{}, invalidf("invalid_arguments", "the deterministic runner does not enable an ambient LLM judge")
	}
	if *flags.binary == "" || *flags.expectedVersion == "" || *flags.agentBundle == "" {
		return modelRunSpec{}, invalidf("invalid_arguments", "--binary, --expected-version and --agent-bundle must not be empty")
	}
	if *flags.costCap < 0 {
		return modelRunSpec{}, invalidf("invalid_arguments", "--cost-cap must not be negative")
	}
	envNames, err := parseEnvNames(*flags.envAllowlist)
	if err != nil {
		return modelRunSpec{}, invalidf("invalid_arguments", "%v", err)
	}
	openAIOAuthFile, err := resolveOpenAIOAuthFile(*flags.openAIOAuth, envNames)
	if err != nil {
		return modelRunSpec{}, err
	}
	if openAIOAuthFile != "" && *flags.costCap > 0 {
		return modelRunSpec{}, invalidf(
			"subscription_cost_cap_unsupported",
			"--cost-cap cannot enforce ChatGPT subscription quota because --openai-oauth has no authoritative per-request USD; frozen counts/timeouts bound scheduled samples only, not provider calls, tokens, or quota",
		)
	}
	if openAIOAuthFile != "" && *flags.allowImpure {
		return modelRunSpec{}, invalidf("impure_oauth_forbidden", "--openai-oauth requires the clean OpenCode --pure profile")
	}
	if openAIOAuthFile != "" && *flags.retainTrace {
		return modelRunSpec{}, invalidf("oauth_trace_retention_forbidden", "runtime-readable OAuth forbids persisted traces")
	}
	if len(envNames) != 0 && *flags.retainTrace {
		return modelRunSpec{}, invalidf("credential_trace_retention_forbidden", "runtime-readable provider environment forbids persisted traces")
	}
	// Direct run/baseline artifacts are deliberately non-authoritative. Use a
	// stable marker instead of raw CLI paths: path spelling must not create a
	// false compatibility difference, while compare still rejects this digest
	// because it cannot equal the required frozen experiment manifest digest.
	manifestDigest, _ := contracts.CanonicalDigest(map[string]any{
		"schema_version": 1,
		"kind":           "skynex-eval-exploratory-provenance",
		"authority":      evaluationAuthorityExploratory,
	})
	return modelRunSpec{
		Variant: variant, Repetitions: repetitions, RepetitionStart: 1, UseCaseRuns: useCaseRuns,
		FixtureRoot: *flags.fixturesDir, AgentBundleRoot: *flags.agentBundle,
		Binary: *flags.binary, ExpectedVersion: *flags.expectedVersion, EnvAllowlist: envNames,
		OpenAIOAuthFile: openAIOAuthFile,
		TraceDir:        *flags.traceDir, RetainTrace: *flags.retainTrace, AllowImpure: *flags.allowImpure,
		CostCapUSD: *flags.costCap, LegacyModelLabel: *flags.legacyModel, ManifestDigest: manifestDigest,
	}, nil
}

func resolveOpenAIOAuthFile(path string, providerEnv []string) (string, error) {
	if path == "" {
		return "", nil
	}
	if strings.TrimSpace(path) == "" {
		return "", invalidf("invalid_arguments", "--openai-oauth must not be blank")
	}
	if len(providerEnv) != 0 {
		return "", invalidf("credential_sources_conflict", "--openai-oauth and --provider-env are mutually exclusive")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", invalidf("invalid_arguments", "resolve --openai-oauth: %v", err)
	}
	return filepath.Clean(absolute), nil
}

func loadSelectedCases(root, suite, caseID string) ([]contracts.Case, error) {
	loaded, err := cases.LoadAllContracts(root)
	if err != nil {
		return nil, invalidf("invalid_cases", "load cases: %v", err)
	}
	selected := selectCases(loaded, suite, caseID)
	if len(selected) == 0 {
		return nil, invalidf("empty_selection", "no cases selected")
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	return selected, nil
}

func executeModelRuns(ctx context.Context, spec modelRunSpec) (result modelRunResult, returnErr error) {
	if len(spec.Cases) == 0 {
		return result, invalidf("empty_selection", "no cases selected")
	}
	var resolvedBinary resolvedOpenCodeBinary
	var err error
	if spec.ResolvedBinary != nil {
		resolvedBinary = *spec.ResolvedBinary
		if spec.Binary != "" && spec.Binary != resolvedBinary.Path {
			return result, invalidf("opencode_binary_mismatch", "model spec binary %s does not match pre-resolved path %s", spec.Binary, resolvedBinary.Path)
		}
		if err := resolvedBinary.Revalidate(); err != nil {
			return result, invalidf("opencode_binary_mismatch", "pre-resolved OpenCode executable drifted: %v", err)
		}
	} else {
		resolvedBinary, err = resolveOpenCodeBinary(spec.Binary)
		if err != nil {
			return result, infraf("opencode_provenance", fmt.Errorf("resolve OpenCode executable once: %w", err))
		}
	}
	spec.Binary = resolvedBinary.Path
	oauthSession := spec.OpenAIOAuthSession
	if oauthSession == nil && spec.OpenAIOAuthFile != "" {
		var authErr error
		oauthSession, authErr = lifecycle.NewOpenAIOAuthSession(spec.OpenAIOAuthFile)
		if authErr != nil {
			return result, invalidf("invalid_openai_oauth", "%v", authErr)
		}
	}
	privateRoot, err := os.MkdirTemp("", "skynex-eval-run-")
	if err != nil {
		return result, infraf("private_workspace", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(privateRoot); cleanupErr != nil {
			markDeferredModelRunFailure(&result)
			returnErr = errors.Join(returnErr, infraf("private_workspace_cleanup", cleanupErr))
		}
	}()
	runsRoot := filepath.Join(privateRoot, "runs")
	preparedFixtures := filepath.Join(privateRoot, "fixtures")
	preparedBundle := filepath.Join(privateRoot, "agent-bundle")
	for _, directory := range []string{runsRoot, preparedFixtures, preparedBundle} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return result, infraf("private_workspace", err)
		}
	}

	preparedCases, fixtureSetDigest, err := prepareFixtures(spec.Cases, spec.FixtureRoot, preparedFixtures, spec.LegacyModelLabel)
	if err != nil {
		return result, invalidf("invalid_fixtures", "%v", err)
	}
	if spec.RequireExactBundle {
		absoluteBundle, absoluteErr := filepath.Abs(spec.AgentBundleRoot)
		if absoluteErr != nil {
			return result, invalidf("invalid_agent_bundle", "%v", absoluteErr)
		}
		if _, copyErr := sandbox.CopyVerifiedTree(absoluteBundle, preparedBundle, sandbox.DefaultSnapshotLimits()); copyErr != nil {
			return result, invalidf("invalid_agent_bundle", "%v", copyErr)
		}
		if credentialErr := rejectCredentialLikeTree(preparedBundle); credentialErr != nil {
			return result, invalidf("invalid_agent_bundle", "%v", credentialErr)
		}
	} else if err := copyConfigBundle(spec.AgentBundleRoot, preparedBundle); err != nil {
		return result, invalidf("invalid_agent_bundle", "%v", err)
	}
	bundleSnapshot, err := sandbox.DigestTree(preparedBundle, sandbox.DefaultSnapshotLimits())
	if err != nil {
		return result, invalidf("invalid_agent_bundle", "%v", err)
	}
	if spec.VerifiedBundleDigest != "" && bundleSnapshot.Digest != spec.VerifiedBundleDigest {
		return result, invalidf("bundle_digest_mismatch", "prepared agent bundle got %s, expected %s", bundleSnapshot.Digest, spec.VerifiedBundleDigest)
	}
	executableClosure := spec.ExecutableClosure
	if executableClosure == nil {
		executableClosure, err = runner.ResolveExecutableClosure(preparedCases, "git")
		if err != nil {
			return result, invalidf("invalid_toolchain_closure", "%v", err)
		}
	} else if err := executableClosure.Revalidate(); err != nil {
		return result, invalidf("invalid_toolchain_closure", "%v", err)
	}
	if spec.ExpectedToolchainsDigest != "" && executableClosure.Digest() != spec.ExpectedToolchainsDigest {
		return result, invalidf("toolchains_mismatch", "got %s, expected %s", executableClosure.Digest(), spec.ExpectedToolchainsDigest)
	}
	gitPath, err := executableClosure.PathFor("git")
	if err != nil {
		return result, invalidf("invalid_toolchain_closure", "%v", err)
	}
	gitSHA, err := currentGitSHA(ctx, gitPath)
	if err != nil {
		return result, invalidf("git_provenance", "%v", err)
	}
	evaluatorDigest, err := executableDigest()
	if err != nil {
		return result, infraf("evaluator_provenance", err)
	}
	openCodeDigest := resolvedBinary.Digest
	if spec.ExpectedOpenCodeBinaryDigest != "" && openCodeDigest != spec.ExpectedOpenCodeBinaryDigest {
		return result, invalidf("opencode_binary_mismatch", "got %s, expected %s", openCodeDigest, spec.ExpectedOpenCodeBinaryDigest)
	}
	harnessDigest := spec.HarnessDigest
	if harnessDigest == "" {
		harnessDigest, _ = contracts.CanonicalDigest(map[string]any{"fixtures": fixtureSetDigest, "cases": effectiveCaseDigest(preparedCases)})
	}
	pricing, err := metrics.NewPricingTable("unavailable-v1", nil)
	if err != nil {
		return result, infraf("pricing", err)
	}
	toolsetDigest, _ := contracts.CanonicalDigest(toolPolicies(preparedCases))
	judgeDigest, _ := contracts.CanonicalDigest(map[string]string{"authority": "deterministic-v1"})
	openCodeFactory := runner.OpenCodeFactory{
		Binary: spec.Binary, ExpectedVersion: spec.ExpectedVersion,
		EnvAllowlist: append([]string(nil), spec.EnvAllowlist...), StartupTimeout: 30 * time.Second,
		AllowImpure: spec.AllowImpure, OpenAIOAuthSession: oauthSession,
	}
	engine, err := runner.NewEngine(runner.EngineConfig{
		RunParent: runsRoot, FixtureRoot: preparedFixtures,
		AgentBundleRoot: preparedBundle, BundleDigest: bundleSnapshot.Digest,
		ExecutableClosure: executableClosure,
		Factory: pinnedRuntimeFactory{
			Inner: openCodeFactory, Binary: spec.Binary,
			ResolvedBinary:       &resolvedBinary,
			ExpectedBinaryDigest: openCodeDigest,
			ExpectedAPIDigest:    spec.ExpectedOpenCodeAPIDigest,
		},
		Pricing: pricing, SnapshotLimits: sandbox.DefaultSnapshotLimits(), TraceDir: spec.TraceDir,
		Provenance: runner.ProvenanceInputs{
			GitSHA: gitSHA, OpenCodeVersion: spec.ExpectedVersion,
			PromptDigest: bundleSnapshot.Digest, ConfigDigest: bundleSnapshot.Digest,
			ToolsetDigest: toolsetDigest, JudgeDigest: judgeDigest,
			ToolchainsDigest: executableClosure.Digest(),
			BundleDigest:     bundleSnapshot.Digest, HarnessDigest: harnessDigest,
			ManifestDigest: spec.ManifestDigest,
		},
	})
	if err != nil {
		return result, invalidf("invalid_runner_configuration", "%v", err)
	}

	contractResult := runner.ContractResult{Suite: spec.Suite, Started: time.Now().UTC(), Complete: true}
	costComplete := true
	costTrackingInvalid := false
	observedCost := 0.0
	recordSample := func(sample contracts.RunResult) {
		contractResult.Samples = append(contractResult.Samples, sample)
		cost, available := observedSampleCost(sample)
		if available {
			observedCost += cost
		} else {
			costComplete = false
			if spec.CostCapUSD > 0 {
				costTrackingInvalid = true
				contractResult.Complete = false
			}
		}
		if sample.Status == contracts.RunStatusAborted || sample.Status == contracts.RunStatusInfraError || sample.Status == contracts.RunStatusBudgetExhausted {
			contractResult.Complete = false
		}
	}
	finalizeResult := func(failed bool) {
		contractResult.Ended = time.Now().UTC()
		result.Result = contractResult
		result.BundleDigest = bundleSnapshot.Digest
		result.HarnessDigest = harnessDigest
		result.ManifestDigest = spec.ManifestDigest
		result.EvaluatorBinaryDigest = evaluatorDigest
		result.OpenCodeBinaryDigest = openCodeDigest
		result.ObservedCostUSD = observedCost
		result.CostEvidenceComplete = costComplete && !failed
		result.PublishedObservedCost = observedCostPointer(observedCost, result.CostEvidenceComplete)
		result.CostCapUSD = spec.CostCapUSD
		result.EffectiveCases = preparedCases
	}
	for _, testCase := range preparedCases {
		repetitions := spec.Repetitions
		if spec.UseCaseRuns {
			repetitions = testCase.Runs.Count
		}
		if repetitions < 1 || repetitions > contracts.MaxRuns {
			return result, invalidf("invalid_repetitions", "case %s repetitions must be between 1 and %d", testCase.ID, contracts.MaxRuns)
		}
		start := spec.RepetitionStart
		if start == 0 {
			start = 1
		}
		for offset := 0; offset < repetitions; offset++ {
			repetition := start + offset
			if repetition > contracts.MaxRuns {
				return result, invalidf("invalid_repetitions", "repetition %d exceeds %d", repetition, contracts.MaxRuns)
			}
			if spec.CostCapUSD > 0 && observedCost >= spec.CostCapUSD {
				result.BudgetExhausted = true
				contractResult.Complete = false
				break
			}
			if digestErr := resolvedBinary.Revalidate(); digestErr != nil {
				return result, invalidf("opencode_binary_mismatch", "OpenCode executable drifted before sample: %v", digestErr)
			}
			sample, runErr := engine.Run(ctx, testCase, runner.RunRequest{
				Variant: spec.Variant, Repetition: repetition, RetainTrace: spec.RetainTrace,
			})
			if digestErr := resolvedBinary.Revalidate(); digestErr != nil {
				contractResult.Complete = false
				finalizeResult(true)
				return result, invalidf("opencode_binary_mismatch", "OpenCode executable drifted during sample: %v", digestErr)
			}
			if closureErr := executableClosure.Revalidate(); closureErr != nil {
				contractResult.Complete = false
				finalizeResult(true)
				return result, invalidf("invalid_toolchain_closure", "effective executable closure drifted during sample: %v", closureErr)
			}
			if bundleErr := revalidatePreparedAgentBundle(preparedBundle, bundleSnapshot.Digest); bundleErr != nil {
				contractResult.Complete = false
				finalizeResult(true)
				return result, invalidf("bundle_digest_mismatch", "%v", bundleErr)
			}
			if runErr != nil {
				retained, retainErr := prepareDeferredEngineSample(sample)
				if retainErr != nil {
					runErr = errors.Join(runErr, retainErr)
				}
				if retained != nil {
					recordSample(*retained)
				}
				contractResult.Complete = false
				finalizeResult(true)
				return result, infraf("runner_failure", runErr)
			}
			recordSample(sample)
			if ctx.Err() != nil || costTrackingInvalid {
				contractResult.Complete = false
				break
			}
		}
		if result.BudgetExhausted || ctx.Err() != nil || costTrackingInvalid {
			break
		}
	}
	if len(contractResult.Samples) == 0 && !result.BudgetExhausted {
		return result, invalidf("empty_result", "runner produced no samples")
	}
	if spec.CostCapUSD > 0 && observedCost >= spec.CostCapUSD {
		result.BudgetExhausted = true
		contractResult.Complete = false
	}
	finalizeResult(false)
	return result, nil
}

// prepareDeferredEngineSample preserves paid work only when Engine.Run
// returned a fully valid, non-invalid result and a later cleanup failed. The
// A/B layer applies the same marker defensively; calling it here ensures direct
// model callers never observe a PASS paired with an error.
func prepareDeferredEngineSample(sample contracts.RunResult) (*contracts.RunResult, error) {
	if err := sample.Validate(); err != nil {
		return nil, fmt.Errorf("discard invalid sample returned with deferred runner error: %w", err)
	}
	if sample.Status == contracts.RunStatusInvalid || sample.Error != nil && sample.Error.Kind == "toolchain_drift" {
		return nil, nil
	}
	retained := []contracts.RunResult{sample}
	markDeferredABSampleFailure(retained)
	if err := retained[0].Validate(); err != nil {
		return nil, fmt.Errorf("deferred runner sample became invalid after fail-closed marking: %w", err)
	}
	return &retained[0], nil
}

func markDeferredModelRunFailure(result *modelRunResult) {
	if result == nil {
		return
	}
	retained := make([]contracts.RunResult, 0, len(result.Result.Samples))
	for _, sample := range result.Result.Samples {
		prepared, _ := prepareDeferredEngineSample(sample)
		if prepared != nil {
			retained = append(retained, *prepared)
		}
	}
	result.Result.Samples = retained
	result.Result.Complete = false
	result.CostEvidenceComplete = false
	result.PublishedObservedCost = nil
}

func revalidatePreparedAgentBundle(path, expectedDigest string) error {
	current, err := sandbox.DigestTree(path, sandbox.DefaultSnapshotLimits())
	if err != nil {
		return fmt.Errorf("revalidate prepared agent bundle: %w", err)
	}
	if current.Digest != expectedDigest {
		return fmt.Errorf("prepared agent bundle drifted: got %s, expected %s", current.Digest, expectedDigest)
	}
	return nil
}

func observedCostPointer(value float64, complete bool) *float64 {
	if !complete {
		return nil
	}
	result := value
	return &result
}

// pinnedRuntimeFactory rechecks paid A/B runtime inputs for every sample. The
// wrapped factory only starts and probes OpenCode; this fence runs before the
// engine creates a session or sends a model request.
type pinnedRuntimeFactory struct {
	Inner                runner.RuntimeFactory
	Binary               string
	ResolvedBinary       *resolvedOpenCodeBinary
	ExpectedBinaryDigest string
	ExpectedAPIDigest    string
}

func (f pinnedRuntimeFactory) Start(ctx context.Context, request runner.RuntimeRequest) (runner.Runtime, error) {
	if f.Inner == nil {
		return nil, fmt.Errorf("runtime factory is required")
	}
	if err := f.verifyBinary(); err != nil {
		return nil, err
	}
	runtimeHandle, err := f.Inner.Start(ctx, request)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (runner.Runtime, error) {
		return nil, errors.Join(cause, runtimeHandle.Close())
	}
	if err := f.verifyBinary(); err != nil {
		return fail(err)
	}
	if f.ExpectedAPIDigest != "" && runtimeHandle.Info().OpenCodeAPI != f.ExpectedAPIDigest {
		return fail(fmt.Errorf("OpenCode OpenAPI digest mismatch: got %s, expected %s", runtimeHandle.Info().OpenCodeAPI, f.ExpectedAPIDigest))
	}
	return runtimeHandle, nil
}

func (f pinnedRuntimeFactory) verifyBinary() error {
	if f.ExpectedBinaryDigest == "" {
		return nil
	}
	if f.ResolvedBinary != nil {
		if f.ResolvedBinary.Digest != f.ExpectedBinaryDigest {
			return fmt.Errorf("OpenCode binary digest mismatch: got %s, expected %s", f.ResolvedBinary.Digest, f.ExpectedBinaryDigest)
		}
		if err := f.ResolvedBinary.Revalidate(); err != nil {
			return fmt.Errorf("revalidate OpenCode binary: %w", err)
		}
		return nil
	}
	digest, err := binaryDigest(f.Binary)
	if err != nil {
		return fmt.Errorf("fingerprint OpenCode binary: %w", err)
	}
	if digest != f.ExpectedBinaryDigest {
		return fmt.Errorf("OpenCode binary digest mismatch: got %s, expected %s", digest, f.ExpectedBinaryDigest)
	}
	return nil
}

func prepareFixtures(sourceCases []contracts.Case, sourceRoot, destinationRoot, legacyModel string) ([]contracts.Case, string, error) {
	prepared := append([]contracts.Case(nil), sourceCases...)
	copied := make(map[string]string)
	for i := range prepared {
		testCase := &prepared[i]
		source := testCase.Fixture.Source
		if source == "" {
			source = "_legacy-empty"
			testCase.Fixture.Source = source
		}
		if digest, exists := copied[source]; exists {
			if testCase.Migration != nil {
				testCase.Fixture.ExpectedDigest = digest
			} else if testCase.Fixture.ExpectedDigest != digest {
				return nil, "", fmt.Errorf("fixture %s digest mismatch: got %s, expected %s", source, digest, testCase.Fixture.ExpectedDigest)
			}
			continue
		}
		destination, err := resolveWithin(destinationRoot, source)
		if err != nil {
			return nil, "", err
		}
		if err := os.MkdirAll(destination, 0o700); err != nil {
			return nil, "", err
		}
		var snapshot sandbox.Snapshot
		if testCase.Fixture.Source == "_legacy-empty" {
			snapshot, err = sandbox.DigestTree(destination, sandbox.DefaultSnapshotLimits())
		} else {
			original, resolveErr := resolveWithin(sourceRoot, testCase.Fixture.Source)
			if resolveErr != nil {
				return nil, "", resolveErr
			}
			snapshot, err = sandbox.CopyVerifiedTree(original, destination, sandbox.DefaultSnapshotLimits())
		}
		if err != nil {
			return nil, "", fmt.Errorf("prepare fixture %s: %w", source, err)
		}
		if testCase.Migration == nil && snapshot.Digest != testCase.Fixture.ExpectedDigest {
			return nil, "", fmt.Errorf("fixture %s digest mismatch: got %s, expected %s", source, snapshot.Digest, testCase.Fixture.ExpectedDigest)
		}
		copied[source] = snapshot.Digest
		if testCase.Migration != nil {
			testCase.Fixture.ExpectedDigest = snapshot.Digest
		}
	}
	for i := range prepared {
		testCase := &prepared[i]
		if testCase.Migration == nil {
			continue
		}
		if testCase.Agent.Model == "" {
			testCase.Agent.Model = legacyModel
		}
		if _, _, err := contracts.ParseModelSelection(testCase.Agent.Model); err != nil {
			return nil, "", fmt.Errorf("case %s agent model: %w", testCase.ID, err)
		}
		if len(testCase.RequirementIDs) == 0 {
			testCase.RequirementIDs = []string{"LEGACY:" + testCase.ID}
		}
		for checkIndex := range testCase.BehaviorChecks {
			if len(testCase.BehaviorChecks[checkIndex].RequirementIDs) == 0 {
				testCase.BehaviorChecks[checkIndex].RequirementIDs = append([]string(nil), testCase.RequirementIDs...)
			}
		}
		testCase.Normalize()
		if err := testCase.Validate(); err != nil {
			return nil, "", fmt.Errorf("prepared legacy case %s: %w", testCase.ID, err)
		}
	}
	digest, err := contracts.CanonicalDigest(copied)
	return prepared, digest, err
}

func copyConfigBundle(sourceRoot, destinationRoot string) error {
	absoluteSource, err := filepath.Abs(sourceRoot)
	if err != nil {
		return err
	}
	source, err := safefs.Open(absoluteSource)
	if err != nil {
		return fmt.Errorf("open agent bundle: %w", err)
	}
	defer source.Close()
	before, err := inspectConfigBundle(absoluteSource, source, func(relative string, info fs.FileInfo, data []byte) error {
		destination := filepath.Join(destinationRoot, filepath.FromSlash(relative))
		if info.IsDir() {
			return os.Mkdir(destination, info.Mode().Perm()|0o700)
		}
		output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, writeErr := output.Write(data)
		closeErr := output.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	after, err := inspectConfigBundle(absoluteSource, source, nil)
	if err != nil {
		return err
	}
	beforeDigest, _ := contracts.CanonicalDigest(before)
	afterDigest, _ := contracts.CanonicalDigest(after)
	if beforeDigest != afterDigest {
		return fmt.Errorf("agent bundle changed while copying: got %s, expected %s", afterDigest, beforeDigest)
	}
	return nil
}

type configBundleEntry struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Digest string `json:"digest,omitempty"`
}

func inspectConfigBundle(absoluteSource string, source *os.Root, visit func(string, fs.FileInfo, []byte) error) ([]configBundleEntry, error) {
	const maxFileBytes = int64(32 << 20)
	const maxTotalBytes = int64(256 << 20)
	const maxEntries = 10_000
	result := make([]configBundleEntry, 0)
	totalBytes := int64(0)
	err := filepath.WalkDir(absoluteSource, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(absoluteSource, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		first := strings.Split(filepath.ToSlash(relative), "/")[0]
		if entry.IsDir() && (first == "node_modules" || first == ".git" || first == ".sandbox") {
			return filepath.SkipDir
		}
		relative = filepath.ToSlash(relative)
		if err := contracts.ValidateRelativePath(relative); err != nil {
			return err
		}
		if credentialLikeBundlePath(relative) {
			return fmt.Errorf("agent bundle contains credential-like file %q", relative)
		}
		entryInfo, err := source.Lstat(relative)
		if err != nil {
			return err
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() && !entryInfo.IsDir() {
			return fmt.Errorf("agent bundle contains unsafe entry %q", relative)
		}
		if len(result) >= maxEntries {
			return fmt.Errorf("agent bundle exceeds %d entries", maxEntries)
		}
		if entryInfo.IsDir() {
			result = append(result, configBundleEntry{Path: relative, Mode: uint32(entryInfo.Mode().Perm())})
			if visit != nil {
				return visit(relative, entryInfo, nil)
			}
			return nil
		}
		data, err := safefs.ReadFileVerified(source, relative, maxFileBytes)
		if err != nil {
			return err
		}
		totalBytes += int64(len(data))
		if totalBytes > maxTotalBytes {
			return fmt.Errorf("agent bundle exceeds %d bytes", maxTotalBytes)
		}
		result = append(result, configBundleEntry{Path: relative, Mode: uint32(entryInfo.Mode().Perm()), Digest: digestBytes(data)})
		if visit != nil {
			return visit(relative, entryInfo, data)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func credentialLikeBundlePath(relative string) bool {
	base := strings.ToLower(filepath.Base(filepath.FromSlash(relative)))
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true
	}
	switch base {
	case "auth.json", "credentials", "credentials.json", "accounts.json", "antigravity-accounts.json", "service-account.json":
		return true
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".pem", ".p12", ".pfx", ".key":
		return true
	default:
		return false
	}
}

func rejectCredentialLikeTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if credentialLikeBundlePath(filepath.ToSlash(relative)) {
			return fmt.Errorf("agent bundle contains credential-like file %q", filepath.ToSlash(relative))
		}
		return nil
	})
}

func currentGitSHA(ctx context.Context, gitPaths ...string) (string, error) {
	gitPath := "git"
	if len(gitPaths) != 0 && gitPaths[0] != "" {
		gitPath = gitPaths[0]
	}
	command := exec.CommandContext(ctx, gitPath, "rev-parse", "HEAD")
	data, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if len(value) < 40 || len(value) > 64 {
		return "", fmt.Errorf("unexpected git SHA %q", value)
	}
	if _, err := strconv.ParseUint(value[:16], 16, 64); err != nil {
		return "", fmt.Errorf("unexpected git SHA %q", value)
	}
	return value, nil
}

func executableDigest() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return regularFileDigest(path)
}

type resolvedOpenCodeBinary struct {
	Path   string
	Digest string

	launcher *runner.ExecutableSnapshot
	target   *runner.ExecutableSnapshot
}

func (r resolvedOpenCodeBinary) Revalidate() error {
	if r.launcher == nil || r.Path == "" || r.Digest == "" {
		return fmt.Errorf("resolved OpenCode executable snapshot is incomplete")
	}
	if r.launcher.Path() != r.Path {
		return fmt.Errorf("resolved OpenCode launcher path changed from %s to %s", r.Path, r.launcher.Path())
	}
	if err := r.launcher.Revalidate(); err != nil {
		return fmt.Errorf("revalidate OpenCode launcher: %w", err)
	}
	expectedDigest := r.launcher.ContentDigest()
	if r.target != nil {
		if err := r.target.Revalidate(); err != nil {
			return fmt.Errorf("revalidate OpenCode launcher target: %w", err)
		}
		var err error
		expectedDigest, err = contracts.CanonicalDigest(map[string]string{
			"kind": "cmd-shim-v1", "launcher": r.launcher.ContentDigest(), "target": r.target.ContentDigest(),
		})
		if err != nil {
			return err
		}
	}
	if expectedDigest != r.Digest {
		return fmt.Errorf("resolved OpenCode digest changed from %s to %s", r.Digest, expectedDigest)
	}
	return nil
}

func resolveOpenCodeBinary(binary string) (resolvedOpenCodeBinary, error) {
	launcher, err := runner.ResolveExecutableSnapshot(binary)
	if err != nil {
		return resolvedOpenCodeBinary{}, fmt.Errorf("find OpenCode binary: %w", err)
	}
	path := launcher.Path()
	launcherDigest := launcher.ContentDigest()
	native, contents, err := inspectExecutable(path)
	if err != nil {
		return resolvedOpenCodeBinary{}, err
	}
	if native {
		if err := launcher.Revalidate(); err != nil {
			return resolvedOpenCodeBinary{}, err
		}
		return resolvedOpenCodeBinary{Path: path, Digest: launcherDigest, launcher: launcher}, nil
	}
	target, err := cmdShimTarget(contents)
	if err != nil {
		return resolvedOpenCodeBinary{}, fmt.Errorf("OpenCode launcher %q: %w", path, err)
	}
	targetSnapshot, err := runner.ResolveExecutableSnapshot(target)
	if err != nil {
		return resolvedOpenCodeBinary{}, fmt.Errorf("resolve OpenCode launcher target: %w", err)
	}
	targetNative, _, err := inspectExecutable(targetSnapshot.Path())
	if err != nil {
		return resolvedOpenCodeBinary{}, fmt.Errorf("inspect OpenCode launcher target: %w", err)
	}
	if !targetNative {
		return resolvedOpenCodeBinary{}, fmt.Errorf("OpenCode launcher target %q is not a native executable", target)
	}
	if err := launcher.Revalidate(); err != nil {
		return resolvedOpenCodeBinary{}, fmt.Errorf("revalidate OpenCode launcher: %w", err)
	}
	if err := targetSnapshot.Revalidate(); err != nil {
		return resolvedOpenCodeBinary{}, fmt.Errorf("revalidate OpenCode launcher target: %w", err)
	}
	digest, err := contracts.CanonicalDigest(map[string]string{
		"kind": "cmd-shim-v1", "launcher": launcherDigest, "target": targetSnapshot.ContentDigest(),
	})
	if err != nil {
		return resolvedOpenCodeBinary{}, err
	}
	return resolvedOpenCodeBinary{Path: path, Digest: digest, launcher: launcher, target: targetSnapshot}, nil
}

func binaryDigest(binary string) (string, error) {
	resolved, err := resolveOpenCodeBinary(binary)
	if err != nil {
		return "", err
	}
	return resolved.Digest, nil
}

func inspectExecutable(path string) (bool, []byte, error) {
	const maxLauncherBytes = 1 << 20
	file, err := os.Open(path)
	if err != nil {
		return false, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, nil, fmt.Errorf("%q is not a regular file", path)
	}
	header := make([]byte, 4)
	n, readErr := io.ReadFull(file, header)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return false, nil, readErr
	}
	header = header[:n]
	if nativeExecutableMagic(header) {
		return true, nil, nil
	}
	if info.Size() < 0 || info.Size() > maxLauncherBytes {
		return false, nil, fmt.Errorf("non-native launcher %q exceeds %d bytes", path, maxLauncherBytes)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, nil, err
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxLauncherBytes+1))
	if err != nil {
		return false, nil, err
	}
	if int64(len(contents)) != info.Size() {
		return false, nil, fmt.Errorf("%q changed while inspecting", path)
	}
	return false, contents, nil
}

func nativeExecutableMagic(header []byte) bool {
	if len(header) >= 4 {
		switch string(header[:4]) {
		case "\x7fELF", "\xfe\xed\xfa\xce", "\xfe\xed\xfa\xcf", "\xce\xfa\xed\xfe", "\xcf\xfa\xed\xfe", "\xca\xfe\xba\xbe", "\xbe\xba\xfe\xca":
			return true
		}
	}
	return len(header) >= 2 && header[0] == 'M' && header[1] == 'Z'
}

func cmdShimTarget(contents []byte) (string, error) {
	const marker = "# cmd-shim-target="
	var target string
	for _, line := range strings.Split(strings.ReplaceAll(string(contents), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, marker) {
			continue
		}
		if target != "" {
			return "", fmt.Errorf("contains multiple %s markers", strings.TrimSuffix(marker, "="))
		}
		target = strings.TrimSpace(strings.TrimPrefix(line, marker))
	}
	if target == "" {
		return "", fmt.Errorf("is a script/wrapper without a pinned cmd-shim target")
	}
	if strings.ContainsRune(target, '\x00') || !filepath.IsAbs(target) {
		return "", fmt.Errorf("cmd-shim target must be an absolute path")
	}
	return filepath.Clean(target), nil
}

func regularFileDigest(path string) (string, error) {
	const maxDigestBytes = int64(1 << 30)
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", path)
	}
	if info.Size() < 0 || info.Size() > maxDigestBytes {
		return "", fmt.Errorf("%q exceeds the %d-byte digest limit", path, maxDigestBytes)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maxDigestBytes+1))
	if err != nil {
		return "", err
	}
	if written != info.Size() {
		return "", fmt.Errorf("%q changed while hashing", path)
	}
	return "sha256:" + fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func observedSampleCost(sample contracts.RunResult) (float64, bool) {
	if sample.Provenance.Extensions[contracts.ProvenanceExtensionBillingMode] == contracts.BillingModeChatGPTSubscription {
		// calculated_cost_usd may remain in the result as an explicitly
		// counterfactual API-price estimate. It is not observed subscription
		// spend and must never drive a USD budget or be reported as such.
		return 0, false
	}
	if sample.Provenance.ProviderCostUSDAuthoritative() && sample.Usage.Tree.ProviderCostUSD != nil {
		return *sample.Usage.Tree.ProviderCostUSD, true
	}
	if sample.Usage.Tree.CalculatedCostUSD != nil {
		return *sample.Usage.Tree.CalculatedCostUSD, true
	}
	return 0, false
}

func effectiveCaseDigest(testCases []contracts.Case) string {
	values := make([]map[string]string, 0, len(testCases))
	for _, testCase := range testCases {
		digest, _ := testCase.Digest()
		values = append(values, map[string]string{"id": testCase.ID, "digest": digest})
	}
	sort.Slice(values, func(i, j int) bool { return values[i]["id"] < values[j]["id"] })
	digest, _ := contracts.CanonicalDigest(values)
	return digest
}

func modelNeutralCaseDigest(testCases []contracts.Case) string {
	digest, _ := publicCaseSetDigest(testCases)
	return digest
}

func toolPolicies(testCases []contracts.Case) []contracts.ToolPolicy {
	result := make([]contracts.ToolPolicy, 0, len(testCases))
	for _, testCase := range testCases {
		result = append(result, testCase.ToolPolicy)
	}
	return result
}

func artifactAggregates(testCases []contracts.Case) (map[string]json.RawMessage, error) {
	critical := declaredCriticalCaseIDs(testCases)
	encoded, err := baseline.CanonicalJSON(map[string]any{"case_ids": critical})
	if err != nil {
		return nil, err
	}
	caseDigest, err := publicCaseSetDigest(testCases)
	if err != nil {
		return nil, err
	}
	encodedDigest, err := baseline.CanonicalJSON(map[string]string{"digest": caseDigest})
	if err != nil {
		return nil, err
	}
	return map[string]json.RawMessage{
		"critical_case_ids":           json.RawMessage(encoded),
		publicCasesDigestAggregateKey: encodedDigest,
	}, nil
}

func declaredCriticalCaseIDs(testCases []contracts.Case) []string {
	critical := make([]string, 0, len(testCases))
	for _, testCase := range testCases {
		if testCase.Critical {
			critical = append(critical, testCase.ID)
		}
	}
	sort.Strings(critical)
	return critical
}

func publicCaseSetDigest(testCases []contracts.Case) (string, error) {
	neutral := append([]contracts.Case(nil), testCases...)
	seen := make(map[string]struct{}, len(testCases))
	for index, testCase := range neutral {
		if _, duplicate := seen[testCase.ID]; duplicate {
			return "", fmt.Errorf("duplicate public case id %q", testCase.ID)
		}
		seen[testCase.ID] = struct{}{}
		// Model and provider have dedicated fingerprint fields. Neutralizing only
		// this field permits a predeclared arm assignment without hiding any other
		// case-contract difference.
		neutral[index].Agent.Model = "skynex/model-neutral"
	}
	sort.Slice(neutral, func(i, j int) bool { return neutral[i].ID < neutral[j].ID })
	return contracts.CanonicalDigest(neutral)
}

func parseEnvNames(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return []string{}, nil
	}
	seen := map[string]struct{}{}
	var result []string
	for _, raw := range strings.Split(value, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("--provider-env contains an empty name")
		}
		for i, r := range name {
			if !(r == '_' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
				return nil, fmt.Errorf("invalid provider environment name %q", name)
			}
		}
		if reservedProviderEnvironment(name) {
			return nil, fmt.Errorf("provider environment name %q may override runtime isolation", name)
		}
		if !supportedProviderEnvironment(name) {
			return nil, fmt.Errorf("provider environment name %q is not a supported provider credential variable", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("duplicate provider environment name %q", name)
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func supportedProviderEnvironment(name string) bool {
	_, ok := map[string]struct{}{
		"OPENAI_API_KEY": {}, "ANTHROPIC_API_KEY": {}, "OPENROUTER_API_KEY": {},
		"GOOGLE_API_KEY": {}, "GEMINI_API_KEY": {}, "GROQ_API_KEY": {},
		"MISTRAL_API_KEY": {}, "COHERE_API_KEY": {}, "DEEPSEEK_API_KEY": {},
		"XAI_API_KEY": {}, "TOGETHER_API_KEY": {}, "PERPLEXITY_API_KEY": {},
		"FIREWORKS_API_KEY": {}, "CEREBRAS_API_KEY": {}, "AZURE_OPENAI_API_KEY": {},
		"AWS_ACCESS_KEY_ID": {}, "AWS_SECRET_ACCESS_KEY": {}, "AWS_SESSION_TOKEN": {},
		"AWS_REGION": {}, "AWS_DEFAULT_REGION": {},
	}[name]
	return ok
}

func reservedProviderEnvironment(name string) bool {
	upper := strings.ToUpper(name)
	return upper == "HOME" || upper == "TMPDIR" || upper == "TEMP" || upper == "TMP" ||
		upper == "APPDATA" || upper == "LOCALAPPDATA" || upper == "USERPROFILE" ||
		upper == "NODE_OPTIONS" || upper == "NODE_PATH" || upper == "BUN_OPTIONS" ||
		upper == "BASH_ENV" || upper == "ENV" || upper == "SHELLOPTS" || upper == "BASHOPTS" ||
		upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY" ||
		strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_") ||
		strings.HasPrefix(upper, "XDG_") || strings.HasPrefix(upper, "OPENCODE_")
}

func resultSetExit(samples []contracts.RunResult) int {
	present := make(map[contracts.RunStatus]bool)
	for _, sample := range samples {
		present[sample.Status] = true
	}
	for _, status := range []contracts.RunStatus{
		contracts.RunStatusInfraError, contracts.RunStatusInvalid, contracts.RunStatusBudgetExhausted,
		contracts.RunStatusAborted, contracts.RunStatusFail, contracts.RunStatusInconclusive,
	} {
		if present[status] {
			return status.ExitCode()
		}
	}
	if len(samples) == 0 {
		return contracts.ExitInvalid
	}
	return contracts.ExitSuccess
}
