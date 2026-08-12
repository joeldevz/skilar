package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/experiment"
	"github.com/joeldevz/skynex/internal/eval/runner"
	"github.com/joeldevz/skynex/internal/eval/stats"
)

const (
	partialABSchemaVersion = 1
	partialABKind          = "skynex-eval-ab-partial"
	maxPartialABBytes      = int64(64 << 20)
)

// partialABIntegrityPayload deliberately excludes IntegrityDigest. The digest
// therefore covers the canonical JSON representation of every other field
// without requiring a self-referential value.
type partialABIntegrityPayload struct {
	SchemaVersion  int                   `json:"schema_version"`
	Kind           string                `json:"kind"`
	ExperimentID   string                `json:"experiment_id"`
	Intent         string                `json:"intent"`
	Authority      string                `json:"authority"`
	ManifestDigest string                `json:"manifest_digest"`
	Plan           stats.ExperimentPlan  `json:"plan"`
	Control        runner.ContractResult `json:"control"`
	Candidate      runner.ContractResult `json:"candidate"`
	HoldoutDigest  string                `json:"holdout_bundle_digest,omitempty"`
	HoldoutCases   int                   `json:"holdout_cases"`
	ExitCode       int                   `json:"exit_code"`
}

type abSampleKey struct {
	CaseID     string
	Variant    stats.Variant
	Repetition int
}

type abResumeExpectation struct {
	ExperimentID    string
	Intent          string
	Authority       string
	ManifestDigest  string
	Suite           string
	PublishedPlan   stats.ExperimentPlan
	ExecutionPlan   stats.ExperimentPlan
	HoldoutDigest   string
	HoldoutCases    int
	HoldoutByRef    map[string]string
	CasesByID       map[string]contracts.Case
	ModelAssignment *experiment.ModelAssignment
	Execution       experiment.Execution
	HarnessDigest   string
	ControlDigest   string
	CandidateDigest string
}

type abResumeState struct {
	Control   runner.ContractResult
	Candidate runner.ContractResult
	Completed map[abSampleKey]struct{}
	RunIDs    map[string]struct{}
	Started   time.Time
}

type abBlockCheckpoint struct {
	control      modelRunResult
	candidate    modelRunResult
	completed    map[abSampleKey]struct{}
	runIDs       map[string]struct{}
	totalCost    float64
	costComplete bool
}

func checkpointABBlock(control, candidate modelRunResult, completed map[abSampleKey]struct{}, runIDs map[string]struct{}, totalCost float64, costComplete bool) abBlockCheckpoint {
	return abBlockCheckpoint{
		control: cloneABModelRunResult(control), candidate: cloneABModelRunResult(candidate),
		completed: cloneABSampleSet(completed), runIDs: cloneABStringSet(runIDs),
		totalCost: totalCost, costComplete: costComplete,
	}
}

func (checkpoint abBlockCheckpoint) Restore(control, candidate *modelRunResult, completed *map[abSampleKey]struct{}, runIDs *map[string]struct{}, totalCost *float64, costComplete *bool) {
	if control != nil {
		*control = cloneABModelRunResult(checkpoint.control)
	}
	if candidate != nil {
		*candidate = cloneABModelRunResult(checkpoint.candidate)
	}
	if completed != nil {
		*completed = cloneABSampleSet(checkpoint.completed)
	}
	if runIDs != nil {
		*runIDs = cloneABStringSet(checkpoint.runIDs)
	}
	if totalCost != nil {
		*totalCost = checkpoint.totalCost
	}
	if costComplete != nil {
		*costComplete = checkpoint.costComplete
	}
}

func cloneABModelRunResult(source modelRunResult) modelRunResult {
	result := source
	result.Result.Samples = append([]contracts.RunResult(nil), source.Result.Samples...)
	result.EffectiveCases = append([]contracts.Case(nil), source.EffectiveCases...)
	if source.PublishedObservedCost != nil {
		value := *source.PublishedObservedCost
		result.PublishedObservedCost = &value
	}
	return result
}

