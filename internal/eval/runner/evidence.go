package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/judges"
	"github.com/joeldevz/skynex/internal/eval/metrics"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	"github.com/joeldevz/skynex/internal/eval/trace"
)

func runOracleCommands(ctx context.Context, workspace *sandbox.Workspace, commands []contracts.Command, closures ...*ExecutableClosure) []sandbox.CommandResult {
	results := make([]sandbox.CommandResult, 0, len(commands))
	for _, command := range mapCommands(commands, closures...) {
		results = append(results, workspace.Run(ctx, command))
	}
	return results
}

func usageFromTrace(collected *trace.Trace, table metrics.PricingTable) (contracts.Usage, contracts.Coordination) {
	sessions := make([]metrics.SessionUsage, 0, len(collected.Sessions))
	sequence := 0
	commandSeen := make(map[string]int)
	readSeen := make(map[string]int)
	coordination := contracts.Coordination{ToolCalls: len(collected.Tools), Retries: len(collected.Retries)}
	for _, sessionTrace := range collected.Sessions {
		session := metrics.SessionUsage{SessionID: sessionTrace.Session.ID, ParentID: sessionTrace.Session.ParentID}
		if session.ParentID != "" {
			coordination.SubagentCalls++
		}
		for _, message := range sessionTrace.Messages {
			for _, part := range message.Parts {
				if part.Type != "step-finish" || !part.Tokens.Present {
					continue
				}
				tokens := metrics.TokenUsage{
					Input: int64(part.Tokens.Input), Output: int64(part.Tokens.Output),
					Reasoning: int64(part.Tokens.Reasoning), CacheRead: int64(part.Tokens.CacheRead),
					CacheWrite: int64(part.Tokens.CacheWrite),
				}
				providerCost := metrics.CostValue{Source: metrics.CostSourceProvider, Reason: "provider_cost_unavailable"}
				if part.Cost != 0 || rawHasField(part.Raw, "cost") {
					providerCost = metrics.CostValue{Available: true, USD: part.Cost, Source: metrics.CostSourceProvider}
				}
				duration := int64(0)
				if part.Time.End >= part.Time.Start {
					duration = part.Time.End - part.Time.Start
				}
				sequence++
				messageID := message.Info.ID + ":" + part.ID
				session.Messages = append(session.Messages, metrics.MessageUsage{
					SessionID: session.SessionID, MessageID: messageID, Sequence: sequence,
					Provider: message.Info.ProviderID, Model: message.Info.ModelID,
					Tokens: tokens, ProviderCost: providerCost,
					CalculatedCost: table.CalculateCost(tokens, message.Info.ProviderID, message.Info.ModelID),
					DurationMS:     duration,
				})
			}
		}
		sessions = append(sessions, session)
	}
	for _, tool := range collected.Tools {
		name := strings.ToLower(tool.Tool)
		canonical := name + ":" + string(tool.Input)
		if name == "bash" || name == "shell" {
			commandSeen[canonical]++
		}
		if name == "read" {
			readSeen[canonical]++
		}
	}
	for _, count := range commandSeen {
		if count > 1 {
			coordination.RepeatedCommands += count - 1
		}
	}
	for _, count := range readSeen {
		if count > 1 {
			coordination.RepeatedReads += count - 1
		}
	}
	summary := metrics.Summarize(collected.RootSessionID, sessions)
	telemetryComplete := collected.TelemetryComplete && summary.Parent.Complete && summary.Tree.Complete
	_ = telemetryComplete // carried by RunResult separately; numeric evidence remains partial here.
	return contracts.Usage{
		Parent: contracts.TokenUsage{
			FirstInputTokens: summary.Parent.FirstInputTokens, PeakInputTokens: summary.Parent.PeakInputTokens,
			SumInputTokens: summary.Parent.Tokens.Input,
			OutputTokens:   summary.Parent.Tokens.Output, ReasoningTokens: summary.Parent.Tokens.Reasoning,
			CacheReadTokens: summary.Parent.Tokens.CacheRead, CacheWriteTokens: summary.Parent.Tokens.CacheWrite,
			ProviderCostUSD: costPointer(summary.Parent.ProviderCost), CalculatedCostUSD: costPointer(summary.Parent.CalculatedCost),
		},
		Tree: contracts.TreeUsage{
			SumInputTokens: summary.Tree.Tokens.Input, OutputTokens: summary.Tree.Tokens.Output,
			ReasoningTokens: summary.Tree.Tokens.Reasoning, CacheReadTokens: summary.Tree.Tokens.CacheRead,
			CacheWriteTokens: summary.Tree.Tokens.CacheWrite, Sessions: summary.Tree.Sessions,
			ProviderCostUSD: costPointer(summary.Tree.ProviderCost), CalculatedCostUSD: costPointer(summary.Tree.CalculatedCost),
		},
	}, coordination
}

