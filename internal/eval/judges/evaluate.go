package judges

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Evaluate applies deterministic policies in authority order. Every generated
// check is hard: a later check or qualitative opinion cannot compensate for a
// failure. A missing required observation is invalid, never inferred as pass.
func Evaluate(evidence Evidence, policy Policy) Verdict {
	if policy.Infrastructure == nil && policy.Filesystem == nil && policy.Acceptance == nil &&
		policy.Behavior == nil && policy.Claims == nil && policy.Security == nil {
		return finalize([]CheckResult{invalidCheck("policy.required", CategoryInfrastructure, "no deterministic policy was declared")})
	}
	checks := make([]CheckResult, 0)
	if policy.Infrastructure != nil {
		checks = append(checks, evaluateInfrastructure(evidence.Infrastructure, *policy.Infrastructure)...)
	}
	if policy.Filesystem != nil {
		checks = append(checks, evaluateFilesystem(evidence.Filesystem, *policy.Filesystem)...)
	}
	if policy.Acceptance != nil {
		checks = append(checks, evaluateAcceptance(evidence.Acceptance, *policy.Acceptance)...)
	}
	if policy.Behavior != nil {
		checks = append(checks, evaluateBehavior(evidence.Behavior, *policy.Behavior)...)
	}
	securityChecks := []CheckResult(nil)
	if policy.Security != nil {
		securityChecks = evaluateSecurity(evidence.Security, *policy.Security)
	}
	if policy.Claims != nil {
		known := append(append([]CheckResult(nil), checks...), securityChecks...)
		checks = append(checks, evaluateClaims(evidence.Claims, *policy.Claims, known)...)
	}
	checks = append(checks, securityChecks...)
	return finalize(checks)
}

// AddQualitativeOpinion appends a soft opinion without allowing a pass to
// replace a deterministic failure or invalid result.
func AddQualitativeOpinion(verdict Verdict, opinion CheckResult) Verdict {
	opinion.Hard = false
	if opinion.Category == "" {
		opinion.Category = CategoryClaims
	}
	verdict.Checks = append(verdict.Checks, opinion)
	if verdict.Status == OutcomePass && opinion.Outcome == OutcomeFail {
		verdict.Status = OutcomeFail
	}
	verdict.AllowsQualitativeOverride = verdict.Status == OutcomePass
	return verdict
}

func finalize(checks []CheckResult) Verdict {
	verdict := Verdict{Status: OutcomePass, Checks: checks, AllowsQualitativeOverride: true}
	hasInvalid := false
	for _, check := range checks {
		if check.Hard && check.Outcome == OutcomeFail {
			verdict.HardFailure = true
		}
		if check.Outcome == OutcomeInvalid {
			hasInvalid = true
		}
	}
	if verdict.HardFailure {
		verdict.Status = OutcomeFail
		verdict.AllowsQualitativeOverride = false
	} else if hasInvalid {
		verdict.Status = OutcomeInvalid
		verdict.AllowsQualitativeOverride = false
	}
	return verdict
}

func passCheck(id string, category Category, summary string, evidenceIDs ...string) CheckResult {
	return CheckResult{ID: id, Category: category, Outcome: OutcomePass, Hard: true, Summary: summary, EvidenceIDs: compactIDs(evidenceIDs)}
}

func failCheck(id string, category Category, summary string, evidenceIDs ...string) CheckResult {
	return CheckResult{ID: id, Category: category, Outcome: OutcomeFail, Hard: true, Summary: summary, EvidenceIDs: compactIDs(evidenceIDs)}
}

func invalidCheck(id string, category Category, summary string, evidenceIDs ...string) CheckResult {
	return CheckResult{ID: id, Category: category, Outcome: OutcomeInvalid, Hard: true, Summary: summary, EvidenceIDs: compactIDs(evidenceIDs)}
}

func compactIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func requireCategoryEvidence(id string, category Category, present bool, complete bool, evidenceID string) []CheckResult {
	if !present {
		return []CheckResult{invalidCheck(id, category, "required evidence is missing")}
	}
	if evidenceID == "" {
		return []CheckResult{invalidCheck(id, category, "required evidence has no lineage identifier")}
	}
	if !complete {
		return []CheckResult{invalidCheck(id, category, "required evidence is incomplete", evidenceID)}
	}
	return nil
}

