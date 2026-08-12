package metrics

import (
	"fmt"
	"math"
	"reflect"
	"sort"
)

// AggregateSession deduplicates messages by (session_id, message_id). Exact
// duplicate observations are ignored; conflicting observations make the result
// incomplete and are never added twice.
func AggregateSession(session SessionUsage) Aggregate {
	result := newAggregate()
	if session.SessionID == "" {
		result.addIssue(Issue{Code: "missing_session_id", Fatal: true})
		return result
	}
	result.Sessions = 1
	messages := append([]MessageUsage(nil), session.Messages...)
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].Sequence < messages[j].Sequence })
	seen := make(map[string]MessageUsage, len(messages))
	first := true
	for _, message := range messages {
		if message.SessionID == "" {
			message.SessionID = session.SessionID
		}
		if message.SessionID != session.SessionID {
			result.addIssue(Issue{Code: "message_session_mismatch", SessionID: session.SessionID, MessageID: message.MessageID, Fatal: true})
			continue
		}
		if message.MessageID == "" {
			result.addIssue(Issue{Code: "missing_message_id", SessionID: session.SessionID, Fatal: true})
			continue
		}
		if previous, ok := seen[message.MessageID]; ok {
			if sameMessageObservation(previous, message) {
				result.addIssue(Issue{Code: "duplicate_message", SessionID: session.SessionID, MessageID: message.MessageID})
			} else {
				result.addIssue(Issue{Code: "conflicting_message", SessionID: session.SessionID, MessageID: message.MessageID, Fatal: true})
			}
			continue
		}
		seen[message.MessageID] = message
		if err := message.Tokens.Validate(); err != nil {
			result.addIssue(Issue{Code: "invalid_token_usage", SessionID: session.SessionID, MessageID: message.MessageID, Detail: err.Error(), Fatal: true})
			continue
		}
		if message.DurationMS < 0 {
			result.addIssue(Issue{Code: "invalid_duration", SessionID: session.SessionID, MessageID: message.MessageID, Fatal: true})
			continue
		}
		if err := result.addMessage(message); err != nil {
			result.addIssue(Issue{Code: "usage_overflow", SessionID: session.SessionID, MessageID: message.MessageID, Detail: err.Error(), Fatal: true})
			continue
		}
		if first {
			result.FirstInputTokens = message.Tokens.Input
			first = false
		}
		if message.Tokens.Input > result.PeakInputTokens {
			result.PeakInputTokens = message.Tokens.Input
		}
	}
	result.finishCosts()
	return result
}

func sameMessageObservation(left, right MessageUsage) bool {
	// Sequence is collector-local ordering metadata. Two snapshots of the same
	// durable message may appear at different positions without changing usage.
	left.Sequence = 0
	right.Sequence = 0
	return reflect.DeepEqual(left, right)
}

// AggregateTree includes only sessions reachable from rootSessionID. Duplicate
// session snapshots are deduplicated. Orphans, cycles, and conflicting snapshots
// make telemetry incomplete rather than inflating totals.
func AggregateTree(rootSessionID string, sessions []SessionUsage) Aggregate {
	result := newAggregate()
	if rootSessionID == "" {
		result.addIssue(Issue{Code: "missing_root_session", Fatal: true})
		return result
	}
	unique := make(map[string]SessionUsage, len(sessions))
	for _, session := range sessions {
		if session.SessionID == "" {
			result.addIssue(Issue{Code: "missing_session_id", Fatal: true})
			continue
		}
		if previous, ok := unique[session.SessionID]; ok {
			if reflect.DeepEqual(previous, session) {
				result.addIssue(Issue{Code: "duplicate_session", SessionID: session.SessionID})
			} else {
				result.addIssue(Issue{Code: "conflicting_session", SessionID: session.SessionID, Fatal: true})
			}
			continue
		}
		unique[session.SessionID] = session
	}
	root, ok := unique[rootSessionID]
	if !ok {
		result.addIssue(Issue{Code: "root_session_not_found", SessionID: rootSessionID, Fatal: true})
		return result
	}
	if root.ParentID != "" {
		result.addIssue(Issue{Code: "root_has_parent", SessionID: rootSessionID, Detail: root.ParentID, Fatal: true})
	}
	children := make(map[string][]string)
	for id, session := range unique {
		if id == rootSessionID {
			continue
		}
		if session.ParentID == "" {
			result.addIssue(Issue{Code: "unlinked_session", SessionID: id, Fatal: true})
			continue
		}
		if _, parentExists := unique[session.ParentID]; !parentExists {
			result.addIssue(Issue{Code: "orphan_session", SessionID: id, Detail: session.ParentID, Fatal: true})
			continue
		}
		children[session.ParentID] = append(children[session.ParentID], id)
	}
	for parent := range children {
		sort.Strings(children[parent])
	}
	state := make(map[string]uint8, len(unique))
	var visit func(string)
	visit = func(id string) {
		if state[id] == 1 {
			result.addIssue(Issue{Code: "session_cycle", SessionID: id, Fatal: true})
			return
		}
		if state[id] == 2 {
			return
		}
		state[id] = 1
		summary := AggregateSession(unique[id])
		result.merge(summary)
		for _, childID := range children[id] {
			visit(childID)
		}
		state[id] = 2
	}
	visit(rootSessionID)
	for id := range unique {
		if state[id] == 0 {
			result.addIssue(Issue{Code: "unreachable_session", SessionID: id, Fatal: true})
		}
	}
	result.finishCosts()
	return result
}

