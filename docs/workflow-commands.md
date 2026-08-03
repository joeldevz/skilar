# OpenCode workflow commands

The breaking workflow migration removes the `manager` and `linear-orchestrator` agents and the `/linear` command without aliases.

OpenCode `/commit` delegates exclusively to `skynex workflow deliver`, which requires current receipt authority and commits the exact candidate tree. `/pr` verifies the delivered receipt but fails closed because remote PR creation is not implemented. `/rollback` records human-readable intent/lineage through inspect/export and never mutates Git; rollback automation is deferred.

Planned workflows require `--plan-file` matching `schemas/workflow-plan.schema.json`. Discovery workflows require `--wayfinder-file` matching `schemas/wayfinder.schema.json`.
