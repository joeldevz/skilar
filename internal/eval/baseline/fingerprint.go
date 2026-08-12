// Package baseline owns immutable baseline artifacts and compatibility checks.
// It intentionally does not depend on runner so both legacy and contract-based
// execution paths can use the same trust boundary.
package baseline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Field is a stable compatibility-fingerprint key and the only vocabulary
// accepted in an experiment's intentional differences.
type Field string

const (
	FieldPromptDigest             Field = "prompt_digest"
	FieldAgentBundleDigest        Field = "agent_bundle_digest"
	FieldHarnessBundleDigest      Field = "harness_bundle_digest"
	FieldEvaluatorBinaryDigest    Field = "evaluator_binary_digest"
	FieldExperimentManifestDigest Field = "experiment_manifest_digest"
	FieldCaseSchemaVersion        Field = "case_schema_version"
	FieldCaseDigest               Field = "case_digest"
	FieldFixtureDigest            Field = "fixture_digest"
	FieldSetupPolicyDigest        Field = "setup_policy_digest"
	FieldOpenCodeVersion          Field = "opencode_version"
	FieldOpenCodeBinaryDigest     Field = "opencode_binary_digest"
	FieldOpenCodeOpenAPIDigest    Field = "opencode_openapi_digest"
	FieldEffectiveConfigDigest    Field = "effective_config_digest"
	FieldEffectiveAgentsDigest    Field = "effective_agents_digest"
	FieldModel                    Field = "model"
	FieldProvider                 Field = "provider"
	FieldToolsetDigest            Field = "toolset_digest"
	FieldPermissionPolicyDigest   Field = "permission_policy_digest"
	FieldExecutionMode            Field = "execution_mode"
	FieldNetworkPolicy            Field = "network_policy"
	FieldProviderAuthMode         Field = "provider_auth_mode"
	FieldBillingMode              Field = "billing_mode"
	FieldCredentialBoundary       Field = "credential_boundary"
	FieldAuthIsolation            Field = "auth_isolation"
	FieldProviderCatalogDigest    Field = "provider_catalog_digest"
	FieldJudgesDigest             Field = "judges_digest"
	FieldJudgeModel               Field = "judge_model"
	FieldLLMJudgeUsed             Field = "llm_judge_used"
	FieldPricingTableDigest       Field = "pricing_table_digest"
	FieldCalculatedCostUsed       Field = "calculated_cost_used"
	FieldHostOS                   Field = "host_os"
	FieldHostArch                 Field = "host_arch"
	FieldToolchainsDigest         Field = "toolchains_digest"
)

var knownFields = []Field{
	FieldPromptDigest,
	FieldAgentBundleDigest,
	FieldHarnessBundleDigest,
	FieldEvaluatorBinaryDigest,
	FieldExperimentManifestDigest,
	FieldCaseSchemaVersion,
	FieldCaseDigest,
	FieldFixtureDigest,
	FieldSetupPolicyDigest,
	FieldOpenCodeVersion,
	FieldOpenCodeBinaryDigest,
	FieldOpenCodeOpenAPIDigest,
	FieldEffectiveConfigDigest,
	FieldEffectiveAgentsDigest,
	FieldModel,
	FieldProvider,
	FieldToolsetDigest,
	FieldPermissionPolicyDigest,
	FieldExecutionMode,
	FieldNetworkPolicy,
	FieldProviderAuthMode,
	FieldBillingMode,
	FieldCredentialBoundary,
	FieldAuthIsolation,
	FieldProviderCatalogDigest,
	FieldJudgesDigest,
	FieldJudgeModel,
	FieldLLMJudgeUsed,
	FieldPricingTableDigest,
	FieldCalculatedCostUsed,
	FieldHostOS,
	FieldHostArch,
	FieldToolchainsDigest,
}

var experimentVariableFields = map[Field]struct{}{
	FieldPromptDigest:           {},
	FieldAgentBundleDigest:      {},
	FieldModel:                  {},
	FieldProvider:               {},
	FieldToolsetDigest:          {},
	FieldPermissionPolicyDigest: {},
}

