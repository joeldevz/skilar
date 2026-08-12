// Package experiment validates and freezes the trusted inputs to a paired
// evaluation. It is deliberately independent of the runner: control and
// candidate code can never supply their own suite, oracle, or gate policy.
package experiment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/stats"
	"github.com/joeldevz/skynex/schemas"
)

const SchemaVersion = 1

const (
	IntentDevelopment  = "development"
	IntentRelease      = "release"
	MinimumReleaseRuns = 10

	ProviderAuthOpenAIOAuthCleanProfileV1 = contracts.ProviderAuthModeOpenAIOAuthCleanProfileV1
	BillingModeChatGPTSubscription        = contracts.BillingModeChatGPTSubscription
	CredentialBoundaryRuntimeReadable     = "runtime-readable"
	CredentialBoundaryProviderProxy       = "provider-proxy"
)

type FrozenBundle struct {
	Root                   string `json:"root"`
	Digest                 string `json:"digest"`
	GitSHA                 string `json:"git_sha,omitempty"`
	DirtyPatchDigest       string `json:"dirty_patch_digest,omitempty"`
	SourceGitSHA           string `json:"source_git_sha,omitempty"`
	SourceDirtyPatchDigest string `json:"source_dirty_patch_digest,omitempty"`
}

type Randomization struct {
	Method               string `json:"method"`
	Seed                 string `json:"seed"`
	SerializeWithinBlock bool   `json:"serialize_within_block"`
}

type Execution struct {
	Mode                  string `json:"mode"`
	Network               string `json:"network"`
	Concurrency           int    `json:"concurrency"`
	ProviderAuth          string `json:"provider_auth"`
	BillingMode           string `json:"billing_mode"`
	CredentialBoundary    string `json:"credential_boundary"`
	OpenCodeVersion       string `json:"opencode_version"`
	EvaluatorBinaryDigest string `json:"evaluator_binary_digest"`
	OpenCodeBinaryDigest  string `json:"opencode_binary_digest,omitempty"`
	OpenCodeOpenAPIDigest string `json:"opencode_openapi_digest,omitempty"`
	ToolchainsDigest      string `json:"toolchains_digest"`
	ContainerImageDigest  string `json:"container_image_digest,omitempty"`
}

type Gates struct {
	CriticalCasePassRate    float64 `json:"critical_case_pass_rate"`
	PassToFailRegressions   int     `json:"pass_to_fail_regressions"`
	ScopeViolations         int     `json:"scope_violations"`
	FalseSuccesses          int     `json:"false_successes"`
	MaxParentPeakInputRatio float64 `json:"max_parent_peak_input_ratio"`
	MaxTreeInputRatio       float64 `json:"max_tree_input_ratio"`
	MaxCostRatio            float64 `json:"max_cost_ratio"`
	MaxWallTimeRatio        float64 `json:"max_wall_time_ratio"`
	MaxRetryRateRatio       float64 `json:"max_retry_rate_ratio"`
	Confidence              float64 `json:"confidence,omitempty"`
	MinimumPairs            int     `json:"minimum_pairs,omitempty"`
}

// ModelAssignment freezes an optional model override per A/B arm. Case models
// remain authoritative when this field is absent. Provider is intentionally
// derived from the provider/model value so the two cannot contradict.
type ModelAssignment struct {
	Control   string `json:"control"`
	Candidate string `json:"candidate"`
}