func rawHasField(raw json.RawMessage, field string) bool {
	if len(raw) == 0 {
		return false
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	_, ok := value[field]
	return ok
}

func costPointer(total metrics.CostTotal) *float64 {
	if !total.Available {
		return nil
	}
	value := total.USD
	return &value
}

func modelDurationMS(collected *trace.Trace) int64 {
	var total int64
	for _, session := range collected.Sessions {
		// Timing.ModelMS is elapsed model time on the root interaction. Summing
		// child sessions can exceed wall time when children overlap, which is a
		// useful workload metric but not a duration.
		if session.Session.ID != collected.RootSessionID {
			continue
		}
		for _, message := range session.Messages {
			for _, part := range message.Parts {
				if part.Type == "step-finish" && part.Time.End >= part.Time.Start {
					total += part.Time.End - part.Time.Start
				}
			}
		}
	}
	return total
}

func buildDeterministicInputs(testCase contracts.Case, before, after sandbox.Snapshot, setupResults, oracleResults []sandbox.CommandResult, collected *trace.Trace, finalText string, info RuntimeInfo, sourceUnchanged, bundleUnchanged bool, conversationErr, traceErr, modelObservationErr, snapshotErr, sourceErr, bundleErr error) (judges.Evidence, judges.Policy, []contracts.EvidenceItem, string) {
	items := make([]contracts.EvidenceItem, 0)
	processClean := true
	for i, result := range setupResults {
		processClean = processClean && result.CleanProcessTree
		evidenceID := fmt.Sprintf("setup_%03d", i+1)
		items = append(items, digestEvidence(evidenceID, "setup-command", result, result.Started))
	}
	for _, result := range oracleResults {
		processClean = processClean && result.CleanProcessTree
	}
	infrastructure := &judges.InfrastructureEvidence{
		EvidenceID: "infrastructure", Complete: true,
		SessionFinished: collected != nil && traceStructurallyComplete(collected),
		TimedOut:        false, Canceled: false, ProcessTreeClean: processClean,
		TelemetryComplete: collected != nil && collected.TelemetryComplete,
	}
	if modelObservationErr != nil {
		infrastructure.Complete = false
		infrastructure.Error = modelObservationErr.Error()
	} else if traceErr != nil && collected == nil {
		infrastructure.Complete = false
		infrastructure.Error = traceErr.Error()
	}
	items = append(items, digestEvidence("infrastructure", "infrastructure", infrastructure, infrastructure.Complete))

	filesystem := &judges.FilesystemEvidence{
		EvidenceID: "filesystem", Complete: snapshotErr == nil,
		Before: fileStates(before), After: fileStates(after),
	}
	items = append(items, digestEvidence("filesystem", "filesystem", filesystem, filesystem.Complete))

	acceptance := &judges.AcceptanceEvidence{EvidenceID: "acceptance", Complete: len(oracleResults) == len(testCase.Oracle.Commands)}
	for i, result := range oracleResults {
		evidenceID := fmt.Sprintf("oracle_%03d", i+1)
		acceptance.Commands = append(acceptance.Commands, judges.CommandEvidence{
			EvidenceID: evidenceID, ID: result.ID, Recorded: true, Completed: result.Completed,
			ExitCode: result.ExitCode, TimedOut: result.TimedOut, Canceled: result.Canceled,
			OutputLimitExceeded: result.OutputLimitExceeded, CleanProcessTree: result.CleanProcessTree,
			InfrastructureError: result.Error,
		})
		items = append(items, digestEvidence(evidenceID, "oracle-command", result, result.Started))
	}
	items = append(items, digestEvidence("acceptance", "acceptance", acceptance, acceptance.Complete))

	behavior, eventItems := behaviorEvidence(collected)
	items = append(items, eventItems...)
	items = append(items, digestEvidence("behavior", "behavior", behavior, behavior.Complete))
	claims := &judges.ClaimEvidence{
		EvidenceID:    "claims",
		Complete:      finalText != "" && !hasResponseContractError(conversationErr) && modelObservationErr == nil && traceStructurallyComplete(collected),
		FinalResponse: finalText,
	}
	if conversationErr != nil {
		code := evaluationCode(conversationErr)
		if code == "" {
			code = "conversation_failed"
		}
		claims.Facts = append(claims.Facts, judges.ClaimFact{EvidenceID: "claims", Name: "conversation", Claimed: "complete", Observed: code})
	}
	items = append(items, digestEvidence("claims", "final-response", claims, claims.Complete))

	security := &judges.SecurityEvidence{
		EvidenceID: "security", Complete: sourceErr == nil && bundleErr == nil && snapshotErr == nil,
		ExecutionMode: string(info.ExecutionMode), NetworkMode: string(info.Network),
		Invariants: []judges.SecurityInvariant{
			{EvidenceID: "filesystem", Name: "safe-filesystem-entries", Satisfied: len(after.UnsafeEntries()) == 0},
			{EvidenceID: "infrastructure", Name: "clean-process-tree", Satisfied: processClean},
			{EvidenceID: "before", Name: "source-fixture-unchanged", Satisfied: sourceUnchanged},
			{EvidenceID: "infrastructure", Name: "agent-bundle-unchanged", Satisfied: bundleUnchanged},
		},
	}
	for _, unsafe := range after.UnsafeEntries() {
		security.Violations = append(security.Violations, judges.SecurityViolation{EvidenceID: "filesystem", Kind: "unsafe-filesystem-entry", Detail: unsafe.Path})
	}
	if collected != nil {
		for _, tool := range collected.Tools {
			toolName := normalizedToolName(tool.Tool, tool.Input)
			if containsFold(testCase.ToolPolicy.ForbiddenTools, toolName) {
				security.Violations = append(security.Violations, judges.SecurityViolation{EvidenceID: "behavior", Kind: "forbidden-tool", Detail: toolName})
			}
		}
	}
	items = append(items, digestEvidence("security", "security", security, security.Complete))

	policy := judges.Policy{
		Infrastructure: &judges.InfrastructurePolicy{
			RequireSessionFinished: true, ForbidTimeout: true, ForbidCancellation: true,
			RequireCleanProcessTree:  testCase.Oracle.RequireCleanProcessTree,
			RequireCompleteTelemetry: false,
		},
		Filesystem: &judges.FilesystemPolicy{
			ExpectedChanges:  append([]string(nil), testCase.Oracle.ExpectedChanges...),
			AllowedChanges:   append([]string(nil), testCase.Oracle.ExpectedChanges...),
			ForbiddenChanges: append([]string(nil), testCase.Oracle.ForbiddenChanges...),
			ExactFiles:       exactFilePolicies(testCase.Oracle.ExpectedFiles), RequireSafeEntries: true,
		},
		Acceptance: &judges.AcceptancePolicy{Commands: commandPolicies(testCase.Oracle.Commands)},
		Behavior:   behaviorPolicy(testCase),
		Claims: &judges.ClaimPolicy{
			SuccessPatterns: []string{`(?i)\b(done|complete|completed|success|successful|passing|implemented|fixed|verified)\b`},
			NoFalseSuccess:  true,
		},
		Security: &judges.SecurityPolicy{
			AllowedExecutionModes: []string{string(testCase.Security.ExecutionMode)}, RequiredNetworkMode: string(testCase.Security.Network),
			RequiredInvariants: []string{"safe-filesystem-entries", "clean-process-tree", "source-fixture-unchanged", "agent-bundle-unchanged"},
			ForbidViolations:   true,
		},
	}
	digest, _ := contracts.CanonicalDigest(policy)
	return judges.Evidence{Infrastructure: infrastructure, Filesystem: filesystem, Acceptance: acceptance, Behavior: behavior, Claims: claims, Security: security}, policy, items, digest
}

func evaluateDeterministically(evidence judges.Evidence, policy judges.Policy) judges.Verdict {
	return judges.Evaluate(evidence, policy)
}

func fileStates(snapshot sandbox.Snapshot) []judges.FileState {
	result := make([]judges.FileState, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		result = append(result, judges.FileState{Path: entry.Path, Kind: string(entry.Kind), Mode: entry.Mode, Digest: entry.SHA256})
	}
	return result
}

func exactFilePolicies(files []contracts.ExpectedFile) []judges.FileExpectation {
	result := make([]judges.FileExpectation, 0, len(files))
	for _, file := range files {
		expected := judges.FileExpectation{Path: file.Path, Digest: file.Digest, Kind: "file"}
		if file.Content != nil {
			expected.Content = []byte(*file.Content)
		}
		if file.Mode != "" {
			mode := uint32(0o644)
			if file.Mode == "0755" {
				mode = 0o755
			}
			expected.Mode = &mode
		}
		result = append(result, expected)
	}
	return result
}

func commandPolicies(commands []contracts.Command) []judges.CommandExpectation {
	result := make([]judges.CommandExpectation, 0, len(commands))
	for _, command := range commands {
		result = append(result, judges.CommandExpectation{ID: command.ID, ExpectedExit: append([]int(nil), command.ExpectedExit...)})
	}
	return result
}

func behaviorEvidence(collected *trace.Trace) (*judges.BehaviorEvidence, []contracts.EvidenceItem) {
	evidence := &judges.BehaviorEvidence{EvidenceID: "behavior", Complete: collected != nil && traceStructurallyComplete(collected)}
	if collected == nil {
		return evidence, nil
	}
	var sequence uint64
	items := make([]contracts.EvidenceItem, 0)
	for _, session := range collected.Sessions {
		if session.Session.ParentID == "" {
			continue
		}
		if !sessionHasCompletedAssistantResponse(session) {
			evidence.Complete = false
			continue
		}
		sequence++
		id := fmt.Sprintf("event_%06d", sequence)
		event := judges.Event{EvidenceID: id, Sequence: sequence, Type: judges.EventDelegation, Name: strings.ToLower(session.Session.Title), ParentID: session.Session.ParentID, ChildID: session.Session.ID, Succeeded: session.Status.Type == "idle"}
		evidence.Events = append(evidence.Events, event)
		items = append(items, digestEvidence(id, "delegation-event", event, true))
	}
	for _, session := range collected.Sessions {
		for _, message := range session.Messages {
			for _, part := range message.Parts {
				if part.Type != "tool" {
					continue
				}
				sequence++
				id := fmt.Sprintf("event_%06d", sequence)
				name := normalizedToolEvent(part)
				event := judges.Event{EvidenceID: id, Sequence: sequence, Type: judges.EventToolCall, Name: name, Succeeded: part.State.Status == "completed"}
				evidence.Events = append(evidence.Events, event)
				items = append(items, digestEvidence(id, "tool-event", event, true))
			}
		}
	}
	for _, retry := range collected.Retries {
		sequence++
		id := fmt.Sprintf("event_%06d", sequence)
		event := judges.Event{EvidenceID: id, Sequence: sequence, Type: judges.EventRetry, Name: fmt.Sprintf("attempt_%d", retry.Attempt), Succeeded: false}
		evidence.Events = append(evidence.Events, event)
		items = append(items, digestEvidence(id, "retry-event", event, true))
	}
	return evidence, items
}

// sessionHasCompletedAssistantResponse distinguishes a real delegated model
// response from an empty/idle session that can be created through the local
// OpenCode API. A delegation is behavior evidence only when it has a terminal
// assistant step with an explicit model identity.
func sessionHasCompletedAssistantResponse(session trace.SessionTrace) bool {
	for _, message := range session.Messages {
		if message.Info.Role != "assistant" || message.Info.ProviderID == "" || message.Info.ModelID == "" {
			continue
		}
		for _, part := range message.Parts {
			if part.Type == "step-finish" {
				return true
			}
		}
	}
	return false
}

func normalizedToolEvent(part client.Part) string {
	name := normalizedToolName(part.Tool, part.State.Input)
	if name == "edit" || name == "write" || name == "apply_patch" {
		return "first_write"
	}
	return name
}

func normalizedToolName(tool string, input []byte) string {
	name := strings.ToLower(tool)
	if name != "bash" && name != "shell" {
		return name
	}
	var payload struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &payload); err != nil || strings.TrimSpace(payload.Command) == "" {
		return name
	}
	command := strings.TrimSpace(strings.ToLower(payload.Command))
	// Synthetic names feed hard behavior checks. Only a single, plainly parsed
	// command is authoritative; prose, echo and shell composition stay "bash".
	if strings.ContainsAny(command, ";\n\r|&`$()") {
		return name
	}
	fields := strings.Fields(command)
	subcommand, subcommandIndex := gitCommand(fields)
	switch {
	case subcommand == "status":
		return "git_status"
	case subcommand == "commit":
		return "git_commit"
	case subcommand == "push":
		return "git_push"
	case subcommand == "add":
		return "git_add"
	case subcommand == "reset":
		return "git_reset"
	case subcommand == "clean":
		return "git_clean"
	case subcommand == "restore" && hasLongOption(fields[subcommandIndex+1:], "--staged"):
		return "git_restore_staged"
	case subcommand == "restore":
		return "git_restore_worktree"
	case readOnlyGitSubcommand(subcommand):
		return "git_inspect"
	case hasCommandPrefix(fields, "skynex", "workflow"):
		return "skynex_workflow"
	case hasCommandPrefix(fields, "gh", "pr") || hasCommandPrefix(fields, "gh-axi", "pr"):
		return "github_pr"
	case hasCommandPrefix(fields, "go", "test") || hasCommandPrefix(fields, "cargo", "test") || hasCommandPrefix(fields, "npm", "test") || hasCommandPrefix(fields, "pnpm", "test"):
		return "validation_run"
	default:
		return name
	}
}

