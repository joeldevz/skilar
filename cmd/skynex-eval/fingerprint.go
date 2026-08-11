package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/runner"
)

const (
	provenanceExtensionAgentBundleDigest     = "x-agent-bundle-digest"
	provenanceExtensionHarnessBundleDigest   = "x-harness-bundle-digest"
	provenanceExtensionManifestDigest        = "x-experiment-manifest-digest"
	provenanceExtensionEffectiveConfigDigest = runner.ProvenanceExtensionEffectiveConfigDigest
	provenanceExtensionEffectiveAgentsDigest = runner.ProvenanceExtensionEffectiveAgentsDigest
	provenanceExtensionToolchainsDigest      = runner.ProvenanceExtensionEffectiveToolchainsDigest
)

func fingerprintForRun(run modelRunResult, testCases []contracts.Case) (baseline.Fingerprint, error) {
	if len(run.Result.Samples) == 0 {
		return baseline.Fingerprint{}, fmt.Errorf("cannot fingerprint an empty result")
	}
	seenRunIDs := make(map[string]struct{}, len(run.Result.Samples))
	seenBlocks := make(map[string]struct{}, len(run.Result.Samples))
	variant := run.Result.Samples[0].Variant
	for i := range run.Result.Samples {
		sample := run.Result.Samples[i]
		if err := sample.Validate(); err != nil {
			return baseline.Fingerprint{}, fmt.Errorf("sample %d: %w", i, err)
		}
		if _, duplicate := seenRunIDs[sample.RunID]; duplicate {
			return baseline.Fingerprint{}, fmt.Errorf("duplicate run id %q", sample.RunID)
		}
		seenRunIDs[sample.RunID] = struct{}{}
		block := fmt.Sprintf("%s\x00%d", sample.CaseID, sample.Repetition)
		if _, duplicate := seenBlocks[block]; duplicate {
			return baseline.Fingerprint{}, fmt.Errorf("duplicate case/repetition block for %s repetition %d", sample.CaseID, sample.Repetition)
		}
		seenBlocks[block] = struct{}{}
		if sample.Variant != variant {
			return baseline.Fingerprint{}, fmt.Errorf("artifact mixes variants %q and %q", variant, sample.Variant)
		}
	}
	effectiveCases := testCases
	if len(run.EffectiveCases) != 0 {
		effectiveCases = run.EffectiveCases
	}
	caseDigest := modelNeutralCaseDigest(effectiveCases)
	caseByID := make(map[string]contracts.Case, len(effectiveCases))
	for _, testCase := range effectiveCases {
		if _, duplicate := caseByID[testCase.ID]; duplicate {
			return baseline.Fingerprint{}, fmt.Errorf("effective case set contains duplicate id %q", testCase.ID)
		}
		caseByID[testCase.ID] = testCase
	}
	seenCases := make(map[string]bool, len(effectiveCases))
	for _, sample := range run.Result.Samples {
		testCase, exists := caseByID[sample.CaseID]
		if !exists {
			return baseline.Fingerprint{}, fmt.Errorf("sample %s references unselected case %q", sample.RunID, sample.CaseID)
		}
		expectedCaseDigest, err := testCase.Digest()
		if err != nil {
			return baseline.Fingerprint{}, fmt.Errorf("case %s: %w", testCase.ID, err)
		}
		if sample.Provenance.CaseDigest != expectedCaseDigest || sample.Provenance.FixtureDigest != testCase.Fixture.ExpectedDigest {
			return baseline.Fingerprint{}, fmt.Errorf("sample %s case or fixture digest does not match effective case %s", sample.RunID, testCase.ID)
		}
		expectedProvider, _, _ := contracts.ParseModelSelection(testCase.Agent.Model)
		if sample.Provenance.Model != testCase.Agent.Model || sample.Provenance.Provider != expectedProvider {
			return baseline.Fingerprint{}, fmt.Errorf("sample %s model/provider does not match effective case %s", sample.RunID, testCase.ID)
		}
		seenCases[testCase.ID] = true
	}
	for _, testCase := range effectiveCases {
		if !seenCases[testCase.ID] {
			return baseline.Fingerprint{}, fmt.Errorf("selected case %s has no retained sample", testCase.ID)
		}
	}
	fixtureValues := make([]map[string]string, 0, len(effectiveCases))
	setupValues := make([]any, 0, len(effectiveCases))
	permissionValues := make([]any, 0, len(effectiveCases))
	for _, testCase := range effectiveCases {
		fixtureValues = append(fixtureValues, map[string]string{"id": testCase.ID, "digest": testCase.Fixture.ExpectedDigest})
		setupValues = append(setupValues, testCase.Setup)
		permissionValues = append(permissionValues, testCase.Security)
	}
	sort.Slice(fixtureValues, func(i, j int) bool { return fixtureValues[i]["id"] < fixtureValues[j]["id"] })
	fixtureDigest, _ := contracts.CanonicalDigest(fixtureValues)
	setupDigest, _ := contracts.CanonicalDigest(setupValues)
	permissionDigest, _ := contracts.CanonicalDigest(permissionValues)
	effectiveConfigDigest := uniformDigest(run.Result.Samples, func(sample contracts.RunResult) string {
		return sample.Provenance.Extensions[provenanceExtensionEffectiveConfigDigest]
	})
	effectiveAgentsDigest := uniformDigest(run.Result.Samples, func(sample contracts.RunResult) string {
		return sample.Provenance.Extensions[provenanceExtensionEffectiveAgentsDigest]
	})
	toolchainsDigest := uniformDigest(run.Result.Samples, func(sample contracts.RunResult) string {
		return sample.Provenance.Extensions[provenanceExtensionToolchainsDigest]
	})
	providerCatalogDigest := uniformDigest(run.Result.Samples, func(sample contracts.RunResult) string {
		return sample.Provenance.Extensions[contracts.ProvenanceExtensionProviderCatalogDigest]
	})
	if providerCatalogDigest == "" {
		providerCatalogDigest, _ = contracts.CanonicalDigest(map[string]string{"status": "unobserved"})
	}

	fingerprint := baseline.Fingerprint{
		PromptDigest: run.BundleDigest, AgentBundleDigest: run.BundleDigest,
		HarnessBundleDigest: run.HarnessDigest, EvaluatorBinaryDigest: run.EvaluatorBinaryDigest,
		ExperimentManifestDigest: run.ManifestDigest, CaseSchemaVersion: contracts.CaseSchemaVersion,
		CaseDigest: caseDigest, FixtureDigest: fixtureDigest, SetupPolicyDigest: setupDigest,
		OpenCodeVersion:        uniformString(run.Result.Samples, func(sample contracts.RunResult) string { return sample.Provenance.OpenCodeVersion }),
		OpenCodeBinaryDigest:   run.OpenCodeBinaryDigest,
		OpenCodeOpenAPIDigest:  uniformDigest(run.Result.Samples, func(sample contracts.RunResult) string { return sample.Provenance.OpenCodeAPIDigest }),
		EffectiveConfigDigest:  effectiveConfigDigest,
		EffectiveAgentsDigest:  effectiveAgentsDigest,
		Model:                  uniformString(run.Result.Samples, func(sample contracts.RunResult) string { return sample.Provenance.Model }),
		Provider:               uniformString(run.Result.Samples, func(sample contracts.RunResult) string { return sample.Provenance.Provider }),
		ToolsetDigest:          uniformDigest(run.Result.Samples, func(sample contracts.RunResult) string { return sample.Provenance.ToolsetDigest }),
		PermissionPolicyDigest: permissionDigest,
		ExecutionMode:          uniformString(run.Result.Samples, func(sample contracts.RunResult) string { return string(sample.Provenance.ExecutionMode) }),
		NetworkPolicy:          uniformString(run.Result.Samples, func(sample contracts.RunResult) string { return string(sample.Provenance.Network) }),
		ProviderAuthMode:       uniformExtension(run.Result.Samples, contracts.ProvenanceExtensionProviderAuthMode, "provider-environment"),
		BillingMode:            uniformExtension(run.Result.Samples, contracts.ProvenanceExtensionBillingMode, "api-usage"),
		CredentialBoundary:     uniformExtension(run.Result.Samples, contracts.ProvenanceExtensionCredentialBoundary, "environment"),
		AuthIsolation:          uniformExtension(run.Result.Samples, contracts.ProvenanceExtensionAuthIsolation, "none"),
		ProviderCatalogDigest:  providerCatalogDigest,
		JudgesDigest:           uniformDigest(run.Result.Samples, func(sample contracts.RunResult) string { return sample.Provenance.JudgeDigest }),
		LLMJudgeUsed:           false, CalculatedCostUsed: calculatedCostUsed(run.Result.Samples),
		PricingTableDigest: uniformDigest(run.Result.Samples, func(sample contracts.RunResult) string { return sample.Provenance.PricingTableDigest }),
		HostOS:             uniformString(run.Result.Samples, func(sample contracts.RunResult) string { return sample.Provenance.Host.OS }),
		HostArch:           uniformString(run.Result.Samples, func(sample contracts.RunResult) string { return sample.Provenance.Host.Arch }),
		ToolchainsDigest:   toolchainsDigest,
	}
	if err := validateFingerprintSampleBinding(fingerprint, run.Result.Samples, "generated fingerprint"); err != nil {
		return baseline.Fingerprint{}, err
	}
	if err := fingerprint.Validate(); err != nil {
		return baseline.Fingerprint{}, err
	}
	return fingerprint, nil
}

