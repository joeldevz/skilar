package baseline

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/contracts"
)

func TestRunResultArtifactValidatesAndPreservesEveryRepetition(t *testing.T) {
	first := validRunResult("run-1", 1)
	second := validRunResult("run-2", 2)
	artifact, err := NewRunArtifact("current", "suite", time.Unix(1, 0), validFingerprint(), []contracts.RunResult{first, second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRunResults(artifact.Samples)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[0].RunID != "run-1" || decoded[1].Repetition != 2 {
		t.Fatalf("raw repetitions not preserved: %+v", decoded)
	}
}

func TestEncodeRunResultsRejectsDuplicateIdentityAndInvalidContract(t *testing.T) {
	result := validRunResult("run-1", 1)
	if _, err := EncodeRunResults([]contracts.RunResult{result, result}); err == nil {
		t.Fatal("duplicate run identity accepted")
	}
	otherID := result
	otherID.RunID = "run-2"
	if _, err := EncodeRunResults([]contracts.RunResult{result, otherID}); err == nil {
		t.Fatal("duplicate case/variant/repetition accepted")
	}
	invalid := result
	invalid.SchemaVersion = 99
	if _, err := EncodeRunResults([]contracts.RunResult{invalid}); err == nil {
		t.Fatal("invalid run contract accepted")
	}
}

func TestDecodeRunResultsRejectsNestedSchemaViolations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*contracts.RunResult)
	}{
		{name: "evidence kind empty", mutate: func(result *contracts.RunResult) { result.Evidence.Items[0].Kind = "" }},
		{name: "evidence kind oversized", mutate: func(result *contracts.RunResult) { result.Evidence.Items[0].Kind = strings.Repeat("k", 257) }},
		{name: "evidence source invalid", mutate: func(result *contracts.RunResult) { result.Evidence.Items[0].Source = "ambient" }},
		{name: "evidence digest invalid", mutate: func(result *contracts.RunResult) { result.Evidence.Items[0].Digest = "sha256:bad" }},
		{name: "evidence path oversized", mutate: func(result *contracts.RunResult) { result.Evidence.Items[0].Path = strings.Repeat("p", 4097) }},
		{name: "evidence summary oversized", mutate: func(result *contracts.RunResult) { result.Evidence.Items[0].Summary = strings.Repeat("s", 65537) }},
		{name: "check type empty", mutate: func(result *contracts.RunResult) { result.Checks[0].Type = "" }},
		{name: "check type oversized", mutate: func(result *contracts.RunResult) { result.Checks[0].Type = strings.Repeat("t", 257) }},
		{name: "check summary oversized", mutate: func(result *contracts.RunResult) { result.Checks[0].Summary = strings.Repeat("s", 65537) }},
		{name: "check evidence ids null", mutate: func(result *contracts.RunResult) { result.Checks[0].EvidenceIDs = nil }},
		{name: "check evidence ids duplicate", mutate: func(result *contracts.RunResult) {
			result.Checks[0].EvidenceIDs = []string{"evidence-1", "evidence-1"}
		}},
		{name: "check evidence id unknown", mutate: func(result *contracts.RunResult) {
			result.Checks[0].EvidenceIDs = []string{"missing"}
		}},
		{name: "check error invalid", mutate: func(result *contracts.RunResult) {
			result.Checks[0].Error = &contracts.RunError{Message: "nested failure"}
		}},
		{name: "check error duplicate evidence ids", mutate: func(result *contracts.RunResult) {
			result.Checks[0].Error = &contracts.RunError{Kind: "nested", Message: "failure", EvidenceIDs: []string{"evidence-1", "evidence-1"}}
		}},
		{name: "check error evidence id unknown", mutate: func(result *contracts.RunResult) {
			result.Checks[0].Error = &contracts.RunError{Kind: "nested", Message: "failure", EvidenceIDs: []string{"missing"}}
		}},
		{name: "result error kind empty", mutate: func(result *contracts.RunResult) {
			makeFailedRun(result, &contracts.RunError{Message: "failure"})
		}},
		{name: "result error kind oversized", mutate: func(result *contracts.RunResult) {
			makeFailedRun(result, &contracts.RunError{Kind: strings.Repeat("k", 257), Message: "failure"})
		}},
		{name: "result error message empty", mutate: func(result *contracts.RunResult) {
			makeFailedRun(result, &contracts.RunError{Kind: "failure"})
		}},
		{name: "result error message oversized", mutate: func(result *contracts.RunResult) {
			makeFailedRun(result, &contracts.RunError{Kind: "failure", Message: strings.Repeat("m", 65537)})
		}},
		{name: "result error evidence ids duplicate", mutate: func(result *contracts.RunResult) {
			makeFailedRun(result, &contracts.RunError{Kind: "failure", Message: "failed", EvidenceIDs: []string{"evidence-1", "evidence-1"}})
		}},
		{name: "result error evidence id unknown", mutate: func(result *contracts.RunResult) {
			makeFailedRun(result, &contracts.RunError{Kind: "failure", Message: "failed", EvidenceIDs: []string{"missing"}})
		}},
		{name: "checks null", mutate: func(result *contracts.RunResult) { result.Checks = nil }},
		{name: "evidence items null", mutate: func(result *contracts.RunResult) { result.Evidence.Items = nil }},
		{name: "pass without checks", mutate: func(result *contracts.RunResult) { result.Checks = []contracts.CheckResult{} }},
		{name: "pass without hard check", mutate: func(result *contracts.RunResult) { result.Checks[0].Hard = false }},
		{name: "passing hard check without evidence", mutate: func(result *contracts.RunResult) {
			result.Checks[0].EvidenceIDs = []string{}
		}},
		{name: "passing hard check with incomplete evidence", mutate: func(result *contracts.RunResult) {
			result.Evidence.Items[0].Complete = false
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validRunResult("run-1", 1)
			test.mutate(&result)
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeRunResults([]json.RawMessage{raw}); err == nil {
				t.Fatal("schema-invalid imported run result was accepted")
			}
		})
	}
}

