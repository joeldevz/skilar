---
description: Record rollback intent and lineage without mutating Git
agent: orchestrator
---

Do not run `git reset`, `git checkout`, `git revert`, or delete files. Inspect the workflow and receipt history, then record/export the rollback intent and lineage using `skynex workflow inspect <workflow-id>` and `skynex workflow export <workflow-id> --out <safe-path>`.

Rollback automation is deferred. Report the candidate/receipt/commit lineage and explain that no repository mutation was performed.
