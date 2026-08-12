package approval

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var ErrApprovalRequired = errors.New("approval: exact current approval required")

type Artifact struct {
	ID, Actor, AuthSource, WorkflowID, Action, ActionDigest string
	BasisGraphOrCandidate, PolicyHash, Rationale            string
	IssuedAt, ExpiresAt                                     time.Time
}
type PrototypeValidation struct {
	ID, WorkflowID, PrototypeID, Validator, Outcome, EvidenceDigest string
	ValidatedAt                                                     time.Time
}

func actionDigest(workflowID, action, basis, policy string) string {
	s := sha256.Sum256([]byte(workflowID + "\x00" + action + "\x00" + basis + "\x00" + policy))
	return hex.EncodeToString(s[:])
}

func Issue(db *sql.DB, artifact Artifact) (Artifact, error) {
	if artifact.WorkflowID == "" || artifact.Action == "" || artifact.Actor == "" || artifact.AuthSource == "" || artifact.Rationale == "" || artifact.BasisGraphOrCandidate == "" || artifact.PolicyHash == "" {
		return Artifact{}, errors.New("approval: incomplete artifact")
	}
	artifact.ActionDigest = actionDigest(artifact.WorkflowID, artifact.Action, artifact.BasisGraphOrCandidate, artifact.PolicyHash)
	if artifact.IssuedAt.IsZero() {
		artifact.IssuedAt = time.Now().UTC()
	}
	if !artifact.ExpiresAt.After(artifact.IssuedAt) {
		return Artifact{}, errors.New("approval: expiry must be after issue")
	}
	artifact.ID = "approval_" + artifact.ActionDigest
	raw, _ := json.Marshal(artifact)
	tx, err := db.Begin()
	if err != nil {
		return Artifact{}, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO approvals(id,workflow_id,action,digest,artifact) VALUES(?,?,?,?,?)`, artifact.ID, artifact.WorkflowID, artifact.Action, artifact.ActionDigest, raw); err != nil {
		return Artifact{}, err
	}
	var existing []byte
	if err = tx.QueryRow(`SELECT artifact FROM approvals WHERE id=?`, artifact.ID).Scan(&existing); err != nil {
		return Artifact{}, err
	}
	var persisted Artifact
	if json.Unmarshal(existing, &persisted) != nil || persisted.ActionDigest != artifact.ActionDigest {
		return Artifact{}, errors.New("approval: immutable conflict")
	}
	if _, err = tx.Exec(`INSERT INTO current_approvals(workflow_id,action,approval_id) VALUES(?,?,?) ON CONFLICT(workflow_id,action) DO UPDATE SET approval_id=excluded.approval_id`, artifact.WorkflowID, artifact.Action, artifact.ID); err != nil {
		return Artifact{}, err
	}
	if err = tx.Commit(); err != nil {
		return Artifact{}, err
	}
	return persisted, nil
}

func Require(db *sql.DB, workflowID, action, basis, policy string, now time.Time) (Artifact, error) {
	var raw []byte
	err := db.QueryRow(`SELECT a.artifact FROM current_approvals c JOIN approvals a ON a.id=c.approval_id WHERE c.workflow_id=? AND c.action=?`, workflowID, action).Scan(&raw)
	if err != nil {
		return Artifact{}, ErrApprovalRequired
	}
	var a Artifact
	if json.Unmarshal(raw, &a) != nil || a.ActionDigest != actionDigest(workflowID, action, basis, policy) || !a.ExpiresAt.After(now) {
		return Artifact{}, ErrApprovalRequired
	}
	return a, nil
}

func Revoke(db *sql.DB, workflowID, action, actor, reason string, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err = tx.QueryRow(`SELECT approval_id FROM current_approvals WHERE workflow_id=? AND action=?`, workflowID, action).Scan(&id); err != nil {
		return ErrApprovalRequired
	}
	if _, err = tx.Exec(`DELETE FROM current_approvals WHERE workflow_id=? AND action=?`, workflowID, action); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO approval_revocations(workflow_id,action,approval_id,actor,reason,occurred_at) VALUES(?,?,?,?,?,?)`, workflowID, action, id, actor, reason, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func RevokeAll(db *sql.DB, workflowID, actor, reason string, now time.Time) error {
	rows, err := db.Query(`SELECT action FROM current_approvals WHERE workflow_id=?`, workflowID)
	if err != nil {
		return err
	}
	var actions []string
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	for _, a := range actions {
		if err = Revoke(db, workflowID, a, actor, reason, now); err != nil {
			return err
		}
	}
	return nil
}