func TestDecodeRunResultsRejectsInvalidEvidenceCompleteType(t *testing.T) {
	result := validRunResult("run-1", 1)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"complete":true`), []byte(`"complete":"true"`), 1)
	if _, err := DecodeRunResults([]json.RawMessage{raw}); err == nil {
		t.Fatal("non-boolean evidence complete field was accepted")
	}
}

func TestDecodeRunResultsRejectsMissingRequiredZeroValueFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{name: "evidence complete", mutate: func(root map[string]interface{}) {
			item := root["evidence"].(map[string]interface{})["items"].([]interface{})[0].(map[string]interface{})
			delete(item, "complete")
		}},
		{name: "check hard", mutate: func(root map[string]interface{}) {
			check := root["checks"].([]interface{})[0].(map[string]interface{})
			delete(check, "hard")
		}},
		{name: "check summary", mutate: func(root map[string]interface{}) {
			check := root["checks"].([]interface{})[0].(map[string]interface{})
			delete(check, "summary")
		}},
		{name: "telemetry complete", mutate: func(root map[string]interface{}) {
			delete(root, "telemetry_complete")
		}},
		{name: "run error retryable", mutate: func(root map[string]interface{}) {
			root["status"] = string(contracts.RunStatusFail)
			root["checks"].([]interface{})[0].(map[string]interface{})["status"] = string(contracts.CheckStatusFail)
			root["error"] = map[string]interface{}{"kind": "failure", "message": "failed"}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validRunResult("run-1", 1)
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]interface{}
			if err := json.Unmarshal(raw, &root); err != nil {
				t.Fatal(err)
			}
			test.mutate(root)
			raw, err = json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeRunResults([]json.RawMessage{raw}); err == nil {
				t.Fatal("result missing a schema-required field was accepted")
			}
		})
	}
}

func makeFailedRun(result *contracts.RunResult, runError *contracts.RunError) {
	result.Status = contracts.RunStatusFail
	result.Checks[0].Status = contracts.CheckStatusFail
	result.Error = runError
}

func validRunResult(runID string, repetition int) contracts.RunResult {
	return contracts.RunResult{
		SchemaVersion: contracts.ResultSchemaVersion,
		RunID:         runID, CaseID: "case", Variant: "control", Repetition: repetition, Status: contracts.RunStatusPass,
		Provenance: contracts.Provenance{
			GitSHA: strings.Repeat("f", 40), CaseDigest: digest("1"), PromptDigest: digest("2"), ConfigDigest: digest("3"),
			FixtureDigest: digest("4"), OpenCodeVersion: "1", Model: "provider/model", Provider: "provider",
			ToolsetDigest: digest("5"), PricingTableDigest: digest("6"),
			ExecutionMode: contracts.ExecutionTrustedLocal, Network: contracts.NetworkHostUnisolated,
			Host: contracts.HostProvenance{OS: "linux", Arch: "amd64"},
		},
		Usage: contracts.Usage{Tree: contracts.TreeUsage{Sessions: 1}},
		Checks: []contracts.CheckResult{{
			ID: "check-1", Type: "test", Status: contracts.CheckStatusPass, Hard: true,
			RequirementIDs: []string{"EVAL-TEST-001"}, Summary: "passed", EvidenceIDs: []string{"evidence-1"},
		}},
		Evidence: contracts.Evidence{Items: []contracts.EvidenceItem{{
			ID: "evidence-1", Kind: "test", Source: contracts.EvidenceEvaluator, Digest: digest("7"), Complete: true,
		}}},
	}
}
