package workflow

import (
	"path/filepath"
	"testing"
	"time"
)

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
