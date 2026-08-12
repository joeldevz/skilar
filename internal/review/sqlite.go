package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type SQLiteStore struct{ db *sql.DB }

func NewSQLiteStore(db *sql.DB) *SQLiteStore { return &SQLiteStore{db: db} }

func (s *SQLiteStore) Issue(req IssueRequest) (Receipt, error) {
	validated, err := NewMemoryStore().Issue(req)
	if err != nil {
		return Receipt{}, err
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback()
	candidateJSON, _ := json.Marshal(req.Candidate)
	assessmentJSON, _ := json.Marshal(req.Assessment)
	if _, err = tx.Exec(`INSERT OR IGNORE INTO review_candidates(id,workflow_id,tree_oid,policy_hash,record) VALUES(?,?,?,?,?)`, req.Candidate.ID, req.Candidate.WorkflowID, req.Candidate.TreeOID, req.Candidate.PolicyHash, candidateJSON); err != nil {
		return Receipt{}, err
	}
	var existingCandidate []byte
	if err = tx.QueryRow(`SELECT record FROM review_candidates WHERE id=?`, req.Candidate.ID).Scan(&existingCandidate); err != nil {
		return Receipt{}, err
	}
	var persisted CandidateRecord
	if json.Unmarshal(existingCandidate, &persisted) != nil || ValidateCandidateRecord(persisted) != nil {
		return Receipt{}, ErrCandidateMismatch
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO semantic_assessments(candidate_record_id,assessment) VALUES(?,?)`, req.Candidate.ID, assessmentJSON); err != nil {
		return Receipt{}, err
	}
	for _, e := range req.Evidence {
		raw, _ := json.Marshal(e)
		if _, err = tx.Exec(`INSERT OR IGNORE INTO review_evidence(id,candidate_record_id,evidence) VALUES(?,?,?)`, e.ID, req.Candidate.ID, raw); err != nil {
			return Receipt{}, err
		}
		var persistedCandidate string
		var persistedRaw []byte
		if err = tx.QueryRow(`SELECT candidate_record_id,evidence FROM review_evidence WHERE id=?`, e.ID).Scan(&persistedCandidate, &persistedRaw); err != nil {
			return Receipt{}, err
		}
		if persistedCandidate != req.Candidate.ID || string(persistedRaw) != string(raw) {
			return Receipt{}, ErrEvidenceMismatch
		}
	}
	var receiptJSON []byte
	queryErr := tx.QueryRow(`SELECT receipt FROM receipts WHERE candidate_record_id=?`, req.Candidate.ID).Scan(&receiptJSON)
	if queryErr == nil {
		var existing Receipt
		if json.Unmarshal(receiptJSON, &existing) != nil {
			return Receipt{}, ErrReceiptExists
		}
		if existing.ID != validated.ID {
			return Receipt{}, ErrReceiptExists
		}
		if err = tx.Commit(); err != nil {
			return Receipt{}, err
		}
		return cloneReceipt(existing), nil
	}
	if !errors.Is(queryErr, sql.ErrNoRows) {
		return Receipt{}, queryErr
	}
	receiptJSON, _ = json.Marshal(validated)
	if _, err = tx.Exec(`INSERT INTO receipts(id,workflow_id,candidate_record_id,receipt) VALUES(?,?,?,?)`, validated.ID, validated.WorkflowID, validated.CandidateRecordID, receiptJSON); err != nil {
		return Receipt{}, mapReceiptConstraint(err)
	}
	if _, err = tx.Exec(`INSERT INTO receipt_authority(workflow_id,receipt_id) VALUES(?,?) ON CONFLICT(workflow_id) DO UPDATE SET receipt_id=excluded.receipt_id`, validated.WorkflowID, validated.ID); err != nil {
		return Receipt{}, err
	}
	if err = tx.Commit(); err != nil {
		return Receipt{}, err
	}
	return cloneReceipt(validated), nil
}

func (s *SQLiteStore) Receipt(id string) (Receipt, error) {
	var raw []byte
	err := s.db.QueryRow(`SELECT receipt FROM receipts WHERE id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, ErrNoAuthority
	}
	if err != nil {
		return Receipt{}, err
	}
	var r Receipt
	if err = json.Unmarshal(raw, &r); err != nil {
		return Receipt{}, err
	}
	return cloneReceipt(r), nil
}
func (s *SQLiteStore) Authority(workflowID string) (Receipt, error) {
	var raw []byte
	err := s.db.QueryRow(`SELECT r.receipt FROM receipt_authority a JOIN receipts r ON r.id=a.receipt_id WHERE a.workflow_id=?`, workflowID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, ErrNoAuthority
	}
	if err != nil {
		return Receipt{}, err
	}
	var r Receipt
	if err = json.Unmarshal(raw, &r); err != nil {
		return Receipt{}, err
	}
	return cloneReceipt(r), nil
}
func (s *SQLiteStore) Invalidate(i Invalidation) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM receipt_authority WHERE workflow_id=? AND receipt_id IN (SELECT id FROM receipts WHERE candidate_record_id=?)`, i.WorkflowID, i.CandidateRecordID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		var exists int
		if scanErr := tx.QueryRow(`SELECT COUNT(*) FROM receipt_authority WHERE workflow_id=?`, i.WorkflowID).Scan(&exists); scanErr != nil {
			return scanErr
		}
		if exists == 0 {
			return ErrNoAuthority
		}
		return ErrCandidateMismatch
	}
	if _, err = tx.Exec(`INSERT INTO receipt_invalidations(workflow_id,candidate_record_id,reason,occurred_at) VALUES(?,?,?,?)`, i.WorkflowID, i.CandidateRecordID, i.Reason, i.OccurredAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}
func mapReceiptConstraint(err error) error {
	if err == nil {
		return nil
	}
	return ErrReceiptExists
}
