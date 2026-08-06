package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// A concurrently installed older Skynex binary can still write a legacy
	// run input after the database has already reached schema 18. Repair that
	// mixed-version state on every writable open so frozen candidates do not
	// become permanently unreviewable merely because the one-time migration
	// has already run.
	if err := s.repairResultTransport(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.tightenFiles(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) repairResultTransport() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = backfillResultTransport(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func OpenRepositorySQLite(repoDir string) (*SQLiteStore, error) {
	path, err := CanonicalDatabasePath(repoDir)
	if err != nil {
		return nil, err
	}
	return OpenSQLite(path)
}

// OpenSQLiteReadOnly opens an existing, current workflow database without
// migrating it, changing permissions, or creating SQLite journal sidecars.
func OpenSQLiteReadOnly(path string) (*SQLiteStore, error) {
	if err := validateExistingDatabasePath(path); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return nil, fmt.Errorf("workflow database has active SQLite sidecar %q", path+suffix)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	// immutable=1 is safe after the sidecar check above and prevents SQLite's
	// read-only WAL implementation from creating -wal/-shm bookkeeping files.
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&immutable=1&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &SQLiteStore{db: db, path: path, now: time.Now}
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version != 18 {
		db.Close()
		return nil, fmt.Errorf("workflow database schema %d requires writable migration to schema 18", version)
	}
	return s, nil
}

func OpenRepositorySQLiteReadOnly(repoDir string) (*SQLiteStore, error) {
	path, err := CanonicalDatabasePath(repoDir)
	if err != nil {
		return nil, err
	}
	return OpenSQLiteReadOnly(path)
}

// OpenSQLiteLiveReadOnly permits an already-active WAL database so status can
// observe a detached worker. It never creates a database or migrates schema;
// both WAL sidecars must already exist and pass the same ownership/link/mode
// checks as the database. Other diagnostic commands retain immutable reads.
func OpenSQLiteLiveReadOnly(path string) (*SQLiteStore, error) {
	if err := validateExistingDatabasePath(path); err != nil {
		return nil, err
	}
	walExists, shmExists := false, false
	for _, item := range []struct {
		suffix string
		exists *bool
	}{{"-wal", &walExists}, {"-shm", &shmExists}} {
		info, err := os.Lstat(path + item.suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		*item.exists = true
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || fileHasMultipleLinks(info) || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("unsafe sqlite sidecar %q", path+item.suffix)
		}
	}
	if walExists != shmExists {
		return nil, errors.New("workflow database has incomplete SQLite WAL sidecars")
	}
	if !walExists {
		return OpenSQLiteReadOnly(path)
	}
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &SQLiteStore{db: db, path: path, now: time.Now}
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version != 18 {
		db.Close()
		return nil, fmt.Errorf("workflow database schema %d requires writable migration to schema 18", version)
	}
	return s, nil
}

func OpenRepositorySQLiteLiveReadOnly(repoDir string) (*SQLiteStore, error) {
	path, err := CanonicalDatabasePath(repoDir)
	if err != nil {
		return nil, err
	}
	return OpenSQLiteLiveReadOnly(path)
}

func validateExistingDatabasePath(path string) error {
	if err := rejectSymlinkComponents(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || fileHasMultipleLinks(info) {
		return fmt.Errorf("unsafe workflow database %q", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("workflow database permissions are too broad: %o", info.Mode().Perm())
	}
	return nil
}

func (s *SQLiteStore) Close() error      { return s.db.Close() }
func (s *SQLiteStore) Path() string      { return s.path }
func (s *SQLiteStore) Database() *sql.DB { return s.db }

func (s *SQLiteStore) migrate() error {
	var version int
	var err error
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > 18 {
		return fmt.Errorf("workflow database schema %d is newer than supported schema 18", version)
	}
	if version == 18 {
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
	if version < 7 {
		const schemaV7 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS opencode_invocations (invocation_id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, attempt_id TEXT NOT NULL, model TEXT NOT NULL, command BLOB NOT NULL, exit_code INTEGER NOT NULL, stdout_digest TEXT NOT NULL, stderr_digest TEXT NOT NULL, evidence_ids BLOB NOT NULL, status TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT NOT NULL);
PRAGMA user_version=7;
COMMIT;`
		if _, err := s.db.Exec(schemaV7); err != nil {
			return err
		}
	}
	if version < 8 {
		const schemaV8 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS workflow_run_inputs (workflow_id TEXT PRIMARY KEY REFERENCES workflows(id), input BLOB NOT NULL);
PRAGMA user_version=8;
COMMIT;`
		if _, err := s.db.Exec(schemaV8); err != nil {
			return err
		}
	}
	if version < 9 {
		const schemaV9 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS review_invocations (id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, candidate_tree TEXT NOT NULL, lens TEXT NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL, output_digest TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS review_findings (id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, candidate_tree TEXT NOT NULL, lens TEXT NOT NULL, finding BLOB NOT NULL);
PRAGMA user_version=9;
COMMIT;`
		if _, err := s.db.Exec(schemaV9); err != nil {
			return err
		}
	}
	if version < 10 {
		const schemaV10 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS approvals (id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, action TEXT NOT NULL, digest TEXT NOT NULL, artifact BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS current_approvals (workflow_id TEXT NOT NULL, action TEXT NOT NULL, approval_id TEXT NOT NULL REFERENCES approvals(id), PRIMARY KEY(workflow_id,action));
CREATE TABLE IF NOT EXISTS approval_revocations (sequence INTEGER PRIMARY KEY AUTOINCREMENT, workflow_id TEXT NOT NULL, action TEXT NOT NULL, approval_id TEXT NOT NULL, actor TEXT NOT NULL, reason TEXT NOT NULL, occurred_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS abort_cleanup_plans (workflow_id TEXT PRIMARY KEY, plan BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS prototype_validations (id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, artifact BLOB NOT NULL);
PRAGMA user_version=10;
COMMIT;`
		if _, err := s.db.Exec(schemaV10); err != nil {
			return err
		}
	}
	if version < 11 {
		const schemaV11 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS artifacts (id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, kind TEXT NOT NULL, digest TEXT NOT NULL, size INTEGER NOT NULL, path TEXT NOT NULL, authoritative INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, retain_until TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS artifact_refs (artifact_id TEXT NOT NULL REFERENCES artifacts(id), owner_kind TEXT NOT NULL, owner_id TEXT NOT NULL, PRIMARY KEY(artifact_id,owner_kind,owner_id));
COMMIT;`
		if _, err = s.db.Exec(schemaV11); err != nil {
			return err
		}
		for _, column := range []string{"stdout_artifact_id", "stderr_artifact_id"} {
			var count int
			if err = s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('opencode_invocations') WHERE name=?`, column).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				if _, err = s.db.Exec(`ALTER TABLE opencode_invocations ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT ''`); err != nil {
					return err
				}
			}
		}
		_, err = s.db.Exec(`PRAGMA user_version=11`)
		if err != nil {
			return err
		}
	}
	if version < 12 {
		const schemaV12 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS workflow_jobs (id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL REFERENCES workflows(id), session_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, pid INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, started_at TEXT NOT NULL DEFAULT '', heartbeat_at TEXT NOT NULL DEFAULT '', finished_at TEXT NOT NULL DEFAULT '', terminal_state TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX IF NOT EXISTS one_live_workflow_job ON workflow_jobs(workflow_id) WHERE state IN ('queued','running','cancel_requested');
CREATE TABLE IF NOT EXISTS workflow_notifications (id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL REFERENCES workflows(id), job_id TEXT NOT NULL UNIQUE REFERENCES workflow_jobs(id), terminal_state TEXT NOT NULL, created_at TEXT NOT NULL, claim_token TEXT NOT NULL DEFAULT '', claimed_by TEXT NOT NULL DEFAULT '', claimed_at TEXT NOT NULL DEFAULT '', acked_at TEXT NOT NULL DEFAULT '');
PRAGMA user_version=12;
COMMIT;`
		if _, err = s.db.Exec(schemaV12); err != nil {
			return err
		}
	}
	if version < 13 {
		const schemaV13 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS invocation_runtime (invocation_id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, attempt_id TEXT NOT NULL, status TEXT NOT NULL, pid INTEGER NOT NULL DEFAULT 0, started_at TEXT NOT NULL, heartbeat_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT '', stdout_preview TEXT NOT NULL DEFAULT '', stderr_preview TEXT NOT NULL DEFAULT '');
PRAGMA user_version=13;
COMMIT;`
		if _, err = s.db.Exec(schemaV13); err != nil {
			return err
		}
	}
	if version < 14 {
		const schemaV14 = `BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS workflow_session_presence (session_id TEXT PRIMARY KEY, last_seen TEXT NOT NULL);
PRAGMA user_version=14;
COMMIT;`
		if _, err = s.db.Exec(schemaV14); err != nil {
			return err
		}
	}
	if version < 15 {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_invocations') WHERE name='error_preview'`).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := s.db.Exec(`ALTER TABLE review_invocations ADD COLUMN error_preview TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		if _, err := s.db.Exec(`PRAGMA user_version=15`); err != nil {
			return err
		}
	}
	if version < 16 {
		for _, column := range []string{"pid INTEGER NOT NULL DEFAULT 0", "heartbeat_at TEXT NOT NULL DEFAULT ''", "last_activity_at TEXT NOT NULL DEFAULT ''", "result_json BLOB NOT NULL DEFAULT ''", "prompt_hash TEXT NOT NULL DEFAULT ''", "policy_hash TEXT NOT NULL DEFAULT ''"} {
			name := strings.Fields(column)[0]
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('review_invocations') WHERE name=?`, name).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				if _, err := s.db.Exec(`ALTER TABLE review_invocations ADD COLUMN ` + column); err != nil {
					return err
				}
			}
		}
		if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS review_checkpoints (id TEXT PRIMARY KEY, workflow_id TEXT NOT NULL, candidate_tree TEXT NOT NULL, policy_hash TEXT NOT NULL, lens TEXT NOT NULL, model TEXT NOT NULL, prompt_hash TEXT NOT NULL, result_json BLOB NOT NULL, created_at TEXT NOT NULL)`); err != nil {
			return err
		}
		if _, err := s.db.Exec(`PRAGMA user_version=16`); err != nil {
			return err
		}
	}
	if version < 17 {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('workflow_jobs') WHERE name='operation'`).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := s.db.Exec(`ALTER TABLE workflow_jobs ADD COLUMN operation TEXT NOT NULL DEFAULT 'run'`); err != nil {
				return err
			}
		}
		if _, err := s.db.Exec(`PRAGMA user_version=17`); err != nil {
			return err
		}
	}
	if version < 18 {
		const schemaV18 = `
CREATE TABLE IF NOT EXISTS verification_run_history (
  workflow_id TEXT NOT NULL,
  revision INTEGER NOT NULL,
  candidate_tree TEXT NOT NULL,
  result BLOB NOT NULL,
  archived_at TEXT NOT NULL,
  PRIMARY KEY(workflow_id,revision)
);
CREATE TABLE IF NOT EXISTS verification_contract_revisions (
  workflow_id TEXT NOT NULL,
  revision INTEGER NOT NULL,
  candidate_tree TEXT NOT NULL,
  check_id TEXT NOT NULL,
  previous_command TEXT NOT NULL,
  replacement_command TEXT NOT NULL,
  actor TEXT NOT NULL,
  reason TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  fencing_token TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(workflow_id,revision),
  UNIQUE(workflow_id,idempotency_key)
);`
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(schemaV18); err != nil {
			return err
		}
		if err := backfillResultTransport(tx); err != nil {
			return err
		}
		if _, err := tx.Exec(`PRAGMA user_version=18`); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// backfillResultTransport declares the transport that in-flight workflows were
// already using. Every run input written before schema 18 came from the Skynex
// OpenCode adapter, which has always spoken skynex-result-file-v1; without this
// they would unmarshal with an empty transport and fail preflight forever with
// no recovery other than abandoning a frozen candidate.
func backfillResultTransport(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT workflow_id,input FROM workflow_run_inputs`)
	if err != nil {
		return err
	}
	type backfilled struct {
		id    string
		input []byte
	}
	var pending []backfilled
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		var declared string
		if value, ok := fields["ResultTransport"]; ok && json.Unmarshal(value, &declared) == nil && declared != "" {
			continue
		}
		encoded, err := json.Marshal(ResultTransportFileV1)
		if err != nil {
			rows.Close()
			return err
		}
		fields["ResultTransport"] = encoded
		updated, err := json.Marshal(fields)
		if err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, backfilled{id: id, input: updated})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range pending {
		if _, err := tx.Exec(`UPDATE workflow_run_inputs SET input=? WHERE workflow_id=?`, item.input, item.id); err != nil {
			return err
		}
	}
	return nil
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

var errGraphVersionRequired = errors.New("execution: the scheduled graph version is required")

// MutationActivation is the exact fenced claim a scheduler makes on one slice.
type MutationActivation struct {
	AttemptID, WorkflowID, SliceID, WorktreeID string
	Owner, FencingToken, BasisTree             string
	AllowedPaths                               []byte
	OperationID                                string
	GraphVersion                               uint64
}

// ActivateExecutionSlice claims a pending slice, records its fenced attempt,
// and advances a ready workflow to executing in one transaction. Committing the
// activation and the state transition together prevents a crash between them
// from leaving a ready workflow holding an active slice, which no later
// completion could reconcile.
func (s *SQLiteStore) ActivateExecutionSlice(a MutationActivation) error {
	if a.GraphVersion == 0 {
		return errGraphVersionRequired
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	res, err := conn.ExecContext(ctx, `UPDATE execution_slice_state SET status='active' WHERE workflow_id=? AND slice_id=? AND status='pending'`, a.WorkflowID, a.SliceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("execution: concurrent writer active")
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO mutation_attempts(attempt_id,workflow_id,slice_id,worktree_id,owner,fencing_token,basis_tree,allowed_paths,operation_id,live) VALUES(?,?,?,?,?,?,?,?,?,1)`, a.AttemptID, a.WorkflowID, a.SliceID, a.WorktreeID, a.Owner, a.FencingToken, a.BasisTree, a.AllowedPaths, a.OperationID); err != nil {
		return err
	}
	if err = beginExecutionIfReady(ctx, conn, a.WorkflowID, a.GraphVersion, s.now); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func beginExecutionIfReady(ctx context.Context, conn *sql.Conn, workflowID string, graphVersion uint64, now func() time.Time) error {
	w, err := scanWorkflow(conn.QueryRowContext(ctx, `SELECT id,state,state_version,route,minimum_risk,basis_tree,resume_target FROM workflows WHERE id=?`, workflowID))
	if err != nil {
		return err
	}
	if w.State == StateExecuting {
		return nil
	}
	if w.State != StateReady {
		return fmt.Errorf("execution: cannot activate a slice while workflow is %s", w.State)
	}
	key := fmt.Sprintf("execution:start:v%d", graphVersion)
	req := Transition{WorkflowID: w.ID, ExpectedState: StateReady, ExpectedVersion: w.StateVersion, NextState: StateExecuting, IdempotencyKey: key}
	var oldReq []byte
	err = conn.QueryRowContext(ctx, `SELECT request FROM idempotency WHERE workflow_id=? AND key=?`, w.ID, key).Scan(&oldReq)
	if err == nil {
		var previous Transition
		if json.Unmarshal(oldReq, &previous) != nil {
			return fmt.Errorf("workflow: corrupt idempotency record")
		}
		if !sameTransition(previous, req) {
			return ErrIdempotencyReuse
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	next := w
	next.State = StateExecuting
	next.StateVersion++
	res, err := conn.ExecContext(ctx, `UPDATE workflows SET state=?,state_version=? WHERE id=? AND state=? AND state_version=?`, next.State, next.StateVersion, next.ID, StateReady, w.StateVersion)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrCASConflict
	}
	artifacts, _ := json.Marshal([]string(nil))
	stamp := now().UTC().Format(time.RFC3339Nano)
	if _, err = conn.ExecContext(ctx, `INSERT INTO transition_events(workflow_id,from_state,to_state,state_version,idempotency_key,artifact_ids,occurred_at) VALUES(?,?,?,?,?,?,?)`, next.ID, StateReady, StateExecuting, next.StateVersion, key, artifacts, stamp); err != nil {
		return err
	}
	reqJSON, _ := json.Marshal(cloneTransition(req))
	resultJSON, _ := json.Marshal(next)
	_, err = conn.ExecContext(ctx, `INSERT INTO idempotency(workflow_id,key,request,result) VALUES(?,?,?,?)`, next.ID, key, reqJSON, resultJSON)
	return err
}

// AdoptAppliedMutation completes a slice whose patch is already durably in the
// worktree but whose broker transaction never committed. The caller must have
// proven, under the worktree lock, that the live tree is the recorded post
// tree; this records the same result the interrupted broker would have.
func (s *SQLiteStore) AdoptAppliedMutation(workflowID, sliceID, attemptID, operationID string, graphVersion uint64) error {
	if graphVersion == 0 {
		return errGraphVersionRequired
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var status, post string
	if err = conn.QueryRowContext(ctx, `SELECT status,post_tree FROM mutation_operations WHERE operation_id=? AND workflow_id=?`, operationID, workflowID).Scan(&status, &post); err != nil {
		return err
	}
	if post == "" || (status != "mutated" && status != "completed") {
		return fmt.Errorf("execution: mutation %s never reached the worktree", operationID)
	}
	if _, err = conn.ExecContext(ctx, `UPDATE mutation_operations SET status='completed' WHERE operation_id=? AND status='mutated'`, operationID); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `UPDATE mutation_attempts SET live=0 WHERE attempt_id=? AND workflow_id=? AND live=1`, attemptID, workflowID); err != nil {
		return err
	}
	res, err := conn.ExecContext(ctx, `UPDATE execution_slice_state SET status='completed' WHERE workflow_id=? AND slice_id=? AND status='active'`, workflowID, sliceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("execution: slice is not active")
	}
	if _, err = completeExecutionIfReady(ctx, conn, workflowID, graphVersion, s.now); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// CompleteExecutionSlice atomically records the terminal slice result and, if
// it was the final slice, advances the workflow to verification. Keeping these
// writes in one transaction prevents a dead detached worker from leaving every
// slice completed while the workflow remains in executing. graphVersion is the
// version of the execution graph the caller is actually scheduling, so the
// completion transition shares the exact lineage of its start transition.
func (s *SQLiteStore) CompleteExecutionSlice(workflowID, sliceID string, graphVersion uint64) error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	res, err := conn.ExecContext(ctx, `UPDATE execution_slice_state SET status='completed' WHERE workflow_id=? AND slice_id=? AND status='active'`, workflowID, sliceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("execution: slice is not active")
	}
	if _, err = completeExecutionIfReady(ctx, conn, workflowID, graphVersion, s.now); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// ReconcileCompletedExecution repairs an execution interrupted between the
// broker's durable mutation commit and the slice or workflow completion that
// used to follow it in a separate transaction. It adopts only slices whose
// mutation already reached 'completed' with its attempt retired, so no fencing
// decision is re-made here, and it does not change a partially executed
// workflow.
func (s *SQLiteStore) ReconcileCompletedExecution(workflowID string, graphVersion uint64) (bool, error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	repaired, err := adoptCompletedMutationSlices(ctx, conn, workflowID)
	if err != nil {
		return false, err
	}
	advanced, err := completeExecutionIfReady(ctx, conn, workflowID, graphVersion, s.now)
	if err != nil {
		return false, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	committed = true
	return repaired || advanced, nil
}

// adoptCompletedMutationSlices completes every slice left 'active' by a worker
// that died after the broker committed its mutation. The mutation must have
// reached 'completed' with its own attempt already retired, and the slice must
// have no live attempt at all, so a slice whose worker is still fenced and
// running is never adopted.
func adoptCompletedMutationSlices(ctx context.Context, conn *sql.Conn, workflowID string) (bool, error) {
	res, err := conn.ExecContext(ctx, `UPDATE execution_slice_state SET status='completed' WHERE workflow_id=? AND status='active' AND slice_id IN (SELECT a.slice_id FROM mutation_attempts a JOIN mutation_operations o ON o.operation_id=a.operation_id AND o.workflow_id=a.workflow_id WHERE a.workflow_id=? AND a.live=0 AND o.status='completed') AND slice_id NOT IN (SELECT l.slice_id FROM mutation_attempts l WHERE l.workflow_id=? AND l.live=1)`, workflowID, workflowID, workflowID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func completeExecutionIfReady(ctx context.Context, conn *sql.Conn, workflowID string, graphVersion uint64, now func() time.Time) (bool, error) {
	if graphVersion == 0 {
		return false, errGraphVersionRequired
	}
	var remaining int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_slice_state WHERE workflow_id=? AND status!='completed'`, workflowID).Scan(&remaining); err != nil {
		return false, err
	}
	if remaining != 0 {
		return false, nil
	}
	w, err := scanWorkflow(conn.QueryRowContext(ctx, `SELECT id,state,state_version,route,minimum_risk,basis_tree,resume_target FROM workflows WHERE id=?`, workflowID))
	if err != nil {
		return false, err
	}
	if w.State == StateVerifying {
		return false, nil
	}
	if w.State != StateExecuting {
		return false, fmt.Errorf("execution: all slices completed but workflow is %s", w.State)
	}
	next := w
	next.State = StateVerifying
	next.StateVersion++
	res, err := conn.ExecContext(ctx, `UPDATE workflows SET state=?,state_version=? WHERE id=? AND state=? AND state_version=?`, next.State, next.StateVersion, next.ID, StateExecuting, w.StateVersion)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, ErrCASConflict
	}
	key := fmt.Sprintf("execution:complete:v%d", graphVersion)
	artifacts, _ := json.Marshal([]string(nil))
	stamp := now().UTC().Format(time.RFC3339Nano)
	if _, err = conn.ExecContext(ctx, `INSERT INTO transition_events(workflow_id,from_state,to_state,state_version,idempotency_key,artifact_ids,occurred_at) VALUES(?,?,?,?,?,?,?)`, next.ID, StateExecuting, StateVerifying, next.StateVersion, key, artifacts, stamp); err != nil {
		return false, err
	}
	req := Transition{WorkflowID: next.ID, ExpectedState: StateExecuting, ExpectedVersion: w.StateVersion, NextState: StateVerifying, IdempotencyKey: key}
	reqJSON, _ := json.Marshal(cloneTransition(req))
	resultJSON, _ := json.Marshal(next)
	if _, err = conn.ExecContext(ctx, `INSERT INTO idempotency(workflow_id,key,request,result) VALUES(?,?,?,?)`, next.ID, key, reqJSON, resultJSON); err != nil {
		return false, err
	}
	return true, nil
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

// ExecutionFenceHeld reports whether any process currently owns the execution
// fence for the workflow. It is read-only, so a caller can refuse before
// creating durable state instead of discovering the conflict mid-run.
func (s *SQLiteStore) ExecutionFenceHeld(workflowID string, now time.Time) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM leases WHERE resource=? AND expires_at>?`, ExecutionFenceResource(workflowID), now.UnixNano()).Scan(&count)
	return count > 0, err
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