// Fingerprint captures inputs that can materially change evaluation behavior.
// JudgeModel and PricingTableDigest are conditionally required by the booleans
// which state whether those authorities were used.
type Fingerprint struct {
	PromptDigest             string `json:"prompt_digest"`
	AgentBundleDigest        string `json:"agent_bundle_digest"`
	HarnessBundleDigest      string `json:"harness_bundle_digest"`
	EvaluatorBinaryDigest    string `json:"evaluator_binary_digest"`
	ExperimentManifestDigest string `json:"experiment_manifest_digest"`
	CaseSchemaVersion        int    `json:"case_schema_version"`
	CaseDigest               string `json:"case_digest"`
	FixtureDigest            string `json:"fixture_digest"`
	SetupPolicyDigest        string `json:"setup_policy_digest"`
	OpenCodeVersion          string `json:"opencode_version"`
	OpenCodeBinaryDigest     string `json:"opencode_binary_digest,omitempty"`
	OpenCodeOpenAPIDigest    string `json:"opencode_openapi_digest,omitempty"`
	EffectiveConfigDigest    string `json:"effective_config_digest"`
	EffectiveAgentsDigest    string `json:"effective_agents_digest"`
	Model                    string `json:"model"`
	Provider                 string `json:"provider"`
	ToolsetDigest            string `json:"toolset_digest"`
	PermissionPolicyDigest   string `json:"permission_policy_digest"`
	ExecutionMode            string `json:"execution_mode"`
	NetworkPolicy            string `json:"network_policy"`
	ProviderAuthMode         string `json:"provider_auth_mode"`
	BillingMode              string `json:"billing_mode"`
	CredentialBoundary       string `json:"credential_boundary"`
	AuthIsolation            string `json:"auth_isolation"`
	ProviderCatalogDigest    string `json:"provider_catalog_digest"`
	JudgesDigest             string `json:"judges_digest"`
	LLMJudgeUsed             bool   `json:"llm_judge_used"`
	JudgeModel               string `json:"judge_model,omitempty"`
	CalculatedCostUsed       bool   `json:"calculated_cost_used"`
	PricingTableDigest       string `json:"pricing_table_digest,omitempty"`
	HostOS                   string `json:"host_os"`
	HostArch                 string `json:"host_arch"`
	ToolchainsDigest         string `json:"toolchains_digest,omitempty"`
}

// Mismatch records both sides without treating an allowed experiment variable
// as an incompatibility.
type Mismatch struct {
	Field   Field  `json:"field"`
	Control string `json:"control"`
	Current string `json:"current"`
	Allowed bool   `json:"allowed"`
}

// CompatibilityReport is fail-closed: validation errors and any unallowed
// mismatch make Compatible false.
type CompatibilityReport struct {
	Compatible             bool       `json:"compatible"`
	ControlDigest          string     `json:"control_digest,omitempty"`
	CurrentDigest          string     `json:"current_digest,omitempty"`
	IntentionalDifferences []Field    `json:"intentional_differences"`
	Mismatches             []Mismatch `json:"mismatches"`
	Errors                 []string   `json:"errors,omitempty"`
}

// CompareFingerprints permits differences only in fields explicitly declared
// by the experiment. Unknown declarations are errors rather than escape hatches.
func CompareFingerprints(control, current Fingerprint, intentional []Field) CompatibilityReport {
	report := CompatibilityReport{IntentionalDifferences: append([]Field(nil), intentional...)}
	allowed := make(map[Field]struct{}, len(intentional))
	known := make(map[Field]struct{}, len(knownFields))
	for _, field := range knownFields {
		known[field] = struct{}{}
	}
	for _, field := range intentional {
		if _, ok := known[field]; !ok {
			report.Errors = append(report.Errors, fmt.Sprintf("unknown intentional difference %q", field))
			continue
		}
		if _, ok := experimentVariableFields[field]; !ok {
			report.Errors = append(report.Errors, fmt.Sprintf("field %q may not be an intentional difference", field))
			continue
		}
		if _, duplicate := allowed[field]; duplicate {
			report.Errors = append(report.Errors, fmt.Sprintf("duplicate intentional difference %q", field))
			continue
		}
		allowed[field] = struct{}{}
	}
	if err := control.Validate(); err != nil {
		report.Errors = append(report.Errors, "control fingerprint: "+err.Error())
	}
	if err := current.Validate(); err != nil {
		report.Errors = append(report.Errors, "current fingerprint: "+err.Error())
	}
	report.ControlDigest, _ = control.Digest()
	report.CurrentDigest, _ = current.Digest()
	controlValues := control.values()
	currentValues := current.values()
	for _, field := range knownFields {
		if controlValues[field] == currentValues[field] {
			continue
		}
		_, isAllowed := allowed[field]
		if !isAllowed {
			isAllowed = derivedEffectiveMismatchAllowed(field, controlValues, currentValues, allowed)
		}
		report.Mismatches = append(report.Mismatches, Mismatch{
			Field: field, Control: controlValues[field], Current: currentValues[field], Allowed: isAllowed,
		})
		if !isAllowed {
			report.Errors = append(report.Errors, fmt.Sprintf("incompatible %s", field))
		}
	}
	sort.Slice(report.Mismatches, func(i, j int) bool { return report.Mismatches[i].Field < report.Mismatches[j].Field })
	sort.Slice(report.IntentionalDifferences, func(i, j int) bool { return report.IntentionalDifferences[i] < report.IntentionalDifferences[j] })
	report.Compatible = len(report.Errors) == 0
	return report
}

