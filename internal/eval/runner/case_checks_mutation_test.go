package runner

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/cases"
	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/judges"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/trace"
)

const (
	publicSuiteCaseCount        = 19
	publicSuiteRequirementCount = 22
)

// This is a harness mutation test, not a model evaluation. It runs every hard
// check from the published suite through the production observer twice: first
// with satisfying evidence and then with the authoritative evidence inverted.
// A status edit is deliberately not one of the mutations.
func TestPublicSuiteHardChecksRejectBehavioralEvidenceMutation(t *testing.T) {
	suiteDir := filepath.Join("..", "..", "..", "eval", "cases", "skynex-orchestrator")
	loaded, err := cases.LoadSuiteContracts(suiteDir)
	if err != nil {
		t.Fatalf("LoadSuiteContracts: %v", err)
	}
	if len(loaded) != publicSuiteCaseCount {
		t.Fatalf("loaded %d public cases, want %d", len(loaded), publicSuiteCaseCount)
	}

	normative := make(map[string]bool)
	covered := make(map[string]bool)
	mutatedChannels := make(map[string]bool)
	for _, testCase := range loaded {
		for _, requirement := range testCase.RequirementIDs {
			normative[requirement] = true
		}
		for _, check := range testCase.BehaviorChecks {
			if !hardCaseCheck(check) {
				continue
			}
			check := check
			testCase := testCase
			t.Run(testCase.ID+"/"+check.ID, func(t *testing.T) {
				probe, channel, err := newCaseCheckMutationProbe(testCase, check)
				if err != nil {
					t.Fatal(err)
				}
				baseline := evaluateMutationProbe(testCase, check, probe)
				if baseline.Status != contracts.CheckStatusPass || !baseline.Hard {
					t.Fatalf("real hard observer rejected satisfying evidence: %+v", baseline)
				}

				probe.invert(testCase, check)
				mutant := evaluateMutationProbe(testCase, check, probe)
				if mutant.Status == contracts.CheckStatusPass {
					t.Fatalf("real hard observer passed after %s evidence was inverted: %+v", channel, mutant)
				}
				if !mutant.Hard {
					t.Fatalf("mutated observer was demoted: %+v", mutant)
				}
				for _, requirement := range check.RequirementIDs {
					covered[requirement] = true
				}
				mutatedChannels[channel] = true
			})
		}
	}

	if len(normative) != publicSuiteRequirementCount {
		t.Fatalf("suite declares %d normative requirements, want %d", len(normative), publicSuiteRequirementCount)
	}
	for requirement := range normative {
		if !covered[requirement] {
			t.Errorf("normative requirement %s has no observer that rejects inverted evidence", requirement)
		}
	}
	for _, channel := range []string{"acceptance", "claims", "durable-tool-output", "filesystem", "session-trace"} {
		if !mutatedChannels[channel] {
			t.Errorf("public hard checks did not exercise a %s mutation", channel)
		}
	}
}

func TestGitStateHardObserverRejectsBeforeAfterMutation(t *testing.T) {
	suiteDir := filepath.Join("..", "..", "..", "eval", "cases", "skynex-orchestrator")
	testCase, err := cases.LoadContract(filepath.Join(suiteDir, "skx_dirty_worktree.yaml"))
	if err != nil {
		t.Fatalf("LoadContract: %v", err)
	}
	before := sandbox.GitStatusEvidence{
		StateDigest: "sha256:state", Head: strings.Repeat("a", 40),
		IndexDigest: "sha256:index", Digest: "sha256:status",
		Entries: []sandbox.GitStatusEntry{{Path: "staged.txt", Kind: "ordinary", IndexStatus: 'M', WorktreeStatus: '.'}},
	}

	for _, test := range []struct {
		name   string
		mutate func(*sandbox.GitStatusEvidence)
	}{
		{name: "head", mutate: func(after *sandbox.GitStatusEvidence) { after.Head = strings.Repeat("b", 40) }},
		{name: "index", mutate: func(after *sandbox.GitStatusEvidence) { after.IndexDigest = "sha256:other-index" }},
		{name: "pre-existing-entry", mutate: func(after *sandbox.GitStatusEvidence) { after.Entries = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			after := before
			after.Entries = append([]sandbox.GitStatusEntry(nil), before.Entries...)
			baseline := withGitStateCheck(judges.Verdict{Status: judges.OutcomePass}, *testCase, before, after, nil)
			if len(baseline.Checks) != 1 || baseline.Checks[0].Outcome != judges.OutcomePass {
				t.Fatalf("unchanged Git before/after evidence did not pass: %+v", baseline)
			}

			test.mutate(&after)
			mutant := withGitStateCheck(judges.Verdict{Status: judges.OutcomePass}, *testCase, before, after, nil)
			if len(mutant.Checks) != 1 || mutant.Checks[0].Outcome != judges.OutcomeFail || !mutant.Checks[0].Hard || !mutant.HardFailure {
				t.Fatalf("real Git hard observer accepted mutated before/after evidence: %+v", mutant)
			}
		})
	}
}

