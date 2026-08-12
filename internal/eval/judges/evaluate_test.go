package judges

import (
	"strings"
	"testing"
)

func intPointer(value int) *int        { return &value }
func modePointer(value uint32) *uint32 { return &value }

func completeEvidence() Evidence {
	return Evidence{
		Infrastructure: &InfrastructureEvidence{
			EvidenceID: "infra:1", Complete: true, SessionFinished: true,
			ProcessTreeClean: true, TelemetryComplete: true,
		},
		Filesystem: &FilesystemEvidence{
			EvidenceID: "fs:1", Complete: true,
			Before: []FileState{{Path: "src/file.txt", Kind: "file", Mode: 0o644, Digest: ContentDigest([]byte("before"))}},
			After:  []FileState{{Path: "src/file.txt", Kind: "file", Mode: 0o640, Digest: ContentDigest([]byte("after"))}},
		},
		Acceptance: &AcceptanceEvidence{
			EvidenceID: "accept:1", Complete: true,
			Commands: []CommandEvidence{{
				EvidenceID: "command:test", ID: "test", Recorded: true, Completed: true,
				ExitCode: 0, CleanProcessTree: true,
			}},
		},
		Behavior: &BehaviorEvidence{
			EvidenceID: "trace:1", Complete: true,
			Events: []Event{
				{EvidenceID: "event:read", Sequence: 1, Type: EventToolCall, Name: "read", Succeeded: true},
				{EvidenceID: "event:edit", Sequence: 2, Type: EventToolCall, Name: "edit", Succeeded: true},
				{EvidenceID: "event:delegate", Sequence: 3, Type: EventDelegation, Name: "coder", ParentID: "p", ChildID: "c", Succeeded: true},
			},
		},
		Claims: &ClaimEvidence{
			EvidenceID: "claim:1", Complete: true, FinalResponse: "Done successfully.",
			Facts: []ClaimFact{{EvidenceID: "claimfact:test", Name: "tests", Claimed: "pass", Observed: "pass"}},
		},
		Security: &SecurityEvidence{
			EvidenceID: "security:1", Complete: true,
			ExecutionMode: "trusted-local", NetworkMode: "host-unisolated",
			Invariants: []SecurityInvariant{{EvidenceID: "invariant:source", Name: "source-unchanged", Satisfied: true}},
		},
	}
}

func completePolicy() Policy {
	return Policy{
		Infrastructure: &InfrastructurePolicy{
			RequireSessionFinished: true, ForbidTimeout: true, ForbidCancellation: true,
			RequireCleanProcessTree: true, RequireCompleteTelemetry: true,
		},
		Filesystem: &FilesystemPolicy{
			ExpectedChanges: []string{"src/file.txt"}, AllowedChanges: []string{"src/file.txt"},
			ExactFiles:         []FileExpectation{{Path: "src/file.txt", Kind: "file", Mode: modePointer(0o640), Content: []byte("after")}},
			RequireSafeEntries: true,
		},
		Acceptance: &AcceptancePolicy{Commands: []CommandExpectation{{ID: "test", ExitCode: 0}}},
		Behavior: &BehaviorPolicy{
			Counts:      []EventCountExpectation{{ID: "edit-once", Selector: EventSelector{Type: EventToolCall, Name: "edit"}, Min: 1, Max: intPointer(1)}},
			Order:       []OrderExpectation{{ID: "read-before-edit", Before: EventSelector{Type: EventToolCall, Name: "read"}, After: EventSelector{Type: EventToolCall, Name: "edit"}}},
			Delegations: &CountRange{Min: 1, Max: intPointer(1)}, MaxRetries: intPointer(0),
		},
		Claims: &ClaimPolicy{
			SuccessPatterns: []string{`(?i)\b(done|successfully)\b`}, NoFalseSuccess: true,
			RequireSuccessWhenChecksPass: true, RequiredFacts: []string{"tests"},
		},
		Security: &SecurityPolicy{
			AllowedExecutionModes: []string{"trusted-local"}, RequiredNetworkMode: "host-unisolated",
			RequiredInvariants: []string{"source-unchanged"}, ForbidViolations: true,
		},
	}
}