// validateArtifactFingerprintBinding prevents a cryptographically sealed but
// semantically self-inconsistent artifact from reaching compatibility checks
// or acceptance gates. Only the artifact's original public samples are passed
// here; sanitized holdout evidence intentionally has redacted provenance.
func validateArtifactFingerprintBinding(artifact *baseline.Artifact, samples []contracts.RunResult) error {
	if artifact == nil {
		return fmt.Errorf("artifact is required")
	}
	return validateFingerprintSampleBinding(artifact.Fingerprint, samples, fmt.Sprintf("artifact %q fingerprint", artifact.Label))
}

func validateFingerprintSampleBinding(fingerprint baseline.Fingerprint, samples []contracts.RunResult, subject string) error {
	if len(samples) == 0 {
		return fmt.Errorf("%s cannot be bound to an empty public sample population", subject)
	}
	if fingerprint.LLMJudgeUsed || fingerprint.JudgeModel != "" {
		return fmt.Errorf("%s cannot claim qualitative-judge evidence in the deterministic comparison path", subject)
	}
	for _, sample := range samples {
		if sample.Provenance.PromptDigest != sample.Provenance.Extensions[provenanceExtensionAgentBundleDigest] {
			return fmt.Errorf("%s public sample %q prompt digest is not bound to its frozen agent bundle", subject, sample.RunID)
		}
		if sample.Provenance.ConfigDigest != sample.Provenance.Extensions[provenanceExtensionEffectiveConfigDigest] {
			return fmt.Errorf("%s public sample %q config digest is not bound to its probed effective config", subject, sample.RunID)
		}
		for _, evidence := range sample.Evidence.Items {
			if evidence.Source == contracts.EvidenceLLMJudge {
				return fmt.Errorf("%s public sample %q contains qualitative-judge evidence", subject, sample.RunID)
			}
		}
	}

	type boundField struct {
		name        baseline.Field
		fingerprint string
		extract     func(contracts.RunResult) string
	}
	fields := []boundField{
		{name: baseline.FieldPromptDigest, fingerprint: fingerprint.PromptDigest, extract: func(sample contracts.RunResult) string { return sample.Provenance.PromptDigest }},
		{name: baseline.FieldOpenCodeVersion, fingerprint: fingerprint.OpenCodeVersion, extract: func(sample contracts.RunResult) string { return sample.Provenance.OpenCodeVersion }},
		{name: baseline.FieldOpenCodeOpenAPIDigest, fingerprint: fingerprint.OpenCodeOpenAPIDigest, extract: func(sample contracts.RunResult) string { return sample.Provenance.OpenCodeAPIDigest }},
		{name: baseline.FieldExecutionMode, fingerprint: fingerprint.ExecutionMode, extract: func(sample contracts.RunResult) string { return string(sample.Provenance.ExecutionMode) }},
		{name: baseline.FieldNetworkPolicy, fingerprint: fingerprint.NetworkPolicy, extract: func(sample contracts.RunResult) string { return string(sample.Provenance.Network) }},
		{name: baseline.FieldProviderAuthMode, fingerprint: fingerprint.ProviderAuthMode, extract: extensionWithFallback(contracts.ProvenanceExtensionProviderAuthMode, "provider-environment")},
		{name: baseline.FieldBillingMode, fingerprint: fingerprint.BillingMode, extract: extensionWithFallback(contracts.ProvenanceExtensionBillingMode, "api-usage")},
		{name: baseline.FieldCredentialBoundary, fingerprint: fingerprint.CredentialBoundary, extract: extensionWithFallback(contracts.ProvenanceExtensionCredentialBoundary, "environment")},
		{name: baseline.FieldAuthIsolation, fingerprint: fingerprint.AuthIsolation, extract: extensionWithFallback(contracts.ProvenanceExtensionAuthIsolation, "none")},
		{name: baseline.FieldHostOS, fingerprint: fingerprint.HostOS, extract: func(sample contracts.RunResult) string { return sample.Provenance.Host.OS }},
		{name: baseline.FieldHostArch, fingerprint: fingerprint.HostArch, extract: func(sample contracts.RunResult) string { return sample.Provenance.Host.Arch }},
	}
	for _, field := range fields {
		observed, err := exactUniformSampleValue(samples, field.extract)
		if err != nil {
			return fmt.Errorf("%s public samples have non-uniform %s: %w", subject, field.name, err)
		}
		if observed != field.fingerprint {
			return fmt.Errorf("%s %s does not match its uniform public samples", subject, field.name)
		}
	}
	observedToolset := uniformDigest(samples, func(sample contracts.RunResult) string { return sample.Provenance.ToolsetDigest })
	if observedToolset == "" || observedToolset != fingerprint.ToolsetDigest {
		return fmt.Errorf("%s %s does not match the canonical set of public-sample tool policies", subject, baseline.FieldToolsetDigest)
	}
	for _, field := range []struct {
		name        baseline.Field
		fingerprint string
		extract     func(contracts.RunResult) string
	}{
		{name: baseline.FieldModel, fingerprint: fingerprint.Model, extract: func(sample contracts.RunResult) string { return sample.Provenance.Model }},
		{name: baseline.FieldProvider, fingerprint: fingerprint.Provider, extract: func(sample contracts.RunResult) string { return sample.Provenance.Provider }},
	} {
		if observed := uniformString(samples, field.extract); observed != field.fingerprint {
			return fmt.Errorf("%s %s does not match the canonical set of public samples", subject, field.name)
		}
	}
	for _, field := range []struct {
		name        baseline.Field
		fingerprint string
		extract     func(contracts.RunResult) string
	}{
		{name: baseline.FieldJudgesDigest, fingerprint: fingerprint.JudgesDigest, extract: func(sample contracts.RunResult) string { return sample.Provenance.JudgeDigest }},
		{name: baseline.FieldPricingTableDigest, fingerprint: fingerprint.PricingTableDigest, extract: func(sample contracts.RunResult) string { return sample.Provenance.PricingTableDigest }},
	} {
		if observed := uniformDigest(samples, field.extract); observed == "" || observed != field.fingerprint {
			return fmt.Errorf("%s %s does not match the canonical set of public samples", subject, field.name)
		}
	}
	if calculatedCostUsed(samples) != fingerprint.CalculatedCostUsed {
		return fmt.Errorf("%s calculated_cost_used does not match its public samples", subject)
	}

	if err := validateProviderCatalogBinding(subject, fingerprint.ProviderCatalogDigest, samples); err != nil {
		return err
	}
	for _, effective := range []struct {
		name        baseline.Field
		fingerprint string
		extract     func(contracts.RunResult) string
	}{
		{name: baseline.FieldEffectiveConfigDigest, fingerprint: fingerprint.EffectiveConfigDigest, extract: func(sample contracts.RunResult) string {
			return sample.Provenance.Extensions[provenanceExtensionEffectiveConfigDigest]
		}},
		{name: baseline.FieldEffectiveAgentsDigest, fingerprint: fingerprint.EffectiveAgentsDigest, extract: func(sample contracts.RunResult) string {
			return sample.Provenance.Extensions[provenanceExtensionEffectiveAgentsDigest]
		}},
		{name: baseline.FieldToolchainsDigest, fingerprint: fingerprint.ToolchainsDigest, extract: func(sample contracts.RunResult) string {
			return sample.Provenance.Extensions[provenanceExtensionToolchainsDigest]
		}},
	} {
		if err := validateCanonicalDigestSetBinding(subject, effective.name, effective.fingerprint, samples, effective.extract); err != nil {
			return err
		}
	}
	for _, optional := range []struct {
		name        baseline.Field
		key         string
		fingerprint string
	}{
		{name: baseline.FieldAgentBundleDigest, key: provenanceExtensionAgentBundleDigest, fingerprint: fingerprint.AgentBundleDigest},
		{name: baseline.FieldHarnessBundleDigest, key: provenanceExtensionHarnessBundleDigest, fingerprint: fingerprint.HarnessBundleDigest},
		{name: baseline.FieldExperimentManifestDigest, key: provenanceExtensionManifestDigest, fingerprint: fingerprint.ExperimentManifestDigest},
	} {
		if err := validateOptionalExtensionBinding(subject, optional.name, optional.key, optional.fingerprint, samples); err != nil {
			return err
		}
	}
	return nil
}