func cloneABSampleSet(source map[abSampleKey]struct{}) map[abSampleKey]struct{} {
	result := make(map[abSampleKey]struct{}, len(source))
	for key := range source {
		result[key] = struct{}{}
	}
	return result
}

func cloneABStringSet(source map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(source))
	for key := range source {
		result[key] = struct{}{}
	}
	return result
}

// abResumeSession owns an exclusive sibling lock for the lifetime of a resume.
// It also remembers the partial inode and digest so an external replacement is
// never overwritten or removed.
type abResumeSession struct {
	path          string
	lockPath      string
	lockFile      *os.File
	lockInfo      os.FileInfo
	partialInfo   os.FileInfo
	partialDigest string
}

func sealPartialABArtifact(artifact *partialABArtifact) error {
	if artifact == nil {
		return fmt.Errorf("partial artifact is required")
	}
	digest, err := partialABDigest(*artifact)
	if err != nil {
		return err
	}
	artifact.IntegrityDigest = digest
	return nil
}

func partialABDigest(artifact partialABArtifact) (string, error) {
	return contracts.CanonicalDigest(partialABIntegrityPayload{
		SchemaVersion: artifact.SchemaVersion, Kind: artifact.Kind,
		ExperimentID: artifact.ExperimentID, Intent: artifact.Intent,
		Authority: artifact.Authority, ManifestDigest: artifact.ManifestDigest,
		Plan: artifact.Plan, Control: artifact.Control, Candidate: artifact.Candidate,
		HoldoutDigest: artifact.HoldoutDigest, HoldoutCases: artifact.HoldoutCases,
		ExitCode: artifact.ExitCode,
	})
}

func verifyPartialABIntegrity(artifact partialABArtifact) error {
	if !contracts.IsDigest(artifact.IntegrityDigest) {
		return fmt.Errorf("integrity_digest is not a canonical sha256 digest")
	}
	digest, err := partialABDigest(artifact)
	if err != nil {
		return fmt.Errorf("compute canonical integrity digest: %w", err)
	}
	if digest != artifact.IntegrityDigest {
		return fmt.Errorf("canonical integrity digest does not match")
	}
	return nil
}

func openABResume(path string, expected abResumeExpectation) (*abResumeSession, abResumeState, error) {
	session, err := acquireABResumeLock(path)
	if err != nil {
		return nil, abResumeState{}, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = session.Close()
		}
	}()

	before, err := os.Lstat(path)
	if err != nil {
		return nil, abResumeState{}, fmt.Errorf("inspect partial artifact: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, abResumeState{}, fmt.Errorf("partial artifact must be a regular non-symlink file")
	}
	var artifact partialABArtifact
	if err := baseline.LoadJSON(path, &artifact, baseline.IOOptions{MaxBytes: maxPartialABBytes, Strict: true}); err != nil {
		return nil, abResumeState{}, fmt.Errorf("load partial artifact: %w", err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, abResumeState{}, fmt.Errorf("partial artifact changed while loading")
	}
	state, err := validatePartialABArtifact(artifact, expected)
	if err != nil {
		return nil, abResumeState{}, err
	}
	session.partialInfo = after
	session.partialDigest = artifact.IntegrityDigest
	closeOnError = false
	return session, state, nil
}

func acquireABResumeLock(partialPath string) (*abResumeSession, error) {
	if partialPath == "" {
		return nil, fmt.Errorf("resume partial path is required")
	}
	lockPath := partialPath + ".resume.lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("partial artifact is already locked by another resume")
		}
		return nil, fmt.Errorf("reserve resume lock: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		// Without an identity obtained from the opened descriptor there is no
		// safe way to unlink by pathname: another process could already have
		// replaced it. Leave the fail-closed lock behind instead of deleting a
		// path that we cannot prove we still own.
		return nil, fmt.Errorf("inspect resume lock: %w", err)
	}
	return &abResumeSession{path: partialPath, lockPath: lockPath, lockFile: file, lockInfo: info}, nil
}

