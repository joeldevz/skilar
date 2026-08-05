package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/verification"
	"github.com/joeldevz/skynex/internal/workflow"
)

func workflowRetryVerification(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	id, ok := flagValue(args, "--id")
	if !ok || id == "" {
		return errors.New("retry-verification requires --id")
	}
	checkID, ok := flagValue(args, "--check-id")
	if !ok || checkID == "" {
		return errors.New("retry-verification requires --check-id")
	}
	replacement, ok := flagValue(args, "--replacement")
	if !ok || strings.TrimSpace(replacement) == "" {
		return errors.New("retry-verification requires --replacement")
	}
	actor, ok := flagValue(args, "--actor")
	if !ok || strings.TrimSpace(actor) == "" {
		return errors.New("retry-verification requires --actor")
	}
	reason, ok := flagValue(args, "--reason")
	if !ok || strings.TrimSpace(reason) == "" {
		return errors.New("retry-verification requires --reason")
	}
	key, ok := flagValue(args, "--idempotency-key")
	if !ok || key == "" {
		return errors.New("retry-verification requires --idempotency-key")
	}
	var priorRevision workflow.VerificationRevision
	if err := store.Database().QueryRow(`SELECT workflow_id,revision,candidate_tree,check_id,previous_command,replacement_command,actor,reason,idempotency_key,attempt_id,fencing_token,created_at FROM verification_contract_revisions WHERE workflow_id=? AND idempotency_key=?`, id, key).Scan(&priorRevision.WorkflowID, &priorRevision.Revision, &priorRevision.CandidateTree, &priorRevision.CheckID, &priorRevision.PreviousCommand, &priorRevision.ReplacementCommand, &priorRevision.Actor, &priorRevision.Reason, &priorRevision.IdempotencyKey, &priorRevision.AttemptID, &priorRevision.FencingToken, &priorRevision.CreatedAt); err == nil {
		if priorRevision.CheckID != checkID || priorRevision.ReplacementCommand != replacement || priorRevision.Actor != actor || priorRevision.Reason != reason {
			return errors.New("retry-verification idempotency key conflicts with an existing request")
		}
		w, getErr := store.Get(id)
		if getErr != nil {
			return getErr
		}
		replayed := verification.Result{}
		if w.State == workflow.StateVerifying {
			var resumedInputRaw []byte
			if getErr = store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, id).Scan(&resumedInputRaw); getErr != nil {
				return getErr
			}
			var resumedInput workflowRunInput
			if getErr = json.Unmarshal(resumedInputRaw, &resumedInput); getErr != nil {
				return getErr
			}
			if replayed, getErr = runRevisedVerification(store, id, resumedInput, priorRevision.CandidateTree); getErr != nil {
				return getErr
			}
		} else {
			var replayedRaw []byte
			if getErr = store.Database().QueryRow(`SELECT result FROM verification_runs WHERE workflow_id=?`, id).Scan(&replayedRaw); getErr != nil {
				return getErr
			}
			if getErr = json.Unmarshal(replayedRaw, &replayed); getErr != nil {
				return getErr
			}
		}
		if !replayed.Passed {
			return errors.New("retry-verification replacement check failed")
		}
		fmt.Fprintf(out, "%s\tverification-revision-%d\tidempotent\n", id, priorRevision.Revision)
		return nil
	}

	var currentRaw, inputRaw []byte
	if err := store.Database().QueryRow(`SELECT result FROM verification_runs WHERE workflow_id=?`, id).Scan(&currentRaw); err != nil {
		return err
	}
	if err := store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, id).Scan(&inputRaw); err != nil {
		return err
	}
	var previous verification.Result
	if err := json.Unmarshal(currentRaw, &previous); err != nil {
		return err
	}
	if previous.Passed || previous.Candidate.TreeOID == "" {
		return errors.New("retry-verification requires a failed verification result")
	}
	var failed *verification.Evidence
	for i := range previous.Evidence {
		item := &previous.Evidence[i]
		if item.ID == checkID {
			failed = item
			break
		}
	}
	if failed == nil || failed.Kind != "check" || failed.ExitCode == 0 || failed.Command != "sh" || len(failed.Args) != 2 || failed.Args[0] != "-c" {
		return errors.New("retry-verification requires the exact failed check evidence id")
	}
	oldCommand := failed.Args[1]
	var input workflowRunInput
	if err := json.Unmarshal(inputRaw, &input); err != nil {
		return err
	}
	match := -1
	for i, command := range input.Checks {
		if command == oldCommand {
			if match >= 0 {
				return errors.New("retry-verification cannot replace an ambiguous duplicate check")
			}
			match = i
		}
	}
	if match < 0 {
		return errors.New("retry-verification failed check is absent from the current contract")
	}
	current, err := gitcandidate.Freeze(input.Seal, gitcandidate.Policy{})
	if err != nil {
		return err
	}
	if current.TreeOID != previous.Candidate.TreeOID {
		return fmt.Errorf("retry-verification candidate changed: expected %s got %s", previous.Candidate.TreeOID, current.TreeOID)
	}
	input.Checks[match] = replacement
	updatedInput, err := json.Marshal(input)
	if err != nil {
		return err
	}
	attemptID, err := randomVerificationID("verification-attempt-")
	if err != nil {
		return err
	}
	fencing, err := randomVerificationID("")
	if err != nil {
		return err
	}
	revision, already, err := store.RetryVerification(context.Background(), workflow.RetryVerificationRequest{
		WorkflowID: id, CandidateTree: previous.Candidate.TreeOID, CheckID: checkID,
		PreviousCommand: oldCommand, ReplacementCommand: replacement, Actor: actor,
		Reason: reason, IdempotencyKey: key, AttemptID: attemptID, FencingToken: fencing,
		PreviousResult: currentRaw, UpdatedRunInput: updatedInput,
	}, time.Now())
	if err != nil {
		return err
	}
	if already {
		fmt.Fprintf(out, "%s\tverification-revision-%d\tidempotent\n", id, revision.Revision)
		return nil
	}
	result, err := runRevisedVerification(store, id, input, previous.Candidate.TreeOID)
	if err != nil {
		return err
	}
	if !result.Passed {
		return errors.New("retry-verification replacement check failed")
	}
	fmt.Fprintf(out, "%s\t%s\tverification-revision-%d\n", id, workflow.StateCandidateFrozen, revision.Revision)
	return nil
}

func runRevisedVerification(store *workflow.SQLiteStore, id string, input workflowRunInput, expectedTree string) (verification.Result, error) {
	plan := verification.Plan{Timeout: input.Timeout}
	for _, command := range input.Checks {
		plan.Checks = append(plan.Checks, verification.Command{Name: "sh", Args: []string{"-c", command}})
	}
	for _, command := range input.Acceptance {
		plan.Acceptance = append(plan.Acceptance, verification.Command{Name: "sh", Args: []string{"-c", command}})
	}
	result, err := (&verification.Runner{Store: store, EngineVersion: "workflow-cli-v1", RiskPolicy: review.RiskPolicy{}}).Run(context.Background(), id, input.Seal, plan)
	if err != nil {
		return result, err
	}
	if result.Candidate.TreeOID != expectedTree {
		return result, errors.New("retry-verification changed the candidate tree")
	}
	return result, nil
}

func randomVerificationID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}