func evaluateInfrastructure(evidence *InfrastructureEvidence, policy InfrastructurePolicy) []CheckResult {
	if missing := requireCategoryEvidence("infrastructure.evidence", CategoryInfrastructure, evidence != nil, evidence != nil && evidence.Complete, evidenceIDInfrastructure(evidence)); missing != nil {
		return missing
	}
	checks := make([]CheckResult, 0, 6)
	id := evidence.EvidenceID
	if evidence.Error != "" {
		checks = append(checks, failCheck("infrastructure.error", CategoryInfrastructure, "infrastructure reported an error: "+evidence.Error, id))
	}
	if policy.RequireSessionFinished {
		if evidence.SessionFinished {
			checks = append(checks, passCheck("infrastructure.session-finished", CategoryInfrastructure, "session finished", id))
		} else {
			checks = append(checks, failCheck("infrastructure.session-finished", CategoryInfrastructure, "session did not finish", id))
		}
	}
	if policy.ForbidTimeout {
		if evidence.TimedOut {
			checks = append(checks, failCheck("infrastructure.timeout", CategoryInfrastructure, "run timed out", id))
		} else {
			checks = append(checks, passCheck("infrastructure.timeout", CategoryInfrastructure, "run did not time out", id))
		}
	}
	if policy.ForbidCancellation {
		if evidence.Canceled {
			checks = append(checks, failCheck("infrastructure.cancellation", CategoryInfrastructure, "run was canceled", id))
		} else {
			checks = append(checks, passCheck("infrastructure.cancellation", CategoryInfrastructure, "run was not canceled", id))
		}
	}
	if policy.RequireCleanProcessTree {
		if evidence.ProcessTreeClean {
			checks = append(checks, passCheck("infrastructure.process-tree", CategoryInfrastructure, "process tree is quiescent", id))
		} else {
			checks = append(checks, failCheck("infrastructure.process-tree", CategoryInfrastructure, "process tree is not quiescent", id))
		}
	}
	if policy.RequireCompleteTelemetry {
		if evidence.TelemetryComplete {
			checks = append(checks, passCheck("infrastructure.telemetry", CategoryInfrastructure, "telemetry is complete", id))
		} else {
			checks = append(checks, failCheck("infrastructure.telemetry", CategoryInfrastructure, "telemetry is incomplete", id))
		}
	}
	return checks
}

func evidenceIDInfrastructure(evidence *InfrastructureEvidence) string {
	if evidence == nil {
		return ""
	}
	return evidence.EvidenceID
}

type fileChange struct {
	path string
	old  *FileState
	new  *FileState
}