func (session *abResumeSession) Close() error {
	if session == nil {
		return nil
	}
	var closeErr error
	if session.lockFile != nil {
		if err := session.lockFile.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			closeErr = errors.Join(closeErr, fmt.Errorf("close resume lock: %w", err))
		}
	}
	if session.lockInfo != nil {
		if err := removeOwnedABFile(session.lockPath, session.lockInfo); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("remove resume lock: %w", err))
		} else {
			session.lockInfo = nil
		}
	}
	return closeErr
}

func (session *abResumeSession) Save(artifact partialABArtifact) error {
	if session == nil {
		return fmt.Errorf("resume session is required")
	}
	if err := sealPartialABArtifact(&artifact); err != nil {
		return err
	}
	if err := session.verifyCurrentPartial(); err != nil {
		return err
	}
	staged, err := stageABJSON(session.path, artifact)
	if err != nil {
		return err
	}
	stagedInfo, err := os.Lstat(staged)
	if err != nil {
		return fmt.Errorf("inspect staged resumed partial: %w", err)
	}
	defer removeABStageIfOwned(staged, stagedInfo)
	info, err := replaceOwnedABFile(session.path, staged, session.partialInfo)
	if err != nil {
		return fmt.Errorf("replace resumed partial artifact: %w", err)
	}
	session.partialInfo = info
	session.partialDigest = artifact.IntegrityDigest
	return nil
}

func removeABStageIfOwned(path string, expected os.FileInfo) {
	current, err := os.Lstat(path)
	if err != nil || !stableABFileIdentity(expected, current) {
		return
	}
	_ = removeOwnedABFile(path, expected)
}

// replaceOwnedABFile atomically exchanges a staged file with the currently
// owned path, then proves that the displaced inode was the expected one before
// deleting it. A raced-in file is exchanged back and preserved.
func replaceOwnedABFile(path, staged string, expected os.FileInfo) (os.FileInfo, error) {
	if expected == nil {
		return nil, fmt.Errorf("owned file identity is required")
	}
	stagedBefore, err := os.Lstat(staged)
	if err != nil || stagedBefore.Mode()&os.ModeSymlink != 0 || !stagedBefore.Mode().IsRegular() {
		if err == nil {
			err = fmt.Errorf("staged replacement is not a regular non-symlink file")
		}
		return nil, err
	}
	if err := exchangeABFiles(staged, path); err != nil {
		return nil, fmt.Errorf("exchange staged and owned files: %w", err)
	}
	displaced, displacedErr := os.Lstat(staged)
	published, publishedErr := os.Lstat(path)
	if displacedErr != nil || publishedErr != nil || !stableABFileIdentity(expected, displaced) ||
		!stableABFileIdentity(stagedBefore, published) {
		rollbackErr := exchangeABFiles(staged, path)
		return nil, errors.Join(fmt.Errorf("owned file changed during atomic replacement"), rollbackErr)
	}
	if err := os.Remove(staged); err != nil {
		return nil, fmt.Errorf("remove displaced owned file: %w", err)
	}
	if err := syncABDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return published, nil
}

func (session *abResumeSession) RemoveAfterSuccess() error {
	if session == nil {
		return nil
	}
	if err := session.verifyCurrentPartial(); err != nil {
		return err
	}
	if err := removeOwnedABFile(session.path, session.partialInfo); err != nil {
		return fmt.Errorf("remove completed partial artifact: %w", err)
	}
	session.partialInfo = nil
	return nil
}