func hasCommandPrefix(fields []string, executable, subcommand string) bool {
	return len(fields) >= 2 && fields[0] == executable && fields[1] == subcommand
}

func gitSubcommand(fields []string) string {
	subcommand, _ := gitCommand(fields)
	return subcommand
}

func gitCommand(fields []string) (string, int) {
	if len(fields) < 2 || !isGitExecutable(fields[0]) {
		return "", -1
	}
	for index := 1; index < len(fields); index++ {
		field := fields[index]
		if field == "-c" || field == "-C" || field == "--git-dir" || field == "--work-tree" || field == "--namespace" || field == "--config-env" {
			index++
			continue
		}
		if strings.HasPrefix(field, "-c") || strings.HasPrefix(field, "-C") || strings.HasPrefix(field, "--git-dir=") || strings.HasPrefix(field, "--work-tree=") || strings.HasPrefix(field, "--namespace=") || strings.HasPrefix(field, "-") {
			continue
		}
		return field, index
	}
	return "", -1
}

func isGitExecutable(executable string) bool {
	if separator := strings.LastIndexAny(executable, `/\\`); separator >= 0 {
		executable = executable[separator+1:]
	}
	return executable == "git" || executable == "git.exe"
}

func hasLongOption(fields []string, option string) bool {
	for _, field := range fields {
		if field == option || strings.HasPrefix(field, option+"=") {
			return true
		}
	}
	return false
}