func evaluateFilesystem(evidence *FilesystemEvidence, policy FilesystemPolicy) []CheckResult {
	if missing := requireCategoryEvidence("filesystem.evidence", CategoryFilesystem, evidence != nil, evidence != nil && evidence.Complete, evidenceIDFilesystem(evidence)); missing != nil {
		return missing
	}
	before, err := indexFiles(evidence.Before)
	if err != nil {
		return []CheckResult{invalidCheck("filesystem.before", CategoryFilesystem, err.Error(), evidence.EvidenceID)}
	}
	after, err := indexFiles(evidence.After)
	if err != nil {
		return []CheckResult{invalidCheck("filesystem.after", CategoryFilesystem, err.Error(), evidence.EvidenceID)}
	}
	changes := changedFiles(before, after)
	checks := make([]CheckResult, 0)
	for _, path := range policy.ExpectedChanges {
		path, err = judgePath(path)
		if err != nil {
			checks = append(checks, invalidCheck("filesystem.expected-change", CategoryFilesystem, err.Error(), evidence.EvidenceID))
			continue
		}
		if _, ok := changes[path]; ok {
			checks = append(checks, passCheck("filesystem.expected-change:"+path, CategoryFilesystem, "expected path changed: "+path, evidence.EvidenceID))
		} else {
			checks = append(checks, failCheck("filesystem.expected-change:"+path, CategoryFilesystem, "expected path did not change: "+path, evidence.EvidenceID))
		}
	}
	for _, path := range policy.ForbiddenChanges {
		path, err = judgePath(path)
		if err != nil {
			checks = append(checks, invalidCheck("filesystem.forbidden-change", CategoryFilesystem, err.Error(), evidence.EvidenceID))
			continue
		}
		if _, ok := changes[path]; ok {
			checks = append(checks, failCheck("filesystem.forbidden-change:"+path, CategoryFilesystem, "forbidden path changed: "+path, evidence.EvidenceID))
		} else {
			checks = append(checks, passCheck("filesystem.forbidden-change:"+path, CategoryFilesystem, "forbidden path stayed unchanged: "+path, evidence.EvidenceID))
		}
	}
	allowed, allowedErr := normalizePaths(policy.AllowedChanges)
	prefixes, prefixErr := normalizePrefixes(policy.AllowedPrefixes)
	forbiddenPrefixes, forbiddenPrefixErr := normalizePrefixes(policy.ForbiddenPrefixes)
	if allowedErr != nil || prefixErr != nil || forbiddenPrefixErr != nil {
		checks = append(checks, invalidCheck("filesystem.scope-policy", CategoryFilesystem, firstError(allowedErr, prefixErr, forbiddenPrefixErr).Error(), evidence.EvidenceID))
	} else {
		if policy.AllowedChanges != nil || policy.AllowedPrefixes != nil {
			var outside []string
			for path := range changes {
				if _, exact := allowed[path]; !exact && !hasPathPrefix(path, prefixes) {
					outside = append(outside, path)
				}
			}
			sort.Strings(outside)
			if len(outside) != 0 {
				checks = append(checks, failCheck("filesystem.allowed-scope", CategoryFilesystem, "changes escaped allowed scope: "+strings.Join(outside, ", "), evidence.EvidenceID))
			} else {
				checks = append(checks, passCheck("filesystem.allowed-scope", CategoryFilesystem, "all changes stayed in allowed scope", evidence.EvidenceID))
			}
		}
		var forbidden []string
		for path := range changes {
			if hasPathPrefix(path, forbiddenPrefixes) {
				forbidden = append(forbidden, path)
			}
		}
		sort.Strings(forbidden)
		if len(forbidden) != 0 {
			checks = append(checks, failCheck("filesystem.forbidden-scope", CategoryFilesystem, "changes entered forbidden scope: "+strings.Join(forbidden, ", "), evidence.EvidenceID))
		} else if policy.ForbiddenPrefixes != nil {
			checks = append(checks, passCheck("filesystem.forbidden-scope", CategoryFilesystem, "forbidden scopes stayed unchanged", evidence.EvidenceID))
		}
	}
	for _, expected := range policy.ExactFiles {
		checks = append(checks, evaluateExactFile(after, expected, evidence.EvidenceID))
	}
	if policy.RequireSafeEntries {
		var unsafe []string
		for _, state := range evidence.After {
			if state.Kind != "file" && state.Kind != "dir" {
				unsafe = append(unsafe, state.Path+" ("+state.Kind+")")
			}
		}
		sort.Strings(unsafe)
		if len(unsafe) == 0 {
			checks = append(checks, passCheck("filesystem.safe-entries", CategoryFilesystem, "workspace contains only regular files and directories", evidence.EvidenceID))
		} else {
			checks = append(checks, failCheck("filesystem.safe-entries", CategoryFilesystem, "workspace contains unsafe entries: "+strings.Join(unsafe, ", "), evidence.EvidenceID))
		}
	}
	return checks
}

func evidenceIDFilesystem(evidence *FilesystemEvidence) string {
	if evidence == nil {
		return ""
	}
	return evidence.EvidenceID
}