func (session *abResumeSession) verifyCurrentPartial() error {
	if session.partialInfo == nil {
		return fmt.Errorf("partial artifact is no longer owned by this resume")
	}
	current, err := os.Lstat(session.path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(session.partialInfo, current) {
		return fmt.Errorf("partial artifact changed during resume")
	}
	var artifact partialABArtifact
	if err := baseline.LoadJSON(session.path, &artifact, baseline.IOOptions{MaxBytes: maxPartialABBytes, Strict: true}); err != nil {
		return fmt.Errorf("recheck partial artifact: %w", err)
	}
	after, err := os.Lstat(session.path)
	if err != nil || !os.SameFile(current, after) || current.Size() != after.Size() || !current.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("partial artifact changed during resume")
	}
	if err := verifyPartialABIntegrity(artifact); err != nil || artifact.IntegrityDigest != session.partialDigest {
		return fmt.Errorf("partial artifact changed during resume")
	}
	return nil
}

func syncABDirectory(directory string) error {
	if directory == "" {
		directory = "."
	}
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open result directory: %w", err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return fmt.Errorf("sync result directory: %w", err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("close result directory: %w", err)
	}
	return nil
}

// removeOwnedABFile first moves the path into a private same-directory
// quarantine and checks the moved inode before unlinking it. If another writer
// won the race, its file is restored with an O_EXCL-style hard link when
// possible and is never deleted.
func removeOwnedABFile(path string, expected os.FileInfo) error {
	if expected == nil {
		return fmt.Errorf("owned file identity is required")
	}
	current, err := os.Lstat(path)
	if err != nil || !sameOwnedABFile(expected, current) {
		return fmt.Errorf("refusing to remove changed result target")
	}
	directory := filepath.Dir(path)
	if directory == "" {
		directory = "."
	}
	quarantine, err := os.MkdirTemp(directory, ".skynex-remove-")
	if err != nil {
		return fmt.Errorf("create private removal quarantine: %w", err)
	}
	quarantinedPath := filepath.Join(quarantine, "owned")
	if err := os.Rename(path, quarantinedPath); err != nil {
		_ = os.Remove(quarantine)
		return fmt.Errorf("quarantine owned result: %w", err)
	}
	moved, statErr := os.Lstat(quarantinedPath)
	if statErr != nil || !sameOwnedABFile(expected, moved) {
		restoreErr := os.Link(quarantinedPath, path)
		if restoreErr == nil {
			_ = os.Remove(quarantinedPath)
			_ = os.Remove(quarantine)
		}
		return fmt.Errorf("result target changed immediately before removal; unexpected file preserved")
	}
	if err := os.Remove(quarantinedPath); err != nil {
		return fmt.Errorf("remove quarantined owned result: %w", err)
	}
	if err := os.Remove(quarantine); err != nil {
		return fmt.Errorf("remove result quarantine: %w", err)
	}
	return syncABDirectory(directory)
}

func sameOwnedABFile(expected, current os.FileInfo) bool {
	return expected != nil && current != nil && current.Mode().IsRegular() &&
		os.SameFile(expected, current) && current.Size() == expected.Size()
}

