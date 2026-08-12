package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/workflow"
)

const (
	workflowDriverExtension = "x-workflow-driver-v1"
	workflowDriverPoll      = 100 * time.Millisecond
)

type workflowDriverConfig struct {
	Mode            string `json:"mode"`
	WorkflowID      string `json:"workflow_id"`
	TerminalState   string `json:"terminal_state"`
	AutonomousTurns int    `json:"autonomous_turns"`
}

type workflowTurnHistoryState uint8

const (
	workflowTurnHistoryInvalid workflowTurnHistoryState = iota
	workflowTurnHistoryInFlight
	workflowTurnHistoryComplete
)

func workflowDriverForCase(testCase contracts.Case) (*workflowDriverConfig, error) {
	raw, present := testCase.Extensions[workflowDriverExtension]
	if !present {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, errors.New("workflow driver extension is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var config workflowDriverConfig
	if err := decoder.Decode(&config); err != nil {
		return nil, errors.New("workflow driver extension is invalid")
	}
	if trailingErr := decoder.Decode(new(any)); !errors.Is(trailingErr, io.EOF) {
		return nil, errors.New("workflow driver extension is invalid")
	}
	if config.Mode != "foreground" && config.Mode != "managed-detach" {
		return nil, errors.New("workflow driver mode is invalid")
	}
	if config.WorkflowID == "" || strings.TrimSpace(config.WorkflowID) != config.WorkflowID {
		return nil, errors.New("workflow driver workflow_id is invalid")
	}
	for _, r := range config.WorkflowID {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return nil, errors.New("workflow driver workflow_id is invalid")
		}
	}
	if config.TerminalState != string(workflow.StateCandidateFrozen) &&
		config.TerminalState != string(workflow.StateReceipted) &&
		config.TerminalState != string(workflow.StateBlocked) {
		return nil, errors.New("workflow driver terminal_state is invalid")
	}
	if config.AutonomousTurns < 0 || config.AutonomousTurns > 2 ||
		config.AutonomousTurns+1 > testCase.Completion.MaxTurns {
		return nil, errors.New("workflow driver autonomous_turns exceeds the case turn budget")
	}
	if config.Mode == "foreground" && config.AutonomousTurns != 0 {
		return nil, errors.New("foreground workflow driver cannot request autonomous turns")
	}
	if config.Mode == "managed-detach" && config.AutonomousTurns == 0 {
		return nil, errors.New("managed-detach workflow driver requires an autonomous turn")
	}
	return &config, nil
}

// waitForWorkflowDriverCompletion keeps the original OpenCode session alive
// while the evaluator-owned workflow plugin turns terminal job notifications
// into additional root-session prompts. The POST response is never treated as
// authority: every autonomous turn is read back from durable message history.
func waitForWorkflowDriverCompletion(
	ctx context.Context,
	api Runtime,
	workspacePath, sessionID string,
	initial *client.Response,
	testCase contracts.Case,
	config workflowDriverConfig,
) (*client.Response, error) {
	if ctx == nil || api == nil || initial == nil || initial.Info.ID == "" {
		return initial, newCodedEvaluationError(evaluationErrorWorkflowDriverInvalid)
	}
	providerID, modelID, err := contracts.ParseModelSelection(testCase.Agent.Model)
	if err != nil {
		return initial, newCodedEvaluationError(evaluationErrorWorkflowDriverInvalid)
	}
	ticker := time.NewTicker(workflowDriverPoll)
	defer ticker.Stop()
	for {
		messages, listErr := api.GetMessagesContext(ctx, sessionID)
		if listErr != nil {
			if ctx.Err() != nil {
				return initial, ctx.Err()
			}
			return initial, newCodedEvaluationError(evaluationErrorWorkflowDriverInvalid)
		}
		latest, turns, state := workflowAutonomousTurns(messages, sessionID, initial, testCase.Agent.Name, providerID, modelID)
		if state == workflowTurnHistoryInvalid || turns > config.AutonomousTurns {
			return initial, newCodedEvaluationError(evaluationErrorWorkflowDriverInvalid)
		}
		if turns == config.AutonomousTurns {
			terminal, terminalErr := workflowTerminalState(workspacePath, config)
			if terminalErr != nil {
				return initial, newCodedEvaluationError(evaluationErrorWorkflowDriverInvalid)
			}
			if terminal {
				return latest, nil
			}
		}
		select {
		case <-ctx.Done():
			return initial, ctx.Err()
		case <-ticker.C:
		}
	}
}

func workflowAutonomousTurns(
	messages []client.Message,
	sessionID string,
	initial *client.Response,
	agent, providerID, modelID string,
) (*client.Response, int, workflowTurnHistoryState) {
	if initial == nil {
		return initial, 0, workflowTurnHistoryInvalid
	}
	initialIndex := -1
	for index := range messages {
		if messages[index].Info.ID == initial.Info.ID {
			if !durableAssistantMatches(messages[index], sessionID, initial) {
				return initial, 0, workflowTurnHistoryInvalid
			}
			initialIndex = index
			break
		}
	}
	if initialIndex < 0 {
		return initial, 0, workflowTurnHistoryInvalid
	}
	latest := initial
	turns := 0
	for index := initialIndex + 1; index < len(messages); {
		user := messages[index]
		if !validWorkflowWakeUser(user, sessionID, agent) {
			return initial, turns, workflowTurnHistoryInvalid
		}
		if index+1 >= len(messages) {
			return latest, turns, workflowTurnHistoryInFlight
		}
		assistant := messages[index+1]
		if validWorkflowWakeAssistant(assistant, sessionID, user.Info.ID, agent, providerID, modelID) {
			copyMessage := assistant
			latest = &copyMessage
			turns++
			index += 2
			continue
		}
		if index+2 == len(messages) && validWorkflowWakeAssistantInFlight(assistant, sessionID, user.Info.ID, agent, providerID, modelID) {
			return latest, turns, workflowTurnHistoryInFlight
		}
		return initial, turns, workflowTurnHistoryInvalid
	}
	return latest, turns, workflowTurnHistoryComplete
}

func validWorkflowWakeUser(message client.Message, sessionID, agent string) bool {
	if message.Info.ID == "" || message.Info.Role != "user" || message.Info.SessionID != sessionID || message.Info.Agent != agent {
		return false
	}
	for _, part := range message.Parts {
		if part.ID == "" || part.Type == "" || part.SessionID != sessionID || part.MessageID != message.Info.ID {
			return false
		}
	}
	return len(message.Parts) != 0
}

func validWorkflowWakeAssistant(message client.Message, sessionID, parentID, agent, providerID, modelID string) bool {
	info := message.Info
	if info.ID == "" || info.Role != "assistant" || info.SessionID != sessionID || info.ParentID != parentID ||
		info.ProviderID != providerID || info.ModelID != modelID || info.Agent != agent || info.Error != nil ||
		info.Finish != "stop" || info.Time.Completed == 0 {
		return false
	}
	for _, part := range message.Parts {
		if part.ID == "" || part.Type == "" || part.SessionID != sessionID || part.MessageID != info.ID {
			return false
		}
	}
	return len(message.Parts) != 0
}

func validWorkflowWakeAssistantInFlight(message client.Message, sessionID, parentID, agent, providerID, modelID string) bool {
	info := message.Info
	if info.ID == "" || info.Role != "assistant" || info.SessionID != sessionID || info.ParentID != parentID ||
		info.ProviderID != providerID || info.ModelID != modelID || info.Agent != agent || info.Error != nil ||
		info.Finish != "" || info.Time.Completed != 0 {
		return false
	}
	for _, part := range message.Parts {
		if part.ID == "" || part.Type == "" || part.SessionID != sessionID || part.MessageID != info.ID {
			return false
		}
	}
	return true
}

func workflowTerminalState(workspacePath string, config workflowDriverConfig) (bool, error) {
	store, err := workflow.OpenRepositorySQLiteLiveReadOnly(workspacePath)
	if err != nil {
		return false, err
	}
	defer store.Close()
	return store.TerminalQuiescent(config.WorkflowID, workflow.State(config.TerminalState))
}

func workflowDriverError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w", newCodedEvaluationError(evaluationErrorWorkflowDriverInvalid))
}