func indexFiles(states []FileState) (map[string]FileState, error) {
	result := make(map[string]FileState, len(states))
	for _, state := range states {
		path, err := judgePath(state.Path)
		if err != nil {
			return nil, fmt.Errorf("invalid filesystem evidence path: %w", err)
		}
		if _, duplicate := result[path]; duplicate {
			return nil, fmt.Errorf("duplicate filesystem evidence path %q", path)
		}
		state.Path = path
		result[path] = state
	}
	return result, nil
}

func changedFiles(before, after map[string]FileState) map[string]fileChange {
	result := make(map[string]fileChange)
	for path, old := range before {
		new, ok := after[path]
		if !ok {
			copy := old
			result[path] = fileChange{path: path, old: &copy}
		} else if old != new {
			oldCopy, newCopy := old, new
			result[path] = fileChange{path: path, old: &oldCopy, new: &newCopy}
		}
	}
	for path, new := range after {
		if _, ok := before[path]; !ok {
			copy := new
			result[path] = fileChange{path: path, new: &copy}
		}
	}
	return result
}

func evaluateExactFile(after map[string]FileState, expected FileExpectation, evidenceID string) CheckResult {
	path, err := judgePath(expected.Path)
	if err != nil {
		return invalidCheck("filesystem.exact-file", CategoryFilesystem, err.Error(), evidenceID)
	}
	state, exists := after[path]
	if expected.Absent {
		if exists {
			return failCheck("filesystem.exact-file:"+path, CategoryFilesystem, "path must be absent: "+path, evidenceID)
		}
		return passCheck("filesystem.exact-file:"+path, CategoryFilesystem, "path is absent as required: "+path, evidenceID)
	}
	if !exists {
		return failCheck("filesystem.exact-file:"+path, CategoryFilesystem, "required path is missing: "+path, evidenceID)
	}
	expectedDigest := expected.Digest
	if expected.Content != nil {
		contentDigest := ContentDigest(expected.Content)
		if expectedDigest != "" && expectedDigest != contentDigest {
			return invalidCheck("filesystem.exact-file:"+path, CategoryFilesystem, "exact-file policy has conflicting digest and content", evidenceID)
		}
		expectedDigest = contentDigest
	}
	var mismatch []string
	if expected.Kind != "" && state.Kind != expected.Kind {
		mismatch = append(mismatch, fmt.Sprintf("kind=%s (want %s)", state.Kind, expected.Kind))
	}
	if expected.Mode != nil && state.Mode != *expected.Mode {
		mismatch = append(mismatch, fmt.Sprintf("mode=%#o (want %#o)", state.Mode, *expected.Mode))
	}
	if expectedDigest != "" && state.Digest != expectedDigest {
		mismatch = append(mismatch, "content digest differs")
	}
	if len(mismatch) != 0 {
		return failCheck("filesystem.exact-file:"+path, CategoryFilesystem, "exact file mismatch for "+path+": "+strings.Join(mismatch, ", "), evidenceID)
	}
	return passCheck("filesystem.exact-file:"+path, CategoryFilesystem, "exact file matched: "+path, evidenceID)
}