func validatePartialABArtifact(artifact partialABArtifact, expected abResumeExpectation) (abResumeState, error) {
	if err := verifyPartialABIntegrity(artifact); err != nil {
		return abResumeState{}, err
	}
	if artifact.SchemaVersion != partialABSchemaVersion || artifact.Kind != partialABKind {
		return abResumeState{}, fmt.Errorf("partial artifact type or schema version does not match")
	}
	if artifact.ExperimentID != expected.ExperimentID || artifact.Intent != expected.Intent ||
		artifact.Authority != expected.Authority || artifact.ManifestDigest != expected.ManifestDigest {
		return abResumeState{}, fmt.Errorf("partial experiment identity, intent, authority, or manifest digest does not match")
	}
	if artifact.HoldoutDigest != expected.HoldoutDigest || artifact.HoldoutCases != expected.HoldoutCases {
		return abResumeState{}, fmt.Errorf("partial holdout population does not match")
	}
	observedPlan, err := contracts.CanonicalJSON(artifact.Plan)
	if err != nil {
		return abResumeState{}, fmt.Errorf("canonicalize partial plan: %w", err)
	}
	expectedPlan, err := contracts.CanonicalJSON(expected.PublishedPlan)
	if err != nil {
		return abResumeState{}, fmt.Errorf("canonicalize expected plan: %w", err)
	}
	if !bytes.Equal(observedPlan, expectedPlan) {
		return abResumeState{}, fmt.Errorf("partial experiment plan does not match exactly")
	}
	if artifact.ExitCode < contracts.ExitSuccess || artifact.ExitCode > contracts.ExitBudgetExhausted {
		return abResumeState{}, fmt.Errorf("partial exit code is not resumable")
	}
	if artifact.Control.Suite != expected.Suite || artifact.Candidate.Suite != expected.Suite ||
		artifact.Control.Complete || artifact.Candidate.Complete {
		return abResumeState{}, fmt.Errorf("partial result set metadata does not match")
	}
	if artifact.Control.Started.IsZero() || !artifact.Control.Started.Equal(artifact.Candidate.Started) ||
		artifact.Control.Ended.IsZero() || !artifact.Control.Ended.Equal(artifact.Candidate.Ended) ||
		artifact.Control.Ended.Before(artifact.Control.Started) {
		return abResumeState{}, fmt.Errorf("partial result timestamps are invalid or inconsistent")
	}

	expectedKeys := make(map[abSampleKey]struct{}, len(expected.ExecutionPlan.Blocks)*2)
	for _, block := range expected.ExecutionPlan.Blocks {
		for _, variant := range block.Order {
			key := abSampleKey{CaseID: block.CaseID, Variant: variant, Repetition: block.Repetition}
			if _, duplicate := expectedKeys[key]; duplicate {
				return abResumeState{}, fmt.Errorf("expected plan contains a duplicate sample coordinate")
			}
			expectedKeys[key] = struct{}{}
		}
	}
	if len(artifact.Control.Samples) > len(expected.ExecutionPlan.Blocks) || len(artifact.Candidate.Samples) > len(expected.ExecutionPlan.Blocks) {
		return abResumeState{}, fmt.Errorf("partial sample population exceeds the experiment plan")
	}

	state := abResumeState{
		Control: artifact.Control, Candidate: artifact.Candidate,
		Completed: make(map[abSampleKey]struct{}, len(artifact.Control.Samples)+len(artifact.Candidate.Samples)),
		RunIDs:    make(map[string]struct{}, len(artifact.Control.Samples)+len(artifact.Candidate.Samples)),
		Started:   artifact.Control.Started,
	}
	state.Control.Samples = append([]contracts.RunResult(nil), artifact.Control.Samples...)
	state.Candidate.Samples = append([]contracts.RunResult(nil), artifact.Candidate.Samples...)
	if err := validatePartialABArm(&state.Control, stats.VariantControl, expectedKeys, expected, state.Completed, state.RunIDs); err != nil {
		return abResumeState{}, fmt.Errorf("control partial samples: %w", err)
	}
	if err := validatePartialABArm(&state.Candidate, stats.VariantCandidate, expectedKeys, expected, state.Completed, state.RunIDs); err != nil {
		return abResumeState{}, fmt.Errorf("candidate partial samples: %w", err)
	}
	return state, nil
}