type caseCheckMutationProbe struct {
	before        sandbox.Snapshot
	after         sandbox.Snapshot
	setupResults  []sandbox.CommandResult
	oracleResults []sandbox.CommandResult
	collected     *trace.Trace
	finalText     string
	verdict       judges.Verdict
}

func newCaseCheckMutationProbe(testCase contracts.Case, check contracts.Check) (caseCheckMutationProbe, string, error) {
	probe := caseCheckMutationProbe{
		before:        mutationSnapshot(nil, "before"),
		after:         mutationSnapshot(nil, "after"),
		setupResults:  acceptedMutationResults(testCase.Setup.Commands),
		oracleResults: acceptedMutationResults(testCase.Oracle.Commands),
		collected:     emptyMutationTrace(),
		finalText:     "work remains blocked pending evidence",
		verdict:       judges.Verdict{Status: judges.OutcomePass},
	}

	switch check.Type {
	case "contains_all":
		probe.finalText = strings.Join(check.Patterns, "\n")
		return probe, "claims", nil
	case "not_contains":
		probe.finalText = "observer-safe response"
		return probe, "claims", nil
	case "not_contains_pattern":
		probe.finalText = "no workflow was run"
		return probe, "claims", nil
	case "tool_called", "tool_called_min":
		minimum := 1
		if check.Min != nil {
			minimum = *check.Min
		} else if value, ok := integerValue(check.Value); ok {
			minimum = value
		}
		for i := 0; i < minimum; i++ {
			addMutationTool(probe.collected, check.Tool, "")
		}
		return probe, "session-trace", nil
	case "tool_not_called":
		return probe, "session-trace", nil
	case "tool_output_contains_all":
		addMutationTool(probe.collected, check.Tool, strings.Join(check.Patterns, "\n"))
		return probe, "durable-tool-output", nil
	case "tool_call_order":
		if len(check.Patterns) < 2 {
			return probe, "", fmt.Errorf("%s/%s has fewer than two ordered events", testCase.ID, check.ID)
		}
		if commandIndex(testCase.Setup.Commands, check.Patterns[0]) < 0 && commandIndex(testCase.Oracle.Commands, check.Patterns[1]) < 0 {
			addMutationTool(probe.collected, check.Patterns[0], "")
			addMutationTool(probe.collected, check.Patterns[1], "")
			return probe, "session-trace", nil
		}
		return probe, "acceptance", nil
	case "subagent_count":
		minimum := intValue(check.Min, 0)
		for i := 0; i < minimum; i++ {
			addMutationChild(probe.collected, check.Patterns)
		}
		return probe, "session-trace", nil
	case "expected_diff":
		probe.after = mutationSnapshot(testCase.Oracle.ExpectedChanges, "after")
		return probe, "filesystem", nil
	case "file_exists":
		probe.after = mutationSnapshot([]string{check.Path}, "after")
		return probe, "filesystem", nil
	case "no_false_success":
		return probe, "acceptance", nil
	default:
		return probe, "", fmt.Errorf("%s/%s uses unsupported mutation check type %q", testCase.ID, check.ID, check.Type)
	}
}

func (probe *caseCheckMutationProbe) invert(testCase contracts.Case, check contracts.Check) {
	switch check.Type {
	case "contains_all":
		probe.finalText = "observer mutation omits the required observations"
	case "not_contains":
		probe.finalText = check.Patterns[0]
	case "not_contains_pattern":
		probe.finalText = "executed skynex workflow"
	case "tool_called", "tool_called_min":
		probe.collected = emptyMutationTrace()
	case "tool_not_called":
		addMutationTool(probe.collected, check.Tool, "")
	case "tool_output_contains_all":
		probe.collected.Tools[0].Output = ""
		probe.collected.Sessions[0].Messages[0].Parts[0].State.Output = ""
	case "tool_call_order":
		setupIndex := commandIndex(testCase.Setup.Commands, check.Patterns[0])
		oracleIndex := commandIndex(testCase.Oracle.Commands, check.Patterns[1])
		if setupIndex >= 0 || oracleIndex >= 0 {
			if setupIndex >= 0 {
				probe.setupResults[setupIndex].Completed = false
			} else {
				probe.oracleResults[oracleIndex].Completed = false
			}
			return
		}
		probe.collected = emptyMutationTrace()
		addMutationTool(probe.collected, check.Patterns[1], "")
		addMutationTool(probe.collected, check.Patterns[0], "")
	case "subagent_count":
		minimum := intValue(check.Min, 0)
		probe.collected = emptyMutationTrace()
		if minimum == 0 {
			addMutationChild(probe.collected, check.Patterns)
		}
	case "expected_diff":
		paths := append([]string(nil), testCase.Oracle.ExpectedChanges...)
		paths = append(paths, "observer-mutation.txt")
		probe.after = mutationSnapshot(paths, "mutated-after")
	case "file_exists":
		probe.after = mutationSnapshot(nil, "mutated-after")
	case "no_false_success":
		probe.finalText = "Done successfully; verified."
		if len(probe.oracleResults) != 0 {
			probe.oracleResults[0].Completed = false
		} else {
			addMutationTool(probe.collected, "observer_failure", "")
			probe.collected.Tools[len(probe.collected.Tools)-1].Status = "error"
			probe.collected.Tools[len(probe.collected.Tools)-1].Error = "mutated failure"
		}
	}
}

