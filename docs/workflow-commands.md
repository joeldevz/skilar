# OpenCode workflow commands

The breaking workflow migration removes the `manager` and `linear-orchestrator` agents and the `/linear` command without aliases.

OpenCode `/commit` delegates exclusively to `skynex workflow deliver`, which requires current receipt authority and commits the exact candidate tree. `/pr` verifies the delivered receipt but fails closed because remote PR creation is not implemented. `/rollback` records human-readable intent/lineage through inspect/export and never mutates Git; rollback automation is deferred.

Planned workflows require `--plan-file` matching `schemas/workflow-plan.schema.json`. Discovery workflows require `--wayfinder-file` matching `schemas/wayfinder.schema.json`.

`start` infers the planned route when the request mentions a sensitive subject (secrets, authentication or OAuth, tokens or API keys, SDKs, dependencies, migrations, payments). An explicit `--route simple` for such a request is rejected instead of being silently upgraded, and an inferred planned start without `--plan-file` requires explicit `--accept`, `--check`, and `--path`.

`skynex workflow --help` lists every subcommand and `skynex workflow <command> --help` prints its exact flags; this document only covers behaviour that the flag help cannot show.

## Runtime preflight

`run` and `review` check the OpenCode runtime before creating any durable execution state, in both the foreground and `--detach` forms: the executable must resolve and be executable, `--model` and `--agent` identifiers must not contain control whitespace, the workflow must declare the `skynex-result-file-v1` result transport, the work directory must exist, and the temporary directory must be writable with at least 256 MiB free. Run inputs persisted before the transport was declared explicitly are backfilled during database migration, so an in-flight workflow never becomes permanently unrunnable.

A failed check returns a structured JSON error carrying `code`, `phase`, `retry_safe`, `mutation_outcome`, and a `next_action` hint, so automation can decide whether retrying is safe without parsing prose. Nothing is mutated: `--detach` creates no job record and spawns no worker, and `review` fails before the `candidate_frozen -> reviewing` transition.

## Detached execution

`run` and `review` accept `--detach`. A detached invocation persists a job record, spawns a background worker process, and returns immediately, so the terminal or OpenCode session that started it can be closed without losing the work. Worker output is appended to `<git-common-dir>/skynex/jobs/<job-id>.log`, next to the `workflows.db` state database.

Detach is Unix-only. On Windows the capability gate rejects the request before any job record or worker process is created, with `detached workflow execution is not supported on Windows; run without --detach`; the foreground run is unaffected.

## Resume and recovery

`resume` reconciles a blocked workflow against the live worktree and must hold the exclusive worktree lock while it does so. That lock has no Windows implementation, so resume is declared unsupported on Windows and fails closed before reading or mutating any workflow state, with `workflow resume is not supported on Windows because exclusive worktree locking is unavailable; abort the workflow or resume it from a Unix host`. A workflow blocked on Windows must be aborted or resumed from a Unix host; every other workflow command, including the foreground `run` and `review`, is portable across Linux and Windows.

Only one live job is admitted at a time. A job is considered healthy only when both its durable heartbeat is at most 30 seconds old and its recorded PID is alive, so a crashed worker or a reused PID is reconciled to `failed` instead of blocking the next run. Use `skynex workflow status` to observe state and `skynex workflow resume` to continue a blocked workflow after reconciliation.

A failed detached job is a durable workflow blocker rather than a dead side-car: the workflow moves to `blocked` with the state it failed from recorded as the resume target. Re-running the same `run --detach` or `review --detach` command retries that blocker directly, bypassing candidate recovery because a job failure never claimed a candidate tree change. Retries are bounded to three attempts per workflow and operation. `status` reports `attempt`, `retries_remaining`, a truncated error preview, and a `next` field that is `wait` while the job is healthy, the exact retry command once it is retryable, and `manual_resolution_required` when the attempts are exhausted; `inspect` reports the same attempt accounting for every job of the workflow.

## Correcting one failed verification check

`retry-verification` fixes a wrong check command without rerunning the coder. It applies only when the persisted verification result failed, and it replaces exactly one check: the `--check-id` must be the evidence ID of a check that actually failed, and its command must appear exactly once in the current contract, so an ambiguous duplicate is rejected.

The candidate is refrozen and compared before and after the replacement run; any change to the tree aborts the retry. Allowed paths, acceptance commands, and every other check are preserved, the previous result is archived as a numbered revision, and the revision record keeps `--actor`, `--reason`, the previous and replacement commands, and the candidate tree. Both the archived result and the revision lineage are visible in `skynex workflow inspect`.

`--idempotency-key` is required: repeating a key replays the recorded revision instead of creating a second one, and reusing a key with a different check, replacement, actor, or reason is rejected. A replacement that also fails makes the command fail and leaves the workflow in the failed verification state rather than freezing a candidate.

## Notifications

`notifications claim | ack | release | presence` is the terminal-notification channel that the OpenCode plugin polls, so a workflow that reaches a terminal state surfaces in the session that owns it.