func validatePartialABArm(result *runner.ContractResult, arm stats.Variant, expectedKeys map[abSampleKey]struct{}, expected abResumeExpectation, completed map[abSampleKey]struct{}, runIDs map[string]struct{}) error {
	for index := range result.Samples {
		sample := result.Samples[index]
		if err := sample.Validate(); err != nil {
			return fmt.Errorf("sample %d is invalid", index)
		}
		if sample.Variant != string(arm) {
			return fmt.Errorf("sample %d has the wrong arm", index)
		}
		if _, duplicate := runIDs[sample.RunID]; duplicate {
			return fmt.Errorf("sample %d duplicates a run id", index)
		}

		actualCaseID := sample.CaseID
		if holdoutCaseID, isHoldout := expected.HoldoutByRef[sample.CaseID]; isHoldout {
			canonical, canonicalErr := contracts.CanonicalJSON(sample)
			redacted, redactedErr := contracts.CanonicalJSON(sanitizeHoldoutRun(sample, sample.CaseID))
			if canonicalErr != nil || redactedErr != nil || !bytes.Equal(canonical, redacted) {
				return fmt.Errorf("sample %d contains non-canonical holdout data", index)
			}
			actualCaseID = holdoutCaseID
		} else if err := validateResumedPublicSample(sample, arm, expected); err != nil {
			return fmt.Errorf("sample %d does not match frozen public inputs", index)
		}

		actualKey := abSampleKey{CaseID: actualCaseID, Variant: arm, Repetition: sample.Repetition}
		if _, exists := expectedKeys[actualKey]; !exists {
			return fmt.Errorf("sample %d is outside the frozen case/variant/repetition population", index)
		}
		if _, duplicate := completed[actualKey]; duplicate {
			return fmt.Errorf("sample %d duplicates a case/variant/repetition coordinate", index)
		}
		completed[actualKey] = struct{}{}
		runIDs[sample.RunID] = struct{}{}
		result.Samples[index].CaseID = actualCaseID
	}
	return nil
}

func validateResumedPublicSample(sample contracts.RunResult, arm stats.Variant, expected abResumeExpectation) error {
	testCase, exists := expected.CasesByID[sample.CaseID]
	if !exists {
		return fmt.Errorf("case is not in the public catalog")
	}
	if expected.ModelAssignment != nil {
		testCase.Agent.Model = expected.ModelAssignment.Control
		if arm == stats.VariantCandidate {
			testCase.Agent.Model = expected.ModelAssignment.Candidate
		}
	}
	caseDigest, err := testCase.Digest()
	if err != nil {
		return fmt.Errorf("digest expected case: %w", err)
	}
	provider, _, err := contracts.ParseModelSelection(testCase.Agent.Model)
	if err != nil {
		return fmt.Errorf("parse expected model: %w", err)
	}
	bundleDigest := expected.ControlDigest
	if arm == stats.VariantCandidate {
		bundleDigest = expected.CandidateDigest
	}
	extensions := sample.Provenance.Extensions
	if sample.Provenance.CaseDigest != caseDigest || sample.Provenance.FixtureDigest != testCase.Fixture.ExpectedDigest ||
		sample.Provenance.Model != testCase.Agent.Model || sample.Provenance.Provider != provider ||
		sample.Provenance.PromptDigest != bundleDigest || sample.Provenance.OpenCodeVersion != expected.Execution.OpenCodeVersion ||
		sample.Provenance.OpenCodeAPIDigest != expected.Execution.OpenCodeOpenAPIDigest ||
		string(sample.Provenance.ExecutionMode) != expected.Execution.Mode || string(sample.Provenance.Network) != expected.Execution.Network ||
		extensions[provenanceExtensionAgentBundleDigest] != bundleDigest ||
		extensions[provenanceExtensionHarnessBundleDigest] != expected.HarnessDigest ||
		extensions[provenanceExtensionManifestDigest] != expected.ManifestDigest ||
		extensions[contracts.ProvenanceExtensionProviderAuthMode] != expected.Execution.ProviderAuth ||
		extensions[contracts.ProvenanceExtensionBillingMode] != expected.Execution.BillingMode ||
		extensions[contracts.ProvenanceExtensionCredentialBoundary] != expected.Execution.CredentialBoundary ||
		extensions[contracts.ProvenanceExtensionAuthIsolation] != contracts.AuthIsolationDedicatedFreshTokenFailStopV1 ||
		extensions[provenanceExtensionToolchainsDigest] != expected.Execution.ToolchainsDigest {
		return fmt.Errorf("sample provenance differs from the frozen experiment")
	}
	return nil
}

