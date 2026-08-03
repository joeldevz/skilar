package workflow

import "time"

type State string

const (
	StateCreated             State = "created"
	StateDiscovering         State = "discovering"
	StateReady               State = "ready"
	StateExecuting           State = "executing"
	StateVerifying           State = "verifying"
	StateCandidateFrozen     State = "candidate_frozen"
	StateReviewing           State = "reviewing"
	StateReceipted           State = "receipted"
	StateDelivered           State = "delivered"
	StateBlocked             State = "blocked"
	StateReplanRequired      State = "replan_required"
	StateIntegrationConflict State = "integration_conflict"
	StateAborted             State = "aborted"
	StateFailed              State = "failed"
)

type Route string

const (
	RouteSimple    Route = "simple"
	RoutePlanned   Route = "planned"
	RouteDiscovery Route = "discovery"
)

type Risk string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

type Workflow struct {
	ID           string
	State        State
	StateVersion uint64
	Route        Route
	MinimumRisk  Risk
	BasisTree    string
	ResumeTarget State
}

type Transition struct {
	WorkflowID      string
	ExpectedState   State
	ExpectedVersion uint64
	NextState       State
	IdempotencyKey  string
	ArtifactIDs     []string
	ResumeTarget    State
}

// Event is an immutable audit record. Store implementations must return copies.
type Event struct {
	Sequence       uint64
	WorkflowID     string
	From           State
	To             State
	StateVersion   uint64
	IdempotencyKey string
	ArtifactIDs    []string
	OccurredAt     time.Time
}

type AttemptStatus string

const (
	AttemptCompleted        AttemptStatus = "completed"
	AttemptBlocked          AttemptStatus = "blocked"
	AttemptRetryableFailure AttemptStatus = "retryable_failure"
	AttemptFailed           AttemptStatus = "failed"
	AttemptCancelled        AttemptStatus = "cancelled"
)

type Attempt struct {
	ID         string
	WorkflowID string
	NodeID     string
	BasisTree  string
	Live       bool
}

type ResultEnvelope struct {
	WorkflowID       string
	NodeID           string
	AttemptID        string
	BaseCandidateOID string
	Status           AttemptStatus
	ArtifactIDs      []string
	EvidenceIDs      []string
}
