package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/orchestration"
	"github.com/joeldevz/skynex/internal/workflow"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Scheduler struct {
	store *workflow.SQLiteStore
	db    *sql.DB
	graph orchestration.ExecutionGraph
}

func NewScheduler(store *workflow.SQLiteStore, g orchestration.ExecutionGraph) (*Scheduler, error) {
	if err := orchestration.ValidateExecution(g); err != nil {
		return nil, err
	}
	s := &Scheduler{store: store, db: store.Database(), graph: g}
	for _, slice := range g.Slices {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO execution_slice_state(workflow_id,slice_id,status) VALUES(?,?,'pending')`, g.WorkflowID, slice.ID); err != nil {
			return nil, err
		}
	}
	return s, nil
}
func (s *Scheduler) NextReady() (*orchestration.Slice, error) {
	status := map[string]string{}
	rows, err := s.db.Query(`SELECT slice_id,status FROM execution_slice_state WHERE workflow_id=?`, s.graph.WorkflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, value string
		if err = rows.Scan(&id, &value); err != nil {
			return nil, err
		}
		status[id] = value
	}
	for _, v := range status {
		if v == "active" {
			return nil, nil
		}
	}
	slices := append([]orchestration.Slice(nil), s.graph.Slices...)
	sort.Slice(slices, func(i, j int) bool { return slices[i].ID < slices[j].ID })
	for _, slice := range slices {
		if status[slice.ID] != "pending" {
			continue
		}
		ready := true
		for _, dep := range slice.Dependencies {
			if status[dep] != "completed" {
				ready = false
			}
		}
		if ready {
			return &slice, nil
		}
	}
	return nil, nil
}

type Attempt struct {
	ID, WorkflowID, SliceID, WorktreeID, Owner, FencingToken, BasisTree, OperationID string
	AllowedPaths                                                                     []string
}

func (s *Scheduler) Start(a Attempt) error {
	ready, err := s.NextReady()
	if err != nil {
		return err
	}
	if ready == nil || ready.ID != a.SliceID {
		return errors.New("execution: slice is not ready")
	}
	allowed, _ := json.Marshal(a.AllowedPaths)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE execution_slice_state SET status='active' WHERE workflow_id=? AND slice_id=? AND status='pending'`, a.WorkflowID, a.SliceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("execution: concurrent writer active")
	}
	if _, err = tx.Exec(`INSERT INTO mutation_attempts(attempt_id,workflow_id,slice_id,worktree_id,owner,fencing_token,basis_tree,allowed_paths,operation_id,live) VALUES(?,?,?,?,?,?,?,?,?,1)`, a.ID, a.WorkflowID, a.SliceID, a.WorktreeID, a.Owner, a.FencingToken, a.BasisTree, allowed, a.OperationID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	w, err := s.store.Get(a.WorkflowID)
	if err != nil {
		return err
	}
	if w.State == workflow.StateReady {
		_, err = s.store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateExecuting, IdempotencyKey: fmt.Sprintf("execution:start:v%d", s.graph.Version)})
	}
	return err
}
func (s *Scheduler) Complete(workflowID, sliceID string) error {
	return s.store.CompleteExecutionSlice(workflowID, sliceID, s.graph.Version)
}

// ReconcileCompletion closes the crash window between the broker's durable
// mutation commit and the slice and workflow completions that follow it. It
// adopts slices whose mutation already completed and advances a fully executed
// workflow, and is safe to call at the start of every execution pass.
func (s *Scheduler) ReconcileCompletion() (bool, error) {
	return s.store.ReconcileCompletedExecution(s.graph.WorkflowID, s.graph.Version)
}

type FileOperation struct {
	Path   string
	Data   []byte
	Mode   os.FileMode
	Delete bool
}
type PatchArtifact struct{ Operations []FileOperation }
type WorkerResult struct {
	Envelope            workflow.ResultEnvelope
	Patch               PatchArtifact
	Owner, FencingToken string
}
type Broker struct {
	Store         *workflow.SQLiteStore
	Seal          gitcandidate.ContextSeal
	Policy        gitcandidate.Policy
	AfterIntent   func() error
	AfterMutation func() error
}

