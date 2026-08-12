# Design Tree - Unified Adaptive Orchestrator

## Design concept

Skynex keeps one user-facing `orchestrator` agent backed by a deterministic Go
workflow engine. The engine owns state, transitions, hashes, budgets, frontier,
and receipts. Agents perform semantic work through artifact IDs instead of
carrying large payloads through chat.

The design combines:

- Wayfinder-style discovery maps for uncertain work.
- Dependency-aware vertical slices for planned execution.
- Receipt-Driven Development for risk-adaptive review and delivery authority.
- Existing Skynex workers and checks where they remain useful.

## Resolved decisions

- D1: Keep exactly one coordinator named `orchestrator` - remove `manager`,
  `linear-orchestrator`, `/linear`, and their independent workflow logic.
- D2: Make an immediate breaking change - do not preserve compatibility aliases
  or legacy orchestration behavior.
- D3: Route every request automatically into one of three paths:
  `simple`, `planned`, or `discovery`.
- D4: Show the selected route, risk, and a one-line rationale, then continue
  without asking for approval.
- D5: Allow an explicit route override - record the actor and reason, but never
  allow an override to lower mandatory risk protections.
- D6: Use `simple` for clear, bounded work that can be executed directly.
- D7: Use `planned` for clear but multi-slice work.
- D8: Use `discovery` when blocking product or technical uncertainty prevents an
  honest execution contract.
- D9: Represent discovery as a dynamic dependency graph with `fog` and
  `frontier`.
- D10: Start with four explicit discovery node types: `research`, `prototype`,
  `grill`, and `task`. Revisit simplification only after observing real usage.
- D11: Present one blocking Grill question at a time - choose the decision that
  unlocks the largest part of the frontier and recalculate after each answer.
- D12: Close discovery when an executable contract exists - the destination is
  clear, acceptance criteria are verifiable, and no blocking decision remains.
  Non-blocking uncertainty stays recorded as assumptions or risks.
- D13: Represent planned execution as a small graph of dependency-aware vertical
  slices, not a prescriptive sequential `PLAN.md` with exact implementation
  snippets.
- D14: Allow execution to return automatically to discovery when new blocking
  evidence invalidates the current plan. Preserve unaffected completed work.
- D15: Run read-only research and review nodes in parallel.
- D16: Run parallel code slices only in isolated Git worktrees, followed by
  deterministic reconciliation into the candidate tree.
- D17: Use TDD automatically and proportionally - no human RED gate. A slice
  adds or updates tests when they provide useful behavioral evidence; trivial
  changes may rely on existing checks.
- D18: Ask for human approval only for a real blocker, irreversible/high-risk
  decision, insufficient evidence, or exhausted repair budget.
- D19: Implement workflow authority in Go - prompts cannot own IDs, legal state
  transitions, hashes, frontier calculation, repair budgets, or receipts.
- D20: Keep chat as the primary interface and expose `skynex workflow` commands
  for `status`, `inspect`, `resume`, `abort`, `export`, and `receipt` diagnostics.
- D21: Persist canonical workflow state in project-local SQLite at
  `.skynex/workflows.db`, ignored by Git.
- D22: Allow the workflow database path to be overridden in
  `.skynex/project-config.yaml`.
- D23: Store maps, slices, findings, evidence, repair contracts, and receipts as
  addressable artifacts with stable IDs.
- D24: Pass artifact IDs between the orchestrator and workers instead of copying
  plans, diffs, findings, or evidence into chat context.
- D25: Export only a human-readable workflow summary and the final receipt by
  default. Keep detailed artifacts in SQLite and export them on demand.
- D26: Do not stage exported summaries or receipts automatically.
- D27: Always produce a lightweight receipt bound to the exact candidate hash.
- D28: Select adversarial review depth with the Gentle-style `0 / 1 / 4` model:
  `0` runs deterministic checks only, `1` runs one relevant review lens, and
  `4` runs all four lenses independently in parallel.