// Manifest contains all choices which must be committed before observing an
// experiment. Roots may be relative to the manifest file's directory.
type Manifest struct {
	SchemaVersion          int              `json:"schema_version"`
	ID                     string           `json:"id"`
	Suite                  string           `json:"suite"`
	Intent                 string           `json:"intent"`
	Harness                FrozenBundle     `json:"harness"`
	Control                FrozenBundle     `json:"control"`
	Candidate              FrozenBundle     `json:"candidate"`
	Holdout                *FrozenBundle    `json:"holdout,omitempty"`
	ModelAssignment        *ModelAssignment `json:"model_assignment,omitempty"`
	IntentionalDifferences []baseline.Field `json:"intentional_differences"`
	PublicCaseCount        int              `json:"public_case_count"`
	PublicCasesDigest      string           `json:"public_cases_digest"`
	CriticalCaseIDs        []string         `json:"critical_case_ids"`
	HoldoutCaseCount       int              `json:"holdout_case_count"`
	Runs                   int              `json:"runs"`
	Randomization          Randomization    `json:"randomization"`
	Execution              Execution        `json:"execution"`
	Gates                  Gates            `json:"gates"`
}

// Load rejects unknown fields, duplicate JSON keys, oversized input, and an
// invalid semantic contract before returning any experiment choices.
func Load(path string) (*Manifest, error) {
	var raw json.RawMessage
	if err := baseline.LoadJSON(path, &raw, baseline.IOOptions{MaxBytes: 1 << 20}); err != nil {
		return nil, fmt.Errorf("load experiment manifest: %w", err)
	}
	if err := validateManifestPresence(raw); err != nil {
		return nil, fmt.Errorf("validate experiment manifest presence: %w", err)
	}
	if err := schemas.ValidateJSON(schemas.EvalExperiment, raw); err != nil {
		return nil, fmt.Errorf("published experiment schema: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode experiment manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("validate experiment manifest: %w", err)
	}
	return &manifest, nil
}

func validateManifestPresence(raw []byte) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("decode manifest object: %w", err)
	}
	if err := requireJSONFields("manifest", document,
		"schema_version", "id", "suite", "intent", "harness", "control", "candidate",
		"intentional_differences", "public_case_count", "public_cases_digest", "critical_case_ids",
		"holdout_case_count", "runs", "randomization", "execution", "gates"); err != nil {
		return err
	}
	for _, name := range []string{"harness", "control", "candidate"} {
		if err := requireNestedJSONFields(document[name], name, "root", "digest"); err != nil {
			return err
		}
	}
	if raw, exists := document["holdout"]; exists {
		if err := requireNestedJSONFields(raw, "holdout", "root", "digest"); err != nil {
			return err
		}
	}
	if raw, exists := document["model_assignment"]; exists {
		if err := requireNestedJSONFields(raw, "model_assignment", "control", "candidate"); err != nil {
			return err
		}
	}
	if err := requireNestedJSONFields(document["randomization"], "randomization", "method", "seed", "serialize_within_block"); err != nil {
		return err
	}
	if err := requireNestedJSONFields(document["execution"], "execution",
		"mode", "network", "concurrency", "provider_auth", "billing_mode", "credential_boundary",
		"opencode_version", "evaluator_binary_digest", "opencode_binary_digest", "opencode_openapi_digest", "toolchains_digest"); err != nil {
		return err
	}
	return requireNestedJSONFields(document["gates"], "gates",
		"critical_case_pass_rate", "pass_to_fail_regressions", "scope_violations", "false_successes",
		"max_parent_peak_input_ratio", "max_tree_input_ratio", "max_cost_ratio", "max_wall_time_ratio", "max_retry_rate_ratio")
}

func requireNestedJSONFields(raw json.RawMessage, location string, fields ...string) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("%s must be a non-null object", location)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return fmt.Errorf("%s must be an object: %w", location, err)
	}
	return requireJSONFields(location, object, fields...)
}