func (b *Broker) Apply(ctx context.Context, result WorkerResult) (string, error) {
	var a Attempt
	var allowedRaw []byte
	var live int
	err := b.Store.Database().QueryRow(`SELECT attempt_id,workflow_id,slice_id,worktree_id,owner,fencing_token,basis_tree,allowed_paths,operation_id,live FROM mutation_attempts WHERE attempt_id=?`, result.Envelope.AttemptID).Scan(&a.ID, &a.WorkflowID, &a.SliceID, &a.WorktreeID, &a.Owner, &a.FencingToken, &a.BasisTree, &allowedRaw, &a.OperationID, &live)
	if err != nil || live != 1 || a.WorkflowID != result.Envelope.WorkflowID || a.Owner != result.Owner || a.FencingToken != result.FencingToken || a.BasisTree != result.Envelope.BaseCandidateOID {
		return "", b.stale(result, "attempt, fencing, or basis mismatch")
	}
	_ = json.Unmarshal(allowedRaw, &a.AllowedPaths)
	if err = b.Store.ValidateLease("worktree:"+a.WorktreeID, a.Owner, a.FencingToken, time.Now()); err != nil {
		return "", b.stale(result, "lease expired")
	}
	lock, err := workflow.AcquireWorktreeLock(b.Seal.GitCommonDir, a.WorktreeID)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	current, err := gitcandidate.Freeze(b.Seal, b.Policy)
	if err != nil {
		return "", err
	}
	if current.TreeOID != a.BasisTree {
		return "", b.stale(result, "pre-tree mismatch")
	}
	if err = validatePatch(result.Patch, a.AllowedPaths, b.Seal.RepositoryRoot); err != nil {
		return "", err
	}
	evidence, _ := json.Marshal(result.Envelope.EvidenceIDs)
	if _, err = b.Store.Database().Exec(`INSERT OR IGNORE INTO mutation_operations(operation_id,workflow_id,attempt_id,pre_tree,status,evidence) VALUES(?,?,?,?,'intended',?)`, a.OperationID, a.WorkflowID, a.ID, a.BasisTree, evidence); err != nil {
		return "", err
	}
	if b.AfterIntent != nil {
		if err = b.AfterIntent(); err != nil {
			return "", err
		}
	}
	if err = applyPatch(b.Seal.RepositoryRoot, result.Patch); err != nil {
		return "", err
	}
	post, err := gitcandidate.Freeze(b.Seal, b.Policy)
	if err != nil {
		return "", err
	}
	if _, err = b.Store.Database().Exec(`UPDATE mutation_operations SET post_tree=?,status='mutated' WHERE operation_id=? AND status='intended'`, post.TreeOID, a.OperationID); err != nil {
		return "", err
	}
	if b.AfterMutation != nil {
		if err = b.AfterMutation(); err != nil {
			return "", err
		}
	}
	tx, err := b.Store.Database().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE mutation_operations SET status='completed' WHERE operation_id=? AND status='mutated'`, a.OperationID); err != nil {
		return "", err
	}
	if _, err = tx.Exec(`UPDATE mutation_attempts SET live=0 WHERE attempt_id=? AND live=1`, a.ID); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return post.TreeOID, nil
}
func (b *Broker) stale(result WorkerResult, reason string) error {
	raw, _ := json.Marshal(result.Envelope)
	_, _ = b.Store.Database().Exec(`INSERT INTO stale_result_audit(attempt_id,reason,envelope,occurred_at) VALUES(?,?,?,?)`, result.Envelope.AttemptID, reason, raw, time.Now().UTC().Format(time.RFC3339Nano))
	return workflow.ErrStaleResult
}
func validatePatch(p PatchArtifact, allowed []string, root string) error {
	set := map[string]bool{}
	for _, path := range allowed {
		set[filepath.ToSlash(filepath.Clean(path))] = true
	}
	for _, op := range p.Operations {
		clean := filepath.ToSlash(filepath.Clean(op.Path))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(op.Path) || !set[clean] {
			return fmt.Errorf("execution: forbidden path %q", op.Path)
		}
		current := root
		parts := strings.Split(clean, "/")
		for _, part := range parts[:len(parts)-1] {
			current = filepath.Join(current, part)
			if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("execution: symlink escape %q", op.Path)
			}
		}
		if info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(clean))); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("execution: symlink escape %q", op.Path)
		}
	}
	return nil
}
func applyPatch(root string, p PatchArtifact) error {
	for _, op := range p.Operations {
		path := filepath.Join(root, filepath.FromSlash(op.Path))
		if op.Delete {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		mode := op.Mode
		if mode == 0 {
			mode = 0o600
		}
		if err := os.WriteFile(path, op.Data, mode); err != nil {
			return err
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	return nil
}
func (b *Broker) Recover(operationID string) (string, error) {
	var pre, post, status, workflowID string
	if err := b.Store.Database().QueryRow(`SELECT pre_tree,post_tree,status,workflow_id FROM mutation_operations WHERE operation_id=?`, operationID).Scan(&pre, &post, &status, &workflowID); err != nil {
		return "", err
	}
	current, err := gitcandidate.Freeze(b.Seal, b.Policy)
	if err != nil {
		return "", err
	}
	if post != "" && current.TreeOID == post {
		return "post", nil
	}
	if current.TreeOID == pre {
		return "pre", nil
	}
	w, err := b.Store.Get(workflowID)
	if err != nil {
		return "", err
	}
	if w.State == workflow.StateExecuting {
		_, err = b.Store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateIntegrationConflict, IdempotencyKey: "execution:conflict:" + operationID})
	}
	return "unknown", err
}