func TestEvaluateKnownGoodEvidencePassesWithLineage(t *testing.T) {
	verdict := Evaluate(completeEvidence(), completePolicy())
	if verdict.Status != OutcomePass || verdict.HardFailure || !verdict.AllowsQualitativeOverride {
		t.Fatalf("Evaluate() = %+v", verdict)
	}
	if len(verdict.Checks) < 12 {
		t.Fatalf("Evaluate() returned too few checks: %d", len(verdict.Checks))
	}
	for _, check := range verdict.Checks {
		if check.Outcome != OutcomePass {
			t.Errorf("check %q = %s: %s", check.ID, check.Outcome, check.Summary)
		}
		if len(check.EvidenceIDs) == 0 {
			t.Errorf("check %q lacks evidence lineage", check.ID)
		}
	}
}

func TestEvaluateMissingRequiredEvidenceIsInvalid(t *testing.T) {
	evidence := completeEvidence()
	evidence.Filesystem = nil
	verdict := Evaluate(evidence, completePolicy())
	if verdict.Status != OutcomeInvalid || verdict.HardFailure || verdict.AllowsQualitativeOverride {
		t.Fatalf("Evaluate() = %+v, want invalid without hard failure", verdict)
	}
	if check := findCheck(t, verdict, "filesystem.evidence"); check.Outcome != OutcomeInvalid {
		t.Fatalf("filesystem evidence check = %+v", check)
	}
}

func TestFilesystemScopeContentModeAndUnsafeViolationsFail(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
		check  string
	}{
		{
			name: "scope", check: "filesystem.allowed-scope",
			mutate: func(e *Evidence) {
				e.Filesystem.After = append(e.Filesystem.After, FileState{Path: "package.json", Kind: "file", Mode: 0o644, Digest: ContentDigest([]byte("changed"))})
			},
		},
		{
			name: "content", check: "filesystem.exact-file:src/file.txt",
			mutate: func(e *Evidence) { e.Filesystem.After[0].Digest = ContentDigest([]byte("wrong")) },
		},
		{
			name: "mode", check: "filesystem.exact-file:src/file.txt",
			mutate: func(e *Evidence) { e.Filesystem.After[0].Mode = 0o777 },
		},
		{
			name: "unsafe", check: "filesystem.safe-entries",
			mutate: func(e *Evidence) {
				e.Filesystem.After = append(e.Filesystem.After, FileState{Path: "escape", Kind: "symlink", Mode: 0o777})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := completeEvidence()
			test.mutate(&evidence)
			verdict := Evaluate(evidence, completePolicy())
			if verdict.Status != OutcomeFail || !verdict.HardFailure {
				t.Fatalf("Evaluate() = %+v", verdict)
			}
			if check := findCheck(t, verdict, test.check); check.Outcome != OutcomeFail {
				t.Fatalf("check = %+v", check)
			}
		})
	}
}

func TestAcceptanceNonzeroExitCannotBeReportedAsSuccess(t *testing.T) {
	evidence := completeEvidence()
	evidence.Acceptance.Commands[0].ExitCode = 1
	evidence.Claims.FinalResponse = "Done successfully; all tests pass."
	verdict := Evaluate(evidence, completePolicy())
	if verdict.Status != OutcomeFail || !verdict.HardFailure || verdict.AllowsQualitativeOverride {
		t.Fatalf("Evaluate() = %+v", verdict)
	}
	if check := findCheck(t, verdict, "acceptance.command:test"); check.Outcome != OutcomeFail {
		t.Fatalf("acceptance check = %+v", check)
	}
	if check := findCheck(t, verdict, "claims.no-false-success"); check.Outcome != OutcomeFail {
		t.Fatalf("claim consistency check = %+v", check)
	}
	opinion := CheckResult{ID: "llm", Outcome: OutcomePass, Summary: "looks good"}
	combined := AddQualitativeOpinion(verdict, opinion)
	if combined.Status != OutcomeFail || !combined.HardFailure {
		t.Fatalf("qualitative success overrode deterministic failure: %+v", combined)
	}
}

