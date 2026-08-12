package runner

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/joeldevz/skynex/internal/workflow"
)

// reconcileManagedWorkflowRuntime is called only after the evaluator
// lifecycle has successfully stopped and verified its complete process group.
// An absent database means the plugin never admitted durable workflow work;
// an existing database must prove that no managed job remains live.
func reconcileManagedWorkflowRuntime(workspacePath string, config *workflowDriverConfig, now time.Time) error {
	if config == nil || config.Mode != "managed-detach" {
		return nil
	}
	databasePath, err := workflow.CanonicalDatabasePath(workspacePath)
	if err != nil {
		return fmt.Errorf("resolve managed workflow database: %w", err)
	}
	if _, err = os.Lstat(databasePath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect managed workflow database: %w", err)
	}
	store, err := workflow.OpenSQLite(databasePath)
	if err != nil {
		return fmt.Errorf("open managed workflow database: %w", err)
	}
	reconcileErr := store.ReconcileManagedEvaluationJobs(config.WorkflowID, now)
	closeErr := store.Close()
	if err = errors.Join(reconcileErr, closeErr); err != nil {
		return fmt.Errorf("attest managed workflow cleanup: %w", err)
	}
	return nil
}