func requireJSONFields(location string, object map[string]json.RawMessage, fields ...string) error {
	for _, field := range fields {
		raw, exists := object[field]
		if !exists || len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%s.%s is required and must not be null", location, field)
		}
	}
	return nil
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must equal %d", SchemaVersion)
	}
	if !manifestIDPattern.MatchString(m.ID) {
		return fmt.Errorf("id is invalid")
	}
	if !manifestIDPattern.MatchString(m.Suite) {
		return fmt.Errorf("suite is invalid")
	}
	if m.Intent != IntentDevelopment && m.Intent != IntentRelease {
		return fmt.Errorf("intent must equal %q or %q", IntentDevelopment, IntentRelease)
	}
	for name, bundle := range map[string]FrozenBundle{
		"harness": m.Harness, "control": m.Control, "candidate": m.Candidate,
	} {
		if err := validateBundle(bundle); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if filepath.Clean(m.Control.Root) == filepath.Clean(m.Candidate.Root) {
		return fmt.Errorf("control and candidate must reference distinct bundle roots")
	}
	if m.Holdout != nil {
		if err := validateBundle(*m.Holdout); err != nil {
			return fmt.Errorf("holdout: %w", err)
		}
		if m.Holdout.SourceGitSHA != "" || m.Holdout.SourceDirtyPatchDigest != "" {
			return fmt.Errorf("holdout source Git provenance must not be published")
		}
	}
	if m.PublicCaseCount < 1 || m.PublicCaseCount > 10_000 {
		return fmt.Errorf("public_case_count must be between 1 and 10000")
	}
	if !contracts.IsDigest(m.PublicCasesDigest) {
		return fmt.Errorf("public_cases_digest must be a canonical sha256 digest")
	}
	if len(m.CriticalCaseIDs) == 0 || len(m.CriticalCaseIDs) > m.PublicCaseCount {
		return fmt.Errorf("critical_case_ids must contain between 1 and public_case_count entries")
	}
	previousCriticalID := ""
	for _, id := range m.CriticalCaseIDs {
		if !manifestIDPattern.MatchString(id) {
			return fmt.Errorf("critical_case_ids contains invalid id %q", id)
		}
		if previousCriticalID != "" && id <= previousCriticalID {
			return fmt.Errorf("critical_case_ids must be sorted and unique")
		}
		previousCriticalID = id
	}
	if m.HoldoutCaseCount < 0 || m.HoldoutCaseCount > 10_000 {
		return fmt.Errorf("holdout_case_count must be between 0 and 10000")
	}
	if m.Holdout == nil && m.HoldoutCaseCount != 0 {
		return fmt.Errorf("holdout_case_count must be zero without a holdout bundle")
	}
	if m.Holdout != nil && m.HoldoutCaseCount == 0 {
		return fmt.Errorf("holdout_case_count must be positive when a holdout bundle is declared")
	}
	if len(m.IntentionalDifferences) == 0 {
		return fmt.Errorf("intentional_differences must not be empty")
	}
	allowedDifferences := map[baseline.Field]bool{
		baseline.FieldPromptDigest: true, baseline.FieldAgentBundleDigest: true,
		baseline.FieldModel: true, baseline.FieldProvider: true,
	}
	seenDifferences := make(map[baseline.Field]struct{}, len(m.IntentionalDifferences))
	for _, field := range m.IntentionalDifferences {
		if !allowedDifferences[field] {
			return fmt.Errorf("intentional_differences contains unsupported field %q", field)
		}
		if _, duplicate := seenDifferences[field]; duplicate {
			return fmt.Errorf("intentional_differences contains duplicate field %q", field)
		}
		seenDifferences[field] = struct{}{}
	}
	if err := validateModelAssignment(m.ModelAssignment, seenDifferences); err != nil {
		return err
	}
	_, promptDifference := seenDifferences[baseline.FieldPromptDigest]
	_, agentDifference := seenDifferences[baseline.FieldAgentBundleDigest]
	if promptDifference != agentDifference {
		return fmt.Errorf("prompt_digest and agent_bundle_digest must be declared together")
	}
	if m.Control.Digest == m.Candidate.Digest {
		if m.ModelAssignment == nil || promptDifference {
			return fmt.Errorf("control and candidate have the same digest without a model-only treatment; treatment was not realized")
		}
	} else if !promptDifference {
		return fmt.Errorf("different control and candidate bundles require prompt_digest and agent_bundle_digest intentional differences")
	}
	if m.Runs < 2 || m.Runs > contracts.MaxRuns {
		return fmt.Errorf("runs must be between 2 and %d", contracts.MaxRuns)
	}
	if m.Intent == IntentRelease {
		if m.Holdout == nil {
			return fmt.Errorf("release intent requires an external holdout bundle")
		}
		if m.Runs < MinimumReleaseRuns {
			return fmt.Errorf("release intent requires at least %d paired runs per case", MinimumReleaseRuns)
		}
	}
	if m.Randomization.Method != stats.BalancedBlockedMethod {
		return fmt.Errorf("randomization.method must equal %q", stats.BalancedBlockedMethod)
	}
	if _, err := parseSeed(m.Randomization.Seed); err != nil {
		return err
	}
	if !m.Randomization.SerializeWithinBlock {
		return fmt.Errorf("randomization.serialize_within_block must be true")
	}
	if err := validateExecution(m.Execution, m.Intent); err != nil {
		return err
	}
	if err := validateGates(m.Gates, m.Runs); err != nil {
		return err
	}
	if m.Intent == IntentRelease {
		if err := validateReleaseGates(m.Gates); err != nil {
			return err
		}
	}
	return nil
}

