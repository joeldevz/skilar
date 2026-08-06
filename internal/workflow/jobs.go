package workflow

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type JobState string

const (
	JobQueued          JobState = "queued"
	JobRunning         JobState = "running"
	JobCancelRequested JobState = "cancel_requested"
	JobSucceeded       JobState = "succeeded"
	JobFailed          JobState = "failed"
	JobCancelled       JobState = "cancelled"
)

type WorkflowJob struct {
	ID, WorkflowID, SessionID, Operation          string
	State                                         JobState
	PID                                           int
	CreatedAt, StartedAt, HeartbeatAt, FinishedAt time.Time
	TerminalState, Error                          string
}

type WorkflowNotification struct {
	ID, WorkflowID, JobID, TerminalState, ClaimToken, ClaimedBy, Operation string
	JobState                                                               JobState
	Error                                                                  string
	CreatedAt, ActivityAt                                                  time.Time
}

const WorkflowSessionPresenceTTL = 20 * time.Second
const MaxWorkflowJobAttempts = 3

const JobDisplacedErrorPrefix = "execution displaced: "

var workflowJobProcessAlive = workflowProcessAlive

type staleWorkflowJob struct{ id, operation string }

type admittedWorkflowJob struct {
	id, operation string
	pid           int
	stale         bool
}

// HasStaleWorkflowJobs is safe for a read-only diagnostic connection. Callers
// can use it to decide whether they need to reopen the database for recovery.
func (s *SQLiteStore) HasStaleWorkflowJobs(workflowID string, now time.Time) (bool, error) {
	stale, err := s.staleWorkflowJobs(workflowID, now)
	return len(stale) != 0, err
}

// LiveWorkflowJob reports the healthy admitted job of a workflow, if any. It
// uses the same liveness predicate that decides staleness, so a caller that
// must not disturb a running worker and the reconciler that retires a dead one
// can never disagree about which jobs are alive.
func (s *SQLiteStore) LiveWorkflowJob(workflowID string, now time.Time) (WorkflowJob, bool, error) {
	admitted, err := s.admittedWorkflowJobs(workflowID, now)
	if err != nil {
		return WorkflowJob{}, false, err
	}
	for _, item := range admitted {
		if item.stale {
			continue
		}
		return WorkflowJob{ID: item.id, WorkflowID: workflowID, Operation: item.operation, PID: item.pid}, true, nil
	}
	return WorkflowJob{}, false, nil
}