func evaluateAcceptance(evidence *AcceptanceEvidence, policy AcceptancePolicy) []CheckResult {
	if missing := requireCategoryEvidence("acceptance.evidence", CategoryAcceptance, evidence != nil, evidence != nil && evidence.Complete, evidenceIDAcceptance(evidence)); missing != nil {
		return missing
	}
	commands := make(map[string]CommandEvidence, len(evidence.Commands))
	for _, command := range evidence.Commands {
		if command.ID == "" {
			return []CheckResult{invalidCheck("acceptance.command", CategoryAcceptance, "command evidence has no command ID", evidence.EvidenceID)}
		}
		if _, duplicate := commands[command.ID]; duplicate {
			return []CheckResult{invalidCheck("acceptance.command:"+command.ID, CategoryAcceptance, "duplicate command evidence", evidence.EvidenceID, command.EvidenceID)}
		}
		commands[command.ID] = command
	}
	checks := make([]CheckResult, 0, len(policy.Commands))
	seen := make(map[string]struct{}, len(policy.Commands))
	for _, expected := range policy.Commands {
		if expected.ID == "" {
			checks = append(checks, invalidCheck("acceptance.command", CategoryAcceptance, "expected command has no ID", evidence.EvidenceID))
			continue
		}
		if _, duplicate := seen[expected.ID]; duplicate {
			checks = append(checks, invalidCheck("acceptance.command:"+expected.ID, CategoryAcceptance, "duplicate command expectation", evidence.EvidenceID))
			continue
		}
		seen[expected.ID] = struct{}{}
		command, ok := commands[expected.ID]
		if !ok || !command.Recorded || command.EvidenceID == "" {
			checks = append(checks, invalidCheck("acceptance.command:"+expected.ID, CategoryAcceptance, "required command evidence is missing", evidence.EvidenceID, command.EvidenceID))
			continue
		}
		if command.InfrastructureError != "" {
			checks = append(checks, invalidCheck("acceptance.command:"+expected.ID, CategoryAcceptance, "command infrastructure failed: "+command.InfrastructureError, evidence.EvidenceID, command.EvidenceID))
			continue
		}
		problems := make([]string, 0)
		if !command.Completed {
			problems = append(problems, "did not complete")
		}
		if command.TimedOut {
			problems = append(problems, "timed out")
		}
		if command.Canceled {
			problems = append(problems, "was canceled")
		}
		if command.OutputLimitExceeded {
			problems = append(problems, "exceeded output limit")
		}
		if !command.CleanProcessTree {
			problems = append(problems, "left descendant processes")
		}
		expectedExits := expected.ExpectedExit
		if len(expectedExits) == 0 {
			expectedExits = []int{expected.ExitCode}
		}
		if !containsExitCode(expectedExits, command.ExitCode) {
			problems = append(problems, fmt.Sprintf("exit=%d (want one of %v)", command.ExitCode, expectedExits))
		}
		if len(problems) != 0 {
			checks = append(checks, failCheck("acceptance.command:"+expected.ID, CategoryAcceptance, "acceptance command failed: "+strings.Join(problems, ", "), evidence.EvidenceID, command.EvidenceID))
		} else {
			checks = append(checks, passCheck("acceptance.command:"+expected.ID, CategoryAcceptance, "acceptance command passed", evidence.EvidenceID, command.EvidenceID))
		}
	}
	return checks
}

func containsExitCode(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func evidenceIDAcceptance(evidence *AcceptanceEvidence) string {
	if evidence == nil {
		return ""
	}
	return evidence.EvidenceID
}

func evaluateBehavior(evidence *BehaviorEvidence, policy BehaviorPolicy) []CheckResult {
	if missing := requireCategoryEvidence("behavior.evidence", CategoryBehavior, evidence != nil, evidence != nil && evidence.Complete, evidenceIDBehavior(evidence)); missing != nil {
		return missing
	}
	events := append([]Event(nil), evidence.Events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Sequence < events[j].Sequence })
	seenSequence := make(map[uint64]struct{}, len(events))
	for _, event := range events {
		if event.EvidenceID == "" {
			return []CheckResult{invalidCheck("behavior.event", CategoryBehavior, "behavior event has no evidence ID", evidence.EvidenceID)}
		}
		if _, duplicate := seenSequence[event.Sequence]; duplicate {
			return []CheckResult{invalidCheck("behavior.sequence", CategoryBehavior, fmt.Sprintf("duplicate event sequence %d", event.Sequence), evidence.EvidenceID, event.EvidenceID)}
		}
		seenSequence[event.Sequence] = struct{}{}
	}
	checks := make([]CheckResult, 0)
	for _, expected := range policy.Counts {
		id := expected.ID
		if id == "" {
			id = string(expected.Selector.Type) + ":" + expected.Selector.Name
		}
		if expected.Min < 0 || (expected.Max != nil && (*expected.Max < expected.Min || *expected.Max < 0)) {
			checks = append(checks, invalidCheck("behavior.count:"+id, CategoryBehavior, "invalid event-count policy", evidence.EvidenceID))
			continue
		}
		count, ids := countEvents(events, expected.Selector)
		if count < expected.Min || (expected.Max != nil && count > *expected.Max) {
			checks = append(checks, failCheck("behavior.count:"+id, CategoryBehavior, formatCountFailure(count, expected.Min, expected.Max), append([]string{evidence.EvidenceID}, ids...)...))
		} else {
			checks = append(checks, passCheck("behavior.count:"+id, CategoryBehavior, fmt.Sprintf("observed %d matching events", count), append([]string{evidence.EvidenceID}, ids...)...))
		}
	}
	for _, expected := range policy.Order {
		id := expected.ID
		if id == "" {
			id = selectorName(expected.Before) + "-before-" + selectorName(expected.After)
		}
		before, beforeOK := firstEvent(events, expected.Before)
		after, afterOK := firstEvent(events, expected.After)
		if !beforeOK || !afterOK {
			checks = append(checks, failCheck("behavior.order:"+id, CategoryBehavior, "required events for ordering were not observed", evidence.EvidenceID))
		} else if before.Sequence >= after.Sequence {
			checks = append(checks, failCheck("behavior.order:"+id, CategoryBehavior, "events occurred in the wrong order", evidence.EvidenceID, before.EvidenceID, after.EvidenceID))
		} else {
			checks = append(checks, passCheck("behavior.order:"+id, CategoryBehavior, "events occurred in the required order", evidence.EvidenceID, before.EvidenceID, after.EvidenceID))
		}
	}
	if policy.Delegations != nil {
		checks = append(checks, evaluateRange("behavior.delegations", EventSelector{Type: EventDelegation}, *policy.Delegations, events, evidence.EvidenceID))
	}
	if policy.MaxRetries != nil {
		rangePolicy := CountRange{Max: policy.MaxRetries}
		checks = append(checks, evaluateRange("behavior.retries", EventSelector{Type: EventRetry}, rangePolicy, events, evidence.EvidenceID))
	}
	return checks
}

