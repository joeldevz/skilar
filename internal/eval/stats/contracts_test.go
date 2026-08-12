package stats

import (
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/contracts"
)

func TestRunResultAdaptersPreserveIdentityStatusAndUnknownCost(t *testing.T) {
	run := validContractRun()
	run.TelemetryComplete = false
	samples, err := SamplesFromRunResults([]contracts.RunResult{run}, MetricTreeCalculatedCost, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].ID != run.RunID || samples[0].Status != StatusPass || samples[0].Value != nil || samples[0].TelemetryComplete {
		t.Fatalf("unexpected sample conversion: %+v", samples)
	}
	outcomes, err := OutcomesFromRunResults([]contracts.RunResult{run})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].Status != StatusPass || outcomes[0].CaseID != run.CaseID {
		t.Fatalf("unexpected outcome conversion: %+v", outcomes)
	}
}

func TestRunResultAdapterCanUseEvaluatorOwnedMetricWithoutTreeTelemetry(t *testing.T) {
	run := validContractRun()
	run.TelemetryComplete = false
	run.Timing.WallMS = 123
	samples, err := SamplesFromRunResults([]contracts.RunResult{run}, MetricWallMS, false)
	if err != nil {
		t.Fatal(err)
	}
	if samples[0].Value == nil || *samples[0].Value != 123 || !samples[0].TelemetryComplete {
		t.Fatalf("wall metric incorrectly depended on tree telemetry: %+v", samples[0])
	}
}

func TestRetryMetricUsesReconciledCoordinationEvidence(t *testing.T) {
	run := validContractRun()
	run.TelemetryComplete = true
	run.Coordination.Retries = 3
	samples, err := SamplesFromRunResults([]contracts.RunResult{run}, MetricRetries, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Value == nil || *samples[0].Value != 3 {
		t.Fatalf("retry samples = %#v", samples)
	}
}

func TestParentFirstInputMetricUsesContractEvidence(t *testing.T) {
	run := validContractRun()
	run.TelemetryComplete = true
	run.Usage.Parent.FirstInputTokens = 17
	run.Usage.Parent.PeakInputTokens = 17
	samples, err := SamplesFromRunResults([]contracts.RunResult{run}, MetricParentFirstInput, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Value == nil || *samples[0].Value != 17 {
		t.Fatalf("parent first-input samples = %#v", samples)
	}
}

func TestProviderCostIsUnavailableForChatGPTSubscriptionOAuth(t *testing.T) {
	run := validContractRun()
	run.TelemetryComplete = true
	zero := 0.0
	run.Usage.Parent.ProviderCostUSD = &zero
	run.Usage.Tree.ProviderCostUSD = &zero
	run.Provenance.Extensions = map[string]string{
		contracts.ProvenanceExtensionProviderAuthMode:   contracts.ProviderAuthModeOpenAIOAuthCleanProfileV1,
		contracts.ProvenanceExtensionBillingMode:        contracts.BillingModeChatGPTSubscription,
		contracts.ProvenanceExtensionCredentialBoundary: contracts.CredentialBoundaryRuntimeReadable,
		contracts.ProvenanceExtensionAuthIsolation:      contracts.AuthIsolationDedicatedFreshTokenFailStopV1,
	}

	for name, metric := range map[string]RunMetric{
		"parent": MetricParentProviderCost,
		"tree":   MetricTreeProviderCost,
	} {
		t.Run(name, func(t *testing.T) {
			value, available := metric(run)
			if available || value != 0 {
				t.Fatalf("subscription provider USD must not be authoritative: value=%v available=%v", value, available)
			}
		})
	}
	if _, err := SamplesFromRunResults([]contracts.RunResult{run}, MetricTreeProviderCost, true); err == nil {
		t.Fatal("contract adapter accepted persisted subscription provider USD")
	}
}

func TestCalculatedCostRemainsExplicitCounterfactualForSubscriptionOAuth(t *testing.T) {
	run := validContractRun()
	run.TelemetryComplete = true
	calculated := 1.25
	run.Usage.Tree.CalculatedCostUSD = &calculated
	run.Provenance.Extensions = map[string]string{
		contracts.ProvenanceExtensionProviderAuthMode:   contracts.ProviderAuthModeOpenAIOAuthCleanProfileV1,
		contracts.ProvenanceExtensionBillingMode:        contracts.BillingModeChatGPTSubscription,
		contracts.ProvenanceExtensionCredentialBoundary: contracts.CredentialBoundaryRuntimeReadable,
		contracts.ProvenanceExtensionAuthIsolation:      contracts.AuthIsolationDedicatedFreshTokenFailStopV1,
	}
	samples, err := SamplesFromRunResults([]contracts.RunResult{run}, MetricTreeCalculatedCost, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Value == nil || *samples[0].Value != calculated {
		t.Fatalf("explicit calculated-cost counterfactual was lost: %+v", samples)
	}
}

func validContractRun() contracts.RunResult {
	digest := "sha256:" + strings.Repeat("a", 64)
	return contracts.RunResult{
		SchemaVersion: contracts.ResultSchemaVersion,
		RunID:         "run-1", CaseID: "case", Variant: "candidate", Repetition: 1, Status: contracts.RunStatusPass,
		Provenance: contracts.Provenance{
			GitSHA: strings.Repeat("f", 40), CaseDigest: digest, PromptDigest: digest, ConfigDigest: digest, FixtureDigest: digest,
			OpenCodeVersion: "1", Model: "openai/model", Provider: "openai", ToolsetDigest: digest, PricingTableDigest: digest,
			ExecutionMode: contracts.ExecutionTrustedLocal, Network: contracts.NetworkHostUnisolated,
			Host: contracts.HostProvenance{OS: "linux", Arch: "amd64"},
		},
		Usage: contracts.Usage{Tree: contracts.TreeUsage{Sessions: 1}},
		Checks: []contracts.CheckResult{{
			ID: "check-1", Type: "test", Status: contracts.CheckStatusPass, Hard: true,
			RequirementIDs: []string{"EVAL-TEST-001"}, Summary: "passed", EvidenceIDs: []string{"evidence-1"},
		}},
		Evidence: contracts.Evidence{Items: []contracts.EvidenceItem{{
			ID: "evidence-1", Kind: "test", Source: contracts.EvidenceEvaluator, Digest: digest, Complete: true,
		}}},
	}
}
