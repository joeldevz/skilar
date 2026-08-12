package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/cases"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/experiment"
	"github.com/joeldevz/skynex/internal/eval/gates"
	"github.com/joeldevz/skynex/internal/eval/runner"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
)

func TestFreezeBuildsReproducibleOfflineCapsule(t *testing.T) {
	repository := projectRoot(t)
	testCase, err := cases.LoadContract(filepath.Join(repository, "eval", "cases", "skynex-orchestrator", "skx_low_direct.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	harness := filepath.Join(workspace, "harness-source")
	control := filepath.Join(workspace, "control-source")
	candidate := filepath.Join(workspace, "candidate-source")
	writeFrozenCaseBundle(t, repository, harness, *testCase)
	writeFreezeTestBundle(t, control, `{"variant":"control"}`)
	writeFreezeTestBundle(t, candidate, `{"variant":"candidate"}`)
	binary := writeFreezeTestBinary(t, workspace)
	openAPI := "sha256:" + strings.Repeat("c", 64)

	probeCalls, modelCalls := 0, 0
	deps := dependencies{
		probeRuntime: func(context.Context, doctorOptions) (doctorResult, error) {
			probeCalls++
			return doctorResult{}, errors.New("freeze must not probe")
		},
		runModel: func(context.Context, modelRunSpec) (modelRunResult, error) {
			modelCalls++
			return modelRunResult{}, errors.New("freeze must not run a model")
		},
	}
	freeze := func(output string) freezeResult {
		t.Helper()
		var stdout, stderr bytes.Buffer
		exit := runCLI(context.Background(), []string{
			"freeze", "--output-dir", output,
			"--harness", harness, "--control", control, "--candidate", candidate,
			"--id", "offline-freeze", "--suite", testCase.Suite,
			"--runs", "5", "--seed", "42", "--binary", binary,
			"--opencode-version", defaultOpenCodeVersion,
			"--opencode-openapi-digest", openAPI,
		}, deps, &stdout, &stderr)
		if exit != contracts.ExitSuccess || stderr.Len() != 0 {
			t.Fatalf("freeze exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
		}
		var wire struct {
			SchemaVersion int          `json:"schema_version"`
			Command       string       `json:"command"`
			Status        string       `json:"status"`
			Data          freezeResult `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		if wire.SchemaVersion != cliSchemaVersion || wire.Command != "freeze" || wire.Status != string(contracts.RunStatusPass) {
			t.Fatalf("unexpected envelope: %+v", wire)
		}
		return wire.Data
	}

	firstOutput := filepath.Join(workspace, "capsule-one")
	secondOutput := filepath.Join(workspace, "capsule-two")
	first := freeze(firstOutput)
	second := freeze(secondOutput)
	if probeCalls != 0 || modelCalls != 0 || first.ModelCalls != 0 || second.ModelCalls != 0 {
		t.Fatalf("freeze performed external work: probes=%d models=%d receipts=%d/%d", probeCalls, modelCalls, first.ModelCalls, second.ModelCalls)
	}
	firstManifestBytes, err := os.ReadFile(filepath.Join(firstOutput, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	secondManifestBytes, err := os.ReadFile(filepath.Join(secondOutput, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstManifestBytes, secondManifestBytes) || first.ManifestDigest != second.ManifestDigest {
		t.Fatalf("identical inputs produced different manifests\nfirst: %s\nsecond: %s", firstManifestBytes, secondManifestBytes)
	}
	manifest, err := experiment.Load(filepath.Join(firstOutput, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Intent != experiment.IntentDevelopment || manifest.Harness.Root != "bundles/harness" ||
		manifest.Control.Root != "bundles/control" || manifest.Candidate.Root != "bundles/candidate" {
		t.Fatalf("unexpected frozen manifest roots/intent: %+v", manifest)
	}
	if manifest.PublicCaseCount != 1 || manifest.PublicCasesDigest == "" ||
		len(manifest.CriticalCaseIDs) != 1 || manifest.CriticalCaseIDs[0] != testCase.ID {
		t.Fatalf("unexpected committed case population: %+v", manifest)
	}
	if manifest.Control.Digest == manifest.Candidate.Digest {
		t.Fatal("frozen treatment collapsed to an identical digest")
	}
	if manifest.Execution.OpenCodeOpenAPIDigest != openAPI || manifest.Execution.OpenCodeBinaryDigest == "" ||
		manifest.Execution.EvaluatorBinaryDigest == "" {
		t.Fatalf("runtime pins missing from manifest: %+v", manifest.Execution)
	}
	if _, err := manifest.VerifyBundles(firstOutput, sandbox.DefaultSnapshotLimits()); err != nil {
		t.Fatalf("published capsule does not verify: %v", err)
	}

	before, err := sandbox.DigestTree(filepath.Join(firstOutput, "bundles", "harness"), sandbox.DefaultSnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firstOutput, "manifest.json"), append(bytes.TrimSpace(firstManifestBytes), '\n', '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := sandbox.DigestTree(filepath.Join(firstOutput, "bundles", "harness"), sandbox.DefaultSnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	if before.Digest != after.Digest {
		t.Fatalf("external manifest changed harness digest: %s != %s", before.Digest, after.Digest)
	}
}

func TestFreezeRejectsUnrealizedTreatmentBeforeOutputOrModel(t *testing.T) {
	repository := projectRoot(t)
	testCase, err := cases.LoadContract(filepath.Join(repository, "eval", "cases", "skynex-orchestrator", "skx_low_direct.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	harness := filepath.Join(workspace, "harness-source")
	control := filepath.Join(workspace, "control-source")
	candidate := filepath.Join(workspace, "candidate-source")
	writeFrozenCaseBundle(t, repository, harness, *testCase)
	writeFreezeTestBundle(t, control, `{"same":true}`)
	writeFreezeTestBundle(t, candidate, `{"same":true}`)
	output := filepath.Join(workspace, "must-not-exist")
	modelCalls, probeCalls := 0, 0
	deps := dependencies{
		probeRuntime: func(context.Context, doctorOptions) (doctorResult, error) {
			probeCalls++
			return doctorResult{}, errors.New("must not probe")
		},
		runModel: func(context.Context, modelRunSpec) (modelRunResult, error) {
			modelCalls++
			return modelRunResult{}, errors.New("must not run")
		},
	}
	var stdout, stderr bytes.Buffer
	exit := runCLI(context.Background(), []string{
		"freeze", "--output-dir", output, "--harness", harness, "--control", control,
		"--candidate", candidate, "--id", "noop", "--suite", testCase.Suite,
	}, deps, &stdout, &stderr)
	if exit != contracts.ExitInvalid || modelCalls != 0 || probeCalls != 0 {
		t.Fatalf("exit=%d probes=%d models=%d stdout=%s stderr=%s", exit, probeCalls, modelCalls, stdout.String(), stderr.String())
	}
	var wire envelope
	if err := json.Unmarshal(stdout.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Error == nil || wire.Error.Kind != "treatment_not_realized" {
		t.Fatalf("unexpected error: %+v", wire.Error)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed freeze left output behind: %v", err)
	}
}

func TestFreezeAllowsModelOnlyTreatmentWithIdenticalBundles(t *testing.T) {
	repository := projectRoot(t)
	testCase, err := cases.LoadContract(filepath.Join(repository, "eval", "cases", "skynex-orchestrator", "skx_low_direct.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	harness := filepath.Join(workspace, "harness-source")
	control := filepath.Join(workspace, "control-source")
	candidate := filepath.Join(workspace, "candidate-source")
	writeFrozenCaseBundle(t, repository, harness, *testCase)
	writeFreezeTestBundle(t, control, `{"same":true}`)
	writeFreezeTestBundle(t, candidate, `{"same":true}`)
	binary := writeFreezeTestBinary(t, workspace)
	output := filepath.Join(workspace, "model-only-capsule")

	result, err := commandFreeze(context.Background(), []string{
		"--output-dir", output, "--harness", harness, "--control", control, "--candidate", candidate,
		"--id", "model-only", "--suite", testCase.Suite,
		"--intentional-differences", "model",
		"--control-model", "openai/control", "--candidate-model", "openai/candidate",
		"--binary", binary, "--opencode-openapi-digest", "sha256:" + strings.Repeat("9", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelCalls != 0 {
		t.Fatalf("model-only freeze made model calls: %+v", result)
	}
	manifest, err := experiment.Load(filepath.Join(output, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Control.Digest != manifest.Candidate.Digest || manifest.ModelAssignment == nil ||
		fmt.Sprint(manifest.IntentionalDifferences) != fmt.Sprint([]baseline.Field{baseline.FieldModel}) {
		t.Fatalf("model-only treatment was not frozen exactly: %+v", manifest)
	}
}

func TestFreezeDifferenceVocabularyMatchesObservableArmInputs(t *testing.T) {
	for _, input := range []string{"agent_bundle_digest", "prompt_digest", "prompt_digest,agent_bundle_digest"} {
		differences, err := parseFreezeDifferences(input)
		if err != nil {
			t.Fatalf("%q: %v", input, err)
		}
		want := []baseline.Field{baseline.FieldAgentBundleDigest, baseline.FieldPromptDigest}
		if fmt.Sprint(differences) != fmt.Sprint(want) {
			t.Fatalf("%q normalized to %v, want %v", input, differences, want)
		}
	}
	for _, input := range []string{"toolset_digest", "permission_policy_digest"} {
		if _, err := parseFreezeDifferences(input); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("unobservable freeze difference %q accepted: %v", input, err)
		}
	}
}

func TestFreezeRejectsOutputInsideInputAndUnsafeTrees(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"harness", "control", "candidate"} {
		writeFreezeTestBundle(t, filepath.Join(workspace, name), name)
	}
	binary := writeFreezeTestBinary(t, workspace)
	digest := "sha256:" + strings.Repeat("d", 64)

	t.Run("output inside harness", func(t *testing.T) {
		output := filepath.Join(workspace, "harness", "capsule")
		var stdout, stderr bytes.Buffer
		exit := runCLI(context.Background(), []string{
			"freeze", "--output-dir", output, "--harness", filepath.Join(workspace, "harness"),
			"--control", filepath.Join(workspace, "control"), "--candidate", filepath.Join(workspace, "candidate"),
			"--id", "inside", "--suite", "suite", "--binary", binary, "--opencode-openapi-digest", digest,
		}, dependencies{}, &stdout, &stderr)
		if exit != contracts.ExitInvalid || !strings.Contains(stderr.String(), "must not be inside") {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
		}
	})

	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link creation is not reliably available to an unprivileged Windows test")
	}
	t.Run("symlink", func(t *testing.T) {
		if err := os.Symlink("bundle.json", filepath.Join(workspace, "candidate", "escape")); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(workspace, "unsafe-capsule")
		var stdout, stderr bytes.Buffer
		exit := runCLI(context.Background(), []string{
			"freeze", "--output-dir", output, "--harness", filepath.Join(workspace, "harness"),
			"--control", filepath.Join(workspace, "control"), "--candidate", filepath.Join(workspace, "candidate"),
			"--id", "unsafe", "--suite", "suite", "--binary", binary, "--opencode-openapi-digest", digest,
		}, dependencies{}, &stdout, &stderr)
		if exit != contracts.ExitInvalid || !strings.Contains(stderr.String(), "unsafe entry") {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
		}
		if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe freeze left output behind: %v", err)
		}
	})
}

func TestFreezePersistsSourceGitReceiptsWithoutLeakingHoldoutProvenance(t *testing.T) {
	repository := projectRoot(t)
	testCase, err := cases.LoadContract(filepath.Join(repository, "eval", "cases", "skynex-orchestrator", "skx_low_direct.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	harness := filepath.Join(workspace, "harness-source")
	control := filepath.Join(workspace, "control-source")
	candidate := filepath.Join(workspace, "candidate-source")
	holdout := filepath.Join(workspace, "holdout-source")
	writeFrozenCaseBundle(t, repository, harness, *testCase)
	writeFreezeTestBundle(t, control, `{"variant":"control"}`)
	writeFreezeTestBundle(t, candidate, `{"variant":"candidate"}`)
	holdoutCase := *testCase
	holdoutCase.ID = testCase.ID + "_private"
	holdoutCase.Critical = false
	writeFrozenCaseBundle(t, repository, holdout, holdoutCase)
	for _, root := range []string{harness, control, candidate, holdout} {
		initializeFreezeGitRepository(t, root)
	}
	gitClosure, err := runner.ResolveExecutableClosure([]contracts.Case{*testCase, holdoutCase}, "git")
	if err != nil {
		t.Fatal(err)
	}
	cleanHoldoutProvenance, err := inspectFreezeSourceGit(holdout, gitClosure)
	if err != nil || cleanHoldoutProvenance == nil {
		t.Fatalf("inspect clean holdout provenance: %+v, %v", cleanHoldoutProvenance, err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "bundle.json"), []byte("{\"variant\":\"candidate-dirty\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	binary := writeFreezeTestBinary(t, workspace)
	openAPI := "sha256:" + strings.Repeat("e", 64)
	output := filepath.Join(workspace, "capsule")
	var stdout, stderr bytes.Buffer
	exit := runCLI(context.Background(), []string{
		"freeze", "--output-dir", output,
		"--harness", harness, "--control", control, "--candidate", candidate, "--holdout", holdout,
		"--id", "git-receipts", "--suite", testCase.Suite,
		"--binary", binary, "--opencode-openapi-digest", openAPI,
	}, dependencies{}, &stdout, &stderr)
	if exit != contracts.ExitSuccess || stderr.Len() != 0 {
		t.Fatalf("freeze exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	var wire struct {
		Data freezeResult `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	manifest, err := experiment.Load(filepath.Join(output, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, bundle := range map[string]experiment.FrozenBundle{
		"harness": manifest.Harness, "control": manifest.Control, "candidate": manifest.Candidate,
	} {
		if bundle.SourceGitSHA == "" {
			t.Fatalf("%s source Git SHA was not persisted: %+v", name, bundle)
		}
		if bundle.GitSHA != "" || bundle.DirtyPatchDigest != "" {
			t.Fatalf("%s copied root incorrectly inherited source-owned Git provenance: %+v", name, bundle)
		}
	}
	if manifest.Harness.SourceDirtyPatchDigest != "" || manifest.Control.SourceDirtyPatchDigest != "" {
		t.Fatalf("clean source recorded a dirty patch: harness=%+v control=%+v", manifest.Harness, manifest.Control)
	}
	if manifest.Candidate.SourceDirtyPatchDigest == "" {
		t.Fatalf("dirty candidate source patch was not persisted: %+v", manifest.Candidate)
	}
	if manifest.Holdout == nil || manifest.Holdout.SourceGitSHA != "" || manifest.Holdout.SourceDirtyPatchDigest != "" {
		t.Fatalf("holdout source provenance leaked into manifest: %+v", manifest.Holdout)
	}
	receipts := make(map[string]freezeBundleResult, len(wire.Data.Bundles))
	for _, receipt := range wire.Data.Bundles {
		receipts[receipt.Name] = receipt
	}
	if receipts["harness"].SourceGitProvenance == nil || receipts["control"].SourceGitProvenance == nil ||
		receipts["candidate"].SourceGitProvenance == nil || receipts["holdout"].SourceGitProvenance != nil {
		t.Fatalf("source receipt publication mismatch: %+v", receipts)
	}
	if receipts["holdout"].FileCount != nil || receipts["holdout"].TotalBytes != nil {
		t.Fatalf("holdout file metadata leaked into freeze receipt: %+v", receipts["holdout"])
	}
	for _, name := range []string{"harness", "control", "candidate"} {
		if receipts[name].FileCount == nil || receipts[name].TotalBytes == nil {
			t.Fatalf("public %s receipt omitted materialization metadata: %+v", name, receipts[name])
		}
	}
	if _, err := manifest.VerifyBundles(output, sandbox.DefaultSnapshotLimits()); err != nil {
		t.Fatalf("source receipts interfered with copied-root verification: %v", err)
	}

	if err := os.WriteFile(filepath.Join(holdout, "private-untracked.txt"), []byte("do not disclose\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirtyHoldoutProvenance, err := inspectFreezeSourceGit(holdout, gitClosure)
	if err != nil || dirtyHoldoutProvenance == nil || dirtyHoldoutProvenance.DirtyPatchDigest == "" {
		t.Fatalf("inspect dirty holdout provenance: %+v, %v", dirtyHoldoutProvenance, err)
	}
	stdout.Reset()
	stderr.Reset()
	exit = runCLI(context.Background(), []string{
		"freeze", "--output-dir", filepath.Join(workspace, "dirty-holdout-capsule"),
		"--harness", harness, "--control", control, "--candidate", candidate, "--holdout", holdout,
		"--id", "dirty-holdout", "--suite", testCase.Suite,
		"--binary", binary, "--opencode-openapi-digest", openAPI,
	}, dependencies{}, &stdout, &stderr)
	if exit != contracts.ExitInvalid {
		t.Fatalf("dirty holdout exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, privateHoldoutError().Error()) ||
		strings.Contains(combined, cleanHoldoutProvenance.GitSHA) ||
		strings.Contains(combined, dirtyHoldoutProvenance.DirtyPatchDigest) ||
		strings.Contains(combined, "source_git") || strings.Contains(combined, "dirty_patch") {
		t.Fatalf("dirty holdout error was not generic: %s", combined)
	}
}

func TestFreezeGitInspectionUsesClosureWithSymlinkSelection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link creation is not reliably available to an unprivileged Windows test")
	}
	repository := filepath.Join(t.TempDir(), "repository")
	writeFreezeTestBundle(t, repository, `{"source":"git"}`)
	initializeFreezeGitRepository(t, repository)

	selected, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(selected)
	if err != nil {
		t.Fatal(err)
	}
	selectionDirectory := t.TempDir()
	selection := filepath.Join(selectionDirectory, "git")
	if err := os.Symlink(target, selection); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", selectionDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	closure, err := runner.ResolveExecutableClosure(nil, "git")
	if err != nil {
		t.Fatalf("resolve closure through Git symlink: %v", err)
	}
	resolved, err := closure.PathFor("git")
	if err != nil {
		t.Fatal(err)
	}
	canonicalTarget, err := filepath.Abs(target)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Clean(canonicalTarget) {
		t.Fatalf("closure Git target = %q, want %q", resolved, canonicalTarget)
	}
	provenance, err := inspectFreezeSourceGit(repository, closure)
	if err != nil {
		t.Fatalf("inspect Git through closure-bound symlink selection: %v", err)
	}
	if provenance == nil || provenance.GitSHA == "" || provenance.Dirty {
		t.Fatalf("unexpected Git provenance: %+v", provenance)
	}
}

func TestFreezeRedactsHoldoutClosureResolutionError(t *testing.T) {
	repository := projectRoot(t)
	testCase, err := cases.LoadContract(filepath.Join(repository, "eval", "cases", "skynex-orchestrator", "skx_low_direct.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	harness := filepath.Join(workspace, "harness-source")
	control := filepath.Join(workspace, "control-source")
	candidate := filepath.Join(workspace, "candidate-source")
	holdout := filepath.Join(workspace, "holdout-source")
	writeFrozenCaseBundle(t, repository, harness, *testCase)
	writeFreezeTestBundle(t, control, `{"variant":"control"}`)
	writeFreezeTestBundle(t, candidate, `{"variant":"candidate"}`)
	holdoutCase := *testCase
	holdoutCase.ID = testCase.ID + "_private"
	holdoutCase.Critical = false
	const secretExecutable = "HOLDOUT_PRIVATE_EXECUTABLE_DO_NOT_DISCLOSE"
	holdoutCase.Security.AllowedExecutables = append(append([]string(nil), testCase.Security.AllowedExecutables...), secretExecutable)
	if err := holdoutCase.Validate(); err != nil {
		t.Fatal(err)
	}
	writeFrozenCaseBundle(t, repository, holdout, holdoutCase)

	var stdout, stderr bytes.Buffer
	exit := runCLI(context.Background(), []string{
		"freeze", "--output-dir", filepath.Join(workspace, "capsule"),
		"--harness", harness, "--control", control, "--candidate", candidate, "--holdout", holdout,
		"--id", "private-toolchain", "--suite", testCase.Suite,
	}, dependencies{}, &stdout, &stderr)
	if exit != contracts.ExitInvalid {
		t.Fatalf("holdout closure failure exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, privateHoldoutError().Error()) || strings.Contains(combined, secretExecutable) {
		t.Fatalf("holdout closure error leaked private declaration: %s", combined)
	}
}

func TestManifestRejectsSameArmOrUnrealizedTreatment(t *testing.T) {
	manifest := validManifestForCapsuleTest()
	manifest.Candidate.Root = manifest.Control.Root
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "distinct bundle roots") {
		t.Fatalf("same arm root accepted: %v", err)
	}
	manifest = validManifestForCapsuleTest()
	manifest.Candidate.Digest = manifest.Control.Digest
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "treatment was not realized") {
		t.Fatalf("same arm digest accepted: %v", err)
	}
	manifest.ModelAssignment = &experiment.ModelAssignment{Control: "openai/control", Candidate: "openai/candidate"}
	manifest.IntentionalDifferences = []baseline.Field{baseline.FieldModel}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("model-only treatment with identical bundles rejected: %v", err)
	}
	manifest.IntentionalDifferences = []baseline.Field{
		baseline.FieldPromptDigest, baseline.FieldAgentBundleDigest, baseline.FieldModel,
	}
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "model-only treatment") {
		t.Fatalf("unrealized bundle difference was accepted: %v", err)
	}
	manifest = validManifestForCapsuleTest()
	holdout := experiment.FrozenBundle{
		Root: "bundles/holdout", Digest: "sha256:" + strings.Repeat("c", 64),
		SourceGitSHA: strings.Repeat("d", 40),
	}
	manifest.Holdout = &holdout
	manifest.HoldoutCaseCount = 1
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "must not be published") {
		t.Fatalf("holdout source Git provenance accepted: %v", err)
	}
}

func TestTreatmentRealizedGateRejectsPlaceboAndMissingDeclarations(t *testing.T) {
	declared := []baseline.Field{baseline.FieldAgentBundleDigest}
	placebo := baseline.CompatibilityReport{
		Compatible:             true,
		IntentionalDifferences: declared,
		Mismatches: []baseline.Mismatch{{
			Field: baseline.FieldAgentBundleDigest, Control: "control", Current: "candidate", Allowed: true,
		}},
	}
	if gate := treatmentRealizedGate(placebo, declared); gate.Status != gates.StatusInvalid || !strings.Contains(gate.Detail, "effective config") {
		t.Fatalf("placebo treatment passed: %+v", gate)
	}

	realized := placebo
	realized.Mismatches = append(realized.Mismatches, baseline.Mismatch{
		Field: baseline.FieldEffectiveAgentsDigest, Control: "control-effective", Current: "candidate-effective", Allowed: true,
	})
	if gate := treatmentRealizedGate(realized, declared); gate.Status != gates.StatusPass {
		t.Fatalf("effective treatment failed: %+v", gate)
	}

	declared = []baseline.Field{baseline.FieldAgentBundleDigest, baseline.FieldToolsetDigest}
	if gate := treatmentRealizedGate(realized, declared); gate.Status != gates.StatusInvalid || !strings.Contains(gate.Detail, string(baseline.FieldToolsetDigest)) {
		t.Fatalf("missing declared mismatch passed: %+v", gate)
	}
}

func writeFreezeTestBundle(t *testing.T, root, contents string) {
	t.Helper()
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bundle.json"), []byte(contents+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeFreezeTestBinary(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "opencode-test")
	if err := os.WriteFile(path, []byte("\x7fELF-frozen-opencode-test-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func initializeFreezeGitRepository(t *testing.T, root string) {
	t.Helper()
	commands := [][]string{
		{"-C", root, "init", "-q"},
		{"-C", root, "add", "--", "."},
		{"-C", root, "-c", "user.name=Skynex Test", "-c", "user.email=skynex-test@example.invalid", "commit", "-q", "-m", "freeze fixture"},
	}
	for _, arguments := range commands {
		command := exec.Command("git", arguments...)
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
		}
	}
}

func validManifestForCapsuleTest() experiment.Manifest {
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	return experiment.Manifest{
		SchemaVersion: experiment.SchemaVersion, ID: "capsule-test", Suite: "suite", Intent: experiment.IntentDevelopment,
		Harness:                experiment.FrozenBundle{Root: "bundles/harness", Digest: digestA},
		Control:                experiment.FrozenBundle{Root: "bundles/control", Digest: digestA},
		Candidate:              experiment.FrozenBundle{Root: "bundles/candidate", Digest: digestB},
		IntentionalDifferences: []baseline.Field{baseline.FieldPromptDigest, baseline.FieldAgentBundleDigest},
		PublicCaseCount:        1, PublicCasesDigest: digestA, CriticalCaseIDs: []string{"critical"}, Runs: 2,
		Randomization: experiment.Randomization{Method: "balanced-blocked-ab-ba", Seed: "1", SerializeWithinBlock: true},
		Execution: experiment.Execution{
			Mode: "trusted-local", Network: "host-unisolated", Concurrency: 1,
			ProviderAuth: experiment.ProviderAuthOpenAIOAuthCleanProfileV1, BillingMode: experiment.BillingModeChatGPTSubscription,
			CredentialBoundary: experiment.CredentialBoundaryRuntimeReadable, OpenCodeVersion: defaultOpenCodeVersion,
			EvaluatorBinaryDigest: digestA, OpenCodeBinaryDigest: digestA, OpenCodeOpenAPIDigest: digestA,
			ToolchainsDigest: digestA,
		},
		Gates: experiment.Gates{
			CriticalCasePassRate: 1, MaxParentPeakInputRatio: .7, MaxTreeInputRatio: 1,
			MaxCostRatio: 1, MaxWallTimeRatio: 1.1, MaxRetryRateRatio: 1, MinimumPairs: 2,
		},
	}
}
