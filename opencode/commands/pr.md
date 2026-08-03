---
description: Verify workflow receipt authority before a pull request
agent: orchestrator
---

Fail closed. First run `skynex workflow receipt <workflow-id>` and `skynex workflow status <workflow-id>` for the ID in `{argument}`. A current authoritative receipt and `delivered` state are mandatory.

Remote PR automation is not implemented in this release. Do not push and do not call `gh pr create`. After verifying authority, report that PR creation is deferred and provide the verified receipt ID and delivered commit for a future remote-delivery adapter.