func validateNewABSamples(samples []contracts.RunResult, expected abSampleKey, completed map[abSampleKey]struct{}, runIDs map[string]struct{}, requireSample bool, holdoutReference string) error {
	if len(samples) > 1 || requireSample && len(samples) != 1 {
		return fmt.Errorf("runner must return exactly one sample for a single A/B coordinate")
	}
	for index := range samples {
		sample := samples[index]
		validated := sample
		if holdoutReference != "" {
			validated = sanitizeHoldoutRun(sample, holdoutReference)
		}
		if err := validated.Validate(); err != nil {
			return fmt.Errorf("runner sample %d is invalid: %w", index, err)
		}
		key := abSampleKey{CaseID: sample.CaseID, Variant: stats.Variant(sample.Variant), Repetition: sample.Repetition}
		if key != expected {
			return fmt.Errorf("runner sample is outside its requested case/variant/repetition coordinate")
		}
		if _, duplicate := completed[key]; duplicate {
			return fmt.Errorf("runner sample duplicates a completed case/variant/repetition coordinate")
		}
		if _, duplicate := runIDs[sample.RunID]; duplicate {
			return fmt.Errorf("runner sample duplicates a retained run id")
		}
	}
	return nil
}

func rejectABSamplesAfterClosureDrift(run *modelRunResult, closureErr error) error {
	if closureErr == nil {
		return nil
	}
	if run != nil {
		run.Result.Samples = nil
	}
	return invalidf("invalid_toolchain_closure", "effective executable closure drifted after an A/B sample: %v", closureErr)
}

func markDeferredABSampleFailure(samples []contracts.RunResult) {
	for index := range samples {
		if samples[index].Status != contracts.RunStatusPass {
			continue
		}
		samples[index].Status = contracts.RunStatusInfraError
		samples[index].Error = &contracts.RunError{
			Kind: "deferred_runner_error", Message: "runner reported an error after producing this sample",
		}
	}
}

func effectiveABCases(testCases []contracts.Case, assignment *experiment.ModelAssignment, variant stats.Variant) []contracts.Case {
	effective := append([]contracts.Case(nil), testCases...)
	if assignment == nil {
		return effective
	}
	model := assignment.Control
	if variant == stats.VariantCandidate {
		model = assignment.Candidate
	}
	for index := range effective {
		effective[index].Agent.Model = model
	}
	return effective
}

func recordABSamples(samples []contracts.RunResult, completed map[abSampleKey]struct{}, runIDs map[string]struct{}) {
	for _, sample := range samples {
		completed[abSampleKey{CaseID: sample.CaseID, Variant: stats.Variant(sample.Variant), Repetition: sample.Repetition}] = struct{}{}
		runIDs[sample.RunID] = struct{}{}
	}
}

func abBlockComplete(completed map[abSampleKey]struct{}, block stats.BlockPlan) bool {
	for _, variant := range block.Order {
		if _, exists := completed[abSampleKey{CaseID: block.CaseID, Variant: variant, Repetition: block.Repetition}]; !exists {
			return false
		}
	}
	return true
}

func validateCompleteABPopulation(completed map[abSampleKey]struct{}, plan stats.ExperimentPlan) error {
	if len(completed) != len(plan.Blocks)*2 {
		return fmt.Errorf("completed A/B population has %d samples, expected %d", len(completed), len(plan.Blocks)*2)
	}
	for _, block := range plan.Blocks {
		if !abBlockComplete(completed, block) {
			return fmt.Errorf("completed A/B population is missing a planned coordinate")
		}
	}
	return nil
}

func sameABOutputPath(first, second string) bool {
	firstAbsolute, firstErr := filepath.Abs(first)
	secondAbsolute, secondErr := filepath.Abs(second)
	return firstErr == nil && secondErr == nil && filepath.Clean(firstAbsolute) == filepath.Clean(secondAbsolute)
}
