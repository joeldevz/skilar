package workflow

import (
	"errors"
	"testing"
)

func newTestStore(t *testing.T) *MemoryStore {
	t.Helper()
	s := NewMemoryStore()
	if _, err := s.Create(Workflow{ID: "wf-1", Route: RouteSimple, MinimumRisk: RiskLow, BasisTree: "tree-1"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHappyPath(t *testing.T) {
	s := newTestStore(t)
	states := []State{StateDiscovering, StateReady, StateExecuting, StateVerifying, StateCandidateFrozen, StateReviewing, StateReceipted, StateDelivered}
	current := StateCreated
	for version, next := range states {
		got, err := s.Transition(Transition{WorkflowID: "wf-1", ExpectedState: current, ExpectedVersion: uint64(version), NextState: next, IdempotencyKey: string(next), ArtifactIDs: []string{"artifact-1"}})
		if err != nil {
			t.Fatalf("%s -> %s: %v", current, next, err)
		}
		if got.State != next || got.StateVersion != uint64(version+1) {
			t.Fatalf("got state %s version %d", got.State, got.StateVersion)
		}
		current = next
	}
	events, err := s.Events("wf-1")
	if err != nil || len(events) != len(states) {
		t.Fatalf("events = %d, %v", len(events), err)
	}
	for i, event := range events {
		if event.Sequence != uint64(i+1) || event.StateVersion != uint64(i+1) || event.To != states[i] {
			t.Fatalf("event %d = %#v", i, event)
		}
	}
	// Returned audit values are copies, not mutable store-owned records.
	events[0].ArtifactIDs[0] = "tampered"
	again, _ := s.Events("wf-1")
	if again[0].ArtifactIDs[0] != "artifact-1" {
		t.Fatal("event storage was mutated by caller")
	}
}

func TestIllegalTransition(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Transition(Transition{WorkflowID: "wf-1", ExpectedState: StateCreated, ExpectedVersion: 0, NextState: StateDelivered, IdempotencyKey: "skip"})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("error = %v", err)
	}
	w, _ := s.Get("wf-1")
	if w.State != StateCreated || w.StateVersion != 0 {
		t.Fatalf("illegal transition mutated workflow: %#v", w)
	}
}

func TestIdempotentTransitionReturnsOriginalResult(t *testing.T) {
	s := newTestStore(t)
	req := Transition{WorkflowID: "wf-1", ExpectedState: StateCreated, ExpectedVersion: 0, NextState: StateDiscovering, IdempotencyKey: "discover", ArtifactIDs: []string{"graph-1"}}
	first, err := s.Transition(req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Transition(req)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || second.StateVersion != 1 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	events, _ := s.Events("wf-1")
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}

	changed := req
	changed.NextState = StateReady
	if _, err := s.Transition(changed); !errors.Is(err, ErrIdempotencyReuse) {
		t.Fatalf("changed idempotency input error = %v", err)
	}
}

func TestCASRejectsStaleStateOrVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   State
		version uint64
	}{
		{"state", StateReady, 0}, {"version", StateCreated, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			_, err := s.Transition(Transition{WorkflowID: "wf-1", ExpectedState: tc.state, ExpectedVersion: tc.version, NextState: StateDiscovering, IdempotencyKey: tc.name})
			if !errors.Is(err, ErrCASConflict) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestBlockedResumesOnlyToRecordedTarget(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Transition(Transition{WorkflowID: "wf-1", ExpectedState: StateCreated, ExpectedVersion: 0, NextState: StateDiscovering, IdempotencyKey: "discover"})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := s.Transition(Transition{WorkflowID: "wf-1", ExpectedState: StateDiscovering, ExpectedVersion: 1, NextState: StateBlocked, ResumeTarget: StateDiscovering, IdempotencyKey: "block"})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.ResumeTarget != StateDiscovering {
		t.Fatalf("resume target = %s", blocked.ResumeTarget)
	}
	if _, err := s.Transition(Transition{WorkflowID: "wf-1", ExpectedState: StateBlocked, ExpectedVersion: 2, NextState: StateReady, IdempotencyKey: "wrong"}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("wrong resume error = %v", err)
	}
	if _, err := s.Transition(Transition{WorkflowID: "wf-1", ExpectedState: StateBlocked, ExpectedVersion: 2, NextState: StateReady, ResumeTarget: StateReady, IdempotencyKey: "forged-target"}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("forged resume target error = %v", err)
	}
	resumed, err := s.Transition(Transition{WorkflowID: "wf-1", ExpectedState: StateBlocked, ExpectedVersion: 2, NextState: StateDiscovering, IdempotencyKey: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ResumeTarget != "" {
		t.Fatalf("resume target not cleared: %s", resumed.ResumeTarget)
	}
}

func TestAcceptResultRejectsStaleAttemptAndBasis(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*MemoryStore, *ResultEnvelope)
	}{
		{"unknown attempt", func(_ *MemoryStore, r *ResultEnvelope) { r.AttemptID = "missing" }},
		{"wrong workflow", func(_ *MemoryStore, r *ResultEnvelope) { r.WorkflowID = "other" }},
		{"wrong node", func(_ *MemoryStore, r *ResultEnvelope) { r.NodeID = "other" }},
		{"wrong basis", func(_ *MemoryStore, r *ResultEnvelope) { r.BaseCandidateOID = "tree-2" }},
		{"superseded", func(s *MemoryStore, _ *ResultEnvelope) {
			if err := s.SupersedeAttempt("attempt-1"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			if err := s.RegisterAttempt(Attempt{ID: "attempt-1", WorkflowID: "wf-1", NodeID: "node-1", BasisTree: "tree-1"}); err != nil {
				t.Fatal(err)
			}
			result := ResultEnvelope{WorkflowID: "wf-1", NodeID: "node-1", AttemptID: "attempt-1", BaseCandidateOID: "tree-1", Status: AttemptCompleted}
			tc.mutate(s, &result)
			if err := s.AcceptResult(result); !errors.Is(err, ErrStaleResult) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAcceptResultConsumesLiveAttemptExactlyOnce(t *testing.T) {
	s := newTestStore(t)
	if err := s.RegisterAttempt(Attempt{ID: "attempt-1", WorkflowID: "wf-1", NodeID: "node-1", BasisTree: "tree-1"}); err != nil {
		t.Fatal(err)
	}
	result := ResultEnvelope{WorkflowID: "wf-1", NodeID: "node-1", AttemptID: "attempt-1", BaseCandidateOID: "tree-1", Status: AttemptCompleted, EvidenceIDs: []string{"evidence-1"}}
	if err := s.AcceptResult(result); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptResult(result); !errors.Is(err, ErrStaleResult) {
		t.Fatalf("duplicate error = %v", err)
	}
}