// Effective runtime documents are consequences of the explicitly declared
// experiment variables. They may differ only when at least one relevant,
// declared cause actually differs; merely declaring a variable does not hide
// otherwise unexplained effective-config drift.
func derivedEffectiveMismatchAllowed(field Field, control, current map[Field]string, allowed map[Field]struct{}) bool {
	causes := map[Field][]Field{
		FieldEffectiveConfigDigest: {
			FieldPromptDigest, FieldAgentBundleDigest, FieldModel, FieldProvider,
			FieldToolsetDigest, FieldPermissionPolicyDigest,
		},
		FieldEffectiveAgentsDigest: {
			FieldPromptDigest, FieldAgentBundleDigest, FieldModel, FieldProvider,
		},
	}
	for _, cause := range causes[field] {
		if _, declared := allowed[cause]; declared && control[cause] != current[cause] {
			return true
		}
	}
	return false
}

// Validate rejects missing fields instead of allowing two empty fingerprints to
// compare equal.
func (f Fingerprint) Validate() error {
	missing := make([]string, 0)
	if f.CaseSchemaVersion <= 0 {
		missing = append(missing, string(FieldCaseSchemaVersion))
	}
	required := map[Field]string{
		FieldPromptDigest:             f.PromptDigest,
		FieldAgentBundleDigest:        f.AgentBundleDigest,
		FieldHarnessBundleDigest:      f.HarnessBundleDigest,
		FieldEvaluatorBinaryDigest:    f.EvaluatorBinaryDigest,
		FieldExperimentManifestDigest: f.ExperimentManifestDigest,
		FieldCaseDigest:               f.CaseDigest,
		FieldFixtureDigest:            f.FixtureDigest,
		FieldSetupPolicyDigest:        f.SetupPolicyDigest,
		FieldOpenCodeVersion:          f.OpenCodeVersion,
		FieldOpenCodeBinaryDigest:     f.OpenCodeBinaryDigest,
		FieldOpenCodeOpenAPIDigest:    f.OpenCodeOpenAPIDigest,
		FieldEffectiveConfigDigest:    f.EffectiveConfigDigest,
		FieldEffectiveAgentsDigest:    f.EffectiveAgentsDigest,
		FieldModel:                    f.Model,
		FieldProvider:                 f.Provider,
		FieldToolsetDigest:            f.ToolsetDigest,
		FieldPermissionPolicyDigest:   f.PermissionPolicyDigest,
		FieldExecutionMode:            f.ExecutionMode,
		FieldNetworkPolicy:            f.NetworkPolicy,
		FieldProviderAuthMode:         f.ProviderAuthMode,
		FieldBillingMode:              f.BillingMode,
		FieldCredentialBoundary:       f.CredentialBoundary,
		FieldAuthIsolation:            f.AuthIsolation,
		FieldProviderCatalogDigest:    f.ProviderCatalogDigest,
		FieldJudgesDigest:             f.JudgesDigest,
		FieldHostOS:                   f.HostOS,
		FieldHostArch:                 f.HostArch,
		FieldToolchainsDigest:         f.ToolchainsDigest,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, string(field))
		}
	}
	if f.LLMJudgeUsed && strings.TrimSpace(f.JudgeModel) == "" {
		missing = append(missing, string(FieldJudgeModel))
	}
	if f.CalculatedCostUsed && strings.TrimSpace(f.PricingTableDigest) == "" {
		missing = append(missing, string(FieldPricingTableDigest))
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("missing required fields: %s", strings.Join(missing, ", "))
	}
	digests := map[Field]string{
		FieldPromptDigest:             f.PromptDigest,
		FieldAgentBundleDigest:        f.AgentBundleDigest,
		FieldHarnessBundleDigest:      f.HarnessBundleDigest,
		FieldEvaluatorBinaryDigest:    f.EvaluatorBinaryDigest,
		FieldExperimentManifestDigest: f.ExperimentManifestDigest,
		FieldCaseDigest:               f.CaseDigest,
		FieldFixtureDigest:            f.FixtureDigest,
		FieldSetupPolicyDigest:        f.SetupPolicyDigest,
		FieldOpenCodeBinaryDigest:     f.OpenCodeBinaryDigest,
		FieldOpenCodeOpenAPIDigest:    f.OpenCodeOpenAPIDigest,
		FieldEffectiveConfigDigest:    f.EffectiveConfigDigest,
		FieldEffectiveAgentsDigest:    f.EffectiveAgentsDigest,
		FieldToolsetDigest:            f.ToolsetDigest,
		FieldPermissionPolicyDigest:   f.PermissionPolicyDigest,
		FieldJudgesDigest:             f.JudgesDigest,
		FieldToolchainsDigest:         f.ToolchainsDigest,
		FieldProviderCatalogDigest:    f.ProviderCatalogDigest,
	}
	if f.PricingTableDigest != "" {
		digests[FieldPricingTableDigest] = f.PricingTableDigest
	}
	for field, digest := range digests {
		if !validSHA256Digest(digest) {
			return fmt.Errorf("%s is not a canonical sha256 digest", field)
		}
	}
	return nil
}

