package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/workflow"
)

type workflowInspection struct {
	Workflow workflow.Workflow `json:"workflow"`
	Events   []workflow.Event  `json:"events"`
	Receipt  *review.Receipt   `json:"authoritative_receipt,omitempty"`
}

func runWorkflowCLI(args []string, cwd string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: skynex workflow status|inspect|resume|abort|export|receipt")
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	store, err := workflow.OpenRepositorySQLite(cwd)
	if err != nil {
		return err
	}
	defer store.Close()
	reviews := review.NewSQLiteStore(store.Database())
	switch args[0] {
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
		return workflowResume(store, args[1:])
	case "export":
		return workflowExport(store, reviews, args[1:], out)
	default:
		return fmt.Errorf("unknown workflow command %q", args[0])
	}
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
	fmt.Fprintf(out, "%s\t%s\t%d\n", updated.ID, updated.State, updated.StateVersion)
	return nil
}

func workflowResume(store *workflow.SQLiteStore, args []string) error {
	id, err := requiredWorkflowID(args)
	if err != nil {
		return err
	}
	w, err := store.Get(id)
	if err != nil {
		return err
	}
	if w.State != workflow.StateBlocked {
		return fmt.Errorf("workflow %s is %s; only blocked workflows can resume", id, w.State)
	}
	return fmt.Errorf("workflow %s remains blocked: safe resume requires drift reconciliation and lock reacquisition, which are not implemented", id)
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
		if arg == "--id" || arg == "--out" || arg == "--idempotency-key" {
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
