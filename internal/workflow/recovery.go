package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/joeldevz/skynex/internal/gitcandidate"
)

var ErrRecoveryBasisMissing = errors.New("workflow: recovery basis artifact is missing")

type RecoveryBasis struct {
	Seal              gitcandidate.ContextSeal `json:"seal"`
	CandidatePolicy   gitcandidate.Policy      `json:"candidate_policy"`
	PreTreeOID        string                   `json:"pre_tree_oid"`
	PostTreeOID       string                   `json:"post_tree_oid"`
	CandidateTreeOID  string                   `json:"candidate_tree_oid"`
	CandidateRecordID string                   `json:"candidate_record_id"`
	PolicyHash        string                   `json:"policy_hash"`
}

func (s *SQLiteStore) SaveRecoveryBasis(workflowID string, b RecoveryBasis) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO recovery_bases(workflow_id,basis) VALUES(?,?) ON CONFLICT(workflow_id) DO UPDATE SET basis=excluded.basis`, workflowID, raw)
	return err
}
func (s *SQLiteStore) RecoveryBasis(workflowID string) (RecoveryBasis, error) {
	var raw []byte
	if err := s.db.QueryRow(`SELECT basis FROM recovery_bases WHERE workflow_id=?`, workflowID).Scan(&raw); err != nil {
		return RecoveryBasis{}, ErrRecoveryBasisMissing
	}
	var b RecoveryBasis
	if json.Unmarshal(raw, &b) != nil {
		return RecoveryBasis{}, ErrRecoveryBasisMissing
	}
	return b, nil
}

type ResumeRequest struct {
	WorkflowID, BlockerID, IdempotencyKey, Owner, FencingToken string
	Now                                                        time.Time
	LeaseDuration                                              time.Duration
}

func (s *SQLiteStore) Resume(ctx context.Context, repo string, req ResumeRequest) (Workflow, error) {
	_ = repo
	w, err := s.Get(req.WorkflowID)
	if err != nil {
		return Workflow{}, err
	}
	if w.State != StateBlocked {
		return Workflow{}, fmt.Errorf("workflow %s is %s; only blocked workflows can resume", w.ID, w.State)
	}
	if w.ResumeTarget == "" {
		return Workflow{}, errors.New("workflow: blocked workflow has no resume target")
	}
	events, err := s.Events(w.ID)
	if err != nil {
		return Workflow{}, err
	}
	matched := false
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].To == StateBlocked {
			for _, id := range events[i].ArtifactIDs {
				if id == req.BlockerID {
					matched = true
				}
			}
			break
		}
	}
	if !matched {
		return Workflow{}, errors.New("workflow: blocker ID does not match the active blocker")
	}
	basis, err := s.RecoveryBasis(w.ID)
	if err != nil {
		return Workflow{}, err
	}
	if basis.Seal.RepositoryRoot == "" || basis.Seal.GitCommonDir == "" {
		return Workflow{}, ErrRecoveryBasisMissing
	}
	lockPath := filepath.Join(basis.Seal.GitCommonDir, "skynex", "worktree.lock")
	lock, err := acquireLocalLock(lockPath)
	if err != nil {
		return Workflow{}, fmt.Errorf("workflow: acquire local lock: %w", err)
	}
	defer lock.Close()
	outcome, currentTree, reconcileErr := reconcileBasis(basis, w.ResumeTarget)
	if reconcileErr != nil {
		return Workflow{}, reconcileErr
	}
	if outcome == "unknown" {
		return s.Transition(Transition{WorkflowID: w.ID, ExpectedState: StateBlocked, ExpectedVersion: w.StateVersion, NextState: StateIntegrationConflict, IdempotencyKey: req.IdempotencyKey, ArtifactIDs: []string{"observed-tree:" + currentTree}})
	}
	if req.Owner == "" {
		req.Owner = fmt.Sprintf("pid-%d", os.Getpid())
	}
	if req.FencingToken == "" {
		req.FencingToken = randomToken()
	}
	if req.Now.IsZero() {
		req.Now = time.Now()
	}
	if req.LeaseDuration <= 0 {
		req.LeaseDuration = time.Minute
	}
	if _, err = s.AcquireLease("worktree:"+basis.Seal.WorktreeID, req.Owner, req.FencingToken, req.Now, req.Now.Add(req.LeaseDuration)); err != nil {
		return Workflow{}, fmt.Errorf("workflow: reconciled but lease reclaim failed: %w", err)
	}
	select {
	case <-ctx.Done():
		return Workflow{}, ctx.Err()
	default:
	}
	return s.Transition(Transition{WorkflowID: w.ID, ExpectedState: StateBlocked, ExpectedVersion: w.StateVersion, NextState: w.ResumeTarget, IdempotencyKey: req.IdempotencyKey})
}

func reconcileBasis(b RecoveryBasis, target State) (string, string, error) {
	current, err := gitcandidate.CaptureContext(b.Seal.RepositoryRoot)
	if err != nil {
		return "", "", err
	}
	if current.WorktreeID != b.Seal.WorktreeID || current.GitCommonDir != b.Seal.GitCommonDir || current.Detached != b.Seal.Detached || current.SymbolicHEAD != b.Seal.SymbolicHEAD || current.BaseCommitOID != b.Seal.BaseCommitOID {
		return "unknown", "context-drift", nil
	}
	candidate, err := gitcandidate.Freeze(b.Seal, b.CandidatePolicy)
	if err != nil {
		return "", "", err
	}
	tree := candidate.TreeOID
	if target == StateCandidateFrozen || target == StateReviewing || target == StateReceipted {
		if b.CandidateRecordID == "" || b.CandidateTreeOID == "" || b.PolicyHash == "" {
			return "", "", ErrRecoveryBasisMissing
		}
		if tree == b.CandidateTreeOID {
			return "post", tree, nil
		}
		return "unknown", tree, nil
	}
	if b.PreTreeOID == "" && b.PostTreeOID == "" {
		return "", "", ErrRecoveryBasisMissing
	}
	if tree == b.PostTreeOID {
		return "post", tree, nil
	}
	if tree == b.PreTreeOID {
		return "pre", tree, nil
	}
	return "unknown", tree, nil
}
func randomToken() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value[:])
}