// Summarize returns parent and complete-tree views from the same normalized
// evidence.
func Summarize(rootSessionID string, sessions []SessionUsage) UsageSummary {
	var parent Aggregate
	for _, session := range sessions {
		if session.SessionID == rootSessionID {
			parent = AggregateSession(session)
			break
		}
	}
	if parent.Sessions == 0 {
		parent = newAggregate()
		parent.addIssue(Issue{Code: "root_session_not_found", SessionID: rootSessionID, Fatal: true})
	}
	return UsageSummary{Parent: parent, Tree: AggregateTree(rootSessionID, sessions)}
}

func newAggregate() Aggregate {
	return Aggregate{Complete: true}
}

func (a *Aggregate) addIssue(issue Issue) {
	a.Issues = append(a.Issues, issue)
	if issue.Fatal {
		a.Complete = false
	}
}

func (a *Aggregate) addMessage(message MessageUsage) error {
	tokens, err := addTokens(a.Tokens, message.Tokens)
	if err != nil {
		return err
	}
	duration, err := safeAdd(a.DurationMS, message.DurationMS)
	if err != nil {
		return err
	}
	a.Tokens = tokens
	a.DurationMS = duration
	a.Messages++
	addCost(&a.ProviderCost, message.ProviderCost)
	addCost(&a.CalculatedCost, message.CalculatedCost)
	return nil
}

func (a *Aggregate) merge(other Aggregate) {
	firstReachableSession := a.Sessions == 0
	tokens, err := addTokens(a.Tokens, other.Tokens)
	if err != nil {
		a.addIssue(Issue{Code: "usage_overflow", Detail: err.Error(), Fatal: true})
	} else {
		a.Tokens = tokens
	}
	duration, err := safeAdd(a.DurationMS, other.DurationMS)
	if err != nil {
		a.addIssue(Issue{Code: "duration_overflow", Detail: err.Error(), Fatal: true})
	} else {
		a.DurationMS = duration
	}
	a.Messages += other.Messages
	a.Sessions += other.Sessions
	if firstReachableSession {
		a.FirstInputTokens = other.FirstInputTokens
	}
	if other.PeakInputTokens > a.PeakInputTokens {
		a.PeakInputTokens = other.PeakInputTokens
	}
	mergeCost(&a.ProviderCost, other.ProviderCost)
	mergeCost(&a.CalculatedCost, other.CalculatedCost)
	a.Issues = append(a.Issues, other.Issues...)
	if !other.Complete {
		a.Complete = false
	}
}

func (a *Aggregate) finishCosts() {
	finishCost(&a.ProviderCost, a.Messages)
	finishCost(&a.CalculatedCost, a.Messages)
}

func addCost(total *CostTotal, value CostValue) {
	if !value.Available || math.IsNaN(value.USD) || math.IsInf(value.USD, 0) || value.USD < 0 {
		total.UnavailableValues++
		reason := value.Reason
		if reason == "" {
			reason = "cost_unavailable"
		}
		addCostReason(total, reason)
		return
	}
	if total.KnownUSD > math.MaxFloat64-value.USD {
		total.UnavailableValues++
		addCostReason(total, "cost_overflow")
		return
	}
	total.KnownUSD += value.USD
}

func mergeCost(total *CostTotal, other CostTotal) {
	total.UnavailableValues += other.UnavailableValues
	for _, reason := range other.UnavailableReasons {
		addCostReason(total, reason)
	}
	if math.IsNaN(other.KnownUSD) || math.IsInf(other.KnownUSD, 0) || other.KnownUSD < 0 || total.KnownUSD > math.MaxFloat64-other.KnownUSD {
		total.UnavailableValues++
		addCostReason(total, "cost_overflow")
		return
	}
	total.KnownUSD += other.KnownUSD
}

func addCostReason(total *CostTotal, reason string) {
	for _, existing := range total.UnavailableReasons {
		if existing == reason {
			return
		}
	}
	total.UnavailableReasons = append(total.UnavailableReasons, reason)
	sort.Strings(total.UnavailableReasons)
}

func finishCost(total *CostTotal, messages int) {
	total.Available = messages > 0 && total.UnavailableValues == 0
	if total.Available {
		total.USD = total.KnownUSD
	} else {
		total.USD = 0
	}
}

func addTokens(left, right TokenUsage) (TokenUsage, error) {
	var result TokenUsage
	var err error
	if result.Input, err = safeAdd(left.Input, right.Input); err != nil {
		return TokenUsage{}, fmt.Errorf("input: %w", err)
	}
	if result.Output, err = safeAdd(left.Output, right.Output); err != nil {
		return TokenUsage{}, fmt.Errorf("output: %w", err)
	}
	if result.Reasoning, err = safeAdd(left.Reasoning, right.Reasoning); err != nil {
		return TokenUsage{}, fmt.Errorf("reasoning: %w", err)
	}
	if result.CacheRead, err = safeAdd(left.CacheRead, right.CacheRead); err != nil {
		return TokenUsage{}, fmt.Errorf("cache read: %w", err)
	}
	if result.CacheWrite, err = safeAdd(left.CacheWrite, right.CacheWrite); err != nil {
		return TokenUsage{}, fmt.Errorf("cache write: %w", err)
	}
	return result, nil
}

func safeAdd(left, right int64) (int64, error) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, fmt.Errorf("integer overflow")
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, fmt.Errorf("integer underflow")
	}
	return left + right, nil
}
