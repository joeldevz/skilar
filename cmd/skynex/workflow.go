package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/approval"
	"github.com/joeldevz/skynex/internal/processregistry"
	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/workflow"
)

type workflowInspection struct {
	Workflow  workflow.Workflow `json:"workflow"`
	Events    []workflow.Event  `json:"events"`
	Receipt   *review.Receipt   `json:"authoritative_receipt,omitempty"`
	RunInput  json.RawMessage   `json:"run_input,omitempty"`
	Approvals []string          `json:"current_approvals,omitempty"`
}

func runWorkflowCLI(args []string, cwd string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: skynex workflow <command>; run `skynex workflow --help` for details")
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		printWorkflowUsage(out)
		return nil
	}
	if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
		return printWorkflowCommandUsage(args[0], out)
	}
	if !workflowCommandKnown(args[0]) {
		return fmt.Errorf("unknown workflow command %q", args[0])
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	path, err := workflow.CanonicalDatabasePath(cwd)
	if err != nil {
		return err
	}
	if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
		if args[0] == "status" {
			if len(args) > 1 {
				return fmt.Errorf("workflow database not found at %s", path)
			}
			fmt.Fprintln(out, "WORKFLOW\tSTATE\tVERSION\tROUTE\tRISK")
			return nil
		}
		if args[0] == "inspect" || args[0] == "receipt" {
			return fmt.Errorf("workflow database not found at %s", path)
		}
	}
	store, err := workflow.OpenRepositorySQLite(cwd)
	if err != nil {
		return err
	}
	defer store.Close()
	reviews := review.NewSQLiteStore(store.Database())
	switch args[0] {
	case "start":
		return workflowStart(store, cwd, args[1:], out)
	case "run":
		return workflowRun(store, cwd, args[1:], out)
	case "review":
		return workflowReview(store, args[1:], out)
	case "deliver":
		return workflowDeliver(store, args[1:], out, nil)
	case "approve":
		return workflowApprove(store, args[1:], out)
	case "revoke-approval":
		return workflowRevokeApproval(store, args[1:], out)
	case "frontier":
		return workflowFrontier(store, args[1:], out)
	case "answer":
		return workflowAnswer(store, args[1:], out)
	case "close-discovery":
		return workflowCloseDiscovery(store, args[1:], out)
	case "status":
		return workflowStatus(store, args[1:], out)
	case "inspect":
		id, err := requiredWorkflowID(args[1:])
		if err != nil {
			return err
		}
		return workflowInspect(store, reviews, id, out)
	case "receipt":
		return workflowReceipt(reviews, args[1:], out)
	case "abort":
		return workflowAbort(store, args[1:], out)
	case "resume":
		return workflowResume(store, cwd, args[1:], out)
	case "export":
		return workflowExport(store, reviews, args[1:], out)
	default:
		return fmt.Errorf("unknown workflow command %q", args[0])
	}
}

func workflowCommandKnown(command string) bool {
	switch command {
	case "start", "run", "review", "deliver", "approve", "revoke-approval", "frontier", "answer", "close-discovery", "status", "inspect", "receipt", "abort", "resume", "export":
		return true
	}
	return false
}

func printWorkflowCommandUsage(command string, out io.Writer) error {
	usage := map[string]string{
		"start":           "Usage: skynex workflow start --id ID --request TEXT --path PATH --check COMMAND --accept COMMAND [--route simple|planned|discovery] [--plan-file FILE] [--wayfinder-file FILE] [--override-actor ACTOR --override-reason REASON] [--model MODEL] [--agent AGENT] [--opencode PATH] [--timeout DURATION]\nSimple requires --id, --request, and repeatable --path, --check, --accept. Planned requires --plan-file. Discovery requires --wayfinder-file.",
		"run":             "Usage: skynex workflow run WORKFLOW_ID",
		"review":          "Usage: skynex workflow review --id WORKFLOW_ID",
		"deliver":         "Usage: skynex workflow deliver --id WORKFLOW_ID --message TEXT --idempotency-key KEY [--author-name NAME --author-email EMAIL]",
		"status":          "Usage: skynex workflow status [WORKFLOW_ID]",
		"inspect":         "Usage: skynex workflow inspect WORKFLOW_ID",
		"receipt":         "Usage: skynex workflow receipt WORKFLOW_ID | --id RECEIPT_ID",
		"approve":         "Usage: skynex workflow approve --id WORKFLOW_ID --action ACTION --actor ACTOR --reason TEXT [--expires DURATION]",
		"revoke-approval": "Usage: skynex workflow revoke-approval --id WORKFLOW_ID --action ACTION --actor ACTOR --reason TEXT",
		"abort":           "Usage: skynex workflow abort WORKFLOW_ID --idempotency-key KEY",
		"resume":          "Usage: skynex workflow resume WORKFLOW_ID --blocker-id ID --idempotency-key KEY",
		"export":          "Usage: skynex workflow export WORKFLOW_ID --out PATH",
		"frontier":        "Usage: skynex workflow frontier --id WORKFLOW_ID",
		"answer":          "Usage: skynex workflow answer --id WORKFLOW_ID --node NODE_ID --answer TEXT --actor ACTOR",
		"close-discovery": "Usage: skynex workflow close-discovery --id WORKFLOW_ID --plan-file FILE",
	}
	value, ok := usage[command]
	if !ok {
		return fmt.Errorf("unknown workflow command %q", command)
	}
	fmt.Fprintln(out, value)
	return nil
}

