package workflow

var forwardTransitions = map[State]map[State]struct{}{
	StateCreated:             set(StateDiscovering),
	StateDiscovering:         set(StateReady),
	StateReady:               set(StateExecuting),
	StateExecuting:           set(StateVerifying, StateIntegrationConflict),
	StateVerifying:           set(StateCandidateFrozen),
	StateCandidateFrozen:     set(StateReviewing),
	StateReviewing:           set(StateReceipted),
	StateReceipted:           set(StateDelivered),
	StateIntegrationConflict: set(StateReady),
	StateReplanRequired:      set(StateDiscovering, StateVerifying),
}

var nonTerminalStates = set(
	StateCreated, StateDiscovering, StateReady, StateExecuting, StateVerifying,
	StateCandidateFrozen, StateReviewing, StateReceipted, StateBlocked,
	StateReplanRequired, StateIntegrationConflict,
)

var blockableStates = set(
	StateDiscovering, StateReady, StateExecuting, StateVerifying,
	StateCandidateFrozen, StateReviewing, StateReceipted,
)

var replanStates = set(
	StateDiscovering, StateReady, StateExecuting, StateVerifying,
	StateCandidateFrozen, StateReviewing, StateReceipted, StateBlocked,
)

func set(states ...State) map[State]struct{} {
	result := make(map[State]struct{}, len(states))
	for _, state := range states {
		result[state] = struct{}{}
	}
	return result
}

func CanTransition(from, to State, resumeTarget State) bool {
	if _, ok := forwardTransitions[from][to]; ok {
		return true
	}
	if _, ok := blockableStates[from]; ok && to == StateBlocked {
		return true
	}
	if _, ok := replanStates[from]; ok && to == StateReplanRequired {
		return true
	}
	if _, ok := nonTerminalStates[from]; ok && (to == StateAborted || to == StateFailed) {
		return true
	}
	if from == StateBlocked && to == resumeTarget && resumeTarget != "" {
		_, valid := blockableStates[to]
		return valid && to != StateBlocked
	}
	if from == StateBlocked && to == StateIntegrationConflict {
		return true
	}
	return false
}
