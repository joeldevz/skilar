package runner

import (
	"os/exec"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/workflow"
)

func TestReconcileManagedWorkflowRuntimeCancelsJobAfterLifecycleStop(t *testing.T) {
	repo := t.TempDir()
	if output, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(workflow.Workflow{ID: "managed-canary"}); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, time.August, 12, 8, 0, 0, 0, time.UTC)
	if _, err = store.CreateWorkflowJobOperation("job-1", "managed-canary", "run", created); err != nil {
		t.Fatal(err)
	}
	if err = store.StartWorkflowJob("job-1", 4242, created.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	finished := created.Add(2 * time.Second)
	config := &workflowDriverConfig{Mode: "managed-detach", WorkflowID: "managed-canary"}
	if err = reconcileManagedWorkflowRuntime(repo, config, finished); err != nil {
		t.Fatal(err)
	}

	store, err = workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	job, err := store.WorkflowJob("job-1")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != workflow.JobCancelled || !job.FinishedAt.Equal(finished) {
		t.Fatalf("reconciled job = %+v", job)
	}
}

func TestReconcileManagedWorkflowRuntimeIsNoopWithoutManagedDatabase(t *testing.T) {
	repo := t.TempDir()
	if output, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	config := &workflowDriverConfig{Mode: "managed-detach", WorkflowID: "managed-canary"}
	if err := reconcileManagedWorkflowRuntime(repo, config, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}