func evidenceIDBehavior(evidence *BehaviorEvidence) string {
	if evidence == nil {
		return ""
	}
	return evidence.EvidenceID
}

func eventMatches(event Event, selector EventSelector) bool {
	return (selector.Type == "" || event.Type == selector.Type) && (selector.Name == "" || event.Name == selector.Name)
}

func countEvents(events []Event, selector EventSelector) (int, []string) {
	count := 0
	var ids []string
	for _, event := range events {
		if eventMatches(event, selector) {
			count++
			ids = append(ids, event.EvidenceID)
		}
	}
	return count, ids
}

func firstEvent(events []Event, selector EventSelector) (Event, bool) {
	for _, event := range events {
		if eventMatches(event, selector) {
			return event, true
		}
	}
	return Event{}, false
}

func evaluateRange(id string, selector EventSelector, expected CountRange, events []Event, evidenceID string) CheckResult {
	if expected.Min < 0 || (expected.Max != nil && (*expected.Max < expected.Min || *expected.Max < 0)) {
		return invalidCheck(id, CategoryBehavior, "invalid count range", evidenceID)
	}
	count, ids := countEvents(events, selector)
	allIDs := append([]string{evidenceID}, ids...)
	if count < expected.Min || (expected.Max != nil && count > *expected.Max) {
		return failCheck(id, CategoryBehavior, formatCountFailure(count, expected.Min, expected.Max), allIDs...)
	}
	return passCheck(id, CategoryBehavior, fmt.Sprintf("observed %d matching events", count), allIDs...)
}

func formatCountFailure(count, min int, max *int) string {
	if max == nil {
		return fmt.Sprintf("observed %d events, want at least %d", count, min)
	}
	return fmt.Sprintf("observed %d events, want range [%d,%d]", count, min, *max)
}

func selectorName(selector EventSelector) string {
	return string(selector.Type) + ":" + selector.Name
}

