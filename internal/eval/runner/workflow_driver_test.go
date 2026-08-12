package runner

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/workflow"
)

func TestWorkflowDriverForCaseIsExplicitAndBounded(t *testing.T) {
	base := contracts.Case{Completion: contracts.CompletionConfig{MaxTurns: 3}}
	if config, err := workflowDriverForCase(base); err != nil || config != nil {
		t.Fatalf("absent workflow driver = %+v, %v", config, err)
	}
	base.Extensions = map[string]any{workflowDriverExtension: map[string]any{
		"mode": "managed-detach", "workflow_id": "canary-low", "terminal_state": "receipted", "autonomous_turns": 2,
	}}
	config, err := workflowDriverForCase(base)
	if err != nil || config == nil || config.WorkflowID != "canary-low" || config.AutonomousTurns != 2 {
		t.Fatalf("valid workflow driver = %+v, %v", config, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"unknown field":  func(value map[string]any) { value["extra"] = true },
		"too many turns": func(value map[string]any) { value["autonomous_turns"] = 3 },
		"bad id":         func(value map[string]any) { value["workflow_id"] = "../secret" },
		"bad state":      func(value map[string]any) { value["terminal_state"] = "delivered" },
		"foreground wake": func(value map[string]any) {
			value["mode"] = "foreground"
			value["autonomous_turns"] = 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := map[string]any{
				"mode": "managed-detach", "workflow_id": "canary-low", "terminal_state": "receipted", "autonomous_turns": 2,
			}
			mutate(value)
			testCase := base
			testCase.Extensions = map[string]any{workflowDriverExtension: value}
			if _, err := workflowDriverForCase(testCase); err == nil {
				t.Fatal("invalid workflow driver was accepted")
			}
		})
	}
}

func TestWorkflowAutonomousTurnsRequireSameDurableAgentAndModel(t *testing.T) {
	initial := workflowDriverAssistant("assistant-0", "user-0", "root", "workflow-orchestrator")
	messages := []client.Message{
		workflowDriverUser("user-0", "root", "workflow-orchestrator"),
		initial,
		workflowDriverUser("user-1", "root", "workflow-orchestrator"),
		workflowDriverAssistant("assistant-1", "user-1", "root", "workflow-orchestrator"),
		workflowDriverUser("user-2", "root", "workflow-orchestrator"),
		workflowDriverAssistant("assistant-2", "user-2", "root", "workflow-orchestrator"),
	}
	latest, turns, state := workflowAutonomousTurns(messages, "root", &initial, "workflow-orchestrator", "openai", "gpt-5.6-terra")
	if state != workflowTurnHistoryComplete || turns != 2 || latest == nil || latest.Info.ID != "assistant-2" {
		t.Fatalf("autonomous turns = latest %+v, turns %d, state %d", latest, turns, state)
	}
	for name, mutate := range map[string]func([]client.Message){
		"wrong agent":  func(values []client.Message) { values[2].Info.Agent = "default" },
		"wrong model":  func(values []client.Message) { values[3].Info.ModelID = "other" },
		"wrong parent": func(values []client.Message) { values[3].Info.ParentID = "user-0" },
		"missing part ownership": func(values []client.Message) {
			values[3].Parts[0].MessageID = "other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			copyMessages := append([]client.Message(nil), messages...)
			for index := range copyMessages {
				copyMessages[index].Parts = append([]client.Part(nil), messages[index].Parts...)
			}
			mutate(copyMessages)
			if _, _, state := workflowAutonomousTurns(copyMessages, "root", &initial, "workflow-orchestrator", "openai", "gpt-5.6-terra"); state != workflowTurnHistoryInvalid {
				t.Fatal("invalid autonomous history was accepted")
			}
		})
	}
}