func readOnlyGitSubcommand(subcommand string) bool {
	switch subcommand {
	case "cat-file", "check-ignore", "diff", "grep", "log", "ls-files", "rev-parse", "show":
		return true
	default:
		return false
	}
}

func behaviorPolicy(testCase contracts.Case) *judges.BehaviorPolicy {
	policy := &judges.BehaviorPolicy{}
	for _, check := range testCase.BehaviorChecks {
		switch check.Type {
		case "subagent_count":
			policy.Delegations = &judges.CountRange{Min: intValue(check.Min, 0), Max: cloneInt(check.Max)}
		}
	}
	return policy
}

func traceStructurallyComplete(collected *trace.Trace) bool {
	return collected != nil && collected.StructurallyComplete()
}

func digestEvidence(id, kind string, value any, complete bool) contracts.EvidenceItem {
	digest, err := contracts.CanonicalDigest(value)
	if err != nil {
		digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		complete = false
	}
	return evidenceItem(id, kind, digest, complete)
}

func gitStatusEvidenceItem(id string, status sandbox.GitStatusEvidence, complete bool) contracts.EvidenceItem {
	digest := status.StateDigest
	if digest == "" {
		// A failed post-run inspection still needs a stable incomplete evidence
		// record so every invalid hard check retains resolvable lineage.
		return digestEvidence(id, "git-status", status, false)
	}
	return evidenceItem(id, "git-status", digest, complete)
}

