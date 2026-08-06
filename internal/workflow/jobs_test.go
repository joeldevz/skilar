package workflow

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func executingJobWorkflow(t *testing.T, s *SQLiteStore, id string) Workflow {
	t.Helper()
	w, err := s.Create(Workflow{ID: id, Route: RouteSimple, MinimumRisk: RiskLow})
	if err != nil {
		t.Fatal(err)
	}
	for i, next := range []State{StateDiscovering, StateReady, StateExecuting} {
		w, err = s.Transition(Transition{WorkflowID: id, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: next, IdempotencyKey: fmt.Sprintf("setup-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func TestFailedDetachedJobAtomicallyBlocksAndCanRetryWithoutRecoveryBasis(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	executingJobWorkflow(t, s, "wf")
	if _, err = s.CreateWorkflowJobOperation("job-1", "wf", "run", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.BindWorkflowJobSession("job-1", "session"); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishWorkflowJob("job-1", JobFailed, "executing", "provider timeout", time.Now()); err != nil {
		t.Fatal(err)
	}
	w, err := s.Get("wf")
	if err != nil || w.State != StateBlocked || w.ResumeTarget != StateExecuting {
		t.Fatalf("workflow=%+v err=%v", w, err)
	}
	events, _ := s.Events("wf")
	last := events[len(events)-1]
	if last.To != StateBlocked || !strings.Contains(strings.Join(last.ArtifactIDs, ","), "job-blocker:job-1") {
		t.Fatalf("event=%+v", last)
	}
	n, err := s.ClaimWorkflowNotification("session", time.Now())
	if err != nil || n == nil || n.TerminalState != string(StateBlocked) || n.Error != "provider timeout" {
		t.Fatalf("notification=%+v err=%v", n, err)
	}
	resumed, err := s.RetryTechnicalWorkflowJob("wf", "run", time.Now())
	if err != nil || resumed.State != StateExecuting || resumed.ResumeTarget != "" {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
}

func TestTechnicalJobRetryIsBoundedToThreeTotalAttempts(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	executingJobWorkflow(t, s, "wf")
	for i := 1; i <= MaxWorkflowJobAttempts; i++ {
		id := fmt.Sprintf("job-%d", i)
		if _, err = s.CreateWorkflowJobOperation(id, "wf", "run", time.Now().Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if err = s.FinishWorkflowJob(id, JobFailed, "", "timeout", time.Now()); err != nil {
			t.Fatal(err)
		}
		if i < MaxWorkflowJobAttempts {
			if _, err = s.RetryTechnicalWorkflowJob("wf", "run", time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = s.RetryTechnicalWorkflowJob("wf", "run", time.Now()); err == nil || !strings.Contains(err.Error(), "retry limit reached") {
		t.Fatalf("err=%v", err)
	}
}

func TestDisplacedJobsDoNotConsumeTechnicalRetryBudget(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	executingJobWorkflow(t, s, "wf")

	if _, err = s.CreateWorkflowJobOperation("job-displaced", "wf", "run", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishWorkflowJob("job-displaced", JobCancelled, "", JobDisplacedErrorPrefix+ErrExecutionFenceLost.Error(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateWorkflowJobOperation("job-cancelled", "wf", "run", time.Now().Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishWorkflowJob("job-cancelled", JobCancelled, "", "cancel requested", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateWorkflowJobOperation("job-failed", "wf", "run", time.Now().Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishWorkflowJob("job-failed", JobFailed, "", "timeout", time.Now()); err != nil {
		t.Fatal(err)
	}

	attempts, err := s.WorkflowJobAttempts("wf", "run")
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d, want 2 (ordinary cancellation and failure)", attempts)
	}
	if _, err = s.RetryTechnicalWorkflowJob("wf", "run", time.Now()); err != nil {
		t.Fatalf("displaced job consumed retry budget: %v", err)
	}
}

func TestDetachedJobLifecycleAndTerminalNotificationDeduplication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflows.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Create(Workflow{ID: "wf", Route: RouteSimple, MinimumRisk: RiskLow}); err != nil {
		t.Fatal(err)
	}

	job, err := s.CreateWorkflowJob("job-1", "wf", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if job.State != JobQueued {
		t.Fatalf("state=%s", job.State)
	}
	if err = s.BindWorkflowJobSession("job-1", "session-1"); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Minute)
	if err = s.StartWorkflowJob("job-1", 4242, started); err != nil {
		t.Fatal(err)
	}
	if err = s.HeartbeatWorkflowJob("job-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishWorkflowJob("job-1", JobSucceeded, "candidate_frozen", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Replay must not create a second terminal notification.
	if err = s.FinishWorkflowJob("job-1", JobSucceeded, "candidate_frozen", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.Database().QueryRow(`SELECT COUNT(*) FROM workflow_notifications WHERE workflow_id='wf'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("notifications=%d err=%v", count, err)
	}

	n, err := s.ClaimWorkflowNotification("session-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n == nil || n.WorkflowID != "wf" || n.Operation != "run" || n.ActivityAt.IsZero() || n.ClaimToken == "" {
		t.Fatalf("notification=%+v", n)
	}
	if err = s.AckWorkflowNotification(n.ID, n.ClaimToken, time.Now()); err != nil {
		t.Fatal(err)
	}
	if next, err := s.ClaimWorkflowNotification("session-1", time.Now()); err != nil || next != nil {
		t.Fatalf("second claim=%+v err=%v", next, err)
	}
}

func TestStaleReviewJobReconcilesAndReleasesCrossOperationAdmission(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Create(Workflow{ID: "wf", Route: RouteSimple, MinimumRisk: RiskLow}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if _, err = s.CreateWorkflowJobOperation("review-old", "wf", "review", old); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileStaleWorkflowJobs("wf", time.Now()); err != nil {
		t.Fatal(err)
	}
	oldJob, err := s.WorkflowJob("review-old")
	if err != nil || oldJob.State != JobFailed || oldJob.Operation != "review" || oldJob.TerminalState != "created" || oldJob.Error != "detached review worker interrupted" {
		t.Fatalf("job=%+v err=%v", oldJob, err)
	}
	if _, err = s.CreateWorkflowJobOperation("run-new", "wf", "run", time.Now()); err != nil {
		t.Fatalf("admission remained blocked: %v", err)
	}
}

func TestRunningWorkflowJobRequiresFreshHeartbeatAndLivePID(t *testing.T) {
	oldProbe := workflowJobProcessAlive
	t.Cleanup(func() { workflowJobProcessAlive = oldProbe })
	now := time.Now().UTC()
	for _, tc := range []struct {
		name      string
		heartbeat time.Time
		alive     bool
		wantState JobState
	}{
		{name: "alive reused PID with stale heartbeat is terminal", heartbeat: now.Add(-time.Minute), alive: true, wantState: JobFailed},
		{name: "fresh live process is preserved", heartbeat: now, alive: true, wantState: JobRunning},
		{name: "fresh dead process is terminal", heartbeat: now, alive: false, wantState: JobFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workflowJobProcessAlive = func(pid int) bool { return pid == 4242 && tc.alive }
			s, err := OpenSQLite(filepath.Join(t.TempDir(), "workflows.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err = s.Create(Workflow{ID: "wf", Route: RouteSimple, MinimumRisk: RiskLow}); err != nil {
				t.Fatal(err)
			}
			if _, err = s.CreateWorkflowJobOperation("job", "wf", "review", now); err != nil {
				t.Fatal(err)
			}
			if err = s.StartWorkflowJob("job", 4242, tc.heartbeat); err != nil {
				t.Fatal(err)
			}
			if err = s.ReconcileStaleWorkflowJobs("wf", now); err != nil {
				t.Fatal(err)
			}
			job, err := s.WorkflowJob("job")
			if err != nil || job.State != tc.wantState {
				t.Fatalf("job=%+v err=%v", job, err)
			}
			if tc.wantState == JobFailed {
				if _, err = s.CreateWorkflowJobOperation("replacement", "wf", "run", now); err != nil {
					t.Fatalf("admission not released: %v", err)
				}
			}
		})
	}
}

func TestFreshLegacyQueuedWorkflowJobIsPreserved(t *testing.T) {
	oldProbe := workflowJobProcessAlive
	workflowJobProcessAlive = func(int) bool { t.Fatal("queued job must not probe a PID"); return false }
	t.Cleanup(func() { workflowJobProcessAlive = oldProbe })
	now := time.Now().UTC()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Create(Workflow{ID: "wf", Route: RouteSimple, MinimumRisk: RiskLow}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateWorkflowJob("legacy", "wf", now); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileStaleWorkflowJobs("wf", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	job, err := s.WorkflowJob("legacy")
	if err != nil || job.State != JobQueued || job.Operation != "run" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}

func TestWorkflowJobCancellationIsPersistedForCrossProcessWorkers(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Create(Workflow{ID: "wf", Route: RouteSimple, MinimumRisk: RiskLow}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateWorkflowJob("job-1", "wf", time.Now()); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Minute)
	if err = s.StartWorkflowJob("job-1", 4242, started); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.CancelWorkflowJobs("wf", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].PID != 4242 || jobs[0].State != JobCancelRequested {
		t.Fatalf("jobs=%+v", jobs)
	}
	if !jobs[0].HeartbeatAt.Equal(started.UTC()) {
		t.Fatalf("cancel refreshed stale heartbeat: %s", jobs[0].HeartbeatAt)
	}
}

func TestNotificationSafelyRebindsOnlyWhenOriginalSessionIsGone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflows.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Create(Workflow{ID: "wf", Route: RouteSimple, MinimumRisk: RiskLow}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateWorkflowJob("job", "wf", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.BindWorkflowJobSession("job", "old-session"); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishWorkflowJob("job", JobFailed, "ignored", "boom", time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err = s.HeartbeatWorkflowSession("old-session", now); err != nil {
		t.Fatal(err)
	}
	other, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	third, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	type claimResult struct {
		n   *WorkflowNotification
		err error
	}
	results := make(chan claimResult, 2)
	go func() {
		n, e := other.ClaimWorkflowNotificationForActive("new-session", []string{"new-session"}, now.Add(5*time.Second))
		results <- claimResult{n, e}
	}()
	go func() {
		n, e := third.ClaimWorkflowNotificationForActive("third-session", []string{"third-session"}, now.Add(5*time.Second))
		results <- claimResult{n, e}
	}()
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err != nil || got.n != nil {
			t.Fatalf("stole active cross-process session notice=%+v err=%v", got.n, got.err)
		}
	}
	n, err := other.ClaimWorkflowNotificationForActive("new-session", []string{"new-session"}, now.Add(WorkflowSessionPresenceTTL+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if n == nil || n.JobState != JobFailed || n.Error != "boom" || n.ClaimedBy != "new-session" {
		t.Fatalf("rebound=%+v", n)
	}
	if second, err := s.ClaimWorkflowNotificationForActive("other", []string{"new-session", "other"}, time.Now()); err != nil || second != nil {
		t.Fatalf("duplicate claim=%+v err=%v", second, err)
	}
}

func TestNotificationKeepsDeclaredActiveSessionAfterHeartbeatLapse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflows.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Create(Workflow{ID: "wf", Route: RouteSimple, MinimumRisk: RiskLow}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateWorkflowJob("job", "wf", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.BindWorkflowJobSession("job", "owner-session"); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishWorkflowJob("job", JobSucceeded, "delivered", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err = s.HeartbeatWorkflowSession("owner-session", now); err != nil {
		t.Fatal(err)
	}
	lapsed := now.Add(WorkflowSessionPresenceTTL + time.Second)
	n, err := s.ClaimWorkflowNotificationForActive("other-session", []string{"other-session", "owner-session"}, lapsed)
	if err != nil {
		t.Fatal(err)
	}
	if n != nil {
		t.Fatalf("stole notification from a declared-active session: %+v", n)
	}
	owned, err := s.ClaimWorkflowNotificationForActive("owner-session", []string{"other-session", "owner-session"}, lapsed)
	if err != nil {
		t.Fatal(err)
	}
	if owned == nil || owned.ClaimedBy != "owner-session" {
		t.Fatalf("owner could not claim its own notification: %+v", owned)
	}
}
