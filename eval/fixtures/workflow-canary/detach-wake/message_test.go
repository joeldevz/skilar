package detachwake

import "testing"

func TestWorkflowStatusText(t *testing.T) {
	if WorkflowStatusText != "ready" {
		t.Fatalf("WorkflowStatusText = %q, want ready", WorkflowStatusText)
	}
}
