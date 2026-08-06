package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type ReplanRequest struct {
	WorkflowID, InvalidationID, Actor, Reason, IdempotencyKey string
	Graph, Contract, RunInput                                 []byte
	Now                                                       time.Time
}

type ReplanRevision struct {
	WorkflowID     string    `json:"workflow_id"`
	InvalidationID string    `json:"invalidation_id"`
	Actor          string    `json:"actor"`
	Reason         string    `json:"reason"`
	IdempotencyKey string    `json:"idempotency_key"`
	Version        uint64    `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
}

// Replan atomically replaces the current executable contract while preserving
// every historical graph, contract, finding, attempt, and receipt record.
func (s *SQLiteStore) Replan(req ReplanRequest) (Workflow, ReplanRevision, error) {
	if req.WorkflowID == "" || req.InvalidationID == "" || req.Actor == "" || req.Reason == "" || req.IdempotencyKey == "" || len(req.Graph) == 0 || len(req.Contract) == 0 || len(req.RunInput) == 0 {
		return Workflow{}, ReplanRevision{}, errors.New("workflow: incomplete replan request")
	}
	if req.Now.IsZero() {
		req.Now = time.Now()
	}
	requestRaw, _ := json.Marshal(map[string]string{
		"invalidation_id": req.InvalidationID, "actor": req.Actor, "reason": req.Reason,
		"graph_digest": digestReplan(req.Graph), "contract_digest": digestReplan(req.Contract), "run_input_digest": digestReplan(req.RunInput),
	})
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var version uint64
	var existingRequest, existingResult []byte
	err = conn.QueryRowContext(ctx, `SELECT version,request,result FROM replan_revisions WHERE workflow_id=? AND idempotency_key=?`, req.WorkflowID, req.IdempotencyKey).Scan(&version, &existingRequest, &existingResult)
	if err == nil {
		if string(existingRequest) != string(requestRaw) {
			return Workflow{}, ReplanRevision{}, errors.New("workflow: replan idempotency key conflicts with an existing request")
		}
		var result Workflow
		if json.Unmarshal(existingResult, &result) != nil {
			return Workflow{}, ReplanRevision{}, errors.New("workflow: corrupt replan result")
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return Workflow{}, ReplanRevision{}, err
		}
		committed = true
		return result, ReplanRevision{WorkflowID: req.WorkflowID, Version: version, InvalidationID: req.InvalidationID, Actor: req.Actor, Reason: req.Reason, IdempotencyKey: req.IdempotencyKey}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ReplanRevision{}, err
	}

	w, err := scanWorkflow(conn.QueryRowContext(ctx, `SELECT id,state,state_version,route,minimum_risk,basis_tree,resume_target FROM workflows WHERE id=?`, req.WorkflowID))
	if err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	if w.State != StateReplanRequired {
		return Workflow{}, ReplanRevision{}, fmt.Errorf("workflow %s is %s; replan requires replan_required", w.ID, w.State)
	}
	var live int
	if err = conn.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM mutation_attempts WHERE workflow_id=? AND live=1) + (SELECT COUNT(*) FROM workflow_jobs WHERE workflow_id=? AND state IN ('queued','running','cancel_requested'))`, w.ID, w.ID).Scan(&live); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	if live != 0 {
		return Workflow{}, ReplanRevision{}, errors.New("workflow: cannot replan while writers are live")
	}
	valid, err := attachedInvalidation(ctx, conn, w.ID, req.InvalidationID)
	if err != nil || !valid {
		if err != nil {
			return Workflow{}, ReplanRevision{}, err
		}
		return Workflow{}, ReplanRevision{}, errors.New("workflow: invalidation ID is not attached to the workflow")
	}

	var previousVersion uint64
	var previousContract []byte
	if err = conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM execution_graphs WHERE workflow_id=?`, w.ID).Scan(&previousVersion); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	if previousVersion == 0 {
		return Workflow{}, ReplanRevision{}, errors.New("workflow: replan requires an existing execution graph")
	}
	if err = conn.QueryRowContext(ctx, `SELECT contract FROM execution_contracts WHERE workflow_id=?`, w.ID).Scan(&previousContract); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	version = previousVersion + 1
	stamp := req.Now.UTC().Format(time.RFC3339Nano)
	parentOfPrevious := uint64(0)
	if previousVersion > 1 {
		parentOfPrevious = previousVersion - 1
	}
	if _, err = conn.ExecContext(ctx, `INSERT OR IGNORE INTO execution_contract_revisions(workflow_id,version,contract,parent_version,invalidation_id,created_at) VALUES(?,?,?,?,?,?)`, w.ID, previousVersion, previousContract, parentOfPrevious, "initial", stamp); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO execution_contract_revisions(workflow_id,version,contract,parent_version,invalidation_id,created_at) VALUES(?,?,?,?,?,?)`, w.ID, version, req.Contract, previousVersion, req.InvalidationID, stamp); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO execution_graphs(workflow_id,version,graph) VALUES(?,?,?)`, w.ID, version, req.Graph); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE execution_contracts SET contract=? WHERE workflow_id=?`, req.Contract, w.ID); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	if res, updateErr := conn.ExecContext(ctx, `UPDATE workflow_run_inputs SET input=? WHERE workflow_id=?`, req.RunInput, w.ID); updateErr != nil {
		return Workflow{}, ReplanRevision{}, updateErr
	} else if n, _ := res.RowsAffected(); n != 1 {
		return Workflow{}, ReplanRevision{}, errors.New("workflow: missing persisted run input")
	}
	if _, err = conn.ExecContext(ctx, `DELETE FROM execution_slice_state WHERE workflow_id=?`, w.ID); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	var verificationTree string
	var verificationResult []byte
	if verifyErr := conn.QueryRowContext(ctx, `SELECT candidate_tree,result FROM verification_runs WHERE workflow_id=?`, w.ID).Scan(&verificationTree, &verificationResult); verifyErr == nil {
		var historyRevision uint64
		if err = conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM verification_run_history WHERE workflow_id=?`, w.ID).Scan(&historyRevision); err != nil {
			return Workflow{}, ReplanRevision{}, err
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO verification_run_history(workflow_id,revision,candidate_tree,result,archived_at) VALUES(?,?,?,?,?)`, w.ID, historyRevision, verificationTree, verificationResult, stamp); err != nil {
			return Workflow{}, ReplanRevision{}, err
		}
		if _, err = conn.ExecContext(ctx, `DELETE FROM verification_runs WHERE workflow_id=?`, w.ID); err != nil {
			return Workflow{}, ReplanRevision{}, err
		}
	} else if !errors.Is(verifyErr, sql.ErrNoRows) {
		return Workflow{}, ReplanRevision{}, verifyErr
	}
	if err = revokeForReplan(ctx, conn, w.ID, req.Actor, req.Reason, stamp); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}

	next := w
	next.State, next.StateVersion, next.Route, next.ResumeTarget = StateReady, w.StateVersion+1, RoutePlanned, ""
	if next.MinimumRisk == RiskLow {
		next.MinimumRisk = RiskMedium
	}
	res, err := conn.ExecContext(ctx, `UPDATE workflows SET state=?,state_version=?,route=?,minimum_risk=?,resume_target='' WHERE id=? AND state=? AND state_version=?`, next.State, next.StateVersion, next.Route, next.MinimumRisk, next.ID, StateReplanRequired, w.StateVersion)
	if err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Workflow{}, ReplanRevision{}, ErrCASConflict
	}
	artifacts, _ := json.Marshal([]string{req.InvalidationID, fmt.Sprintf("contract:v%d", version), fmt.Sprintf("execution:%d", version)})
	if _, err = conn.ExecContext(ctx, `INSERT INTO transition_events(workflow_id,from_state,to_state,state_version,idempotency_key,artifact_ids,occurred_at) VALUES(?,?,?,?,?,?,?)`, w.ID, StateReplanRequired, StateReady, next.StateVersion, "replan:"+req.IdempotencyKey, artifacts, stamp); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	resultRaw, _ := json.Marshal(next)
	if _, err = conn.ExecContext(ctx, `INSERT INTO replan_revisions(workflow_id,version,invalidation_id,actor,reason,idempotency_key,request,result,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, w.ID, version, req.InvalidationID, req.Actor, req.Reason, req.IdempotencyKey, requestRaw, resultRaw, stamp); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Workflow{}, ReplanRevision{}, err
	}
	committed = true
	return next, ReplanRevision{WorkflowID: w.ID, Version: version, InvalidationID: req.InvalidationID, Actor: req.Actor, Reason: req.Reason, IdempotencyKey: req.IdempotencyKey, CreatedAt: req.Now.UTC()}, nil
}

func attachedInvalidation(ctx context.Context, conn *sql.Conn, workflowID, id string) (bool, error) {
	var count int
	err := conn.QueryRowContext(ctx, `SELECT COUNT(*)
FROM transition_events, json_each(transition_events.artifact_ids)
WHERE workflow_id=? AND to_state='replan_required' AND json_each.value=?
AND sequence=(SELECT MAX(sequence) FROM transition_events WHERE workflow_id=? AND to_state='replan_required')`, workflowID, id, workflowID).Scan(&count)
	return count > 0, err
}

func revokeForReplan(ctx context.Context, conn *sql.Conn, workflowID, actor, reason, stamp string) error {
	rows, err := conn.QueryContext(ctx, `SELECT action,approval_id FROM current_approvals WHERE workflow_id=?`, workflowID)
	if err != nil {
		return err
	}
	type item struct{ action, id string }
	var approvals []item
	for rows.Next() {
		var current item
		if err = rows.Scan(&current.action, &current.id); err != nil {
			rows.Close()
			return err
		}
		approvals = append(approvals, current)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, current := range approvals {
		if _, err = conn.ExecContext(ctx, `INSERT INTO approval_revocations(workflow_id,action,approval_id,actor,reason,occurred_at) VALUES(?,?,?,?,?,?)`, workflowID, current.action, current.id, actor, "replan: "+reason, stamp); err != nil {
			return err
		}
	}
	if _, err = conn.ExecContext(ctx, `DELETE FROM current_approvals WHERE workflow_id=?`, workflowID); err != nil {
		return err
	}
	var candidateID string
	err = conn.QueryRowContext(ctx, `SELECT r.candidate_record_id FROM receipt_authority a JOIN receipts r ON r.id=a.receipt_id WHERE a.workflow_id=?`, workflowID).Scan(&candidateID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `DELETE FROM receipt_authority WHERE workflow_id=?`, workflowID); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO receipt_invalidations(workflow_id,candidate_record_id,reason,occurred_at) VALUES(?,?,?,?)`, workflowID, candidateID, "replan: "+reason, stamp)
	return err
}

func digestReplan(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func (s *SQLiteStore) ReplanRevisions(workflowID string) ([]ReplanRevision, error) {
	rows, err := s.db.Query(`SELECT version,invalidation_id,actor,reason,idempotency_key,created_at FROM replan_revisions WHERE workflow_id=? ORDER BY version`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []ReplanRevision
	for rows.Next() {
		item := ReplanRevision{WorkflowID: workflowID}
		var created string
		if err = rows.Scan(&item.Version, &item.InvalidationID, &item.Actor, &item.Reason, &item.IdempotencyKey, &created); err != nil {
			return nil, err
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		revisions = append(revisions, item)
	}
	return revisions, rows.Err()
}