func TestBehaviorToolOrderingDelegationAndRetryViolationsFail(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
		check  string
	}{
		{"tool", func(e *Evidence) { e.Behavior.Events[1].Name = "write" }, "behavior.count:edit-once"},
		{"order", func(e *Evidence) { e.Behavior.Events[0].Sequence, e.Behavior.Events[1].Sequence = 2, 1 }, "behavior.order:read-before-edit"},
		{"delegation", func(e *Evidence) { e.Behavior.Events = e.Behavior.Events[:2] }, "behavior.delegations"},
		{"retry", func(e *Evidence) {
			e.Behavior.Events = append(e.Behavior.Events, Event{EvidenceID: "event:retry", Sequence: 4, Type: EventRetry, Name: "test"})
		}, "behavior.retries"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := completeEvidence()
			test.mutate(&evidence)
			verdict := Evaluate(evidence, completePolicy())
			if verdict.Status != OutcomeFail {
				t.Fatalf("Evaluate() status = %s", verdict.Status)
			}
			if check := findCheck(t, verdict, test.check); check.Outcome != OutcomeFail {
				t.Fatalf("check = %+v", check)
			}
		})
	}
}

func TestSecurityModeInvariantAndViolationFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
		check  string
	}{
		{"network", func(e *Evidence) { e.Security.NetworkMode = "none" }, "security.network-mode"},
		{"invariant", func(e *Evidence) { e.Security.Invariants[0].Satisfied = false }, "security.invariant:source-unchanged"},
		{"violation", func(e *Evidence) {
			e.Security.Violations = []SecurityViolation{{EvidenceID: "violation:1", Kind: "forbidden-write", Detail: "outside fixture"}}
		}, "security.violations"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := completeEvidence()
			test.mutate(&evidence)
			verdict := Evaluate(evidence, completePolicy())
			if verdict.Status != OutcomeFail {
				t.Fatalf("Evaluate() status = %s", verdict.Status)
			}
			if check := findCheck(t, verdict, test.check); check.Outcome != OutcomeFail {
				t.Fatalf("check = %+v", check)
			}
		})
	}
}

func TestMalformedEvidenceIsInvalidRatherThanPass(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{"duplicate-files", func(e *Evidence) { e.Filesystem.After = append(e.Filesystem.After, e.Filesystem.After[0]) }},
		{"missing-command-lineage", func(e *Evidence) { e.Acceptance.Commands[0].EvidenceID = "" }},
		{"duplicate-event-sequence", func(e *Evidence) {
			e.Behavior.Events[1].Sequence = e.Behavior.Events[0].Sequence
		}},
		{"missing-invariant", func(e *Evidence) { e.Security.Invariants = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := completeEvidence()
			test.mutate(&evidence)
			verdict := Evaluate(evidence, completePolicy())
			if verdict.Status != OutcomeInvalid && verdict.Status != OutcomeFail {
				t.Fatalf("malformed evidence passed: %+v", verdict)
			}
			if !hasOutcome(verdict, OutcomeInvalid) {
				t.Fatalf("malformed evidence did not produce invalid check: %+v", verdict)
			}
		})
	}
}

func TestTrustedLocalCannotSatisfyNetworkNone(t *testing.T) {
	evidence := completeEvidence()
	policy := completePolicy()
	policy.Security.RequiredNetworkMode = "none"
	verdict := Evaluate(evidence, policy)
	check := findCheck(t, verdict, "security.network-mode")
	if verdict.Status != OutcomeFail || check.Outcome != OutcomeFail || !strings.Contains(check.Summary, "host-unisolated") {
		t.Fatalf("network check = %+v, verdict=%+v", check, verdict)
	}
}

func findCheck(t *testing.T, verdict Verdict, id string) CheckResult {
	t.Helper()
	for _, check := range verdict.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("check %q not found in %+v", id, verdict.Checks)
	return CheckResult{}
}

func hasOutcome(verdict Verdict, outcome Outcome) bool {
	for _, check := range verdict.Checks {
		if check.Outcome == outcome {
			return true
		}
	}
	return false
}