func evaluateMutationProbe(testCase contracts.Case, check contracts.Check, probe caseCheckMutationProbe) contracts.CheckResult {
	testCase.BehaviorChecks = []contracts.Check{check}
	evidence := make([]contracts.EvidenceItem, 0, len(check.EvidenceIDs))
	for _, id := range check.EvidenceIDs {
		evidence = append(evidence, contracts.EvidenceItem{ID: id, Complete: true})
	}
	results := evaluateCaseChecks(
		testCase, probe.before, probe.after, probe.setupResults, probe.oracleResults,
		probe.collected, probe.finalText, probe.verdict, evidence,
	)
	return results[0]
}

func acceptedMutationResults(commands []contracts.Command) []sandbox.CommandResult {
	results := make([]sandbox.CommandResult, len(commands))
	for i, command := range commands {
		exit := 0
		if len(command.ExpectedExit) != 0 {
			exit = command.ExpectedExit[0]
		}
		results[i] = sandbox.CommandResult{
			ID: command.ID, Started: true, Completed: true, ExitCode: exit, CleanProcessTree: true,
		}
	}
	return results
}

func mutationSnapshot(paths []string, label string) sandbox.Snapshot {
	entries := make([]sandbox.Entry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, sandbox.Entry{Path: path, Kind: sandbox.EntryFile, Mode: 0o644, SHA256: "sha256:" + label})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return sandbox.Snapshot{Digest: "sha256:" + label, Entries: entries, FileCount: len(entries)}
}

func emptyMutationTrace() *trace.Trace {
	return &trace.Trace{
		RootSessionID: "root", TelemetryComplete: true,
		Sessions: []trace.SessionTrace{{Session: client.Session{ID: "root"}, Status: client.SessionStatus{Type: "idle"}}},
	}
}

func addMutationTool(collected *trace.Trace, name, output string) {
	toolName, input := mutationToolIdentity(name)
	index := len(collected.Tools) + 1
	messageID := fmt.Sprintf("assistant-%03d", index)
	partID := fmt.Sprintf("tool-%03d", index)
	callID := fmt.Sprintf("call-%03d", index)
	part := client.Part{
		ID: partID, SessionID: "root", MessageID: messageID, Type: "tool", CallID: callID, Tool: toolName,
		State: client.ToolState{Status: "completed", Input: input, Output: output, Time: client.PartTime{Start: int64(index)}},
	}
	collected.Sessions[0].Messages = append(collected.Sessions[0].Messages, client.Message{
		Info:  client.ResponseInfo{ID: messageID, SessionID: "root", Role: "assistant", Finish: "stop"},
		Parts: []client.Part{part},
	})
	collected.Tools = append(collected.Tools, trace.ToolCall{
		SessionID: "root", MessageID: messageID, PartID: partID, CallID: callID,
		Tool: toolName, Status: "completed", Input: input, Output: output,
	})
}

func mutationToolIdentity(name string) (string, json.RawMessage) {
	commands := map[string]string{
		"git_status":         "git status --short",
		"git_commit":         "git commit -m mutation",
		"git_add":            "git add mutation.txt",
		"git_reset":          "git reset HEAD",
		"git_restore_staged": "git restore --staged staged.txt",
		"git_clean":          "git clean -fd",
		"git_push":           "git push",
		"skynex_workflow":    "skynex workflow run",
		"github_pr":          "gh pr create",
		"validation_run":     "go test ./...",
	}
	if command, ok := commands[name]; ok {
		encoded, _ := json.Marshal(map[string]string{"command": command})
		return "bash", encoded
	}
	if name == "first_write" {
		return "edit", json.RawMessage(`{"filePath":"mutation.txt"}`)
	}
	return "suite_" + name, nil
}

func addMutationChild(collected *trace.Trace, patterns []string) {
	index := len(collected.Sessions)
	id := fmt.Sprintf("child-%03d", index)
	messageID := id + "-assistant"
	collected.Sessions[0].Children = append(collected.Sessions[0].Children, id)
	collected.Sessions = append(collected.Sessions, trace.SessionTrace{
		Session: client.Session{ID: id, ParentID: "root"}, Status: client.SessionStatus{Type: "idle"},
		Messages: []client.Message{{
			Info: client.ResponseInfo{
				ID: messageID, SessionID: id, Role: "assistant", Finish: "stop",
				ProviderID: "test-provider", ModelID: "test-model",
			},
			Parts: []client.Part{
				{ID: id + "-text", SessionID: id, MessageID: messageID, Type: "text", Text: strings.Join(patterns, " ")},
				{ID: id + "-finish", SessionID: id, MessageID: messageID, Type: "step-finish", Reason: "stop"},
			},
		}},
	})
}
