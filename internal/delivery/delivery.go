package delivery

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/review"
)

var (
	ErrInvalidAuthority = errors.New("delivery: invalid receipt authority")
	ErrCompatibility    = errors.New("delivery: policy or engine is incompatible")
	ErrContextDrift     = errors.New("delivery: candidate context drifted")
	ErrRefConflict      = errors.New("delivery: destination ref compare-and-swap failed")
	ErrIdempotencyReuse = errors.New("delivery: idempotency key reused with different intent")
)

type AuthorityStore interface {
	Authority(string) (review.Receipt, error)
}

type Request struct {
	WorkflowID              string
	Candidate               review.CandidateRecord
	CandidatePolicy         gitcandidate.Policy
	ExpectedReceiptID       string
	ExpectedPolicyHash      string
	CompatibleEngineVersion string
	Message                 string
	IdempotencyKey          string
}

type Result struct {
	CommitOID string
	TreeOID   string
	Ref       string
	ReceiptID string
	Recovered bool
}

type Intent struct{ WorkflowID, IdempotencyKey, CandidateRecordID, ReceiptID, Ref, OldCommitOID, CommitOID, TreeOID, Message string }
type IntentStore interface {
	Get(workflowID, key string) (Intent, bool)
	Put(Intent) error
}

type MemoryIntentStore struct {
	mu      sync.Mutex
	intents map[string]Intent
}

func NewMemoryIntentStore() *MemoryIntentStore {
	return &MemoryIntentStore{intents: map[string]Intent{}}
}
func intentKey(workflowID, key string) string { return workflowID + "\x00" + key }
func (s *MemoryIntentStore) Get(workflowID, key string) (Intent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.intents[intentKey(workflowID, key)]
	return i, ok
}
func (s *MemoryIntentStore) Put(i Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := intentKey(i.WorkflowID, i.IdempotencyKey)
	if old, ok := s.intents[key]; ok {
		if old == i {
			return nil
		}
		return ErrIdempotencyReuse
	}
	s.intents[key] = i
	return nil
}

type Gate struct {
	Authority AuthorityStore
	Intents   IntentStore
	// AfterGateCheck is a test/integration seam invoked after the exact commit and
	// intent exist but before ref CAS. The commit never rereads the worktree.
	AfterGateCheck func()
}

func (g *Gate) Commit(ctx context.Context, req Request) (Result, error) {
	if g.Authority == nil || g.Intents == nil || req.IdempotencyKey == "" || req.Message == "" {
		return Result{}, errors.New("delivery: incomplete request")
	}
	authority, err := g.Authority.Authority(req.WorkflowID)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidAuthority, err)
	}
	if authority.ID != req.ExpectedReceiptID || authority.WorkflowID != req.WorkflowID || authority.CandidateRecordID != req.Candidate.ID || authority.CandidateTreeOID != req.Candidate.TreeOID {
		return Result{}, ErrInvalidAuthority
	}
	if authority.PolicyHash != req.ExpectedPolicyHash || req.Candidate.PolicyHash != req.ExpectedPolicyHash || authority.EngineVersion != req.CompatibleEngineVersion || req.Candidate.EngineVersion != req.CompatibleEngineVersion {
		return Result{}, ErrCompatibility
	}
	seal := req.Candidate.Seal
	if seal.Detached || seal.SymbolicHEAD == "" || seal.BaseRef == "" || seal.SymbolicHEAD != seal.BaseRef {
		return Result{}, ErrContextDrift
	}
	if err := review.ValidateCandidateRecord(req.Candidate); err != nil {
		return Result{}, ErrInvalidAuthority
	}
	if req.Candidate.CandidatePolicyHash != req.CandidatePolicy.Hash() {
		return Result{}, ErrCompatibility
	}
	if previous, ok := g.Intents.Get(req.WorkflowID, req.IdempotencyKey); ok {
		return g.recover(ctx, req, authority, previous)
	}
	frozen := gitcandidate.Candidate{Seal: seal, TreeOID: req.Candidate.TreeOID, Manifest: req.Candidate.Manifest, PolicyHash: req.Candidate.CandidatePolicyHash}
	drift, err := gitcandidate.DetectDrift(frozen, req.CandidatePolicy)
	if err != nil {
		return Result{}, err
	}
	if drift.Any() {
		return Result{}, fmt.Errorf("%w: %s", ErrContextDrift, strings.Join(drift.Reasons, ", "))
	}
	commitOID, err := commitTree(ctx, seal.RepositoryRoot, req.Candidate.TreeOID, seal.BaseCommitOID, req.Message)
	if err != nil {
		return Result{}, err
	}
	if tree, err := gitText(ctx, seal.RepositoryRoot, "rev-parse", commitOID+"^{tree}"); err != nil || tree != req.Candidate.TreeOID {
		return Result{}, errors.New("delivery: created commit tree mismatch")
	}
	intent := Intent{WorkflowID: req.WorkflowID, IdempotencyKey: req.IdempotencyKey, CandidateRecordID: req.Candidate.ID, ReceiptID: authority.ID, Ref: seal.BaseRef, OldCommitOID: seal.BaseCommitOID, CommitOID: commitOID, TreeOID: req.Candidate.TreeOID, Message: req.Message}
	if err := g.Intents.Put(intent); err != nil {
		return Result{}, err
	}
	if g.AfterGateCheck != nil {
		g.AfterGateCheck()
	}
	if err := updateRef(ctx, seal.RepositoryRoot, intent); err != nil {
		return Result{}, err
	}
	return Result{CommitOID: commitOID, TreeOID: intent.TreeOID, Ref: intent.Ref, ReceiptID: intent.ReceiptID}, nil
}

func (g *Gate) recover(ctx context.Context, req Request, authority review.Receipt, intent Intent) (Result, error) {
	if intent.CandidateRecordID != req.Candidate.ID || intent.ReceiptID != authority.ID || intent.TreeOID != req.Candidate.TreeOID || intent.Message != req.Message {
		return Result{}, ErrIdempotencyReuse
	}
	current, err := gitText(ctx, req.Candidate.Seal.RepositoryRoot, "rev-parse", intent.Ref)
	if err != nil {
		return Result{}, err
	}
	if current == intent.CommitOID {
		tree, err := gitText(ctx, req.Candidate.Seal.RepositoryRoot, "rev-parse", current+"^{tree}")
		if err != nil || tree != intent.TreeOID {
			return Result{}, ErrRefConflict
		}
		return Result{CommitOID: intent.CommitOID, TreeOID: intent.TreeOID, Ref: intent.Ref, ReceiptID: intent.ReceiptID, Recovered: true}, nil
	}
	if current != intent.OldCommitOID {
		return Result{}, ErrRefConflict
	}
	if err := updateRef(ctx, req.Candidate.Seal.RepositoryRoot, intent); err != nil {
		return Result{}, err
	}
	return Result{CommitOID: intent.CommitOID, TreeOID: intent.TreeOID, Ref: intent.Ref, ReceiptID: intent.ReceiptID, Recovered: true}, nil
}

func commitTree(ctx context.Context, repo, tree, parent, message string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "commit-tree", tree, "-p", parent)
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(message + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git commit-tree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
func updateRef(ctx context.Context, repo string, intent Intent) error {
	cmd := exec.CommandContext(ctx, "git", "update-ref", intent.Ref, intent.CommitOID, intent.OldCommitOID)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", ErrRefConflict, strings.TrimSpace(string(out)))
	}
	return nil
}
func gitText(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
