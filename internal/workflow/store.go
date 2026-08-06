package workflow

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrNotFound          = errors.New("workflow: not found")
	ErrAlreadyExists     = errors.New("workflow: already exists")
	ErrIllegalTransition = errors.New("workflow: illegal transition")
	ErrCASConflict       = errors.New("workflow: compare-and-swap conflict")
	ErrIdempotencyReuse  = errors.New("workflow: idempotency key reused with different input")
	ErrStaleResult       = errors.New("workflow: stale result")
	ErrLeaseConflict     = errors.New("workflow: lease conflict")
	// ErrExecutionFenceHeld marks a refusal to execute, not a failure of the
	// work itself. A caller that turns job failures into durable blockers must
	// not block a workflow whose only problem is that someone else owns it.
	ErrExecutionFenceHeld = errors.New("workflow: another process is executing this workflow")
	// ErrExecutionFenceLost marks the loss of exclusivity mid-run. The holder
	// must stop before its next mutation rather than act on a stale claim.
	ErrExecutionFenceLost = errors.New("workflow: execution fence was lost")
)

// ExecutionFenceResource names the lease granting one process exclusive
// execution of exactly one workflow. It is scoped strictly to the workflow ID,
// never to the repository or worktree, so distinct workflows — including
// alternative solutions to the same problem, each in its own worktree and
// branch — execute concurrently without ever contending for this fence.
func ExecutionFenceResource(workflowID string) string {
	return "workflow-execution:" + workflowID
}

// Store captures the persistence boundary needed by the engine. A SQLite store
// can implement this contract without changing transition callers.
type Store interface {
	Create(Workflow) (Workflow, error)
	Get(string) (Workflow, error)
	Transition(Transition) (Workflow, error)
	Events(string) ([]Event, error)
	RegisterAttempt(Attempt) error
	SupersedeAttempt(string) error
	AcceptResult(ResultEnvelope) error
	AcquireLease(resource, owner, token string, now, expiresAt time.Time) (Lease, error)
	HeartbeatLease(resource, owner, token string, now, expiresAt time.Time) (Lease, error)
}

type transitionRecord struct {
	request Transition
	result  Workflow
}

type MemoryStore struct {
	mu          sync.Mutex
	workflows   map[string]Workflow
	events      map[string][]Event
	idempotency map[string]transitionRecord
	attempts    map[string]Attempt
	results     map[string]ResultEnvelope
	now         func() time.Time
	sequence    uint64
	leases      map[string]Lease
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		workflows: make(map[string]Workflow), events: make(map[string][]Event),
		idempotency: make(map[string]transitionRecord), attempts: make(map[string]Attempt),
		results: make(map[string]ResultEnvelope), now: time.Now,
		leases: make(map[string]Lease),
	}
}

func (s *MemoryStore) AcquireLease(resource, owner, token string, now, expiresAt time.Time) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if resource == "" || owner == "" || token == "" || !expiresAt.After(now) {
		return Lease{}, ErrLeaseConflict
	}
	if current, ok := s.leases[resource]; ok && current.ExpiresAt.After(now) {
		return Lease{}, ErrLeaseConflict
	}
	lease := Lease{Resource: resource, Owner: owner, FencingToken: token, ExpiresAt: expiresAt.UTC()}
	s.leases[resource] = lease
	return lease, nil
}

func (s *MemoryStore) HeartbeatLease(resource, owner, token string, now, expiresAt time.Time) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.leases[resource]
	if !ok || current.Owner != owner || current.FencingToken != token || !current.ExpiresAt.After(now) || !expiresAt.After(now) {
		return Lease{}, ErrLeaseConflict
	}
	current.ExpiresAt = expiresAt.UTC()
	s.leases[resource] = current
	return current, nil
}

func (s *MemoryStore) Create(w Workflow) (Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w.ID == "" {
		return Workflow{}, fmt.Errorf("workflow: empty id")
	}
	if _, exists := s.workflows[w.ID]; exists {
		return Workflow{}, ErrAlreadyExists
	}
	if w.State == "" {
		w.State = StateCreated
	}
	if w.State != StateCreated || w.StateVersion != 0 {
		return Workflow{}, fmt.Errorf("workflow: create requires created at version zero")
	}
	s.workflows[w.ID] = cloneWorkflow(w)
	return cloneWorkflow(w), nil
}

func (s *MemoryStore) Get(id string) (Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workflows[id]
	if !ok {
		return Workflow{}, ErrNotFound
	}
	return cloneWorkflow(w), nil
}