func printWorkflowUsage(out io.Writer) {
	fmt.Fprintln(out, `Usage: skynex workflow <command> [options]

Commands:
  start               Create a simple, planned, or discovery workflow
  run                 Execute ready slices with OpenCode and verify the result
  review              Review a frozen candidate and issue its receipt
  deliver             Commit the exact receipt-authorized candidate tree
  status              List workflows or show one workflow
  inspect             Show workflow events, inputs, approvals, and authority
  receipt             Show the current or a historical receipt
  approve             Approve an exact high-risk action basis
  revoke-approval     Revoke a current approval
  abort               Stop work and revoke attempts, leases, and approvals
  resume              Reconcile and resume a blocked workflow
  export              Export a workflow summary or receipt
  frontier            Show the next blocking discovery question
  answer              Record an attributed discovery answer
  close-discovery     Close discovery with an explicit execution plan

Run skynex workflow <command> --help for command-specific options.`)
}

func workflowStatus(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	if len(args) > 0 {
		w, err := store.Get(args[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "WORKFLOW\tSTATE\tVERSION\tROUTE\tRISK\n%s\t%s\t%d\t%s\t%s\n", w.ID, w.State, w.StateVersion, w.Route, w.MinimumRisk)
		return nil
	}
	rows, err := store.Database().Query(`SELECT id,state,state_version,route,minimum_risk FROM workflows ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Fprintln(out, "WORKFLOW\tSTATE\tVERSION\tROUTE\tRISK")
	for rows.Next() {
		var w workflow.Workflow
		if err := rows.Scan(&w.ID, &w.State, &w.StateVersion, &w.Route, &w.MinimumRisk); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\t%s\t%d\t%s\t%s\n", w.ID, w.State, w.StateVersion, w.Route, w.MinimumRisk)
	}
	return rows.Err()
}

func workflowInspect(store *workflow.SQLiteStore, reviews *review.SQLiteStore, id string, out io.Writer) error {
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	events, err := store.Events(id)
	if err != nil {
		return err
	}
	inspection := workflowInspection{Workflow: w, Events: events}
	var input []byte
	if e := store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, id).Scan(&input); e == nil {
		inspection.RunInput = input
	}
	rows, e := store.Database().Query(`SELECT action||':'||approval_id FROM current_approvals WHERE workflow_id=? ORDER BY action`, id)
	if e == nil {
		defer rows.Close()
		for rows.Next() {
			var value string
			if rows.Scan(&value) == nil {
				inspection.Approvals = append(inspection.Approvals, value)
			}
		}
	}
	if receipt, e := reviews.Authority(id); e == nil {
		inspection.Receipt = &receipt
	} else if !errors.Is(e, review.ErrNoAuthority) {
		return e
	}
	return writeJSON(out, inspection)
}

func workflowReceipt(reviews *review.SQLiteStore, args []string, out io.Writer) error {
	if id, ok := flagValue(args, "--id"); ok {
		r, err := reviews.Receipt(id)
		if err != nil {
			return err
		}
		return writeJSON(out, r)
	}
	id, err := requiredWorkflowID(args)
	if err != nil {
		return errors.New("receipt requires a workflow ID or --id RECEIPT_ID")
	}
	r, err := reviews.Authority(id)
	if err != nil {
		return err
	}
	return writeJSON(out, r)
}

func workflowAbort(store *workflow.SQLiteStore, args []string, out io.Writer) error {
	id, err := requiredWorkflowID(args)
	if err != nil {
		return err
	}
	key, ok := flagValue(args, "--idempotency-key")
	if !ok || key == "" {
		return errors.New("abort requires --idempotency-key")
	}
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	if w.State == workflow.StateAborted {
		_ = cleanupAbortedWorkflow(store, id)
		events, eventErr := store.Events(id)
		if eventErr != nil {
			return eventErr
		}
		for _, event := range events {
			if event.IdempotencyKey == key && event.To == workflow.StateAborted {
				updated, replayErr := store.Transition(workflow.Transition{WorkflowID: id, ExpectedState: event.From, ExpectedVersion: event.StateVersion - 1, NextState: workflow.StateAborted, IdempotencyKey: key})
				if replayErr != nil {
					return replayErr
				}
				fmt.Fprintf(out, "%s\t%s\t%d\n", updated.ID, updated.State, updated.StateVersion)
				return nil
			}
		}
	}
	if terminal(w.State) {
		return fmt.Errorf("workflow %s is already terminal in %s", id, w.State)
	}
	updated, err := store.Transition(workflow.Transition{WorkflowID: id, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateAborted, IdempotencyKey: key})
	if err != nil {
		return err
	}
	if err = cleanupAbortedWorkflow(store, id); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\t%d\n", updated.ID, updated.State, updated.StateVersion)
	return nil
}

func cleanupAbortedWorkflow(store *workflow.SQLiteStore, id string) error {
	cancelled := processregistry.CancelWorkflow(id)
	_, _ = store.Database().Exec(`UPDATE mutation_attempts SET live=0 WHERE workflow_id=?`, id)
	_, _ = store.Database().Exec(`DELETE FROM leases WHERE resource IN (SELECT 'worktree:'||worktree_id FROM mutation_attempts WHERE workflow_id=?)`, id)
	_ = approval.RevokeAll(store.Database(), id, "workflow-abort", "kill switch", time.Now())
	plan, _ := json.Marshal(map[string]any{"workflow_id": id, "processes_cancelled": cancelled, "attempts_revoked": true, "leases_revoked": true, "approvals_revoked": true})
	_, err := store.Database().Exec(`INSERT INTO abort_cleanup_plans(workflow_id,plan) VALUES(?,?) ON CONFLICT(workflow_id) DO UPDATE SET plan=excluded.plan`, id, plan)
	return err
}

func workflowResume(store *workflow.SQLiteStore, repo string, args []string, out io.Writer) error {
	id, err := requiredWorkflowID(args)
	if err != nil {
		return err
	}
	blocker, ok := flagValue(args, "--blocker-id")
	if !ok || blocker == "" {
		return errors.New("resume requires --blocker-id")
	}
	key, ok := flagValue(args, "--idempotency-key")
	if !ok || key == "" {
		return errors.New("resume requires --idempotency-key")
	}
	updated, err := store.Resume(context.Background(), repo, workflow.ResumeRequest{WorkflowID: id, BlockerID: blocker, IdempotencyKey: key})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\t%s\t%d\n", updated.ID, updated.State, updated.StateVersion)
	return nil
}

func workflowExport(store *workflow.SQLiteStore, reviews *review.SQLiteStore, args []string, out io.Writer) error {
	id, err := requiredWorkflowID(args)
	if err != nil {
		return err
	}
	path, ok := flagValue(args, "--out")
	if !ok || path == "" {
		return errors.New("export requires --out PATH")
	}
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	events, err := store.Events(id)
	if err != nil {
		return err
	}
	value := workflowInspection{Workflow: w, Events: events}
	if r, e := reviews.Authority(id); e == nil {
		value.Receipt = &r
	} else if !errors.Is(e, review.ErrNoAuthority) {
		return e
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err = writeAtomic0600(path, data); err != nil {
		return err
	}
	fmt.Fprintln(out, path)
	return nil
}

func writeAtomic0600(path string, data []byte) error {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("export destination is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	temp, err := os.CreateTemp(dir, ".skynex-export-")
	if err != nil {
		return err
	}
	name := temp.Name()
	ok := false
	defer func() {
		temp.Close()
		if !ok {
			os.Remove(name)
		}
	}()
	if err = temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err = temp.Write(data); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}
func requiredWorkflowID(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--id" || arg == "--out" || arg == "--idempotency-key" || arg == "--blocker-id" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "--") && strings.Contains(arg, "=") {
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			return arg, nil
		}
	}
	return "", errors.New("workflow ID is required")
}
func flagValue(args []string, name string) (string, bool) {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1], true
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"="), true
		}
	}
	return "", false
}
func terminal(state workflow.State) bool {
	return state == workflow.StateDelivered || state == workflow.StateAborted || state == workflow.StateFailed
}
func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