func validateReleaseGates(gates Gates) error {
	if gates.CriticalCasePassRate != 1 || gates.PassToFailRegressions != 0 || gates.ScopeViolations != 0 || gates.FalseSuccesses != 0 {
		return fmt.Errorf("release gates require critical_case_pass_rate=1 and zero pass-to-fail, scope, and false-success allowances")
	}
	for name, value := range map[string]struct{ observed, maximum float64 }{
		"max_parent_peak_input_ratio": {gates.MaxParentPeakInputRatio, 0.70},
		"max_tree_input_ratio":        {gates.MaxTreeInputRatio, 1.00},
		"max_cost_ratio":              {gates.MaxCostRatio, 1.00},
		"max_wall_time_ratio":         {gates.MaxWallTimeRatio, 1.10},
		"max_retry_rate_ratio":        {gates.MaxRetryRateRatio, 1.00},
	} {
		if value.observed > value.maximum {
			return fmt.Errorf("release gates.%s must be at most %.2f", name, value.maximum)
		}
	}
	confidence := gates.Confidence
	if confidence == 0 {
		confidence = 0.95
	}
	if confidence < 0.95 {
		return fmt.Errorf("release gates.confidence must be at least 0.95")
	}
	if gates.MinimumPairs < MinimumReleaseRuns {
		return fmt.Errorf("release gates.minimum_pairs must be at least %d", MinimumReleaseRuns)
	}
	return nil
}

func validateModelAssignment(assignment *ModelAssignment, differences map[baseline.Field]struct{}) error {
	_, modelDeclared := differences[baseline.FieldModel]
	_, providerDeclared := differences[baseline.FieldProvider]
	if assignment == nil {
		if modelDeclared || providerDeclared {
			return fmt.Errorf("model_assignment is required when model or provider is an intentional difference")
		}
		return nil
	}
	controlProvider, _, controlErr := contracts.ParseModelSelection(assignment.Control)
	candidateProvider, _, candidateErr := contracts.ParseModelSelection(assignment.Candidate)
	if controlErr != nil || candidateErr != nil {
		return fmt.Errorf("model_assignment values are invalid: %v", errors.Join(controlErr, candidateErr))
	}
	modelChanged := assignment.Control != assignment.Candidate
	providerChanged := controlProvider != candidateProvider
	if !modelChanged {
		return fmt.Errorf("model_assignment control and candidate must differ")
	}
	if !modelDeclared {
		return fmt.Errorf("model_assignment requires model in intentional_differences")
	}
	if providerChanged != providerDeclared {
		if providerChanged {
			return fmt.Errorf("provider must be declared as an intentional difference when model_assignment changes provider")
		}
		return fmt.Errorf("provider is declared as an intentional difference but model_assignment keeps the same provider")
	}
	return nil
}

