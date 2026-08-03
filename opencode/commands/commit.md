---
description: Deliver the authoritative Skynex workflow candidate as an exact-tree local commit
agent: orchestrator
---

Fail closed. Do not stage files or call `git commit` directly.

1. Run `skynex workflow status` and identify the workflow ID supplied in `{argument}`.
2. Require the workflow to be `receipted` with current receipt authority.
3. Run:
   `skynex workflow deliver --id <workflow-id> --message "<message>" --idempotency-key "opencode-commit-<workflow-id>"`
4. Report the exact commit/tree/receipt returned by the delivery gate.

Never push. If no receipted workflow ID is provided, stop and explain the required workflow commands.
