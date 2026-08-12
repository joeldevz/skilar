package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"time"
)

type invocation struct {
	AttemptID string `json:"attempt_id"`
	Status    string `json:"status"`
}

type job struct {
	Operation     string
	State         string
	TerminalState string
}

type inspection struct {
	Workflow struct {
		ID    string
		State string
	}
	AuthoritativeReceipt json.RawMessage `json:"authoritative_receipt"`
	Invocations          []invocation
	Jobs                 []job
	RunInput             json.RawMessage `json:"run_input"`
}

type sliceRunConfig struct {
	Paths  []string
	Checks []string
}

type runInput struct {
	Request, Model, Agent, Executable string
	ModelExplicit, AgentExplicit      bool
	ResultTransport                   string
	Acceptance, Checks, AllowedPaths  []string
	Timeout                           time.Duration
	Seal                              json.RawMessage
	SliceConfigs                      map[string]sliceRunConfig
}

const expectedRequest = "Change only button.go so SubmitButtonColor is #EF4444"
const expectedPath = "button.go"

type repeatedFlag []string

func (values *repeatedFlag) String() string { return strings.Join(*values, ",") }
func (values *repeatedFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	id := flag.String("id", "", "expected workflow ID")
	state := flag.String("state", "", "expected workflow state")
	receipt := flag.String("receipt", "either", "yes, no, or either")
	minimumInvocations := flag.Int("min-invocations", 0, "minimum invocation count")
	sameAttemptStatuses := flag.String("same-attempt-statuses", "", "comma-separated statuses that must share one attempt")
	var requiredStatuses repeatedFlag
	var requiredJobs repeatedFlag
	flag.Var(&requiredStatuses, "require-status", "required invocation status; repeatable")
	flag.Var(&requiredJobs, "require-job", "required operation:state:terminal-state; repeatable")
	flag.Parse()

	if *id == "" || *state == "" {
		fail("--id and --state are required")
	}
	command := exec.Command("skynex", "workflow", "inspect", *id)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	raw, err := command.Output()
	if err != nil {
		fail("inspect failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var observed inspection
	if err := json.Unmarshal(raw, &observed); err != nil {
		fail("decode inspect JSON: %v", err)
	}
	if observed.Workflow.ID != *id || observed.Workflow.State != *state {
		fail("workflow = %s/%s, want %s/%s", observed.Workflow.ID, observed.Workflow.State, *id, *state)
	}
	validateRunInput(observed.RunInput)
	hasReceipt := len(observed.AuthoritativeReceipt) != 0 && string(observed.AuthoritativeReceipt) != "null"
	if *receipt == "yes" && !hasReceipt {
		fail("authoritative receipt is missing")
	}
	if *receipt == "no" && hasReceipt {
		fail("unexpected authoritative receipt")
	}
	if len(observed.Invocations) < *minimumInvocations {
		fail("invocations = %d, want at least %d", len(observed.Invocations), *minimumInvocations)
	}
	for _, wanted := range requiredStatuses {
		if !hasInvocationStatus(observed.Invocations, wanted) {
			fail("required invocation status %q is missing", wanted)
		}
	}
	if *sameAttemptStatuses != "" {
		wanted := splitNonEmpty(*sameAttemptStatuses)
		attempt := ""
		for _, status := range wanted {
			current, ok := attemptForStatus(observed.Invocations, status)
			if !ok {
				fail("required same-attempt status %q is missing", status)
			}
			if attempt == "" {
				attempt = current
			} else if current != attempt {
				fail("statuses %q do not share one attempt", *sameAttemptStatuses)
			}
		}
	}
	for _, wanted := range requiredJobs {
		parts := strings.Split(wanted, ":")
		if len(parts) != 3 || !hasJob(observed.Jobs, parts[0], parts[1], parts[2]) {
			fail("required job %q is missing", wanted)
		}
	}
	fmt.Printf("workflow %s is %s with %d invocations and %d jobs\n", *id, *state, len(observed.Invocations), len(observed.Jobs))
}

func validateRunInput(raw json.RawMessage) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input runInput
	if err := decoder.Decode(&input); err != nil {
		fail("decode run input: %v", err)
	}
	wantCommand := []string{"go test ./..."}
	wantPaths := []string{expectedPath}
	wantSlices := map[string]sliceRunConfig{"slice_main": {Paths: wantPaths, Checks: wantCommand}}
	if input.Request != expectedRequest || input.Model != "" || input.ModelExplicit ||
		input.Agent != "workflow-worker" || input.AgentExplicit || input.Executable != "./fake-opencode" ||
		input.ResultTransport != "skynex-result-file-v1" || input.Timeout != 45*time.Second ||
		!reflect.DeepEqual(input.Acceptance, wantCommand) || !reflect.DeepEqual(input.Checks, wantCommand) ||
		!reflect.DeepEqual(input.AllowedPaths, wantPaths) || !reflect.DeepEqual(input.SliceConfigs, wantSlices) || len(input.Seal) == 0 {
		fail("persisted run input differs from the evaluator-owned authority contract")
	}
}

func hasInvocationStatus(invocations []invocation, wanted string) bool {
	for _, item := range invocations {
		if item.Status == wanted {
			return true
		}
	}
	return false
}

func attemptForStatus(invocations []invocation, wanted string) (string, bool) {
	for _, item := range invocations {
		if item.Status == wanted && item.AttemptID != "" {
			return item.AttemptID, true
		}
	}
	return "", false
}

func hasJob(jobs []job, operation, state, terminal string) bool {
	for _, item := range jobs {
		if item.Operation == operation && item.State == state && item.TerminalState == terminal {
			return true
		}
	}
	return false
}

func splitNonEmpty(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
