// Package judges evaluates immutable, implementation-neutral evidence. It does
// not execute agent prose or derive authority from a candidate workspace.
package judges

type Outcome string

const (
	OutcomePass    Outcome = "pass"
	OutcomeFail    Outcome = "fail"
	OutcomeInvalid Outcome = "invalid"
)

type Category string

const (
	CategoryInfrastructure Category = "infrastructure"
	CategoryFilesystem     Category = "filesystem"
	CategoryAcceptance     Category = "acceptance"
	CategoryBehavior       Category = "behavior"
	CategoryClaims         Category = "claim-consistency"
	CategorySecurity       Category = "security"
)

type CheckResult struct {
	ID             string
	Category       Category
	Outcome        Outcome
	Hard           bool
	Summary        string
	RequirementIDs []string
	EvidenceIDs    []string
}

type Verdict struct {
	Status                    Outcome
	Checks                    []CheckResult
	HardFailure               bool
	AllowsQualitativeOverride bool
}

type Evidence struct {
	Infrastructure *InfrastructureEvidence
	Filesystem     *FilesystemEvidence
	Acceptance     *AcceptanceEvidence
	Behavior       *BehaviorEvidence
	Claims         *ClaimEvidence
	Security       *SecurityEvidence
}

type Policy struct {
	Infrastructure *InfrastructurePolicy
	Filesystem     *FilesystemPolicy
	Acceptance     *AcceptancePolicy
	Behavior       *BehaviorPolicy
	Claims         *ClaimPolicy
	Security       *SecurityPolicy
}

type InfrastructureEvidence struct {
	EvidenceID        string
	Complete          bool
	SessionFinished   bool
	TimedOut          bool
	Canceled          bool
	ProcessTreeClean  bool
	TelemetryComplete bool
	Error             string
}

type InfrastructurePolicy struct {
	RequireSessionFinished   bool
	ForbidTimeout            bool
	ForbidCancellation       bool
	RequireCleanProcessTree  bool
	RequireCompleteTelemetry bool
}

type FileState struct {
	Path   string
	Kind   string
	Mode   uint32
	Digest string
}

type FilesystemEvidence struct {
	EvidenceID string
	Complete   bool
	Before     []FileState
	After      []FileState
}

type FileExpectation struct {
	Path    string
	Absent  bool
	Kind    string
	Mode    *uint32
	Digest  string
	Content []byte
}

type FilesystemPolicy struct {
	ExpectedChanges    []string
	AllowedChanges     []string
	AllowedPrefixes    []string
	ForbiddenChanges   []string
	ForbiddenPrefixes  []string
	ExactFiles         []FileExpectation
	RequireSafeEntries bool
}

type CommandEvidence struct {
	EvidenceID          string
	ID                  string
	Recorded            bool
	Completed           bool
	ExitCode            int
	TimedOut            bool
	Canceled            bool
	OutputLimitExceeded bool
	CleanProcessTree    bool
	InfrastructureError string
}

type AcceptanceEvidence struct {
	EvidenceID string
	Complete   bool
	Commands   []CommandEvidence
}

type CommandExpectation struct {
	ID string
	// ExpectedExit is authoritative when non-empty. ExitCode retains source
	// compatibility with the original single-exit judge contract.
	ExpectedExit []int
	ExitCode     int
}

type AcceptancePolicy struct {
	Commands []CommandExpectation
}

type EventType string

const (
	EventToolCall   EventType = "tool-call"
	EventDelegation EventType = "delegation"
	EventRetry      EventType = "retry"
)

type Event struct {
	EvidenceID string
	Sequence   uint64
	Type       EventType
	Name       string
	ParentID   string
	ChildID    string
	Succeeded  bool
}

type BehaviorEvidence struct {
	EvidenceID string
	Complete   bool
	Events     []Event
}

type EventSelector struct {
	Type EventType
	Name string
}

type EventCountExpectation struct {
	ID       string
	Selector EventSelector
	Min      int
	Max      *int
}

type OrderExpectation struct {
	ID     string
	Before EventSelector
	After  EventSelector
}

type CountRange struct {
	Min int
	Max *int
}

type BehaviorPolicy struct {
	Counts      []EventCountExpectation
	Order       []OrderExpectation
	Delegations *CountRange
	MaxRetries  *int
}

type ClaimFact struct {
	EvidenceID string
	Name       string
	Claimed    string
	Observed   string
}

type ClaimEvidence struct {
	EvidenceID    string
	Complete      bool
	FinalResponse string
	Facts         []ClaimFact
}

type ClaimPolicy struct {
	SuccessPatterns              []string
	NoFalseSuccess               bool
	RequireSuccessWhenChecksPass bool
	RequiredFacts                []string
}

type SecurityInvariant struct {
	EvidenceID string
	Name       string
	Satisfied  bool
}

type SecurityViolation struct {
	EvidenceID string
	Kind       string
	Detail     string
}

type SecurityEvidence struct {
	EvidenceID    string
	Complete      bool
	ExecutionMode string
	NetworkMode   string
	Invariants    []SecurityInvariant
	Violations    []SecurityViolation
}

type SecurityPolicy struct {
	AllowedExecutionModes []string
	RequiredNetworkMode   string
	RequiredInvariants    []string
	ForbidViolations      bool
}