func evaluateClaims(evidence *ClaimEvidence, policy ClaimPolicy, knownChecks []CheckResult) []CheckResult {
	if missing := requireCategoryEvidence("claims.evidence", CategoryClaims, evidence != nil, evidence != nil && evidence.Complete, evidenceIDClaims(evidence)); missing != nil {
		return missing
	}
	success, err := matchesAnyRegex(evidence.FinalResponse, policy.SuccessPatterns)
	if err != nil {
		return []CheckResult{invalidCheck("claims.success-pattern", CategoryClaims, "invalid success pattern: "+err.Error(), evidence.EvidenceID)}
	}
	knownFailure := false
	knownInvalid := false
	for _, check := range knownChecks {
		knownFailure = knownFailure || (check.Hard && check.Outcome == OutcomeFail)
		knownInvalid = knownInvalid || check.Outcome == OutcomeInvalid
	}
	checks := make([]CheckResult, 0)
	if policy.NoFalseSuccess {
		if success && knownFailure {
			checks = append(checks, failCheck("claims.no-false-success", CategoryClaims, "final response claims success despite failed or invalid deterministic evidence", evidence.EvidenceID))
		} else if knownInvalid {
			checks = append(checks, invalidCheck("claims.no-false-success", CategoryClaims, "claim consistency cannot be established from incomplete deterministic evidence", evidence.EvidenceID))
		} else {
			checks = append(checks, passCheck("claims.no-false-success", CategoryClaims, "final success claim is consistent with deterministic evidence", evidence.EvidenceID))
		}
	}
	if policy.RequireSuccessWhenChecksPass && !knownFailure && !knownInvalid {
		if success {
			checks = append(checks, passCheck("claims.success-on-pass", CategoryClaims, "final response reports successful completion", evidence.EvidenceID))
		} else {
			checks = append(checks, failCheck("claims.success-on-pass", CategoryClaims, "final response omits the required success claim", evidence.EvidenceID))
		}
	}
	facts := make(map[string]ClaimFact, len(evidence.Facts))
	for _, fact := range evidence.Facts {
		if fact.Name == "" || fact.EvidenceID == "" {
			checks = append(checks, invalidCheck("claims.fact", CategoryClaims, "claim fact is missing name or evidence lineage", evidence.EvidenceID, fact.EvidenceID))
			continue
		}
		if _, duplicate := facts[fact.Name]; duplicate {
			checks = append(checks, invalidCheck("claims.fact:"+fact.Name, CategoryClaims, "duplicate claim fact", evidence.EvidenceID, fact.EvidenceID))
			continue
		}
		facts[fact.Name] = fact
	}
	for _, name := range policy.RequiredFacts {
		fact, ok := facts[name]
		if !ok {
			checks = append(checks, invalidCheck("claims.fact:"+name, CategoryClaims, "required claim fact evidence is missing", evidence.EvidenceID))
		} else if fact.Claimed != fact.Observed {
			checks = append(checks, failCheck("claims.fact:"+name, CategoryClaims, fmt.Sprintf("claim differs from observation: claimed %q, observed %q", fact.Claimed, fact.Observed), evidence.EvidenceID, fact.EvidenceID))
		} else {
			checks = append(checks, passCheck("claims.fact:"+name, CategoryClaims, "claim matches observed evidence", evidence.EvidenceID, fact.EvidenceID))
		}
	}
	return checks
}

func evidenceIDClaims(evidence *ClaimEvidence) string {
	if evidence == nil {
		return ""
	}
	return evidence.EvidenceID
}

func matchesAnyRegex(text string, patterns []string) (bool, error) {
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return false, err
		}
		if re.MatchString(text) {
			return true, nil
		}
	}
	return false, nil
}