func (s *MemoryStore) Transition(req Transition) (Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.IdempotencyKey == "" {
		return Workflow{}, fmt.Errorf("workflow: empty idempotency key")
	}
	idemKey := req.WorkflowID + "\x00" + req.IdempotencyKey
	if previous, ok := s.idempotency[idemKey]; ok {
		if !sameTransition(previous.request, req) {
			return Workflow{}, ErrIdempotencyReuse
		}
		return cloneWorkflow(previous.result), nil
	}
	w, ok := s.workflows[req.WorkflowID]
	if !ok {
		return Workflow{}, ErrNotFound
	}
	if w.State != req.ExpectedState || w.StateVersion != req.ExpectedVersion {
		return Workflow{}, ErrCASConflict
	}
	resumeTarget := req.ResumeTarget
	if w.State == StateBlocked {
		resumeTarget = w.ResumeTarget
	}
	if !CanTransition(w.State, req.NextState, resumeTarget) {
		return Workflow{}, ErrIllegalTransition
	}
	if req.NextState == StateBlocked {
		if _, valid := blockableStates[req.ResumeTarget]; !valid || req.ResumeTarget != w.State {
			return Workflow{}, fmt.Errorf("%w: blocked transition requires the source state as resume target", ErrIllegalTransition)
		}
	}
	from := w.State
	w.State = req.NextState
	w.StateVersion++
	if req.NextState == StateBlocked {
		w.ResumeTarget = req.ResumeTarget
	} else if from == StateBlocked {
		w.ResumeTarget = ""
	}
	s.sequence++
	event := Event{Sequence: s.sequence, WorkflowID: w.ID, From: from, To: w.State,
		StateVersion: w.StateVersion, IdempotencyKey: req.IdempotencyKey,
		ArtifactIDs: append([]string(nil), req.ArtifactIDs...), OccurredAt: s.now().UTC()}
	s.workflows[w.ID] = cloneWorkflow(w)
	s.events[w.ID] = append(s.events[w.ID], event)
	s.idempotency[idemKey] = transitionRecord{request: cloneTransition(req), result: cloneWorkflow(w)}
	return cloneWorkflow(w), nil
}

func (s *MemoryStore) Events(id string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.workflows[id]; !ok {
		return nil, ErrNotFound
	}
	result := make([]Event, len(s.events[id]))
	for i, event := range s.events[id] {
		result[i] = event
		result[i].ArtifactIDs = append([]string(nil), event.ArtifactIDs...)
	}
	return result, nil
}

func (s *MemoryStore) RegisterAttempt(a Attempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workflows[a.WorkflowID]
	if !ok {
		return ErrNotFound
	}
	if a.ID == "" || a.NodeID == "" || a.BasisTree == "" || a.BasisTree != w.BasisTree {
		return ErrStaleResult
	}
	if _, exists := s.attempts[a.ID]; exists {
		return ErrAlreadyExists
	}
	a.Live = true
	s.attempts[a.ID] = a
	return nil
}

func (s *MemoryStore) SupersedeAttempt(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[id]
	if !ok {
		return ErrNotFound
	}
	a.Live = false
	s.attempts[id] = a
	return nil
}

func (s *MemoryStore) AcceptResult(result ResultEnvelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[result.AttemptID]
	w, workflowExists := s.workflows[result.WorkflowID]
	if !ok || !workflowExists || !a.Live || a.WorkflowID != result.WorkflowID ||
		a.NodeID != result.NodeID || a.BasisTree != result.BaseCandidateOID ||
		w.BasisTree != result.BaseCandidateOID {
		return ErrStaleResult
	}
	if _, exists := s.results[result.AttemptID]; exists {
		return ErrStaleResult
	}
	a.Live = false
	s.attempts[a.ID] = a
	s.results[a.ID] = cloneResult(result)
	return nil
}

func sameTransition(a, b Transition) bool {
	if a.WorkflowID != b.WorkflowID || a.ExpectedState != b.ExpectedState || a.ExpectedVersion != b.ExpectedVersion ||
		a.NextState != b.NextState || a.IdempotencyKey != b.IdempotencyKey || a.ResumeTarget != b.ResumeTarget || len(a.ArtifactIDs) != len(b.ArtifactIDs) {
		return false
	}
	for i := range a.ArtifactIDs {
		if a.ArtifactIDs[i] != b.ArtifactIDs[i] {
			return false
		}
	}
	return true
}

func cloneWorkflow(w Workflow) Workflow { return w }
func cloneTransition(t Transition) Transition {
	t.ArtifactIDs = append([]string(nil), t.ArtifactIDs...)
	return t
}
func cloneResult(r ResultEnvelope) ResultEnvelope {
	r.ArtifactIDs = append([]string(nil), r.ArtifactIDs...)
	r.EvidenceIDs = append([]string(nil), r.EvidenceIDs...)
	return r
}