func (s *SQLiteStore) ReconcileStaleWorkflowJobs(workflowID string, now time.Time) error {
	found, err := s.staleWorkflowJobs(workflowID, now)
	if err != nil {
		return err
	}
	for _, item := range found {
		if err := s.FinishWorkflowJob(item.id, JobFailed, "interrupted", "detached "+item.operation+" worker interrupted", now); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) staleWorkflowJobs(workflowID string, now time.Time) ([]staleWorkflowJob, error) {
	admitted, err := s.admittedWorkflowJobs(workflowID, now)
	if err != nil {
		return nil, err
	}
	var found []staleWorkflowJob
	for _, item := range admitted {
		if item.stale {
			found = append(found, staleWorkflowJob{item.id, item.operation})
		}
	}
	return found, nil
}

func (s *SQLiteStore) admittedWorkflowJobs(workflowID string, now time.Time) ([]admittedWorkflowJob, error) {
	rows, err := s.db.Query(`SELECT id,operation,state,pid,created_at,heartbeat_at FROM workflow_jobs WHERE workflow_id=? AND state IN (?,?,?)`, workflowID, JobQueued, JobRunning, JobCancelRequested)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []admittedWorkflowJob
	for rows.Next() {
		var id, operation string
		var state JobState
		var pid int
		var created, heartbeat string
		if err := rows.Scan(&id, &operation, &state, &pid, &created, &heartbeat); err != nil {
			return nil, err
		}
		createdAt, _ := time.Parse(time.RFC3339Nano, created)
		heartbeatAt, _ := time.Parse(time.RFC3339Nano, heartbeat)
		reference := heartbeatAt
		if reference.IsZero() {
			reference = createdAt
		}
		fresh := !reference.IsZero() && now.Sub(reference) >= 0 && now.Sub(reference) <= 30*time.Second
		live := pid > 0 && workflowJobProcessAlive(pid)
		stale := false
		switch state {
		case JobQueued:
			// Legacy queued jobs have no PID until the child starts. Give a fresh
			// queue record time to start, but never let an abandoned queue retain
			// the one-live-job admission indefinitely.
			stale = !fresh
		case JobRunning, JobCancelRequested:
			// PID liveness alone is insufficient: an OS may have reused a stale
			// PID for an unrelated process. A running job is healthy only when
			// both its durable heartbeat and its process are current.
			stale = !fresh || !live
		}
		found = append(found, admittedWorkflowJob{id: id, operation: operation, pid: pid, stale: stale})
	}
	return found, rows.Err()
}

func (s *SQLiteStore) HeartbeatWorkflowSession(sessionID string, now time.Time) error {
	if sessionID == "" {
		return errors.New("workflow session id is required")
	}
	_, err := s.db.Exec(`INSERT INTO workflow_session_presence(session_id,last_seen) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET last_seen=excluded.last_seen`, sessionID, dbTime(now))
	return err
}

func dbTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (s *SQLiteStore) CreateWorkflowJob(id, workflowID string, now time.Time) (WorkflowJob, error) {
	return s.CreateWorkflowJobOperation(id, workflowID, "run", now)
}

func (s *SQLiteStore) CreateWorkflowJobOperation(id, workflowID, operation string, now time.Time) (WorkflowJob, error) {
	if id == "" || workflowID == "" {
		return WorkflowJob{}, errors.New("workflow job requires id and workflow id")
	}
	if operation != "run" && operation != "review" {
		return WorkflowJob{}, fmt.Errorf("unsupported workflow job operation %q", operation)
	}
	_, err := s.db.Exec(`INSERT INTO workflow_jobs(id,workflow_id,operation,state,pid,created_at) VALUES(?,?,?, ?,0,?)`, id, workflowID, operation, JobQueued, dbTime(now))
	if err != nil {
		return WorkflowJob{}, err
	}
	return s.WorkflowJob(id)
}

func scanJob(row interface{ Scan(...any) error }) (WorkflowJob, error) {
	var j WorkflowJob
	var created, started, heartbeat, finished string
	err := row.Scan(&j.ID, &j.WorkflowID, &j.SessionID, &j.Operation, &j.State, &j.PID, &created, &started, &heartbeat, &finished, &j.TerminalState, &j.Error)
	if err != nil {
		return j, err
	}
	j.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	j.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	j.HeartbeatAt, _ = time.Parse(time.RFC3339Nano, heartbeat)
	j.FinishedAt, _ = time.Parse(time.RFC3339Nano, finished)
	return j, nil
}

func (s *SQLiteStore) WorkflowJob(id string) (WorkflowJob, error) {
	return scanJob(s.db.QueryRow(`SELECT id,workflow_id,session_id,operation,state,pid,created_at,started_at,heartbeat_at,finished_at,terminal_state,error FROM workflow_jobs WHERE id=?`, id))
}

func (s *SQLiteStore) BindWorkflowJobSession(id, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE workflow_jobs SET session_id=? WHERE id=? AND state=?`, sessionID, id, JobQueued)
	return err
}

func (s *SQLiteStore) SetWorkflowJobPID(id string, pid int) error {
	if pid <= 0 {
		return errors.New("workflow job pid must be positive")
	}
	res, err := s.db.Exec(`UPDATE workflow_jobs SET pid=? WHERE id=? AND state IN (?,?) AND (pid=0 OR pid=?)`, pid, id, JobQueued, JobRunning, pid)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		var existingPID int
		var state JobState
		if scanErr := s.db.QueryRow(`SELECT pid,state FROM workflow_jobs WHERE id=?`, id).Scan(&existingPID, &state); scanErr == nil && existingPID == pid && (state == JobSucceeded || state == JobFailed || state == JobCancelled) {
			return nil
		}
		return errors.New("workflow job is no longer live")
	}
	return nil
}

func (s *SQLiteStore) StartWorkflowJob(id string, pid int, now time.Time) error {
	res, err := s.db.Exec(`UPDATE workflow_jobs SET state=?,pid=?,started_at=?,heartbeat_at=? WHERE id=? AND state=?`, JobRunning, pid, dbTime(now), dbTime(now), id, JobQueued)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("workflow job is not queued")
	}
	return nil
}

func (s *SQLiteStore) HeartbeatWorkflowJob(id string, now time.Time) error {
	res, err := s.db.Exec(`UPDATE workflow_jobs SET heartbeat_at=? WHERE id=? AND state IN (?,?)`, dbTime(now), id, JobRunning, JobCancelRequested)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("workflow job is not live")
	}
	return nil
}

func (s *SQLiteStore) FinishWorkflowJob(id string, state JobState, terminal, message string, now time.Time) error {
	if state != JobSucceeded && state != JobFailed && state != JobCancelled {
		return fmt.Errorf("invalid terminal job state %q", state)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var workflowID, operation string
	var previous JobState
	if err = tx.QueryRow(`SELECT workflow_id,operation,state FROM workflow_jobs WHERE id=?`, id).Scan(&workflowID, &operation, &previous); err != nil {
		return err
	}
	if previous == JobSucceeded || previous == JobFailed || previous == JobCancelled {
		return tx.Commit()
	}
	var realTerminal State
	if err = tx.QueryRow(`SELECT state FROM workflows WHERE id=?`, workflowID).Scan(&realTerminal); err != nil {
		return err
	}
	// A detached technical failure is a durable workflow blocker, not merely a
	// dead side-car process. Keep the source state as the exact resume target so
	// a bounded retry can continue without fabricating a Git recovery basis.
	if state == JobFailed {
		if _, blockable := blockableStates[realTerminal]; blockable {
			var version uint64
			if err = tx.QueryRow(`SELECT state_version FROM workflows WHERE id=?`, workflowID).Scan(&version); err != nil {
				return err
			}
			res, updateErr := tx.Exec(`UPDATE workflows SET state=?,state_version=state_version+1,resume_target=? WHERE id=? AND state=? AND state_version=?`, StateBlocked, realTerminal, workflowID, realTerminal, version)
			if updateErr != nil {
				return updateErr
			}
			if n, _ := res.RowsAffected(); n == 1 {
				artifacts, _ := json.Marshal([]string{"job-blocker:" + id, "job-evidence:" + id})
				key := "job-failed:" + id
				if _, err = tx.Exec(`INSERT INTO transition_events(workflow_id,from_state,to_state,state_version,idempotency_key,artifact_ids,occurred_at) VALUES(?,?,?,?,?,?,?)`, workflowID, realTerminal, StateBlocked, version+1, key, artifacts, dbTime(now)); err != nil {
					return err
				}
				realTerminal = StateBlocked
			}
		}
	}
	terminal = string(realTerminal)
	if _, err = tx.Exec(`UPDATE workflow_jobs SET state=?,terminal_state=?,error=?,finished_at=?,heartbeat_at=? WHERE id=? AND state NOT IN (?,?,?)`, state, terminal, message, dbTime(now), dbTime(now), id, JobSucceeded, JobFailed, JobCancelled); err != nil {
		return err
	}
	nid := "notification:" + id
	if _, err = tx.Exec(`INSERT OR IGNORE INTO workflow_notifications(id,workflow_id,job_id,terminal_state,created_at) VALUES(?,?,?,?,?)`, nid, workflowID, id, terminal, dbTime(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// RetryTechnicalWorkflowJob resumes the blocker created by the latest failed
// detached job. It deliberately bypasses candidate recovery reconciliation:
// the job failure itself did not authorize or claim a candidate tree change.
// Admission is bounded to three total attempts for each workflow/operation.
func (s *SQLiteStore) RetryTechnicalWorkflowJob(workflowID, operation string, now time.Time) (Workflow, error) {
	w, err := s.Get(workflowID)
	if err != nil {
		return Workflow{}, err
	}
	if w.State != StateBlocked || w.ResumeTarget == "" {
		return Workflow{}, fmt.Errorf("workflow %s is not blocked by a retryable job", workflowID)
	}
	var jobID string
	var state JobState
	if err = s.db.QueryRow(`SELECT id,state FROM workflow_jobs WHERE workflow_id=? AND operation=? ORDER BY rowid DESC LIMIT 1`, workflowID, operation).Scan(&jobID, &state); err != nil {
		return Workflow{}, err
	}
	if state != JobFailed {
		return Workflow{}, fmt.Errorf("workflow %s latest %s job is not failed", workflowID, operation)
	}
	var attempts int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM workflow_jobs WHERE workflow_id=? AND operation=? AND NOT (state=? AND error LIKE ?)`, workflowID, operation, JobCancelled, JobDisplacedErrorPrefix+"%").Scan(&attempts); err != nil {
		return Workflow{}, err
	}
	if attempts >= MaxWorkflowJobAttempts {
		return Workflow{}, fmt.Errorf("workflow %s %s retry limit reached (%d attempts)", workflowID, operation, attempts)
	}
	events, err := s.Events(workflowID)
	if err != nil {
		return Workflow{}, err
	}
	want := "job-blocker:" + jobID
	matched := false
	for i := len(events) - 1; i >= 0 && !matched; i-- {
		if events[i].To != StateBlocked {
			continue
		}
		for _, artifact := range events[i].ArtifactIDs {
			matched = matched || artifact == want
		}
		break
	}
	if !matched {
		return Workflow{}, errors.New("workflow: active blocker is not the latest failed job")
	}
	return s.Transition(Transition{WorkflowID: workflowID, ExpectedState: StateBlocked, ExpectedVersion: w.StateVersion, NextState: w.ResumeTarget, IdempotencyKey: "technical-retry:" + jobID})
}

