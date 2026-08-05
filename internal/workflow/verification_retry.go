package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type VerificationRevision struct {
	WorkflowID         string `json:"workflow_id"`
	Revision           int    `json:"revision"`
	CandidateTree      string `json:"candidate_tree"`
	CheckID            string `json:"check_id"`
	PreviousCommand    string `json:"previous_command"`
	ReplacementCommand string `json:"replacement_command"`
	Actor              string `json:"actor"`
	Reason             string `json:"reason"`
	IdempotencyKey     string `json:"idempotency_key"`
	AttemptID          string `json:"attempt_id"`
	FencingToken       string `json:"fencing_token"`
	CreatedAt          string `json:"created_at"`
}

type RetryVerificationRequest struct {
	WorkflowID, CandidateTree, CheckID  string
	PreviousCommand, ReplacementCommand string
	Actor, Reason, IdempotencyKey       string
	AttemptID, FencingToken             string
	PreviousResult, UpdatedRunInput     []byte
}

// RetryVerification archives the previous immutable result, records the
// contract revision, replaces the current run input and moves the same
// candidate back to verifying in one SQLite transaction.
func (s *SQLiteStore) RetryVerification(ctx context.Context, req RetryVerificationRequest, now time.Time) (VerificationRevision, bool, error) {
	if req.WorkflowID == "" || req.CandidateTree == "" || req.CheckID == "" || req.Actor == "" || req.Reason == "" || req.IdempotencyKey == "" || req.AttemptID == "" || req.FencingToken == "" || len(req.PreviousResult) == 0 || len(req.UpdatedRunInput) == 0 {
		return VerificationRevision{}, false, errors.New("retry verification: incomplete request")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return VerificationRevision{}, false, err
	}
	defer tx.Rollback()
	var existing VerificationRevision
	err = tx.QueryRowContext(ctx, `SELECT workflow_id,revision,candidate_tree,check_id,previous_command,replacement_command,actor,reason,idempotency_key,attempt_id,fencing_token,created_at FROM verification_contract_revisions WHERE workflow_id=? AND idempotency_key=?`, req.WorkflowID, req.IdempotencyKey).Scan(&existing.WorkflowID, &existing.Revision, &existing.CandidateTree, &existing.CheckID, &existing.PreviousCommand, &existing.ReplacementCommand, &existing.Actor, &existing.Reason, &existing.IdempotencyKey, &existing.AttemptID, &existing.FencingToken, &existing.CreatedAt)
	if err == nil {
		if existing.CheckID != req.CheckID || existing.ReplacementCommand != req.ReplacementCommand || existing.Actor != req.Actor || existing.Reason != req.Reason {
			return VerificationRevision{}, false, errors.New("retry verification: idempotency key conflicts with an existing request")
		}
		return existing, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return VerificationRevision{}, false, err
	}
	var state, resume State
	var version uint64
	if err = tx.QueryRowContext(ctx, `SELECT state,state_version,resume_target FROM workflows WHERE id=?`, req.WorkflowID).Scan(&state, &version, &resume); err != nil {
		return VerificationRevision{}, false, err
	}
	if state != StateReplanRequired && !(state == StateBlocked && resume == StateVerifying) {
		return VerificationRevision{}, false, fmt.Errorf("retry verification requires verification replan or blocked state, got %s", state)
	}
	var currentTree string
	var currentResult []byte
	if err = tx.QueryRowContext(ctx, `SELECT candidate_tree,result FROM verification_runs WHERE workflow_id=?`, req.WorkflowID).Scan(&currentTree, &currentResult); err != nil {
		return VerificationRevision{}, false, err
	}
	if currentTree != req.CandidateTree || string(currentResult) != string(req.PreviousResult) {
		return VerificationRevision{}, false, errors.New("retry verification: current result changed")
	}
	var revision int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM verification_contract_revisions WHERE workflow_id=?`, req.WorkflowID).Scan(&revision); err != nil {
		return VerificationRevision{}, false, err
	}
	stamp := dbTime(now.UTC())
	if _, err = tx.ExecContext(ctx, `INSERT INTO verification_run_history(workflow_id,revision,candidate_tree,result,archived_at) VALUES(?,?,?,?,?)`, req.WorkflowID, revision, req.CandidateTree, req.PreviousResult, stamp); err != nil {
		return VerificationRevision{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO verification_contract_revisions(workflow_id,revision,candidate_tree,check_id,previous_command,replacement_command,actor,reason,idempotency_key,attempt_id,fencing_token,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, req.WorkflowID, revision, req.CandidateTree, req.CheckID, req.PreviousCommand, req.ReplacementCommand, req.Actor, req.Reason, req.IdempotencyKey, req.AttemptID, req.FencingToken, stamp); err != nil {
		return VerificationRevision{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE workflow_run_inputs SET input=? WHERE workflow_id=?`, req.UpdatedRunInput, req.WorkflowID); err != nil {
		return VerificationRevision{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM verification_runs WHERE workflow_id=?`, req.WorkflowID); err != nil {
		return VerificationRevision{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflows SET state=?,state_version=state_version+1,resume_target='' WHERE id=? AND state=? AND state_version=?`, StateVerifying, req.WorkflowID, state, version)
	if err != nil {
		return VerificationRevision{}, false, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return VerificationRevision{}, false, fmt.Errorf("%w: concurrent retry verification", ErrIllegalTransition)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO transition_events(workflow_id,from_state,to_state,state_version,idempotency_key,artifact_ids,occurred_at) VALUES(?,?,?,?,?,?,?)`, req.WorkflowID, state, StateVerifying, version+1, "retry-verification:"+req.IdempotencyKey, fmt.Sprintf(`["verification-revision:%d","candidate-tree:%s"]`, revision, req.CandidateTree), stamp); err != nil {
		return VerificationRevision{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return VerificationRevision{}, false, err
	}
	return VerificationRevision{WorkflowID: req.WorkflowID, Revision: revision, CandidateTree: req.CandidateTree, CheckID: req.CheckID, PreviousCommand: req.PreviousCommand, ReplacementCommand: req.ReplacementCommand, Actor: req.Actor, Reason: req.Reason, IdempotencyKey: req.IdempotencyKey, AttemptID: req.AttemptID, FencingToken: req.FencingToken, CreatedAt: stamp}, false, nil
}
