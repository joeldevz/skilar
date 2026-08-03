package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db   *sql.DB
	path string
	now  func() time.Time
}

func OpenSQLite(path string) (*SQLiteStore, error) {
	if err := prepareDatabasePath(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &SQLiteStore{db: db, path: path, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.tightenFiles(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func OpenRepositorySQLite(repoDir string) (*SQLiteStore, error) {
	path, err := CanonicalDatabasePath(repoDir)
	if err != nil {
		return nil, err
	}
	return OpenSQLite(path)
}

func (s *SQLiteStore) Close() error      { return s.db.Close() }
func (s *SQLiteStore) Path() string      { return s.path }
func (s *SQLiteStore) Database() *sql.DB { return s.db }

func (s *SQLiteStore) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > 7 {
		return fmt.Errorf("workflow database schema %d is newer than supported schema 7", version)
	}
	if version == 7 {
		return nil
	}
	if version == 0 {
		const schemaV1 = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS workflows (
 id TEXT PRIMARY KEY, state TEXT NOT NULL, state_version INTEGER NOT NULL,
 route TEXT NOT NULL, minimum_risk TEXT NOT NULL, basis_tree TEXT NOT NULL, resume_target TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS transition_events (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT, workflow_id TEXT NOT NULL REFERENCES workflows(id),
 from_state TEXT NOT NULL, to_state TEXT NOT NULL, state_version INTEGER NOT NULL,
 idempotency_key TEXT NOT NULL, artifact_ids BLOB NOT NULL, occurred_at TEXT NOT NULL,
 UNIQUE(workflow_id, idempotency_key)
);
CREATE TABLE IF NOT EXISTS idempotency (
 workflow_id TEXT NOT NULL, key TEXT NOT NULL, request BLOB NOT NULL, result BLOB NOT NULL,
 PRIMARY KEY(workflow_id, key)
);
CREATE TABLE IF NOT EXISTS attempts (
 id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL REFERENCES workflows(id), node_id TEXT NOT NULL,
 basis_tree TEXT NOT NULL, live INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS results (
 attempt_id TEXT PRIMARY KEY REFERENCES attempts(id), envelope BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS leases (
 resource TEXT PRIMARY KEY, owner TEXT NOT NULL, fencing_token TEXT NOT NULL, expires_at INTEGER NOT NULL
);
PRAGMA user_version=1;`
		if _, err := s.db.Exec(schemaV1); err != nil {
			return err
		}
	}
	if version < 2 {
		const schemaV2 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS review_candidates (id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, tree_oid TEXT NOT NULL, policy_hash TEXT NOT NULL, record BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS semantic_assessments (candidate_record_id TEXT PRIMARY KEY REFERENCES review_candidates(id), assessment BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS review_evidence (id TEXT PRIMARY KEY, candidate_record_id TEXT NOT NULL REFERENCES review_candidates(id), evidence BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS receipts (id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, candidate_record_id TEXT NOT NULL UNIQUE REFERENCES review_candidates(id), receipt BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS receipt_authority (workflow_id TEXT PRIMARY KEY, receipt_id TEXT NOT NULL REFERENCES receipts(id));
CREATE TABLE IF NOT EXISTS receipt_invalidations (sequence INTEGER PRIMARY KEY AUTOINCREMENT, workflow_id TEXT NOT NULL, candidate_record_id TEXT NOT NULL, reason TEXT NOT NULL, occurred_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS delivery_intents (workflow_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, intent BLOB NOT NULL, PRIMARY KEY(workflow_id,idempotency_key));
PRAGMA user_version=2;
COMMIT;`
		if _, err := s.db.Exec(schemaV2); err != nil {
			return err
		}
	}
	if version < 3 {
		const schemaV3 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS recovery_bases (workflow_id TEXT PRIMARY KEY REFERENCES workflows(id), basis BLOB NOT NULL);
PRAGMA user_version=3;
COMMIT;`
		if _, err := s.db.Exec(schemaV3); err != nil {
			return err
		}
	}
	if version < 4 {
		const schemaV4 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS orchestration_routes (workflow_id TEXT PRIMARY KEY REFERENCES workflows(id), decision BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS wayfinder_graphs (workflow_id TEXT NOT NULL REFERENCES workflows(id), version INTEGER NOT NULL, graph BLOB NOT NULL, PRIMARY KEY(workflow_id,version));
CREATE TABLE IF NOT EXISTS execution_contracts (workflow_id TEXT PRIMARY KEY REFERENCES workflows(id), contract BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS execution_graphs (workflow_id TEXT NOT NULL REFERENCES workflows(id), version INTEGER NOT NULL, graph BLOB NOT NULL, PRIMARY KEY(workflow_id,version));
PRAGMA user_version=4;
COMMIT;`
		if _, err := s.db.Exec(schemaV4); err != nil {
			return err
		}
	}
	if version < 5 {
		const schemaV5 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS execution_slice_state (workflow_id TEXT NOT NULL, slice_id TEXT NOT NULL, status TEXT NOT NULL, PRIMARY KEY(workflow_id,slice_id));
CREATE UNIQUE INDEX IF NOT EXISTS one_active_mutation ON execution_slice_state(workflow_id) WHERE status='active';
CREATE TABLE IF NOT EXISTS mutation_attempts (attempt_id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, slice_id TEXT NOT NULL, worktree_id TEXT NOT NULL, owner TEXT NOT NULL, fencing_token TEXT NOT NULL, basis_tree TEXT NOT NULL, allowed_paths BLOB NOT NULL, operation_id TEXT NOT NULL UNIQUE, live INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS mutation_operations (operation_id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, attempt_id TEXT NOT NULL, pre_tree TEXT NOT NULL, post_tree TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, evidence BLOB NOT NULL DEFAULT '[]');
CREATE TABLE IF NOT EXISTS stale_result_audit (sequence INTEGER PRIMARY KEY AUTOINCREMENT, attempt_id TEXT NOT NULL, reason TEXT NOT NULL, envelope BLOB NOT NULL, occurred_at TEXT NOT NULL);
PRAGMA user_version=5;
COMMIT;`
		if _, err := s.db.Exec(schemaV5); err != nil {
			return err
		}
	}
	if version < 6 {
		const schemaV6 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS verification_evidence (id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, candidate_tree TEXT NOT NULL, kind TEXT NOT NULL, evidence BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS verification_runs (workflow_id TEXT PRIMARY KEY, candidate_tree TEXT NOT NULL, result BLOB NOT NULL);
PRAGMA user_version=6;
COMMIT;`
		if _, err := s.db.Exec(schemaV6); err != nil {
			return err
		}
	}
	const schemaV7 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS opencode_invocations (invocation_id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, attempt_id TEXT NOT NULL, model TEXT NOT NULL, command BLOB NOT NULL, exit_code INTEGER NOT NULL, stdout_digest TEXT NOT NULL, stderr_digest TEXT NOT NULL, evidence_ids BLOB NOT NULL, status TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT NOT NULL);
PRAGMA user_version=7;
COMMIT;`
	_, err := s.db.Exec(schemaV7)
	return err
}

func (s *SQLiteStore) tightenFiles() error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := s.path + suffix
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || fileHasMultipleLinks(info) {
				return fmt.Errorf("unsafe sqlite file %q", path)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) Create(w Workflow) (Workflow, error) {
	if w.ID == "" {
		return Workflow{}, fmt.Errorf("workflow: empty id")
	}
	if w.State == "" {
		w.State = StateCreated
	}
	if w.State != StateCreated || w.StateVersion != 0 {
		return Workflow{}, fmt.Errorf("workflow: create requires created at version zero")
	}
	_, err := s.db.Exec(`INSERT INTO workflows(id,state,state_version,route,minimum_risk,basis_tree,resume_target) VALUES(?,?,?,?,?,?,?)`, w.ID, w.State, w.StateVersion, w.Route, w.MinimumRisk, w.BasisTree, w.ResumeTarget)
	if err != nil {
		if isConstraint(err) {
			return Workflow{}, ErrAlreadyExists
		}
		return Workflow{}, err
	}
	return w, nil
}

func scanWorkflow(row interface{ Scan(...any) error }) (Workflow, error) {
	var w Workflow
	err := row.Scan(&w.ID, &w.State, &w.StateVersion, &w.Route, &w.MinimumRisk, &w.BasisTree, &w.ResumeTarget)
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	return w, err
}

func (s *SQLiteStore) Get(id string) (Workflow, error) {
	return scanWorkflow(s.db.QueryRow(`SELECT id,state,state_version,route,minimum_risk,basis_tree,resume_target FROM workflows WHERE id=?`, id))
}

func (s *SQLiteStore) Transition(req Transition) (Workflow, error) {
	if req.IdempotencyKey == "" {
		return Workflow{}, fmt.Errorf("workflow: empty idempotency key")
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Workflow{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Workflow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var oldReq, oldResult []byte
	err = conn.QueryRowContext(ctx, `SELECT request,result FROM idempotency WHERE workflow_id=? AND key=?`, req.WorkflowID, req.IdempotencyKey).Scan(&oldReq, &oldResult)
	if err == nil {
		var previousReq Transition
		var result Workflow
		if json.Unmarshal(oldReq, &previousReq) != nil || json.Unmarshal(oldResult, &result) != nil {
			return Workflow{}, fmt.Errorf("workflow: corrupt idempotency record")
		}
		if !sameTransition(previousReq, req) {
			return Workflow{}, ErrIdempotencyReuse
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return Workflow{}, err
		}
		committed = true
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, err
	}
	w, err := scanWorkflow(conn.QueryRowContext(ctx, `SELECT id,state,state_version,route,minimum_risk,basis_tree,resume_target FROM workflows WHERE id=?`, req.WorkflowID))
	if err != nil {
		return Workflow{}, err
	}
	if w.State != req.ExpectedState || w.StateVersion != req.ExpectedVersion {
		return Workflow{}, ErrCASConflict
	}
	if !CanTransition(w.State, req.NextState, w.ResumeTarget) {
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
	res, err := conn.ExecContext(ctx, `UPDATE workflows SET state=?,state_version=?,resume_target=? WHERE id=? AND state=? AND state_version=?`, w.State, w.StateVersion, w.ResumeTarget, w.ID, req.ExpectedState, req.ExpectedVersion)
	if err != nil {
		return Workflow{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Workflow{}, ErrCASConflict
	}
	artifacts, _ := json.Marshal(req.ArtifactIDs)
	occurred := s.now().UTC().Format(time.RFC3339Nano)
	if _, err = conn.ExecContext(ctx, `INSERT INTO transition_events(workflow_id,from_state,to_state,state_version,idempotency_key,artifact_ids,occurred_at) VALUES(?,?,?,?,?,?,?)`, w.ID, from, w.State, w.StateVersion, req.IdempotencyKey, artifacts, occurred); err != nil {
		return Workflow{}, err
	}
	reqJSON, _ := json.Marshal(cloneTransition(req))
	resultJSON, _ := json.Marshal(w)
	if _, err = conn.ExecContext(ctx, `INSERT INTO idempotency(workflow_id,key,request,result) VALUES(?,?,?,?)`, w.ID, req.IdempotencyKey, reqJSON, resultJSON); err != nil {
		return Workflow{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Workflow{}, err
	}
	committed = true
	_ = s.tightenFiles()
	return w, nil
}

func (s *SQLiteStore) Events(id string) ([]Event, error) {
	if _, err := s.Get(id); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT sequence,workflow_id,from_state,to_state,state_version,idempotency_key,artifact_ids,occurred_at FROM transition_events WHERE workflow_id=? ORDER BY sequence`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		var artifacts []byte
		var occurred string
		if err := rows.Scan(&e.Sequence, &e.WorkflowID, &e.From, &e.To, &e.StateVersion, &e.IdempotencyKey, &artifacts, &occurred); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(artifacts, &e.ArtifactIDs); err != nil {
			return nil, err
		}
		e.OccurredAt, err = time.Parse(time.RFC3339Nano, occurred)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *SQLiteStore) RegisterAttempt(a Attempt) error {
	w, err := s.Get(a.WorkflowID)
	if err != nil {
		return err
	}
	if a.ID == "" || a.NodeID == "" || a.BasisTree == "" || a.BasisTree != w.BasisTree {
		return ErrStaleResult
	}
	a.Live = true
	_, err = s.db.Exec(`INSERT INTO attempts(id,workflow_id,node_id,basis_tree,live) VALUES(?,?,?,?,1)`, a.ID, a.WorkflowID, a.NodeID, a.BasisTree)
	if isConstraint(err) {
		return ErrAlreadyExists
	}
	return err
}
func (s *SQLiteStore) SupersedeAttempt(id string) error {
	r, err := s.db.Exec(`UPDATE attempts SET live=0 WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) AcceptResult(result ResultEnvelope) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var a Attempt
	var live int
	err = tx.QueryRow(`SELECT id,workflow_id,node_id,basis_tree,live FROM attempts WHERE id=?`, result.AttemptID).Scan(&a.ID, &a.WorkflowID, &a.NodeID, &a.BasisTree, &live)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleResult
	}
	if err != nil {
		return err
	}
	var basis string
	if err = tx.QueryRow(`SELECT basis_tree FROM workflows WHERE id=?`, result.WorkflowID).Scan(&basis); errors.Is(err, sql.ErrNoRows) {
		return ErrStaleResult
	}
	if err != nil {
		return err
	}
	if live != 1 || a.WorkflowID != result.WorkflowID || a.NodeID != result.NodeID || a.BasisTree != result.BaseCandidateOID || basis != result.BaseCandidateOID {
		return ErrStaleResult
	}
	payload, _ := json.Marshal(cloneResult(result))
	r, err := tx.Exec(`UPDATE attempts SET live=0 WHERE id=? AND live=1`, a.ID)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return ErrStaleResult
	}
	if _, err = tx.Exec(`INSERT INTO results(attempt_id,envelope) VALUES(?,?)`, a.ID, payload); err != nil {
		if isConstraint(err) {
			return ErrStaleResult
		}
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) AcquireLease(resource, owner, token string, now, expiresAt time.Time) (Lease, error) {
	if resource == "" || owner == "" || token == "" || !expiresAt.After(now) {
		return Lease{}, ErrLeaseConflict
	}
	r, err := s.db.Exec(`INSERT INTO leases(resource,owner,fencing_token,expires_at) VALUES(?,?,?,?) ON CONFLICT(resource) DO UPDATE SET owner=excluded.owner,fencing_token=excluded.fencing_token,expires_at=excluded.expires_at WHERE leases.expires_at<=?`, resource, owner, token, expiresAt.UnixNano(), now.UnixNano())
	if err != nil {
		return Lease{}, err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return Lease{}, ErrLeaseConflict
	}
	return Lease{resource, owner, token, expiresAt.UTC()}, nil
}
func (s *SQLiteStore) HeartbeatLease(resource, owner, token string, now, expiresAt time.Time) (Lease, error) {
	if !expiresAt.After(now) {
		return Lease{}, ErrLeaseConflict
	}
	r, err := s.db.Exec(`UPDATE leases SET expires_at=? WHERE resource=? AND owner=? AND fencing_token=? AND expires_at>?`, expiresAt.UnixNano(), resource, owner, token, now.UnixNano())
	if err != nil {
		return Lease{}, err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return Lease{}, ErrLeaseConflict
	}
	return Lease{resource, owner, token, expiresAt.UTC()}, nil
}

func (s *SQLiteStore) ValidateLease(resource, owner, token string, now time.Time) error {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM leases WHERE resource=? AND owner=? AND fencing_token=? AND expires_at>?`, resource, owner, token, now.UnixNano()).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrLeaseConflict
	}
	return nil
}

func isConstraint(err error) bool {
	return err != nil && (contains(err.Error(), "constraint failed") || contains(err.Error(), "UNIQUE constraint"))
}
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