// Plan creates the committed balanced AB/BA order for all selected cases.
func (m Manifest) Plan(caseIDs []string) (stats.ExperimentPlan, error) {
	if err := m.Validate(); err != nil {
		return stats.ExperimentPlan{}, err
	}
	seed, _ := parseSeed(m.Randomization.Seed)
	return stats.NewBalancedBlockedPlan(caseIDs, m.Runs, seed)
}

type VerifiedBundle struct {
	Name         string
	AbsoluteRoot string
	Expected     FrozenBundle
	Snapshot     sandbox.Snapshot
}

// FrozenSet retains the exact pre-run observations so candidate or harness
// drift can be detected after each block, not merely at process startup.
type FrozenSet struct {
	Bundles []VerifiedBundle
	limits  sandbox.SnapshotLimits
}

// VerifyBundles resolves every root against baseDir, rejects unsafe trees, and
// compares its canonical digest with the manifest before any model call.
func (m Manifest) VerifyBundles(baseDir string, limits sandbox.SnapshotLimits) (*FrozenSet, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	baseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve manifest directory: %w", err)
	}
	specs := []struct {
		name   string
		bundle FrozenBundle
	}{
		{name: "harness", bundle: m.Harness},
		{name: "control", bundle: m.Control},
		{name: "candidate", bundle: m.Candidate},
	}
	if m.Holdout != nil {
		specs = append(specs, struct {
			name   string
			bundle FrozenBundle
		}{name: "holdout", bundle: *m.Holdout})
	}
	type resolvedSpec struct {
		name   string
		bundle FrozenBundle
		root   string
	}
	resolved := make([]resolvedSpec, 0, len(specs))
	for _, spec := range specs {
		root := spec.bundle.Root
		if !filepath.IsAbs(root) {
			root = filepath.Join(baseDir, root)
		}
		root, err = filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve %s bundle: %w", spec.name, err)
		}
		root = filepath.Clean(root)
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("canonicalize %s bundle: %w", spec.name, err)
		}
		root = filepath.Clean(root)
		for _, previous := range resolved {
			overlaps, overlapErr := bundleRootsOverlap(previous.root, root)
			if overlapErr != nil {
				return nil, fmt.Errorf("compare %s and %s bundle roots: %w", previous.name, spec.name, overlapErr)
			}
			if overlaps {
				return nil, fmt.Errorf("%s and %s bundle roots must be disjoint", previous.name, spec.name)
			}
		}
		resolved = append(resolved, resolvedSpec{name: spec.name, bundle: spec.bundle, root: root})
	}

	set := &FrozenSet{limits: limits}
	for _, spec := range resolved {
		root := spec.root
		snapshot, digestErr := sandbox.DigestTree(root, limits)
		if digestErr != nil {
			return nil, fmt.Errorf("digest %s bundle: %w", spec.name, digestErr)
		}
		if snapshot.Digest != spec.bundle.Digest {
			return nil, fmt.Errorf("%s bundle digest mismatch: got %s, expected %s", spec.name, snapshot.Digest, spec.bundle.Digest)
		}
		if gitErr := verifyBundleGitProvenance(root, spec.bundle); gitErr != nil {
			return nil, fmt.Errorf("verify %s bundle Git provenance: %w", spec.name, gitErr)
		}
		set.Bundles = append(set.Bundles, VerifiedBundle{
			Name: spec.name, AbsoluteRoot: root, Expected: spec.bundle, Snapshot: snapshot,
		})
	}
	return set, nil
}

func bundleRootsOverlap(left, right string) (bool, error) {
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return false, err
		}
		if relative == "." {
			return true, nil
		}
		if !filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true, nil
		}
	}
	return false, nil
}