- D29: Calculate risk with deterministic Go rules plus semantic assessment. Hard
  rules set a minimum; semantic assessment may elevate but never lower it.
- D30: Use four adversarial lenses: Risk, Readability, Reliability, and
  Resilience.
- D31: Verify functional intent and acceptance criteria once before 4R review,
  outside the four-lens fan-out.
- D32: Replace overlapping default validation fan-outs with deterministic checks,
  a reviewer parameterized by a selected 4R lens, one bounded fixer, and a
  separate read-only fix validator.
- D33: Only severe findings caused by the candidate and backed by reproducible
  evidence may block or enter automatic repair. Record pre-existing findings
  separately.
- D34: Permit one automatic bounded repair attempt. Register finding IDs,
  allowed paths, prohibited changes, and repair budget before editing. Default
  to at most 5 files and 120 changed lines; make both limits configurable.
- D35: After repair, use a distinct read-only validator to return `pass`, `fail`,
  or `inconclusive` for each finding ID.
- D36: If the repair fails, exceeds budget, or remains inconclusive, replan the
  affected slice or create a human blocker according to risk. Never enter an
  unbounded fix loop.
- D37: Generate a receipt for every integrated slice and a final aggregate
  receipt for the complete workflow.
- D38: Make receipts delivery authority for the exact candidate. A byte change
  invalidates the relevant receipt.
- D39: Make receipt enforcement configurable as `advisory` or `enforced` per
  project. High-risk policy may forbid bypass.
- D40: Build V1 as a local-only system. Define tracker adapter boundaries, but do
  not implement Linear or GitHub synchronization initially.
- D41: Treat dependency, schema, and public API changes discovered during repair
  as replan conditions rather than permitted bounded-fix work.
- D42: Implement post-delivery rollback as a new workflow linked to the original
  receipt. The revert candidate receives proportional validation and its own
  receipt; rollback never mutates historical authorization records.
- D43: Apply the lightweight receipt requirement to the `simple` route as well as
  planned and discovery workflows.

## Canonical flow

```text
request
  -> classify(simple | planned | discovery)
  -> announce route + risk + reason
  -> discovery map when blocking uncertainty exists
  -> executable contract
  -> dependency-aware slices
  -> execute in isolated worktree when parallel
  -> verify acceptance criteria
  -> deterministic checks
  -> freeze candidate hash
  -> review depth 0 | 1 | 4
  -> optional one bounded fix
  -> read-only fix validation
  -> slice receipt
  -> reconcile next frontier
  -> aggregate final receipt
  -> commit/push/PR gate according to project policy
  -> optional linked rollback workflow after delivery
```

## Open assumptions

- A1: SQLite is available through the existing Go toolchain without introducing
  an unacceptable binary distribution cost - validate driver choice in the PRD
  and technical plan.
- A2: Git worktrees are available for repositories that opt into parallel code
  slices - define a sequential fallback for non-Git or constrained environments.
- A3: Candidate hashing can exclude known workflow metadata without allowing
  meaningful source changes to evade receipt invalidation.
- A4: Risk rules can be configured without becoming another large policy DSL -
  start with a small built-in schema and project overrides.
- A5: Current worker agents can be repurposed or consolidated without preserving
  their conflicting return-envelope contracts.

## Out of scope

- Linear synchronization or a replacement `/linear` workflow.
- GitHub issue-map synchronization.
- Compatibility aliases for `manager` or `linear-orchestrator`.
- A hosted or multi-user workflow database.
- Mandatory full 4R review for every change.
- Unlimited automatic repair loops.
- Treating a receipt as proof that software is universally correct.
- Simplifying the four discovery node types before real usage data exists.

## Ready for PRD

YES. Product behavior and architectural boundaries are resolved. The PRD must
turn the open assumptions into explicit technical acceptance criteria and define
the V1 migration/removal inventory.