func (s *SQLiteStore) WorkflowJobAttempts(workflowID, operation string) (int, error) {
	var attempts int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM workflow_jobs WHERE workflow_id=? AND operation=? AND NOT (state=? AND error LIKE ?)`, workflowID, operation, JobCancelled, JobDisplacedErrorPrefix+"%").Scan(&attempts)
	return attempts, err
}

func (s *SQLiteStore) CancelWorkflowJobs(workflowID string, _ time.Time) ([]WorkflowJob, error) {
	if _, err := s.db.Exec(`UPDATE workflow_jobs SET state=? WHERE workflow_id=? AND state IN (?,?)`, JobCancelRequested, workflowID, JobQueued, JobRunning); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,workflow_id,session_id,operation,state,pid,created_at,started_at,heartbeat_at,finished_at,terminal_state,error FROM workflow_jobs WHERE workflow_id=? AND state=?`, workflowID, JobCancelRequested)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []WorkflowJob
	for rows.Next() {
		j, e := scanJob(rows)
		if e != nil {
			return nil, e
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func notificationToken() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}

func (s *SQLiteStore) ClaimWorkflowNotification(consumer string, now time.Time) (*WorkflowNotification, error) {
	return s.claimWorkflowNotification(consumer, nil, false, now)
}

func (s *SQLiteStore) ClaimWorkflowNotificationForActive(consumer string, activeSessions []string, now time.Time) (*WorkflowNotification, error) {
	return s.claimWorkflowNotification(consumer, activeSessions, true, now)
}

func (s *SQLiteStore) claimWorkflowNotification(consumer string, activeSessions []string, allowRebind bool, now time.Time) (*WorkflowNotification, error) {
	if consumer == "" {
		return nil, errors.New("notification consumer is required")
	}
	token, err := notificationToken()
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var n WorkflowNotification
	var created, activity, boundSession string
	cutoff := dbTime(now.Add(-5 * time.Minute))
	condition := "j.session_id=?"
	queryArgs := []any{cutoff, consumer}
	if allowRebind {
		// A caller-declared live session keeps its notification even when its
		// presence heartbeat has lapsed, so a stalled heartbeat alone cannot
		// hand a still-live wake prompt to another consumer.
		var active []any
		seen := map[string]bool{}
		for _, session := range activeSessions {
			if session == "" || session == consumer || seen[session] {
				continue
			}
			seen[session] = true
			active = append(active, session)
		}
		stale := "NOT EXISTS (SELECT 1 FROM workflow_session_presence p WHERE p.session_id=j.session_id AND p.last_seen>=?)"
		if len(active) > 0 {
			stale = "j.session_id NOT IN (?" + strings.Repeat(",?", len(active)-1) + ") AND " + stale
			queryArgs = append(queryArgs, active...)
		}
		condition = "(j.session_id=? OR (" + stale + "))"
		queryArgs = append(queryArgs, dbTime(now.Add(-WorkflowSessionPresenceTTL)))
	}
	query := `SELECT n.id,n.workflow_id,n.job_id,n.terminal_state,n.created_at,j.session_id,j.operation,j.state,j.error,j.heartbeat_at FROM workflow_notifications n JOIN workflow_jobs j ON j.id=n.job_id WHERE n.acked_at='' AND (n.claim_token='' OR n.claimed_at<?) AND ` + condition + ` ORDER BY n.created_at,n.id LIMIT 1`
	err = tx.QueryRow(query, queryArgs...).Scan(&n.ID, &n.WorkflowID, &n.JobID, &n.TerminalState, &created, &boundSession, &n.Operation, &n.JobState, &n.Error, &activity)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`UPDATE workflow_notifications SET claim_token=?,claimed_by=?,claimed_at=? WHERE id=? AND acked_at='' AND (claim_token='' OR claimed_at<?)`, token, consumer, dbTime(now), n.ID, cutoff)
	if err != nil {
		return nil, err
	}
	affected, _ := res.RowsAffected()
	if affected != 1 {
		return nil, nil
	}
	if boundSession != consumer {
		if _, err = tx.Exec(`UPDATE workflow_jobs SET session_id=? WHERE id=? AND session_id=?`, consumer, n.JobID, boundSession); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	n.ClaimToken = token
	n.ClaimedBy = consumer
	n.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	n.ActivityAt, _ = time.Parse(time.RFC3339Nano, activity)
	return &n, nil
}

func (s *SQLiteStore) AckWorkflowNotification(id, token string, now time.Time) error {
	if id == "" || token == "" {
		return errors.New("notification id and claim token are required")
	}
	res, err := s.db.Exec(`UPDATE workflow_notifications SET acked_at=? WHERE id=? AND claim_token=? AND acked_at=''`, dbTime(now), id, token)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("notification claim is not current")
	}
	return nil
}

func (s *SQLiteStore) ReleaseWorkflowNotification(id, token string) error {
	res, err := s.db.Exec(`UPDATE workflow_notifications SET claim_token='',claimed_by='',claimed_at='' WHERE id=? AND claim_token=? AND acked_at=''`, id, token)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("notification claim is not current")
	}
	return nil
}
