package runner

import (
	"testing"

	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/judges"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/trace"
)

func TestToolOutputContainsAllUsesDurableToolEvidence(t *testing.T) {
	check := contracts.Check{
		Type: "tool_output_contains_all", Tool: "worker_result",
		Patterns: []string{"workflow_id", "attempt-2", "tree-v1"},
	}
	collected := &trace.Trace{Tools: []trace.ToolCall{{
		Tool: "worker_worker_result", Status: "completed",
		Output: `{"workflow_id":"wf","attempt_id":"attempt-2","base":"tree-v1"}`,
	}}}
	observation := observeCaseCheck(check, contracts.Case{}, sandbox.Snapshot{}, sandbox.Snapshot{}, nil, nil, collected, "", judges.Verdict{})
	if observation.status != contracts.CheckStatusPass {
		t.Fatalf("durable envelope was rejected: %+v", observation)
	}
	collected.Tools[0].Output = `{"workflow_id":"wf","attempt_id":"attempt-1","base":"tree-v1"}`
	observation = observeCaseCheck(check, contracts.Case{}, sandbox.Snapshot{}, sandbox.Snapshot{}, nil, nil, collected, "workflow_id attempt-2 tree-v1", judges.Verdict{})
	if observation.status != contracts.CheckStatusFail {
		t.Fatalf("assistant prose compensated for a mismatched durable envelope: %+v", observation)
	}
}
