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

`status` and `inspect` open the database read-only and stay read-only while every job of the inspected workflows is healthy. Only when their liveness probe finds an orphaned worker do they reopen read-write to persist that reconciliation: the dead job is failed and the workflow moves to `blocked` with the state it failed from as its resume target. They never mutate the worktree, the candidate, or the execution graph.

`resume` reconciles against a recovery basis that is persisted by the workflow itself: `start` records the context seal and the frozen basis tree, each brokered mutation records its exact pre and post trees, and verification records the candidate record, tree, and policy hash. A workflow created before this basis existed has none, so `resume` fails closed with `workflow: recovery basis artifact is missing`; retry the failed job with the `next` command reported by `status`, or abort it.

An execution interrupted between the broker's durable mutation commit and the slice completion that follows it is repaired on the next run. Reconciliation adopts only slices whose mutation already reached `completed` with its attempt retired and no live attempt remaining, so a still-fenced worker is never displaced, and it then advances a fully executed workflow to verification.

A worker that died mid-apply — after its patch reached the worktree but before the broker committed — is reconciled when its still-live attempt is inherited. Under the worktree lease, the recorded pre and post trees are compared against the live tree: a patch that fully landed is adopted as the completed mutation the broker would have recorded, a worktree still at the basis tree is simply dispatched again, and any other tree fails closed into `integration_conflict` instead of retrying a claim that can never succeed.

Only a process that owns the work may perform that reconciliation. Executing a workflow takes a durable execution fence keyed to a private per-process identity, held by the detached worker and the foreground CLI alike, so two concurrent executions of the same workflow can never share or adopt each other's attempt. The attempt row publishes the worktree owner and fencing token — any second process could reproduce them and heartbeat that lease — but nothing publishes the fence identity. A second executor is refused immediately; the fence expires 30 seconds after its last heartbeat, so a crashed executor frees the workflow on the same schedule as a dead detached worker.

On top of that fence, a foreground `run` or `review` refuses up front while a healthy detached worker holds the workflow, naming the live job and the exact abort command instead of failing on the fence. The same liveness predicate that retires a dead worker decides it, so a crashed worker never blocks the next run.

`integration_conflict` is a fail-closed sink: no command advances it, so `status` and `inspect` report the exact `skynex workflow abort` invocation as the next action instead of `wait`. `status` prints it on a `NEXT` line even when the workflow has no job record, and `inspect` carries it as `next_action`.

Claiming a slice and moving the workflow from `ready` to `executing` commit together, so an interrupted activation can never leave a `ready` workflow holding an active slice.

A failed detached job is a durable workflow blocker rather than a dead side-car: the workflow moves to `blocked` with the state it failed from recorded as the resume target. Re-running the same `run --detach` or `review --detach` command retries that blocker directly, bypassing candidate recovery because a job failure never claimed a candidate tree change. Retries are bounded to three attempts per workflow and operation. `status` reports `attempt`, `retries_remaining`, a truncated error preview, and a `next` field that is `wait` while the job is healthy, the exact retry command once it is retryable, and `manual_resolution_required` when the attempts are exhausted; `inspect` reports the same attempt accounting for every job of the workflow.

## Correcting one failed verification check

`retry-verification` fixes a wrong check command without rerunning the coder. It applies only when the persisted verification result failed, and it replaces exactly one check: the `--check-id` must be the evidence ID of a check that actually failed, and its command must appear exactly once in the current contract, so an ambiguous duplicate is rejected.

The candidate is refrozen and compared before and after the replacement run; any change to the tree aborts the retry. Allowed paths, acceptance commands, and every other check are preserved, the previous result is archived as a numbered revision, and the revision record keeps `--actor`, `--reason`, the previous and replacement commands, and the candidate tree. Both the archived result and the revision lineage are visible in `skynex workflow inspect`.

`--idempotency-key` is required: repeating a key replays the recorded revision instead of creating a second one, and reusing a key with a different check, replacement, actor, or reason is rejected. A replacement that also fails makes the command fail and leaves the workflow in the failed verification state rather than freezing a candidate.

## Notifications

`notifications claim | ack | release | presence` is the terminal-notification channel that the OpenCode plugin polls, so a workflow that reaches a terminal state surfaces in the session that owns it.
