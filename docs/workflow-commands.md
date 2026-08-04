# OpenCode workflow commands

The breaking workflow migration removes the `manager` and `linear-orchestrator` agents and the `/linear` command without aliases.

OpenCode `/commit` delegates exclusively to `skynex workflow deliver`, which requires current receipt authority and commits the exact candidate tree. `/pr` verifies the delivered receipt but fails closed because remote PR creation is not implemented. `/rollback` records human-readable intent/lineage through inspect/export and never mutates Git; rollback automation is deferred.

Planned workflows require `--plan-file` matching `schemas/workflow-plan.schema.json`. Discovery workflows require `--wayfinder-file` matching `schemas/wayfinder.schema.json`.

`skynex workflow --help` lists every subcommand and `skynex workflow <command> --help` prints its exact flags; this document only covers behaviour that the flag help cannot show.

## Detached execution

`run` and `review` accept `--detach`. A detached invocation persists a job record, spawns a background worker process, and returns immediately, so the terminal or OpenCode session that started it can be closed without losing the work. Worker output is appended to `<git-common-dir>/skynex/jobs/<job-id>.log`, next to the `workflows.db` state database.

Detach is Unix-only. On Windows the capability gate rejects the request before any job record or worker process is created, with `detached workflow execution is not supported on Windows; run without --detach`; the foreground run is unaffected.

Only one live job is admitted at a time. A job is considered healthy only when both its durable heartbeat is at most 30 seconds old and its recorded PID is alive, so a crashed worker or a reused PID is reconciled to `failed` instead of blocking the next run. Use `skynex workflow status` to observe state and `skynex workflow resume` to continue a blocked workflow after reconciliation.

`notifications claim | ack | release | presence` is the terminal-notification channel that the OpenCode plugin polls, so a workflow that reaches a terminal state surfaces in the session that owns it.
