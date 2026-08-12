package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/cases"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/experiment"
	"github.com/joeldevz/skynex/internal/eval/reporter"
	"github.com/joeldevz/skynex/internal/eval/runner"
	"github.com/joeldevz/skynex/internal/eval/stats"
)

type resumeABFixture struct {
	directory       string
	manifestPath    string
	harnessRoot     string
	holdoutRoot     string
	holdoutCaseID   string
	holdoutSecret   string
	controlRoot     string
	candidateRoot   string
	binary          string
	binaryDigest    string
	openAPIDigest   string
	evaluatorDigest string
	oauthPath       string
	manifest        experiment.Manifest
}

func TestABResumeDoesNotRepeatPaidSamplesAndRejectsTamperingBeforeProbe(t *testing.T) {
	fixture := newResumeABFixture(t)
	prefix := filepath.Join(fixture.directory, "results", "resume")
	partialPath := prefix + ".partial.json"

	probeCalls := 0
	modelCalls := 0
	coordinates := make(map[abSampleKey]int)
	ctx, cancel := context.WithCancel(context.Background())
	firstDeps := fixture.dependencies(t, &probeCalls, &modelCalls, coordinates, nil)
	firstRunner := firstDeps.runModel
	firstDeps.runModel = func(runContext context.Context, spec modelRunSpec) (modelRunResult, error) {
		if modelCalls == 5 {
			var checkpoint partialABArtifact
			if err := baseline.LoadJSON(partialPath, &checkpoint, baseline.IOOptions{MaxBytes: maxPartialABBytes, Strict: true}); err != nil {
				t.Fatalf("per-sample checkpoint was not readable before the next runner call: %v", err)
			}
			if err := verifyPartialABIntegrity(checkpoint); err != nil || len(checkpoint.Control.Samples)+len(checkpoint.Candidate.Samples) != 5 {
				t.Fatalf("per-sample checkpoint was not durable before interruption: artifact=%+v error=%v", checkpoint, err)
			}
			cancel()
			return modelRunResult{}, runContext.Err()
		}
		return firstRunner(runContext, spec)
	}
	partialResult, err := commandAB(ctx, fixture.args(prefix, "", true), firstDeps)
	if exit, kind := classifyCommandError(err); exit != contracts.ExitAborted || kind != "aborted" {
		t.Fatalf("canceled fake A/B classification = exit %d kind %q error %v", exit, kind, err)
	}
	if partialResult.ExitCode != contracts.ExitAborted || partialResult.PartialPath != partialPath || modelCalls != 5 {
		t.Fatalf("canceled fake A/B did not retain exactly five samples: result=%+v calls=%d", partialResult, modelCalls)
	}

	var partial partialABArtifact
	if err := baseline.LoadJSON(partialPath, &partial, baseline.IOOptions{MaxBytes: maxPartialABBytes, Strict: true}); err != nil {
		t.Fatal(err)
	}
	if err := verifyPartialABIntegrity(partial); err != nil {
		t.Fatalf("saved partial has no valid canonical integrity digest: %v", err)
	}
	if got := len(partial.Control.Samples) + len(partial.Candidate.Samples); got != 5 {
		t.Fatalf("partial retained %d samples, want 5", got)
	}
	rawPartial, err := os.ReadFile(partialPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fixture.holdoutCaseID, fixture.holdoutSecret} {
		if strings.Contains(string(rawPartial), secret) {
			t.Fatalf("partial leaked holdout secret %q", secret)
		}
	}

	assertRejectedBeforeExternalWork := func(name, path string) {
		t.Helper()
		beforeProbes, beforeModels := probeCalls, modelCalls
		_, resumeErr := commandAB(context.Background(), fixture.args("", path, false), fixture.dependencies(t, &probeCalls, &modelCalls, coordinates, nil))
		if resumeErr == nil {
			t.Fatalf("%s partial was accepted", name)
		}
		if exit, kind := classifyCommandError(resumeErr); exit != contracts.ExitInvalid || kind != "invalid_resume_partial" {
			t.Fatalf("%s partial classification = exit %d kind %q: %v", name, exit, kind, resumeErr)
		}
		if probeCalls != beforeProbes || modelCalls != beforeModels {
			t.Fatalf("%s partial reached external work: probes %d->%d models %d->%d", name, beforeProbes, probeCalls, beforeModels, modelCalls)
		}
	}

	tampered := partial
	if len(tampered.Control.Samples) == 0 {
		t.Fatal("test plan unexpectedly retained no control sample")
	}
	tampered.Control.Samples = append([]contracts.RunResult(nil), tampered.Control.Samples...)
	tampered.Control.Samples[0].Timing.WallMS++
	tamperedPath := filepath.Join(fixture.directory, "results", "tampered.partial.json")
	if err := reporter.Save(tampered, tamperedPath); err != nil {
		t.Fatal(err)
	}
	assertRejectedBeforeExternalWork("integrity-tampered", tamperedPath)

	mismatched := partial
	mismatched.ExperimentID = "different-experiment"
	if err := sealPartialABArtifact(&mismatched); err != nil {
		t.Fatal(err)
	}
	mismatchPath := filepath.Join(fixture.directory, "results", "mismatch.partial.json")
	if err := reporter.Save(mismatched, mismatchPath); err != nil {
		t.Fatal(err)
	}
	assertRejectedBeforeExternalWork("re-sealed mismatch", mismatchPath)

	planMismatch := partial
	planMismatch.Plan.Seed++
	if err := sealPartialABArtifact(&planMismatch); err != nil {
		t.Fatal(err)
	}
	planMismatchPath := filepath.Join(fixture.directory, "results", "plan-mismatch.partial.json")
	if err := reporter.Save(planMismatch, planMismatchPath); err != nil {
		t.Fatal(err)
	}
	assertRejectedBeforeExternalWork("re-sealed plan mismatch", planMismatchPath)

	duplicate := partial
	duplicate.Control.Samples = append([]contracts.RunResult(nil), duplicate.Control.Samples...)
	duplicate.Control.Samples = append(duplicate.Control.Samples, duplicate.Control.Samples[0])
	if err := sealPartialABArtifact(&duplicate); err != nil {
		t.Fatal(err)
	}
	duplicatePath := filepath.Join(fixture.directory, "results", "duplicate.partial.json")
	if err := reporter.Save(duplicate, duplicatePath); err != nil {
		t.Fatal(err)
	}
	assertRejectedBeforeExternalWork("re-sealed duplicate population", duplicatePath)

	lock, err := acquireABResumeLock(partialPath)
	if err != nil {
		t.Fatal(err)
	}
	assertRejectedBeforeExternalWork("concurrently locked", partialPath)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	candidateTarget := prefix + ".candidate.json"
	lateResult, lateErr := commandAB(
		context.Background(), fixture.args("", partialPath, false),
		fixture.dependencies(t, &probeCalls, &modelCalls, coordinates, func(successes int) {
			if successes != 3 {
				return
			}
			if err := os.Remove(candidateTarget); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(candidateTarget, []byte("external candidate target"), 0o600); err != nil {
				t.Fatal(err)
			}
		}),
	)
	if lateErr == nil || lateResult.PartialPath != partialPath || lateResult.ExitCode != contracts.ExitInfrastructure {
		t.Fatalf("late publication conflict did not preserve resumable evidence: result=%+v error=%v", lateResult, lateErr)
	}
	wantCalls := len(lateResult.Plan.Blocks) * 2
	if modelCalls != wantCalls || len(coordinates) != wantCalls {
		t.Fatalf("first resume calls=%d unique coordinates=%d, want exactly %d", modelCalls, len(coordinates), wantCalls)
	}
	if _, err := os.Stat(prefix + ".control.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partially published control artifact was not rolled back: %v", err)
	}
	if contents, err := os.ReadFile(candidateTarget); err != nil || string(contents) != "external candidate target" {
		t.Fatalf("external candidate target was overwritten or removed: contents=%q error=%v", contents, err)
	}
	if err := os.Remove(candidateTarget); err != nil {
		t.Fatal(err)
	}

	// Preserve the completed journal bytes so the test can later emulate a
	// process dying after every final was published but immediately before the
	// journal was retired.
	completedPartial, err := os.ReadFile(partialPath)
	if err != nil {
		t.Fatal(err)
	}
	modelsBeforeFinalResume := modelCalls
	finalResult, err := commandAB(
		context.Background(), fixture.args("", partialPath, false),
		fixture.dependencies(t, &probeCalls, &modelCalls, coordinates, nil),
	)
	if err != nil {
		t.Fatalf("second resume after publication rollback failed: %v", err)
	}
	if finalResult.Comparison == nil || finalResult.ExitCode != contracts.ExitSuccess {
		t.Fatalf("resumed A/B did not publish a passing comparison: %+v comparison=%+v", finalResult, finalResult.Comparison)
	}
	if modelCalls != modelsBeforeFinalResume {
		t.Fatalf("final-only resume repeated %d model calls", modelCalls-modelsBeforeFinalResume)
	}
	if modelCalls != wantCalls || len(coordinates) != wantCalls {
		t.Fatalf("fake model calls=%d unique coordinates=%d, want exactly %d", modelCalls, len(coordinates), wantCalls)
	}
	for key, count := range coordinates {
		if count != 1 {
			t.Fatalf("paid coordinate %+v ran %d times", key, count)
		}
	}
	if _, err := os.Stat(partialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful resume did not retire its partial: %v", err)
	}
	if _, err := os.Stat(partialPath + ".resume.lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful resume left its lock behind: %v", err)
	}
	for _, path := range []string{finalResult.ControlPath, finalResult.CandidatePath, prefix + ".comparison.json"} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("final artifact %q was not published: %v", path, err)
		}
	}

	if err := os.WriteFile(partialPath, completedPartial, 0o600); err != nil {
		t.Fatal(err)
	}
	modelsBeforePublishedFinalRecovery := modelCalls
	recovered, err := commandAB(
		context.Background(), fixture.args("", partialPath, false),
		fixture.dependencies(t, &probeCalls, &modelCalls, coordinates, nil),
	)
	if err != nil || recovered.ExitCode != contracts.ExitSuccess || recovered.Comparison == nil {
		t.Fatalf("resume could not reuse exact finals left by a simulated crash: result=%+v error=%v", recovered, err)
	}
	if modelCalls != modelsBeforePublishedFinalRecovery {
		t.Fatalf("published-final recovery repeated %d model calls", modelCalls-modelsBeforePublishedFinalRecovery)
	}
	if _, err := os.Stat(partialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published-final recovery did not retire its partial: %v", err)
	}
}