func validateCanonicalDigestSetBinding(subject string, name baseline.Field, fingerprintValue string, samples []contracts.RunResult, extract func(contracts.RunResult) string) error {
	for _, sample := range samples {
		if value := extract(sample); !canonicalSHA256(value) {
			return fmt.Errorf("%s public sample %q has invalid or missing %s provenance", subject, sample.RunID, name)
		}
	}
	if observed := uniformDigest(samples, extract); observed != fingerprintValue {
		return fmt.Errorf("%s %s does not match the canonical set of public samples", subject, name)
	}
	return nil
}

func canonicalSHA256(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range strings.TrimPrefix(value, "sha256:") {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func extensionWithFallback(key, fallback string) func(contracts.RunResult) string {
	return func(sample contracts.RunResult) string {
		if value := sample.Provenance.Extensions[key]; value != "" {
			return value
		}
		return fallback
	}
}

func exactUniformSampleValue(samples []contracts.RunResult, extract func(contracts.RunResult) string) (string, error) {
	value := extract(samples[0])
	for index := 1; index < len(samples); index++ {
		if extract(samples[index]) != value {
			return "", fmt.Errorf("mixed values")
		}
	}
	return value, nil
}

func validateProviderCatalogBinding(subject, fingerprintValue string, samples []contracts.RunResult) error {
	present := 0
	for _, sample := range samples {
		if _, exists := sample.Provenance.Extensions[contracts.ProvenanceExtensionProviderCatalogDigest]; exists {
			present++
		}
	}
	if fingerprintValue != "" && uniformExtension(samples, contracts.ProvenanceExtensionProviderAuthMode, "provider-environment") == contracts.ProviderAuthModeOpenAIOAuthCleanProfileV1 && present != len(samples) {
		return fmt.Errorf("%s OAuth public samples must all include %s", subject, baseline.FieldProviderCatalogDigest)
	}
	if present != 0 && present != len(samples) {
		return fmt.Errorf("%s public samples have non-uniform %s presence", subject, baseline.FieldProviderCatalogDigest)
	}
	observed := ""
	if present == 0 {
		observed, _ = contracts.CanonicalDigest(map[string]string{"status": "unobserved"})
	} else {
		var err error
		observed, err = exactUniformSampleValue(samples, func(sample contracts.RunResult) string {
			return sample.Provenance.Extensions[contracts.ProvenanceExtensionProviderCatalogDigest]
		})
		if err != nil {
			return fmt.Errorf("%s public samples have non-uniform %s: %w", subject, baseline.FieldProviderCatalogDigest, err)
		}
	}
	if observed != fingerprintValue {
		return fmt.Errorf("%s %s does not match its uniform public samples", subject, baseline.FieldProviderCatalogDigest)
	}
	return nil
}

func validateOptionalExtensionBinding(subject string, name baseline.Field, key, fingerprintValue string, samples []contracts.RunResult) error {
	present := 0
	for _, sample := range samples {
		if _, exists := sample.Provenance.Extensions[key]; exists {
			present++
		}
	}
	if present == 0 {
		return fmt.Errorf("%s public samples lack required %s provenance", subject, name)
	}
	if present != len(samples) {
		return fmt.Errorf("%s public samples have non-uniform %s extension presence", subject, name)
	}
	observed, err := exactUniformSampleValue(samples, func(sample contracts.RunResult) string {
		return sample.Provenance.Extensions[key]
	})
	if err != nil {
		return fmt.Errorf("%s public samples have non-uniform %s extension: %w", subject, name, err)
	}
	if observed != fingerprintValue {
		return fmt.Errorf("%s %s does not match its public-sample extension", subject, name)
	}
	return nil
}

func uniformExtension(samples []contracts.RunResult, key, fallback string) string {
	return uniformString(samples, func(sample contracts.RunResult) string {
		if value := sample.Provenance.Extensions[key]; value != "" {
			return value
		}
		return fallback
	})
}

func uniformString(samples []contracts.RunResult, extract func(contracts.RunResult) string) string {
	values := make([]string, 0, len(samples))
	seen := make(map[string]struct{})
	for _, sample := range samples {
		value := extract(sample)
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	sort.Strings(values)
	if len(values) == 1 {
		return values[0]
	}
	return "mixed:" + strings.Join(values, ",")
}

func uniformDigest(samples []contracts.RunResult, extract func(contracts.RunResult) string) string {
	values := make([]string, 0, len(samples))
	seen := make(map[string]struct{})
	for _, sample := range samples {
		value := extract(sample)
		if value == "" {
			return ""
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	sort.Strings(values)
	if len(values) == 1 {
		return values[0]
	}
	digest, _ := contracts.CanonicalDigest(values)
	return digest
}

func calculatedCostUsed(samples []contracts.RunResult) bool {
	for _, sample := range samples {
		if sample.Usage.Parent.CalculatedCostUSD != nil || sample.Usage.Tree.CalculatedCostUSD != nil {
			return true
		}
	}
	return false
}