// Digest identifies the complete fingerprint, including experiment variables.
func (f Fingerprint) Digest() (string, error) {
	if err := f.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("marshal fingerprint: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (f Fingerprint) values() map[Field]string {
	return map[Field]string{
		FieldPromptDigest:             f.PromptDigest,
		FieldAgentBundleDigest:        f.AgentBundleDigest,
		FieldHarnessBundleDigest:      f.HarnessBundleDigest,
		FieldEvaluatorBinaryDigest:    f.EvaluatorBinaryDigest,
		FieldExperimentManifestDigest: f.ExperimentManifestDigest,
		FieldCaseSchemaVersion:        strconv.Itoa(f.CaseSchemaVersion),
		FieldCaseDigest:               f.CaseDigest,
		FieldFixtureDigest:            f.FixtureDigest,
		FieldSetupPolicyDigest:        f.SetupPolicyDigest,
		FieldOpenCodeVersion:          f.OpenCodeVersion,
		FieldOpenCodeBinaryDigest:     f.OpenCodeBinaryDigest,
		FieldOpenCodeOpenAPIDigest:    f.OpenCodeOpenAPIDigest,
		FieldEffectiveConfigDigest:    f.EffectiveConfigDigest,
		FieldEffectiveAgentsDigest:    f.EffectiveAgentsDigest,
		FieldModel:                    f.Model,
		FieldProvider:                 f.Provider,
		FieldToolsetDigest:            f.ToolsetDigest,
		FieldPermissionPolicyDigest:   f.PermissionPolicyDigest,
		FieldExecutionMode:            f.ExecutionMode,
		FieldNetworkPolicy:            f.NetworkPolicy,
		FieldProviderAuthMode:         f.ProviderAuthMode,
		FieldBillingMode:              f.BillingMode,
		FieldCredentialBoundary:       f.CredentialBoundary,
		FieldAuthIsolation:            f.AuthIsolation,
		FieldProviderCatalogDigest:    f.ProviderCatalogDigest,
		FieldJudgesDigest:             f.JudgesDigest,
		FieldJudgeModel:               f.JudgeModel,
		FieldLLMJudgeUsed:             strconv.FormatBool(f.LLMJudgeUsed),
		FieldPricingTableDigest:       f.PricingTableDigest,
		FieldCalculatedCostUsed:       strconv.FormatBool(f.CalculatedCostUsed),
		FieldHostOS:                   f.HostOS,
		FieldHostArch:                 f.HostArch,
		FieldToolchainsDigest:         f.ToolchainsDigest,
	}
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	hexValue := strings.TrimPrefix(value, "sha256:")
	if strings.ToLower(hexValue) != hexValue {
		return false
	}
	decoded, err := hex.DecodeString(hexValue)
	return err == nil && len(decoded) == sha256.Size
}