func TestABPostRunClosureDriftDiscardsUntrustedCoordinate(t *testing.T) {
	run := modelRunResult{Result: runner.ContractResult{Samples: []contracts.RunResult{{RunID: "paid"}}}}
	err := rejectABSamplesAfterClosureDrift(&run, errors.New("closure changed"))
	if err == nil || len(run.Result.Samples) != 0 {
		t.Fatalf("post-run closure drift retained a reusable paid coordinate: samples=%v error=%v", run.Result.Samples, err)
	}
	if exit, kind := classifyCommandError(err); exit != contracts.ExitInvalid || kind != "invalid_toolchain_closure" {
		t.Fatalf("closure drift classification = exit %d kind %q", exit, kind)
	}
}

func TestABBundleDriftCheckpointRollsBackWholeBlock(t *testing.T) {
	firstKey := abSampleKey{CaseID: "case", Variant: stats.VariantControl, Repetition: 1}
	secondKey := abSampleKey{CaseID: "case", Variant: stats.VariantCandidate, Repetition: 1}
	control := modelRunResult{Result: runner.ContractResult{Samples: []contracts.RunResult{{RunID: "stable"}}}}
	candidate := modelRunResult{}
	completed := map[abSampleKey]struct{}{firstKey: {}}
	runIDs := map[string]struct{}{"stable": {}}
	checkpoint := checkpointABBlock(control, candidate, completed, runIDs, 1.25, true)
	candidate.Result.Samples = append(candidate.Result.Samples, contracts.RunResult{RunID: "drifted"})
	completed[secondKey] = struct{}{}
	runIDs["drifted"] = struct{}{}
	totalCost, costComplete := 2.5, false
	checkpoint.Restore(&control, &candidate, &completed, &runIDs, &totalCost, &costComplete)
	if len(candidate.Result.Samples) != 0 || len(completed) != 1 || len(runIDs) != 1 {
		t.Fatalf("block rollback retained drift-affected evidence: candidate=%v completed=%v runIDs=%v", candidate.Result.Samples, completed, runIDs)
	}
	if _, exists := completed[secondKey]; exists || totalCost != 1.25 || !costComplete {
		t.Fatalf("block rollback did not restore accounting: completed=%v cost=%v complete=%v", completed, totalCost, costComplete)
	}
}

func TestRemoveOwnedABFilePreservesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.json")
	if err := os.WriteFile(path, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	owned, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedABFile(path, owned); err == nil {
		t.Fatal("replacement was accepted as the owned partial")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "replacement" {
		t.Fatalf("replacement was deleted or changed: contents=%q error=%v", contents, err)
	}
}

func TestABResumeReusesOnlyExactPinnedFinal(t *testing.T) {
	directory := t.TempDir()
	finalPath := filepath.Join(directory, "campaign.control.json")
	stagedPath := filepath.Join(directory, "staged.control.json")
	canonical := []byte("{\"kind\":\"expected-final\"}\n")
	if err := os.WriteFile(finalPath, canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedPath, canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	reservations, err := reserveABOutputsAllowExisting(true, finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservations.PublishOrReuse(finalPath, stagedPath); err != nil {
		t.Fatalf("exact preexisting final was not reusable: %v", err)
	}
	if err := reservations.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(finalPath)
	if err != nil || string(contents) != string(canonical) {
		t.Fatalf("reused final changed: contents=%q error=%v", contents, err)
	}

	mismatchPath := filepath.Join(directory, "campaign.candidate.json")
	mismatchStaged := filepath.Join(directory, "staged.candidate.json")
	if err := os.WriteFile(mismatchPath, []byte("external evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mismatchStaged, canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	reservations, err = reserveABOutputsAllowExisting(true, mismatchPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservations.PublishOrReuse(mismatchPath, mismatchStaged); err == nil {
		t.Fatal("non-matching preexisting final was accepted")
	}
	if err := reservations.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err = os.ReadFile(mismatchPath)
	if err != nil || string(contents) != "external evidence\n" {
		t.Fatalf("mismatched preexisting final was overwritten: contents=%q error=%v", contents, err)
	}
}

func TestDeferredRunnerErrorCannotRetainPassingSample(t *testing.T) {
	samples := []contracts.RunResult{{Status: contracts.RunStatusPass}}
	markDeferredABSampleFailure(samples)
	if samples[0].Status != contracts.RunStatusInfraError || samples[0].Error == nil {
		t.Fatalf("deferred runner error retained PASS: %+v", samples[0])
	}
}

func newResumeABFixture(t *testing.T) resumeABFixture {
	t.Helper()
	repository := projectRoot(t)
	loaded, err := cases.LoadContract(filepath.Join(repository, "eval", "cases", "skynex-orchestrator", "skx_low_direct.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	publicCase := *loaded
	holdoutLoaded, err := cases.LoadContract(filepath.Join(repository, "eval", "cases", "skynex-orchestrator", "skx_low_tdd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	holdoutCase := *holdoutLoaded
	holdoutCase.ID = "secret_resume_holdout_case"
	holdoutCase.Extensions = map[string]any{"x-visibility": "external-holdout"}
	if err := holdoutCase.Validate(); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	harnessRoot := filepath.Join(directory, "harness")
	holdoutRoot := filepath.Join(directory, "holdout")
	writeFrozenCaseBundle(t, repository, harnessRoot, publicCase)
	writeFrozenCaseBundle(t, repository, holdoutRoot, holdoutCase)
	controlRoot := filepath.Join(directory, "control")
	candidateRoot := filepath.Join(directory, "candidate")
	for index, root := range []string{controlRoot, candidateRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "opencode.json"), []byte(fmt.Sprintf("{\"variant\":%d}\n", index)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(directory, "opencode")
	if err := os.WriteFile(binary, []byte("\x7fELF-resume-test-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	binarySHA, err := binaryDigest(binary)
	if err != nil {
		t.Fatal(err)
	}
	evaluatorSHA, err := executableDigest()
	if err != nil {
		t.Fatal(err)
	}
	toolchains, err := runner.ResolveExecutableClosure([]contracts.Case{publicCase, holdoutCase}, "git")
	if err != nil {
		t.Fatal(err)
	}
	publicDigest, err := publicCaseSetDigest([]contracts.Case{publicCase})
	if err != nil {
		t.Fatal(err)
	}
	openAPISHA := "sha256:" + strings.Repeat("c", 64)
	manifest := experiment.Manifest{
		SchemaVersion: experiment.SchemaVersion, ID: "resume-test", Suite: publicCase.Suite,
		Intent:    experiment.IntentDevelopment,
		Harness:   experiment.FrozenBundle{Root: "harness", Digest: snapshotDigest(t, harnessRoot)},
		Control:   experiment.FrozenBundle{Root: "control", Digest: snapshotDigest(t, controlRoot)},
		Candidate: experiment.FrozenBundle{Root: "candidate", Digest: snapshotDigest(t, candidateRoot)},
		Holdout:   &experiment.FrozenBundle{Root: "holdout", Digest: snapshotDigest(t, holdoutRoot)},
		ModelAssignment: &experiment.ModelAssignment{
			Control: "openai/resume-control", Candidate: "openai/resume-candidate",
		},
		IntentionalDifferences: []baseline.Field{baseline.FieldPromptDigest, baseline.FieldAgentBundleDigest, baseline.FieldModel},
		PublicCaseCount:        1, PublicCasesDigest: publicDigest,
		CriticalCaseIDs: declaredCriticalCaseIDs([]contracts.Case{publicCase}), HoldoutCaseCount: 1, Runs: 2,
		Randomization: experiment.Randomization{Method: stats.BalancedBlockedMethod, Seed: "19", SerializeWithinBlock: true},
		Execution: experiment.Execution{
			Mode: string(contracts.ExecutionTrustedLocal), Network: string(contracts.NetworkHostUnisolated), Concurrency: 1,
			ProviderAuth: experiment.ProviderAuthOpenAIOAuthCleanProfileV1, BillingMode: experiment.BillingModeChatGPTSubscription,
			CredentialBoundary: experiment.CredentialBoundaryRuntimeReadable, OpenCodeVersion: defaultOpenCodeVersion,
			EvaluatorBinaryDigest: evaluatorSHA, OpenCodeBinaryDigest: binarySHA, OpenCodeOpenAPIDigest: openAPISHA,
			ToolchainsDigest: toolchains.Digest(),
		},
		Gates: experiment.Gates{
			CriticalCasePassRate: 1, PassToFailRegressions: 0, ScopeViolations: 0, FalseSuccesses: 0,
			MaxParentPeakInputRatio: 2, MaxTreeInputRatio: 2, MaxCostRatio: 2, MaxWallTimeRatio: 2,
			MaxRetryRateRatio: 2, Confidence: 0.95, MinimumPairs: 2,
		},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(directory, "manifest.json")
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return resumeABFixture{
		directory: directory, manifestPath: manifestPath, harnessRoot: harnessRoot, holdoutRoot: holdoutRoot,
		holdoutCaseID: holdoutCase.ID, holdoutSecret: "RESUME_HOLDOUT_PRIVATE_TEXT",
		controlRoot: controlRoot, candidateRoot: candidateRoot, binary: binary,
		binaryDigest: binarySHA, openAPIDigest: openAPISHA, evaluatorDigest: evaluatorSHA,
		oauthPath: writeOpenAIOAuthFile(t, directory), manifest: manifest,
	}
}

func (fixture resumeABFixture) args(prefix, resume string, includePrefix bool) []string {
	args := []string{
		"--allow-model-calls", "--require-holdout", "--manifest", fixture.manifestPath, "--openai-oauth", fixture.oauthPath,
		"--cases-dir", filepath.Join(fixture.harnessRoot, "cases"),
		"--fixtures-dir", filepath.Join(fixture.harnessRoot, "fixtures"), "--binary", fixture.binary,
	}
	if includePrefix {
		args = append(args, "--output-prefix", prefix)
	}
	if resume != "" {
		args = append(args, "--resume-partial", resume)
	}
	return args
}

func (fixture resumeABFixture) dependencies(t *testing.T, probeCalls, modelCalls *int, coordinates map[abSampleKey]int, afterSuccess func(int)) dependencies {
	t.Helper()
	successes := 0
	return dependencies{
		probeRuntime: func(_ context.Context, options doctorOptions) (doctorResult, error) {
			*probeCalls++
			return doctorResult{
				Healthy: true, Version: options.ExpectedVersion, ExpectedVersion: options.ExpectedVersion,
				CapturedAt: time.Unix(1, 0).UTC().Format(time.RFC3339Nano), ModelCalls: 0,
				Endpoints: []doctorEndpoint{{Name: "/doc", Digest: fixture.openAPIDigest}},
			}, nil
		},
		runModel: func(runContext context.Context, spec modelRunSpec) (modelRunResult, error) {
			if err := runContext.Err(); err != nil {
				return modelRunResult{}, err
			}
			*modelCalls++
			testCase := spec.Cases[0]
			key := abSampleKey{CaseID: testCase.ID, Variant: stats.Variant(spec.Variant), Repetition: spec.RepetitionStart}
			coordinates[key]++
			caseDigest, err := testCase.Digest()
			if err != nil {
				t.Fatal(err)
			}
			sample := validRun(fmt.Sprintf("resume_run_%d", *modelCalls), spec.Variant, contracts.RunStatusPass)
			sample.CaseID = testCase.ID
			sample.Repetition = spec.RepetitionStart
			sample.Provenance.CaseDigest = caseDigest
			sample.Provenance.PromptDigest = spec.VerifiedBundleDigest
			sample.Provenance.FixtureDigest = testCase.Fixture.ExpectedDigest
			sample.Provenance.OpenCodeVersion = spec.ExpectedVersion
			sample.Provenance.OpenCodeAPIDigest = spec.ExpectedOpenCodeAPIDigest
			sample.Provenance.Model = testCase.Agent.Model
			sample.Provenance.Provider, _, _ = strings.Cut(testCase.Agent.Model, "/")
			sample.Provenance.Extensions = map[string]string{
				contracts.ProvenanceExtensionProviderAuthMode:      contracts.ProviderAuthModeOpenAIOAuthCleanProfileV1,
				contracts.ProvenanceExtensionBillingMode:           contracts.BillingModeChatGPTSubscription,
				contracts.ProvenanceExtensionCredentialBoundary:    contracts.CredentialBoundaryRuntimeReadable,
				contracts.ProvenanceExtensionAuthIsolation:         contracts.AuthIsolationDedicatedFreshTokenFailStopV1,
				contracts.ProvenanceExtensionProviderCatalogDigest: "sha256:" + strings.Repeat("d", 64),
				"x-observed-provider":                              sample.Provenance.Provider,
				"x-observed-model":                                 strings.TrimPrefix(sample.Provenance.Model, sample.Provenance.Provider+"/"),
				provenanceExtensionAgentBundleDigest:               spec.VerifiedBundleDigest,
				provenanceExtensionHarnessBundleDigest:             spec.HarnessDigest,
				provenanceExtensionManifestDigest:                  spec.ManifestDigest,
				provenanceExtensionEffectiveConfigDigest:           sample.Provenance.ConfigDigest,
				provenanceExtensionEffectiveAgentsDigest:           sample.Provenance.ConfigDigest,
				provenanceExtensionToolchainsDigest:                spec.ExecutableClosure.Digest(),
			}
			sample.Usage.Parent.CalculatedCostUSD = sample.Usage.Parent.ProviderCostUSD
			sample.Usage.Tree.CalculatedCostUSD = sample.Usage.Tree.ProviderCostUSD
			sample.Usage.Parent.ProviderCostUSD = nil
			sample.Usage.Tree.ProviderCostUSD = nil
			if testCase.ID == fixture.holdoutCaseID {
				sample.Checks[0].Summary = fixture.holdoutSecret
				sample.Evidence.BeforeTree = fixture.holdoutSecret
			}
			if err := sample.Validate(); err != nil {
				t.Fatalf("fake resume sample is invalid: %v", err)
			}
			successes++
			if afterSuccess != nil {
				afterSuccess(successes)
			}
			return modelRunResult{
				Result: runner.ContractResult{
					Suite: spec.Suite, Samples: []contracts.RunResult{sample},
					Started: time.Unix(1, 0).UTC(), Ended: time.Unix(2, 0).UTC(), Complete: true,
				},
				BundleDigest: spec.VerifiedBundleDigest, HarnessDigest: spec.HarnessDigest,
				ManifestDigest: spec.ManifestDigest, EvaluatorBinaryDigest: fixture.evaluatorDigest,
				OpenCodeBinaryDigest: fixture.binaryDigest, CostEvidenceComplete: false,
				EffectiveCases: []contracts.Case{testCase},
			}, nil
		},
	}
}
