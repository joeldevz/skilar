package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/joeldevz/skynex/internal/orchestration"
	"github.com/joeldevz/skynex/internal/workflow"
)

func workflowReplan(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	id := valueOrEmpty(args, "--id")
	invalidationID := valueOrEmpty(args, "--finding-id")
	planPath := valueOrEmpty(args, "--plan-file")
	actor := valueOrEmpty(args, "--actor")
	reason := valueOrEmpty(args, "--reason")
	key := valueOrEmpty(args, "--idempotency-key")
	if id == "" || invalidationID == "" || planPath == "" || actor == "" || reason == "" || key == "" {
		return errors.New("replan requires --id, --finding-id, --plan-file, --actor, --reason, and --idempotency-key")
	}
	rawPlan, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	var plan planFile
	if json.Unmarshal(rawPlan, &plan) != nil || len(plan.Slices) == 0 {
		return errors.New("invalid replan plan file")
	}
	var previousVersion uint64
	if err = store.Database().QueryRow(`SELECT COALESCE(MAX(version),0) FROM execution_graphs WHERE workflow_id=?`, id).Scan(&previousVersion); err != nil {
		return err
	}
	targetVersion := previousVersion + 1
	var replayVersion uint64
	if replayErr := store.Database().QueryRow(`SELECT version FROM replan_revisions WHERE workflow_id=? AND idempotency_key=?`, id, key).Scan(&replayVersion); replayErr == nil {
		targetVersion = replayVersion
	}
	graph := orchestration.ExecutionGraph{WorkflowID: id, Version: targetVersion}
	configs := map[string]sliceRunConfig{}
	var acceptance, checks, paths []string
	for _, slice := range plan.Slices {
		if slice.ID == "" || slice.Title == "" || len(slice.AcceptanceCriteria) == 0 || len(slice.Paths) == 0 || len(slice.Checks) == 0 {
			return errors.New("replan slices require id, title, acceptance_criteria, paths, and checks")
		}
		graph.Slices = append(graph.Slices, orchestration.Slice{ID: slice.ID, Title: slice.Title, AcceptanceCriteria: slice.AcceptanceCriteria, Dependencies: slice.Dependencies})
		configs[slice.ID] = sliceRunConfig{Paths: append([]string(nil), slice.Paths...), Checks: append([]string(nil), slice.Checks...)}
		acceptance = append(acceptance, slice.AcceptanceCriteria...)
		checks = append(checks, slice.Checks...)
		paths = append(paths, slice.Paths...)
	}
	if err = orchestration.ValidateExecution(graph); err != nil {
		return err
	}
	var inputRaw []byte
	if err = store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, id).Scan(&inputRaw); err != nil {
		return err
	}
	var input workflowRunInput
	if err = json.Unmarshal(inputRaw, &input); err != nil {
		return err
	}
	input.Acceptance, input.Checks, input.AllowedPaths, input.SliceConfigs = acceptance, checks, paths, configs
	contract := orchestration.ExecutableContract{Destination: input.Request, AcceptanceCriteria: acceptance}
	if err = orchestration.ValidateContract(contract); err != nil {
		return err
	}
	graphRaw, _ := json.Marshal(graph)
	contractRaw, _ := json.Marshal(contract)
	inputRaw, _ = json.Marshal(input)
	result, revision, err := store.Replan(workflow.ReplanRequest{WorkflowID: id, InvalidationID: invalidationID, Actor: actor, Reason: reason, IdempotencyKey: key, Graph: graphRaw, Contract: contractRaw, RunInput: inputRaw, Now: time.Now()})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\tgraph=v%d\tinvalidation=%s\n", result.ID, result.State, revision.Version, revision.InvalidationID)
	return nil
}