func TestWorkflowAutonomousTurnsTreatsOnlyOneValidTailAsInFlight(t *testing.T) {
	initial := workflowDriverAssistant("assistant-0", "user-0", "root", "workflow-orchestrator")
	base := []client.Message{
		workflowDriverUser("user-0", "root", "workflow-orchestrator"),
		initial,
		workflowDriverUser("user-1", "root", "workflow-orchestrator"),
	}
	latest, turns, state := workflowAutonomousTurns(base, "root", &initial, "workflow-orchestrator", "openai", "gpt-5.6-terra")
	if state != workflowTurnHistoryInFlight || turns != 0 || latest.Info.ID != initial.Info.ID {
		t.Fatalf("user-only tail = latest %s turns %d state %d", latest.Info.ID, turns, state)
	}

	streaming := workflowDriverAssistant("assistant-1", "user-1", "root", "workflow-orchestrator")
	streaming.Info.Finish = ""
	streaming.Info.Time.Completed = 0
	latest, turns, state = workflowAutonomousTurns(append(base, streaming), "root", &initial, "workflow-orchestrator", "openai", "gpt-5.6-terra")
	if state != workflowTurnHistoryInFlight || turns != 0 || latest.Info.ID != initial.Info.ID {
		t.Fatalf("streaming tail = latest %s turns %d state %d", latest.Info.ID, turns, state)
	}

	complete := workflowDriverAssistant("assistant-1", "user-1", "root", "workflow-orchestrator")
	latest, turns, state = workflowAutonomousTurns(append(base, complete), "root", &initial, "workflow-orchestrator", "openai", "gpt-5.6-terra")
	if state != workflowTurnHistoryComplete || turns != 1 || latest.Info.ID != complete.Info.ID {
		t.Fatalf("complete tail = latest %s turns %d state %d", latest.Info.ID, turns, state)
	}

	streaming.Info.Agent = "default"
	if _, _, state = workflowAutonomousTurns(append(base, streaming), "root", &initial, "workflow-orchestrator", "openai", "gpt-5.6-terra"); state != workflowTurnHistoryInvalid {
		t.Fatalf("invalid streaming identity accepted with state %d", state)
	}
}

func TestWorkflowTerminalStateRejectsLiveManagedJobs(t *testing.T) {
	repo := t.TempDir()
	if output, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(workflow.Workflow{ID: "canary-low"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Database().Exec(`UPDATE workflows SET state=? WHERE id=?`, workflow.StateReceipted, "canary-low"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Database().Exec(`INSERT INTO review_candidates(id,workflow_id,tree_oid,policy_hash,record) VALUES('candidate-1','canary-low','tree','policy','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Database().Exec(`INSERT INTO receipts(id,workflow_id,candidate_record_id,receipt) VALUES('receipt-1','canary-low','candidate-1','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Database().Exec(`INSERT INTO receipt_authority(workflow_id,receipt_id) VALUES('canary-low','receipt-1')`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	config := workflowDriverConfig{WorkflowID: "canary-low", TerminalState: string(workflow.StateReceipted)}
	if terminal, err := workflowTerminalState(repo, config); err != nil || !terminal {
		t.Fatalf("terminal = %t, %v", terminal, err)
	}
	store, err = workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkflowJobOperation("job-1", "canary-low", "run", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if terminal, err := workflowTerminalState(repo, config); err != nil || terminal {
		t.Fatalf("terminal with queued job = %t, %v", terminal, err)
	}
}

func TestWorkflowTerminalStateRejectsUnacknowledgedNotification(t *testing.T) {
	repo := t.TempDir()
	if output, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	store, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Create(workflow.Workflow{ID: "canary-low"}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Database().Exec(`UPDATE workflows SET state=? WHERE id=?`, workflow.StateCandidateFrozen, "canary-low"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = store.Database().Exec(
		`INSERT INTO workflow_jobs(id,workflow_id,state,created_at,finished_at,terminal_state) VALUES('job-1','canary-low','succeeded',?,?,?)`,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), workflow.StateCandidateFrozen,
	); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Database().Exec(
		`INSERT INTO workflow_notifications(id,workflow_id,job_id,terminal_state,created_at) VALUES('notice-1','canary-low','job-1',?,?)`,
		workflow.StateCandidateFrozen, now.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	config := workflowDriverConfig{WorkflowID: "canary-low", TerminalState: string(workflow.StateCandidateFrozen)}
	if terminal, err := workflowTerminalState(repo, config); err != nil || terminal {
		t.Fatalf("terminal with unacked notification = %t, %v", terminal, err)
	}
	if _, err = store.Database().Exec(`UPDATE workflow_notifications SET acked_at=? WHERE id='notice-1'`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if terminal, err := workflowTerminalState(repo, config); err != nil || !terminal {
		t.Fatalf("terminal after ack = %t, %v", terminal, err)
	}
}

func workflowDriverUser(id, sessionID, agent string) client.Message {
	return client.Message{
		Info:  client.ResponseInfo{ID: id, SessionID: sessionID, Role: "user", Agent: agent},
		Parts: []client.Part{{ID: "part-" + id, SessionID: sessionID, MessageID: id, Type: "text", Text: "wake"}},
	}
}

func workflowDriverAssistant(id, parentID, sessionID, agent string) client.Message {
	return client.Message{
		Info: client.ResponseInfo{
			ID: id, SessionID: sessionID, Role: "assistant", ParentID: parentID,
			Agent: agent, ProviderID: "openai", ModelID: "gpt-5.6-terra", Finish: "stop",
			Time: client.MessageTime{Created: 1, Completed: 2},
		},
		Parts: []client.Part{{ID: "part-" + id, SessionID: sessionID, MessageID: id, Type: "text", Text: filepath.Base(id)}},
	}
}