// VerifyUnchanged fails closed if any frozen input has changed or become
// unsafe since it was first verified.
func (s *FrozenSet) VerifyUnchanged() error {
	if s == nil || len(s.Bundles) == 0 {
		return fmt.Errorf("no verified bundles")
	}
	for _, bundle := range s.Bundles {
		current, err := sandbox.DigestTree(bundle.AbsoluteRoot, s.limits)
		if err != nil {
			return fmt.Errorf("recheck %s bundle: %w", bundle.Name, err)
		}
		if current.Digest != bundle.Snapshot.Digest {
			return fmt.Errorf("%s bundle drifted: got %s, expected %s", bundle.Name, current.Digest, bundle.Snapshot.Digest)
		}
		if gitErr := verifyBundleGitProvenance(bundle.AbsoluteRoot, bundle.Expected); gitErr != nil {
			return fmt.Errorf("recheck %s bundle Git provenance: %w", bundle.Name, gitErr)
		}
	}
	return nil
}

func validateBundle(bundle FrozenBundle) error {
	if strings.TrimSpace(bundle.Root) == "" || strings.ContainsRune(bundle.Root, '\x00') || len(bundle.Root) > 4096 {
		return fmt.Errorf("root is required and must not contain NUL")
	}
	if !digestPattern.MatchString(bundle.Digest) {
		return fmt.Errorf("digest must be sha256")
	}
	if bundle.GitSHA != "" && !gitSHAPattern.MatchString(bundle.GitSHA) {
		return fmt.Errorf("git_sha must contain 40 to 64 lowercase hexadecimal characters")
	}
	if bundle.DirtyPatchDigest != "" && !digestPattern.MatchString(bundle.DirtyPatchDigest) {
		return fmt.Errorf("dirty_patch_digest must be sha256")
	}
	if bundle.DirtyPatchDigest != "" && bundle.GitSHA == "" {
		return fmt.Errorf("dirty_patch_digest requires git_sha")
	}
	if bundle.SourceGitSHA != "" && !gitSHAPattern.MatchString(bundle.SourceGitSHA) {
		return fmt.Errorf("source_git_sha must contain 40 to 64 lowercase hexadecimal characters")
	}
	if bundle.SourceDirtyPatchDigest != "" && !digestPattern.MatchString(bundle.SourceDirtyPatchDigest) {
		return fmt.Errorf("source_dirty_patch_digest must be sha256")
	}
	if bundle.SourceDirtyPatchDigest != "" && bundle.SourceGitSHA == "" {
		return fmt.Errorf("source_dirty_patch_digest requires source_git_sha")
	}
	return nil
}