func contractChecks(checks []judges.CheckResult, requirementIDs []string, reserved []contracts.Check) []contracts.CheckResult {
	result := make([]contracts.CheckResult, 0, len(checks))
	seen := make(map[string]struct{}, len(checks)+len(reserved))
	for _, check := range reserved {
		seen[check.ID] = struct{}{}
	}
	for _, check := range checks {
		id := uniqueResultID(safeResultID("judge_"+check.ID), seen)
		status := contracts.CheckStatusInvalid
		switch check.Outcome {
		case judges.OutcomePass:
			status = contracts.CheckStatusPass
		case judges.OutcomeFail:
			status = contracts.CheckStatusFail
		}
		ids := check.RequirementIDs
		if len(ids) == 0 {
			ids = requirementIDs
		}
		result = append(result, contracts.CheckResult{
			ID: id, Type: string(check.Category), Status: status, Hard: check.Hard,
			Summary: check.Summary, RequirementIDs: append([]string(nil), ids...),
			EvidenceIDs: append([]string(nil), check.EvidenceIDs...),
		})
	}
	return result
}

func uniqueResultID(base string, seen map[string]struct{}) string {
	for sequence := 1; ; sequence++ {
		candidate := base
		if sequence > 1 {
			suffix := fmt.Sprintf("_%d", sequence)
			limit := contracts.MaxCaseIDBytes - len(suffix)
			if limit < 1 {
				limit = 1
			}
			if len(candidate) > limit {
				candidate = candidate[:limit]
			}
			candidate += suffix
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		return candidate
	}
}

func statusFromVerdict(outcome judges.Outcome) contracts.RunStatus {
	switch outcome {
	case judges.OutcomePass:
		return contracts.RunStatusPass
	case judges.OutcomeFail:
		return contracts.RunStatusFail
	default:
		return contracts.RunStatusInvalid
	}
}

func safeResultID(value string) string {
	value = strings.ToLower(value)
	var result strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			result.WriteRune(r)
		} else {
			result.WriteByte('_')
		}
	}
	clean := strings.Trim(result.String(), "_-")
	if clean == "" || clean[0] < 'a' || clean[0] > 'z' {
		clean = "check_" + clean
	}
	if len(clean) > contracts.MaxCaseIDBytes {
		clean = clean[:contracts.MaxCaseIDBytes]
	}
	return clean
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func intValue(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func integerValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), typed == float64(int(typed))
	default:
		return 0, false
	}
}

var successClaimPattern = regexp.MustCompile(`(?i)\b(done|complete|completed|success|successful|passing|implemented|fixed|verified)\b`)
