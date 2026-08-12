# Workflow V2 public canary

This public suite is a fast rejection filter for `workflow-orchestrator` changes.
It is not a holdout and it is not release evidence.

The suite contains exactly three critical cases with one run each:

- `wfv2_canary_low_complete`: an evaluator-owned deterministic worker lets the
  real Workflow V2 engine complete one LOW slice in the foreground and freeze a
  verified candidate.
- `wfv2_canary_detach_wake`: evaluator-owned deterministic worker/reviewer output
  isolates durable detach, plugin notification, same-session wake, automatic
  review continuation, and receipt issuance.
- `wfv2_canary_recovery_safety`: a deterministic malformed first invocation
  leaves an active persisted attempt; the orchestrator must preserve the workflow
  and attempt identities, reject an unrelated stale result, and recover without
  approval or delivery.

Only the root `workflow-orchestrator` turn is model-backed in this canary. The
Workflow V2 engine is real, while worker and reviewer responses come from
evaluator-owned deterministic adapters; nested live-model execution belongs to
the full public suite.

## Driver contract

Every case declares `extensions.x-workflow-driver-v1` with exactly:

- `mode`: `foreground` or `managed-detach`;
- `workflow_id`: the fixed public workflow identity;
- `terminal_state`: the state the driver must observe before finishing the case;
- `autonomous_turns`: additional plugin-triggered turns, from zero through two.

For `managed-detach`, the driver must retain the same OpenCode session after the
initial response, observe each claimed notification, allow the plugin prompt to
run, wait for the declared terminal state, and collect the complete multi-turn
trace. Reaching the text of a state without the persisted state and notification
evidence is a failure. The hard four-minute case timeout includes all autonomous
turns and clean quiescence.

## Promotion semantics

A candidate is promoted only when both A/B arms complete all three cases (6/6)
with contractual `pass`, zero hard or security regression, no control-pass to
candidate-fail coordinate, and candidate tree input-token usage no greater than
1.15 times control. A behavioral failure, timeout, or ratio above 1.15 rejects the
candidate. `invalid`, `infra_error`, `aborted`, incomplete telemetry, or incomplete
cleanup makes the canary inconclusive rather than blaming the candidate. No retries
and no secret holdout are used at this level. Six samples at four minutes each
impose a strict maximum model-execution budget of 24 minutes, leaving the enclosing
30-minute canary deadline for preflight, comparison, and artifact sealing.