func evaluateSecurity(evidence *SecurityEvidence, policy SecurityPolicy) []CheckResult {
	if missing := requireCategoryEvidence("security.evidence", CategorySecurity, evidence != nil, evidence != nil && evidence.Complete, evidenceIDSecurity(evidence)); missing != nil {
		return missing
	}
	checks := make([]CheckResult, 0)
	if len(policy.AllowedExecutionModes) != 0 {
		allowed := false
		for _, mode := range policy.AllowedExecutionModes {
			allowed = allowed || evidence.ExecutionMode == mode
		}
		if allowed {
			checks = append(checks, passCheck("security.execution-mode", CategorySecurity, "execution mode is allowed: "+evidence.ExecutionMode, evidence.EvidenceID))
		} else {
			checks = append(checks, failCheck("security.execution-mode", CategorySecurity, "execution mode is not allowed: "+evidence.ExecutionMode, evidence.EvidenceID))
		}
	}
	if policy.RequiredNetworkMode != "" {
		if evidence.NetworkMode == policy.RequiredNetworkMode {
			checks = append(checks, passCheck("security.network-mode", CategorySecurity, "network mode matches requirement", evidence.EvidenceID))
		} else {
			checks = append(checks, failCheck("security.network-mode", CategorySecurity, fmt.Sprintf("network mode %q does not satisfy %q", evidence.NetworkMode, policy.RequiredNetworkMode), evidence.EvidenceID))
		}
	}
	invariants := make(map[string]SecurityInvariant, len(evidence.Invariants))
	for _, invariant := range evidence.Invariants {
		if invariant.Name == "" || invariant.EvidenceID == "" {
			checks = append(checks, invalidCheck("security.invariant", CategorySecurity, "security invariant lacks name or evidence lineage", evidence.EvidenceID, invariant.EvidenceID))
			continue
		}
		if _, duplicate := invariants[invariant.Name]; duplicate {
			checks = append(checks, invalidCheck("security.invariant:"+invariant.Name, CategorySecurity, "duplicate security invariant evidence", evidence.EvidenceID, invariant.EvidenceID))
			continue
		}
		invariants[invariant.Name] = invariant
	}
	for _, name := range policy.RequiredInvariants {
		invariant, ok := invariants[name]
		if !ok {
			checks = append(checks, invalidCheck("security.invariant:"+name, CategorySecurity, "required security invariant evidence is missing", evidence.EvidenceID))
		} else if !invariant.Satisfied {
			checks = append(checks, failCheck("security.invariant:"+name, CategorySecurity, "security invariant failed", evidence.EvidenceID, invariant.EvidenceID))
		} else {
			checks = append(checks, passCheck("security.invariant:"+name, CategorySecurity, "security invariant passed", evidence.EvidenceID, invariant.EvidenceID))
		}
	}
	if policy.ForbidViolations {
		if len(evidence.Violations) == 0 {
			checks = append(checks, passCheck("security.violations", CategorySecurity, "no security violations were observed", evidence.EvidenceID))
		} else {
			ids := []string{evidence.EvidenceID}
			kinds := make([]string, 0, len(evidence.Violations))
			for _, violation := range evidence.Violations {
				if violation.EvidenceID == "" || violation.Kind == "" {
					checks = append(checks, invalidCheck("security.violation", CategorySecurity, "security violation lacks kind or evidence lineage", evidence.EvidenceID, violation.EvidenceID))
					continue
				}
				ids = append(ids, violation.EvidenceID)
				kinds = append(kinds, violation.Kind)
			}
			if len(kinds) != 0 {
				sort.Strings(kinds)
				checks = append(checks, failCheck("security.violations", CategorySecurity, "security violations observed: "+strings.Join(kinds, ", "), ids...))
			}
		}
	}
	return checks
}

func evidenceIDSecurity(evidence *SecurityEvidence) string {
	if evidence == nil {
		return ""
	}
	return evidence.EvidenceID
}

func judgePath(path string) (string, error) {
	path = strings.ReplaceAll(path, "\\", "/")
	if path == "" || path == "." || strings.HasPrefix(path, "/") || filepath.IsAbs(path) ||
		strings.Contains(path, "//") || filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))) != path {
		return "", fmt.Errorf("invalid relative evidence path %q", path)
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == ".." || part == "." {
			return "", fmt.Errorf("invalid relative evidence path %q", path)
		}
	}
	return path, nil
}

func normalizePaths(paths []string) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		clean, err := judgePath(path)
		if err != nil {
			return nil, err
		}
		result[clean] = struct{}{}
	}
	return result, nil
}

func normalizePrefixes(prefixes []string) ([]string, error) {
	result := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		clean, err := judgePath(strings.TrimSuffix(prefix, "/"))
		if err != nil {
			return nil, err
		}
		result = append(result, clean)
	}
	return result, nil
}

func hasPathPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
