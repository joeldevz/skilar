package gates

import "testing"

func TestHardGatesPassRegressAndInvalidate(t *testing.T) {
	passRate, zero, complete := 1.0, 0, true
	thresholds := HardThresholds{CriticalCasePassRate: 1, PassToFailMaximum: 0, ScopeViolationMax: 0, FalseSuccessMax: 0}
	results := EvaluateHardGates(thresholds, HardEvidence{
		CriticalCasePassRate: &passRate, PassToFail: &zero, ScopeViolations: &zero,
		FalseSuccesses: &zero, TelemetryComplete: &complete,
	})
	if decision := Combine(results...); decision.Status != StatusPass || decision.ExitCode != ExitPass {
		t.Fatalf("passing hard gates = %+v", decision)
	}
	one := 1
	results = EvaluateHardGates(thresholds, HardEvidence{
		CriticalCasePassRate: &passRate, PassToFail: &one, ScopeViolations: &zero,
		FalseSuccesses: &zero, TelemetryComplete: &complete,
	})
	if decision := Combine(results...); decision.Status != StatusRegression || decision.ExitCode != ExitRegression {
		t.Fatalf("hard regression = %+v", decision)
	}
	incomplete := false
	results = EvaluateHardGates(thresholds, HardEvidence{
		CriticalCasePassRate: &passRate, PassToFail: &zero, ScopeViolations: &zero,
		FalseSuccesses: &zero, TelemetryComplete: &incomplete,
	})
	if decision := Combine(results...); decision.Status != StatusInvalid || decision.ExitCode != ExitInvalid {
		t.Fatalf("incomplete hard evidence = %+v", decision)
	}
}

func TestDecisionPrecedenceAndStableExitCodes(t *testing.T) {
	decision := Combine(
		Result{Name: "noise", Status: StatusInconclusive, Reason: ReasonCICrossesThreshold},
		Result{Name: "regression", Status: StatusRegression, Reason: ReasonHardGateFailed},
		Result{Name: "invalid", Status: StatusInvalid, Reason: ReasonEvidenceInvalid},
	)
	if decision.Status != StatusInvalid || decision.ExitCode != 2 || len(decision.Reasons) != 3 {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if ExitCode(StatusInfraError) != 5 || ExitCode(Status("unknown")) != ExitInvalid {
		t.Fatal("exit-code contract changed")
	}
}

func TestNotApplicableGateDoesNotBecomeAPassOrFailureReason(t *testing.T) {
	result := Result{
		Name:   "tree_cost_usd",
		Status: StatusNotApplicable,
		Reason: ReasonNotApplicable,
		Detail: "ChatGPT subscription has no authoritative per-request USD",
	}
	decision := Combine(result)
	if decision.Status != StatusPass || decision.ExitCode != ExitPass {
		t.Fatalf("not-applicable gate changed the decision: %+v", decision)
	}
	if len(decision.Reasons) != 0 || len(decision.Results) != 1 || decision.Results[0].Status != StatusNotApplicable {
		t.Fatalf("not-applicable result was lost or presented as a decision failure: %+v", decision)
	}
	if ExitCode(StatusNotApplicable) != ExitPass {
		t.Fatalf("not-applicable exit code = %d", ExitCode(StatusNotApplicable))
	}
}

func TestQualityOnlyHardGatesDoNotRequireTelemetry(t *testing.T) {
	passRate, zero := 1.0, 0
	results := EvaluateHardGates(HardThresholds{CriticalCasePassRate: 1}, HardEvidence{
		CriticalCasePassRate: &passRate, PassToFail: &zero, ScopeViolations: &zero, FalseSuccesses: &zero,
	})
	if len(results) != 4 {
		t.Fatalf("quality-only evaluation unexpectedly added telemetry gate: %+v", results)
	}
	if decision := Combine(results...); decision.Status != StatusPass {
		t.Fatalf("quality-only evidence invalidated: %+v", decision)
	}
}