func validateExecution(execution Execution, intent string) error {
	if execution.Concurrency < 1 || execution.Concurrency > 64 {
		return fmt.Errorf("execution.concurrency must be between 1 and 64")
	}
	if execution.ProviderAuth != ProviderAuthOpenAIOAuthCleanProfileV1 {
		return fmt.Errorf("execution.provider_auth must equal %q", ProviderAuthOpenAIOAuthCleanProfileV1)
	}
	if execution.BillingMode != BillingModeChatGPTSubscription {
		return fmt.Errorf("execution.billing_mode must equal %q", BillingModeChatGPTSubscription)
	}
	if execution.CredentialBoundary != CredentialBoundaryRuntimeReadable && execution.CredentialBoundary != CredentialBoundaryProviderProxy {
		return fmt.Errorf("execution.credential_boundary must equal %q or %q", CredentialBoundaryRuntimeReadable, CredentialBoundaryProviderProxy)
	}
	if execution.CredentialBoundary == CredentialBoundaryRuntimeReadable && execution.Mode != "trusted-local" {
		return fmt.Errorf("runtime-readable OAuth requires execution.mode=trusted-local")
	}
	if execution.CredentialBoundary == CredentialBoundaryProviderProxy && (execution.Mode != "isolated-container" || execution.Network != "provider-proxy-only") {
		return fmt.Errorf("provider-proxy credential boundary requires isolated-container/provider-proxy-only execution")
	}
	if intent == IntentRelease && execution.CredentialBoundary != CredentialBoundaryProviderProxy {
		return fmt.Errorf("release intent requires execution.credential_boundary=%q", CredentialBoundaryProviderProxy)
	}
	if strings.TrimSpace(execution.OpenCodeVersion) == "" || len(execution.OpenCodeVersion) > 128 {
		return fmt.Errorf("execution.opencode_version is required")
	}
	for name, value := range map[string]string{
		"evaluator_binary_digest": execution.EvaluatorBinaryDigest,
		"opencode_binary_digest":  execution.OpenCodeBinaryDigest,
		"opencode_openapi_digest": execution.OpenCodeOpenAPIDigest,
		"toolchains_digest":       execution.ToolchainsDigest,
	} {
		if !digestPattern.MatchString(value) {
			return fmt.Errorf("execution.%s must be sha256", name)
		}
	}
	if execution.ContainerImageDigest != "" && !digestPattern.MatchString(execution.ContainerImageDigest) {
		return fmt.Errorf("execution.container_image_digest must be sha256")
	}
	switch execution.Mode {
	case "trusted-local":
		if execution.Network != "host-unisolated" {
			return fmt.Errorf("trusted-local must report execution.network=host-unisolated")
		}
		if execution.ContainerImageDigest != "" {
			return fmt.Errorf("trusted-local cannot declare a container image")
		}
	case "isolated-container":
		if execution.Network != "none" && execution.Network != "provider-proxy-only" && execution.Network != "registry-allowlist" {
			return fmt.Errorf("isolated-container has unsupported network policy %q", execution.Network)
		}
		if execution.ContainerImageDigest == "" {
			return fmt.Errorf("isolated-container requires execution.container_image_digest")
		}
	default:
		return fmt.Errorf("execution.mode is unsupported")
	}
	return nil
}

func validateGates(gates Gates, runs int) error {
	if gates.CriticalCasePassRate != 1 {
		return fmt.Errorf("gates.critical_case_pass_rate must equal 1")
	}
	if gates.PassToFailRegressions != 0 || gates.ScopeViolations != 0 || gates.FalseSuccesses != 0 {
		return fmt.Errorf("pass-to-fail, scope-violation, and false-success gate allowances must equal zero")
	}
	for name, ratio := range map[string]float64{
		"max_parent_peak_input_ratio": gates.MaxParentPeakInputRatio,
		"max_tree_input_ratio":        gates.MaxTreeInputRatio,
		"max_cost_ratio":              gates.MaxCostRatio,
		"max_wall_time_ratio":         gates.MaxWallTimeRatio,
		"max_retry_rate_ratio":        gates.MaxRetryRateRatio,
	} {
		if ratio <= 0 || ratio > 1000 {
			return fmt.Errorf("gates.%s must be greater than 0 and at most 1000", name)
		}
	}
	confidence := gates.Confidence
	if confidence == 0 {
		confidence = 0.95
	}
	if confidence <= 0.5 || confidence >= 1 {
		return fmt.Errorf("gates.confidence must be greater than 0.5 and less than 1")
	}
	minimumPairs := gates.MinimumPairs
	if minimumPairs == 0 {
		minimumPairs = 5
	}
	if minimumPairs < 2 || minimumPairs > runs {
		return fmt.Errorf("gates.minimum_pairs must be between 2 and runs")
	}
	return nil
}

func parseSeed(seed string) (uint64, error) {
	if !seedPattern.MatchString(seed) {
		return 0, fmt.Errorf("randomization.seed must contain 1 to 20 decimal digits")
	}
	value, err := strconv.ParseUint(seed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("randomization.seed is outside uint64: %w", err)
	}
	return value, nil
}

var (
	manifestIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	gitSHAPattern     = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
	seedPattern       = regexp.MustCompile(`^[0-9]{1,20}$`)
)
