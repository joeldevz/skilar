# Design Tree - Unified Adaptive Orchestrator, Revised V1

## Document status

This document supersedes the decisions in `design-tree.md` for the architecture
spike. The source document remains unchanged as design history.

Status: **Ready for architecture spike, not implementation PRD.**

## Design concept

Skynex presents one user-facing `orchestrator`, backed by two internal authorities
implemented in Go:

- The **Orchestration Engine** owns routing, discovery, execution slices,
  dependencies, worktree mutation, and integration.
- The **Review Engine** owns the frozen candidate, effective risk, Receipt-Driven
  Development (RDD), correction decisions, the final receipt, and the managed
  delivery gate.

The engines exchange immutable artifact IDs and state transitions. Neither an
agent nor a prompt can own workflow authority.

The design deliberately separates two graphs:

- The **Wayfinder graph** resolves uncertainty through decisions, fog, Research,
  Prototype, and Grill nodes.
- The **execution graph** contains production-ready vertical slices.

The graphs have distinct schemas and lifecycles. Typed lineage relations connect
them without turning discovery artifacts into production code.

## V1 proof obligations

Git-backed managed V1 proves only three promises:

1. A workflow resumes correctly after interruption.
2. Review effort is proportional to deterministic and semantic risk.
3. Skynex never authorizes bytes different from the bytes reviewed.

Features that do not strengthen one of these guarantees should be deferred.

## Resolved decisions

- D1: Keep exactly one coordinator named `orchestrator`. Internally separate the
  Orchestration Engine from the Review Engine. Remove `manager`,
  `linear-orchestrator`, `/linear`, and their independent workflow authority.
- D2: Make an immediate breaking change. Do not preserve compatibility aliases or
  legacy orchestration behavior.
- D3: Route every request automatically into one of three paths: `simple`,
  `planned`, or `discovery`.
- D4: Show the selected route, provisional minimum risk, and a one-line rationale,
  then continue without asking for routine approval. Announce final effective
  risk only after it is recalculated for the frozen candidate.
- D5: Allow an explicit route override. Persist the actor and reason, but never
  allow an override to lower mandatory risk protections.
- D6: Use `simple` for clear, bounded work that can be represented as one
  production slice.
- D7: Use `planned` for clear but multi-slice work.
- D8: Use `discovery` when blocking product or technical uncertainty prevents an
  honest execution contract.
- D9: Represent discovery in a Wayfinder graph with explicit fog, frontier, and
  typed dependency relations.
- D10: Use five discovery node types: `decision`, `fog`, `research`, `prototype`,
  and `grill`. Production tasks do not belong in this graph.
- D11: Present one blocking Grill question at a time. Choose the decision that
  unlocks the largest part of the frontier and recalculate after each answer.
- D12: Close discovery when an executable contract exists: the destination is
  clear, acceptance criteria are verifiable, and no blocking decision remains.
  Record non-blocking uncertainty as assumptions or risks.
- D13: Represent production work in a separate dependency-aware execution graph
  of vertical slices, not in the Wayfinder graph and not as a prescriptive
  sequential `PLAN.md` containing exact implementation snippets.
- D14: Allow execution evidence to invalidate discovery conclusions and return the
  workflow to discovery. Preserve unaffected completed work and connect both
  graphs with `unblocks`, `invalidates`, and `derived_from` lineage records.
- D15: Run read-only research and review nodes in parallel when their inputs are
  immutable.
- D16: Run all mutating production slices sequentially in V1 with one writer per
  worktree. Defer isolated parallel worktrees and deterministic reconciliation to
  V1.1.
- D17: Use TDD automatically and proportionally, without a human RED gate. A slice
  adds or updates tests when they provide useful behavioral evidence; trivial
  changes may rely on existing checks.
- D18: Continue low-risk work automatically. Require human approval for high-risk
  or irreversible actions and human validation for every Prototype. Keep a kill
  switch available at all times, record manual route overrides, and never require
  routine approval for each slice.
- D19: Implement workflow authority in Go. Prompts cannot own IDs, legal state
  transitions, candidate identity, frontier calculation, risk floors, repair
  budgets, or receipts.
- D20: Keep chat as the primary interface and expose `skynex workflow` commands for
  `status`, `inspect`, `resume`, `abort`, `export`, and `receipt` diagnostics.
- D21: In Git repositories, persist canonical workflow state at the resolved
  absolute path `$(git rev-parse --git-common-dir)/skynex/workflows.db` so all
  worktrees share one authority.
- D22: Keep the V1 database location fixed and worktree-safe. Defer path
  configuration until a later release.
- D23: Store maps, slices, findings, evidence, correction contracts, candidates,
  transition events, and receipts as addressable artifacts with stable IDs.
- D24: Pass artifact IDs between the orchestrator and workers instead of copying
  plans, diffs, findings, or evidence into chat context.
- D25: Export only a human-readable workflow summary and the final receipt by
  default. Keep detailed artifacts in canonical storage and export them only on
  explicit request.
- D26: Do not stage exported summaries or receipts automatically.
- D27: Produce one authoritative final receipt bound to one exact Git candidate
  tree. Slice completion produces checkpoints and evidence records, not delivery
  receipts.
- D28: Select adversarial review depth with the Gentle-style `0 / 1 / 4` model:
  `0` runs deterministic checks only, `1` runs one recorded relevant lens, and
  `4` runs all four lenses independently in parallel.
- D29: Calculate risk with deterministic Go rules plus semantic assessment. Go
  rules set the minimum; semantic assessment may elevate but never lower it. Every
  elevation must be auditable and must raise or preserve review depth.
- D30: Use four adversarial lenses: Risk, Readability, Reliability, and
  Resilience.
- D31: Verify functional intent and acceptance criteria once before 4R review,
  outside the four-lens fan-out.
- D32: Replace overlapping validation fan-outs with deterministic checks,
  acceptance verification, and reviewers parameterized by selected 4R lenses.
  Keep correction decisions separate from worktree mutation.
- D33: Only severe findings caused by the candidate and backed by reproducible
  evidence may block delivery or enter a future automatic repair flow. Record
  pre-existing findings separately.
- D34: In V1, a blocking finding transitions to `blocked` or `replan_required`.
  Do not perform automatic repair.
- D35: In V1.1, any automatic repair must be bounded both absolutely and
  proportionally: at most 5 files and 120 changed lines, and never more files or
  changed lines than the candidate it repairs.
- D36: After any manual or future automatic correction, rerun deterministic
  checks, acceptance verification, candidate freeze, and risk-adaptive review.
  Failed, over-budget, or inconclusive correction transitions to
  `replan_required`; never enter an unbounded fix loop.
- D37: Generate checkpoint and evidence records per slice, then exactly one
  authoritative receipt for the final candidate.
- D38: Make the final receipt delivery authority for its exact candidate tree,
  policy, and evidence set. Any candidate drift invalidates delivery authority.
- D39: Enforce receipt verification only for operations managed by Skynex in V1,
  including rebound `/commit` and `/pr` flows and any push performed by `/pr`.
  Standalone external Git operations remain outside Skynex enforcement.
- D40: Build V1 as a local-only system. Define tracker adapter boundaries, but do
  not implement Linear or GitHub synchronization initially.
- D41: Treat dependency, schema, and public API changes discovered during
  correction as replan conditions rather than permitted bounded-repair work.
- D42: Record rollback lineage to the original candidate and receipt in V1, but
  defer automated post-delivery rollback workflows. A later automated rollback
  must create and review a new revert candidate without mutating historical
  authorization records.
- D43: Apply the single final receipt requirement to the `simple` route as well as
  `planned` and `discovery` workflows.

## Architecture decisions in priority order

### 1. Separate internal authorities

| Authority | Owns | Must not own |
| --- | --- | --- |
| Orchestration Engine | Route selection, Wayfinder graph, execution graph, slices, dependency readiness, writer lease, mutation broker, and integration | Risk calculation, review verdicts, receipt issuance, or delivery authorization |
| Review Engine | Frozen candidate record, deterministic risk floor, semantic risk elevation, RDD depth, findings, correction decision, final receipt, and managed delivery gate | Route selection, discovery frontier, production slice scheduling, or mutation of a frozen candidate |

The Review Engine owns the correction lifecycle but not the writer. In V1 a
blocking correction decision returns control to the Orchestration Engine as
`blocked` or `replan_required`. In a future bounded-repair flow, the Review Engine
issues a correction contract and the Orchestration Engine schedules the mutation.
The result is always a new candidate that passes the complete verification and
review cycle.

The user sees one `orchestrator`; internal authority boundaries are visible only
through status, diagnostics, and audit records.

The Review Engine exposes a deterministic risk-policy operation to the
Orchestration Engine. This lets routing display a provisional minimum risk without
transferring ownership of risk policy. The Review Engine recalculates the final
minimum and effective risk against the frozen candidate.

### 2. Separate the two graphs

The Wayfinder graph and execution graph use different node schemas and cannot
share executable node IDs.

| Graph | Nodes | Completion condition | Mutability |
| --- | --- | --- | --- |
| Wayfinder | Decision, Fog, Research, Prototype, Grill | An executable contract exists and no blocking decision remains | Dynamic while `discovering` |
| Execution | Production slices with acceptance criteria and dependencies | Every required slice is integrated and verified | Versioned; changes preserve lineage and may require replanning |

Cross-graph relations are immutable lineage records:

- `unblocks`: a discovery outcome makes an execution slice schedulable.
- `invalidates`: new evidence makes a discovery conclusion or execution slice
  obsolete.
- `derived_from`: a production contract or slice traces to discovery evidence.

A Prototype is disposable evidence, never production input. Prototype source is
kept outside the candidate. Human validation is required before its conclusions
can unblock discovery, and any production implementation derived from it must be
created as a new execution slice. Skynex never copies or promotes Prototype bytes
automatically into a deliverable candidate.

### 3. Define the state machine

The minimum happy path is:

```text
created -> discovering -> ready -> executing -> verifying
        -> candidate_frozen -> reviewing -> receipted -> delivered
```

Alternative states are:

```text
blocked | replan_required | integration_conflict | aborted | failed
```

`blocked`, `replan_required`, and `integration_conflict` retain a recorded
`resume_target`. `aborted` is terminal unless a new workflow is created. `failed`
means the engine cannot recover safely from available state; an ordinary process
crash does not imply `failed`.

All routes enter `discovering`. For `simple` and `planned`, this is a durable
zero-node Wayfinder graph that closes immediately in a separate CAS transition.
This keeps one recoverable state path without pretending that discovery work ran.

Every transition is a compare-and-swap (CAS) on `workflow_id`, `state_version`,
and expected state. In one SQLite transaction it writes the new state, produced
artifact references, and an immutable transition event with an idempotency key.
Repeating a committed transition returns the original result. An incomplete
transaction rolls back and can be retried after lease recovery.

| Transition | Preconditions | Actor | Produced artifacts | Idempotency and crash recovery |
| --- | --- | --- | --- | --- |
| `created -> discovering` | Request, repository identity, symbolic HEAD or detached state, base ref, base commit, and base tree persisted; no active execution lease | Orchestration Engine, using a Review Engine risk-floor artifact | Route decision, creation-time repository context seal, provisional deterministic risk floor, initial or zero-node Wayfinder graph | `workflow_id:discover:v1` returns the existing graph; retry after transaction rollback |
| `discovering -> ready` | Executable contract exists; no blocking fog or unvalidated Prototype; symbolic HEAD identity, base ref, commit, and tree still match the creation-time context seal | Orchestration Engine | Frozen executable contract carrying the unchanged context-seal ID, graph version, assumptions and risks | Contract and context-seal digests form the idempotency basis; repository drift transitions to `replan_required` instead of resealing |
| `ready -> executing` | Execution graph valid; dependencies resolvable; current symbolic HEAD or detached state, base ref, commit, and tree still match the same creation-time context seal; writer lease and worktree lock acquired | Orchestration Engine | Execution attempt, fenced lease, and the unchanged context-seal ID plus pre-execution tree | Same live attempt resumes; an expired lease is reclaimed only after worktree and ref reconciliation; phase-local resealing is forbidden |
| `executing -> verifying` | Required slices completed sequentially; integrations applied; symbolic HEAD or detached state and base ref have not moved; no conflict | Orchestration Engine | Slice checkpoints, integrated-tree record, evidence IDs | Completion CAS rejects late slice results; recovery compares the worktree and ref context with pre- and post-slice records |
| `verifying -> candidate_frozen` | Acceptance and deterministic checks pass; worktree is stable; execution base ref, commit, and symbolic HEAD identity still match; candidate lock acquired | Review Engine | Candidate record, ordered manifest, check and acceptance evidence | Candidate record and context-seal digest are the idempotency basis; drift cancels the transition and requires a new verification cycle |
| `candidate_frozen -> reviewing` | Candidate, policy hash, engine version, and final deterministic risk floor are immutable | Review Engine | Semantic assessment, effective-risk record, RDD plan, review attempt IDs | Review claims are keyed by candidate record ID and review generation; stale claims cannot complete |
| `reviewing -> receipted` | Required `0 / 1 / 4` review complete; no unresolved blocking finding; all evidence targets the frozen candidate | Review Engine | One immutable final receipt and receipt index record | Idempotency key covers workflow, candidate record, policy, engine, risk, evidence-set, findings, and lineage digests; a uniqueness constraint permits one current authoritative receipt per workflow |
| `receipted -> delivered` | Worktree still matches candidate at gate entry; HEAD and base ref still match the freeze context; current receipt and policy match; required HITL approval exists; destination expected state is known | Review Engine delivery gate | Commit or destination object built directly from candidate tree OID, delivery event, destination metadata, verified receipt ID | Operation uses a destination-specific idempotency key and compare-and-swap; recovery queries destination object and tree OID before retrying |
| `{discovering, ready, executing, verifying, candidate_frozen, reviewing, receipted} -> blocked` | Missing human decision, high-risk approval, required evidence, or exhausted retryable infrastructure attempts | Engine owning the concrete source state | Blocker record, concrete resume target, required actor/action | CAS names the actual source state; repeating the blocker ID is a no-op; resolution is a separate transition |
| `{discovering, ready, executing, verifying, candidate_frozen, reviewing, receipted} -> replan_required` | Evidence invalidates the plan, a non-retryable agent reports `failed`, checks or acceptance fail, candidate drifts, a correction is required, scope is exceeded, or policy changes materially | Engine owning the concrete source state | Invalidation, failure, or correction record and affected graph IDs | CAS names the actual source state; invalidation-set digest prevents duplicate replans |
| `executing -> integration_conflict` | Expected base tree, symbolic HEAD identity, base ref, or base commit differs; a slice cannot apply; or the worktree contains unexplained mutation | Orchestration Engine | Conflict record, expected and observed ref, commit, and tree context plus affected paths | Conflict detection is repeatable against the same context seal; no writer resumes until an explicit resolution transition |
| `integration_conflict -> ready` | Conflict is resolved, resulting tree is understood, and execution graph remains valid | Orchestration Engine or human operator | Resolution evidence and revised execution graph version | Resolution digest is idempotent; changed resolution goes to `replan_required` instead |
| `blocked -> {discovering, ready, executing, verifying, candidate_frozen, reviewing, receipted}` | Named blocker resolved; target equals the stored concrete resume target; graph, basis tree, and policy still match; every lease and lock required by the target has been reacquired | Owning engine after required human action | Approval or resolution evidence and newly fenced leases and locks | CAS verifies blocker ID, exact target, basis, and lock tokens; inability to reacquire remains `blocked`, while drift routes to `replan_required` |
| `replan_required -> discovering` | Invalidation set persisted; no writer lease remains | Orchestration Engine | New Wayfinder graph version and preserved unaffected lineage | Replan generation is keyed by invalidation-set digest |
| `{created, discovering, ready, executing, verifying, candidate_frozen, reviewing, receipted, blocked, replan_required, integration_conflict} -> aborted` | Kill switch or explicit operator abort; active claims revoked | Human operator or trusted local command | Abort event, revoked attempt IDs, cleanup plan | CAS names the actual source state; repeated abort is a no-op; mutation broker rejects revoked tokens |
| `{created, discovering, ready, executing, verifying, candidate_frozen, reviewing, receipted, blocked, replan_required, integration_conflict} -> failed` | State cannot be reconciled safely or a non-recoverable engine invariant fails | Engine owning the concrete source state | Failure evidence and diagnostic export reference | CAS names the actual source state; failure event is idempotent; recovery requires a new explicit workflow or supported repair command |

Retryable node or check failures do not change workflow state immediately. The
engine closes the attempt, persists its evidence, and may issue a new attempt
against the same basis within policy. A blocked dependency transitions to
`blocked`; behavioral, acceptance, or deterministic-check failure transitions to
`replan_required`; unexplained worktree state transitions to
`integration_conflict`.

A correction has one legal V1 cycle:

```text
reviewing -> replan_required -> discovering -> ready -> executing
          -> verifying -> candidate_frozen -> reviewing
```

The correction record becomes input to the new discovery and execution graph
versions. No transition resumes mutation against the old frozen candidate.

### 4. Formalize the Go-to-agent contract

Go creates every `workflow_id`, `node_id`, and `attempt_id`, persists the input
artifact IDs, records a lease, and supplies the Git tree OID the agent is allowed
to inspect or mutate. Agents return references, not workflow decisions.

Every result envelope includes:

```json
{
  "workflow_id": "wf_...",
  "node_id": "node_...",
  "attempt_id": "attempt_...",
  "base_candidate_hash": "<git-tree-oid>",
  "status": "completed",
  "artifact_ids": [],
  "evidence_ids": []
}
```

Valid `status` values are `completed`, `blocked`, `retryable_failure`, `failed`,
and `cancelled`. Each status has a Go-owned outcome mapping; an agent cannot name
the next workflow state. `retryable_failure` may create a new attempt only while
the basis tree and retry policy remain unchanged. `cancelled` cannot be reused as
completion evidence.

The outcome mapping is deterministic: `completed` may complete the node after
evidence validation; `blocked` creates a blocker; `retryable_failure` closes only
the attempt and either retries within budget or creates an infrastructure blocker;
`failed` always persists the failure evidence and transitions the workflow to
`replan_required`; and `cancelled` closes the attempt without node evidence.
Workflow-level `failed` remains reserved for unrecoverable engine or state
invariants, not an ordinary agent failure.

`base_candidate_hash` retains the contract field name but contains the repository's
native Git tree OID, including its object format (`sha1` or `sha256`) in the claim
metadata. Before final freeze it identifies the execution basis tree; during
review it identifies the frozen candidate tree.

The engine accepts a result only when all identifiers exist, the attempt owns a
live lease, the node is in the expected state, and the current basis tree equals
`base_candidate_hash`. An expired or superseded `attempt_id`, an unknown artifact,
or a different candidate is rejected atomically as a stale result. Rejection is
audited but cannot change workflow state.

### 5. Define the candidate exactly

V1 managed delivery is Git-backed. Candidate identity uses Git objects rather
than a custom file hash.

Candidate scope is the entire repository worktree except Git-internal storage,
Skynex canonical state under the Git common directory, and untracked
Skynex-generated exports registered under `.skynex/exports/` or
`.skynex/receipts/`. A tracked file under those directories is included because
tracking it is an explicit user action. The fixed generated-export exclusions are
part of the candidate policy hash.

The Review Engine starts a temporary index from the base tree, overlays the
filesystem version or deletion of every tracked path, and adds every included
untracked path. The user's ordinary index is never an input and is never mutated.
An unmerged index or a staged-only change that differs from both base and worktree
is ambiguous and blocks freeze until the user resolves or explicitly discards
that index state.

The candidate record contains:

- Symbolic HEAD ref name or explicit detached state, base ref name, base commit OID,
  and base tree OID as the context seal.
- Candidate Git tree OID, which is the authoritative candidate identity.
- Repository object format.
- Every tracked worktree path and tracked deletion in repository scope.
- Every included untracked path. By default this means all non-ignored untracked
  files in the managed worktree; ignored files are included only when explicitly
  selected as deliverable inputs.
- An ordered manifest of path, Git mode, object OID, and object kind.
- Executable modes, symlink mode `120000`, and symlink target blobs without
  following the symlink.
- Gitlink mode `160000` and referenced submodule commit OIDs.
- The candidate inclusion/exclusion policy hash and version.
- The engine version and freeze timestamp.
- The worktree ID and the checks and acceptance evidence used before freeze.

The Git tree OID already commits to path names, file contents, modes, and symlink
targets. The ordered manifest is an inspectable projection, not a second identity
scheme. Policy and engine metadata are bound by the receipt but do not replace
the Git tree OID as candidate identity.

While reviewing, Skynex holds candidate and writer-lock leases and reviews an
immutable materialization of the frozen tree. It cannot prevent a human or an
external process from editing the original worktree, so it detects drift before
every review result and at managed delivery gate entry. A content, path-set, mode,
symlink, or gitlink change in candidate scope, movement of the recorded base ref,
or a change from the recorded symbolic HEAD ref to another ref or detached state,
invalidates the candidate authority
immediately, rejects late review results, and transitions to `replan_required`.
The frozen Git object may remain in the object database for audit, but its tree OID
alone cannot restore authority after the context seal is broken. Ignored runtime
files outside candidate scope do not affect candidate identity.

### 6. Correct the receipt model

There is one authoritative receipt for the final candidate. Slices emit
checkpoints and evidence records only. Every receipt is immutable. A separate
mutable `receipt_authority` index names the one current receipt ID for a workflow;
historical receipts for superseded candidates remain audit records but are absent
from that index and cannot pass a delivery gate.

The final receipt contains or references:

- Workflow ID, final candidate Git tree OID, base commit OID, and base tree OID.
- Ordered candidate manifest ID and worktree identity.
- Policy version and hash, engine version, route, and effective risk.
- Acceptance, deterministic-check, and selected review evidence IDs and digests.
- Findings, dispositions, pre-existing finding classification, and correction
  lineage.
- Discovery-to-execution lineage and slice checkpoint IDs.
- Receipt creation time, receipt schema version, and issuance statement.

Any candidate drift removes delivery authority even though the immutable receipt
record remains available for audit. Candidate, policy, engine-compatibility, or
evidence-basis invalidation clears the matching `receipt_authority` index entry in
the same CAS transaction that records invalidation and the state transition. A
correction, whether manual in V1 or bounded and automatic in a later release,
invalidates the old candidate. The corrected work must rerun checks, acceptance,
freeze, risk selection, and review before a new final receipt can become
authoritative.

At the entry to `/commit`, `/pr`, or another managed delivery operation, the gate
rebuilds the temporary-index tree and verifies candidate OID, policy hash, engine
version compatibility, receipt authority, any required human approval, and that
HEAD has the same symbolic-ref identity or detached state while the recorded base
ref still resolves to the freeze-time base commit. The operation then consumes the
frozen candidate tree directly rather than rereading the mutable worktree:

- Managed commit uses Git plumbing to create a commit whose tree is exactly the
  candidate OID, then updates the intended ref with compare-and-swap against the
  recorded base ref.
- Managed `/pr` pushes only a previously verified commit with that candidate tree
  and creates or updates the PR against that exact remote commit.
- Crash recovery queries the local ref, remote ref, or PR head and accepts success
  only when its commit and tree OIDs match the recorded delivery intent.

External worktree edits after gate entry cannot alter delivered bytes. If they
occur before gate entry, candidate drift invalidates authority. If they occur
during delivery, the exact frozen tree may be delivered, but the now-divergent
worktree is recorded and must be replanned before any later managed operation.

### 7. Make SQLite worktree-safe

For a Git repository, canonical state lives at:

```sh
$(git rev-parse --git-common-dir)/skynex/workflows.db
```

The engine resolves a relative `--git-common-dir` result against the repository
before opening the database. The shared `skynex` directory is created with mode
`0700`; the database and SQLite sidecar files use mode `0600`.

Every existing path component is opened relative to an already validated parent
without following symlinks. The engine requires current-user ownership, rejects
non-directory parents, rejects non-regular database, WAL, and SHM files, and
rejects group- or world-accessible modes rather than silently weakening them.
New files use exclusive creation before SQLite opens them. A substituted,
foreign-owned, or hard-linked authority path fails closed with a diagnostic.

All worktrees of the repository share this database. Rows carry repository,
workflow, and worktree identities so candidate locks and writer leases coordinate
across processes. The V1 location is fixed and cannot be overridden by project
configuration.

The worktree-local `.skynex/` directory may hold project configuration,
human-readable exports, and exported receipts. It is never canonical workflow
state. Managed V1 workflows require a Git repository. A non-Git request falls back
to ordinary advisory agent operation without workflow resumption, receipt, or
managed-delivery guarantees.

### 8. Define crash recovery and concurrency

Every execution attempt and worktree or candidate lock has a lease owner, random
fencing token, acquisition time, expiry, and heartbeat. Heartbeats extend only the
matching live token. An expired lease may be reclaimed after the engine reconciles
canonical state with the observed worktree tree and acquires the worktree's local
OS-level exclusive lock.

Agents never write the managed worktree directly. They operate against immutable
inputs and return patch or mutation artifacts. A Go mutation broker is the sole
managed-worktree writer and checks the current fencing token, expected tree OID,
allowed paths, and operation ID immediately before every filesystem mutation. An
adapter that requires a write-capable process must run it in a disposable
attempt-specific worktree; only the broker can integrate its output. This fences
an expired process from the authoritative worktree even if it continues running.

The mutation broker holds an OS-level exclusive lock under the validated Git
common directory for the entire check-and-mutate critical section. Lease takeover
requires acquiring that same lock before CAS reclamation, so expiry alone can
never create a concurrent writer between token validation and filesystem write.
A process crash releases the OS lock; a hung process must be terminated or the
workflow remains `blocked` rather than admitting a second writer.

Concurrency follows these rules:

- State transitions use CAS on state and monotonic `state_version`.
- One mutation-broker lease may mutate a worktree at a time.
- A candidate lock prevents Skynex mutations from freeze through receipt or
  invalidation.
- Read-only workers may run in parallel against the same immutable tree.
- Attempt completion checks the live lease, attempt ID, node state, and basis tree.
- Late, duplicated, or superseded results are retained as audit events and
  rejected as state changes.
- V1 serializes mutating slices. V1.1 may add isolated worktree writers, but not
  shared-worktree mutation.

Before each external mutation, the engine records the expected pre-operation tree
and intended operation. Afterward it records the observed post-operation tree and
artifacts. On recovery:

- If the worktree matches the recorded post-operation tree, completion is
  committed idempotently.
- If it matches the pre-operation tree, the operation may be retried with the same
  idempotency key.
- If it matches neither, the workflow enters `integration_conflict` with expected
  and observed tree evidence.

Lock reclamation uses CAS on the expired fencing token and never deletes a live
lock by path alone. Resume never trusts chat history or an agent's claim of
completion; it rebuilds state from SQLite, Git objects, transition events, and
live locks.

### 9. Make semantic risk elevation auditable

The ordered risk levels are `low < medium < high`, mapped monotonically to review
depth `0 < 1 < 4`. Go rules calculate `minimum_risk` and minimum depth from route,
paths, change kinds, and policy. A semantic model can only produce
`effective_risk >= minimum_risk`; the selected review depth is the greater of the
deterministic and semantic depths. Prompts and route overrides cannot reduce
either floor. Policy may raise a mapping but cannot map a higher risk to a lower
depth.

Every semantic assessment persists:

- Evidence IDs and candidate OID used for assessment.
- Structured justification and the requested elevation.
- Model provider, model identifier, and model version when available.
- Prompt template ID, rendered redacted prompt, and prompt hash.
- Policy version and hash.
- Minimum risk, effective risk, and selected `0 / 1 / 4` depth.
- The selected lens and rationale when depth `1` is used.

A changed candidate or policy makes the assessment stale. Semantic assessment is
required for candidate review in V1. If it is unavailable after bounded retries,
the workflow enters `blocked`; it never substitutes a lower deterministic-only
depth or silently lowers a previously established effective risk.

### 10. Make the repair budget proportional

V1 blocks or replans rather than repairing automatically. The future V1.1 repair
contract uses both an absolute and a candidate-relative ceiling:

```text
allowed_files = min(5, candidate_changed_files)
allowed_lines = min(120, candidate_changed_lines)
```

The candidate metrics are calculated from the base tree to the frozen candidate
using Git diff semantics and stored before correction. A correction cannot touch
paths outside its registered finding scope and cannot be larger than the change
it repairs. Binary, dependency, schema, public API, or generated changes without a
safe line metric require `replan_required` rather than budget expansion.

Budget validation occurs before mutation and again against the resulting tree.
Exceeding either limit invalidates the attempt and requires replanning.

### 11. Define HITL explicitly

- Low-risk, reversible work continues automatically.
- High-risk or irreversible decisions require explicit human approval bound to
  the exact action basis.
- Every Prototype requires human validation before its conclusions can unblock
  discovery.
- A kill switch can abort any non-terminal workflow and revoke active attempts.
- Manual route overrides record actor, reason, old route, new route, timestamp,
  and applicable risk floor.
- Routine slice-by-slice approval is prohibited unless a slice independently
  crosses a high-risk or irreversible boundary.

Human approval never substitutes for deterministic checks, candidate freeze,
review, or receipt verification.

Every approval is an immutable artifact containing actor identity and
authentication source, workflow ID, decision or node ID, action digest, graph or
candidate basis, policy hash, rationale, issue time, and optional expiry.
Discovery and execution approvals bind to graph version and basis tree; delivery
approval binds to candidate record and policy hash. Repeating an approval request
with the same action digest returns the existing decision. Revocation creates a
separate immutable event and atomically clears the mutable current-approval index.
Revoked or expired approval transitions to `blocked` and cannot satisfy resume.

### 12. Add invariants

The Go engine enforces these invariants at transition boundaries and verifies them
again during crash recovery:

1. No prompt, agent, or manual route override can reduce the deterministic minimum
   risk.
2. No receipt retains delivery authority after candidate drift.
3. No Prototype enters a candidate directly or through automatic promotion.
4. Only one writer can mutate a worktree at a time.
5. Every transition is safe to repeat with the same idempotency key.
6. No managed delivery occurs without verifying the current authoritative receipt.
7. No result from an expired attempt or different basis candidate can change state.
8. No correction can mutate a frozen candidate in place; correction always creates
   a new candidate lifecycle.
9. No authoritative receipt is issued before acceptance, checks, and required
   review target the same frozen candidate.
10. No phase can replace the creation-time repository context seal; repository
    drift requires `replan_required` or `integration_conflict`.

An invariant violation records failure evidence, revokes active leases, and enters
`failed` unless the state machine defines a safer `replan_required` or
`integration_conflict` transition.

### 13. Secure and retain artifacts

Canonical storage may contain source diffs, logs, model prompts, and accidental
secrets. V1 applies these controls:

- The shared database directory uses `0700`; database, WAL, SHM, backup, and
  temporary export files use `0600`.
- Configurable input and export paths are opened without following symlinks. Every
  path component is validated, and export uses atomic create-and-rename in the
  validated destination.
- Secret scanning and redaction run before persistence. Tokens, credentials,
  private keys, and configured sensitive patterns are replaced with typed
  redaction markers. The raw secret is not stored merely to prove redaction.
- The default maximum is 16 MiB per artifact and 256 MiB of non-receipt artifacts
  per workflow. Logs are chunked. Hitting a limit blocks the affected evidence
  step; authoritative evidence is never silently truncated.
- Active workflows retain all required artifacts. After a terminal state, raw
  logs and traces expire after 30 days and non-authoritative checkpoints and diffs
  expire after 90 days.
- Final receipts, candidate manifests, findings, lineage, delivery events, and
  evidence referenced by an authoritative receipt are retained until explicit
  secure prune. Pruning receipt evidence first revokes delivery authority and is
  forbidden for an undelivered candidate. A tombstone with IDs and digests is not
  sufficient for a future delivery gate.
- SQLite backups follow the same permission, retention, redaction, and prune rules
  as their source database and are inventoried before deletion.
- Default export includes only the human summary and final receipt. Detailed or
  sensitive artifacts require explicit selection, repeat redaction, and are
  written with restrictive permissions.

Size and retention defaults may become policy settings after V1, but policy may
not permit silent evidence loss or weaken receipt verification.

### 14. Execute a breaking migration

The migration is intentionally destructive to legacy orchestration surfaces and
must be implemented as an installer-owned inventory:

1. Remove installed and embedded `manager` and `linear-orchestrator` agents.
2. Remove the `/linear` command and its independent workflow behavior.
3. Rebind `/commit` and `/pr` to final receipt verification for the current
   candidate before their managed Git operations.
4. Rebind `/rollback` in V1 to record rollback intent and lineage, explain that
   automation is deferred, and never imply that an old receipt authorizes revert
   bytes.
5. Teach fresh install, update, and cleanup paths to remove legacy managed files
   even when the new package no longer ships them.
6. Replace legacy manager and Linear flow evals with state recovery, proportional
   risk, candidate drift, stale result, and receipt-gate evals.
7. Update user documentation, agent documentation, command help, schemas, examples,
   and embedded configuration in the same breaking release.
8. Add migration tests for both clean installs and upgrades containing every
   removed legacy asset.
9. Do not import legacy workflow authority automatically. An updater with an
   active legacy workflow stops and requires completion, abort, or human-readable
   export. Inactive legacy databases are preserved as mode `0600` archives until
   explicit prune; obsolete path overrides are reported and ignored by V1.

There are no compatibility aliases. The installer must preserve user-owned files
that merely share a name and remove only assets proven to be Skynex-managed.
Ownership proof is an installation-manifest path plus matching content digest. For
pre-manifest versions, cleanup may use only exact digests of known embedded
assets; a modified or unknown file is preserved and reported.

## Canonical V1 flow

```text
request
  -> Orchestration Engine: classify(simple | planned | discovery)
  -> persist route + deterministic risk floor + rationale
  -> Wayfinder graph when blocking uncertainty exists
  -> human validation for every Prototype
  -> immutable executable contract
  -> separate execution graph
  -> sequential production slices under one writer lease
  -> slice checkpoints + evidence
  -> acceptance verification + deterministic checks
  -> Review Engine: freeze exact Git candidate tree
  -> persist semantic risk elevation and review plan
  -> review depth 0 | 1 | 4 against the frozen tree
  -> severe finding: blocked | replan_required
  -> correction: replan -> execute -> verify -> freeze -> review again
  -> one authoritative final receipt
  -> managed delivery gate rebuilds tree and verifies receipt
  -> delivered
```

## V1 scope overrides

The following changes explicitly supersede the corresponding source decisions:

| Decision | V1 | Deferred |
| --- | --- | --- |
| D16 | Sequential mutating execution; parallel read-only work | Parallel isolated worktrees and reconciliation in V1.1 |
| D22 | Fixed database under the resolved Git common directory | Configurable canonical database path |
| D34-D36 | Block or replan on severe candidate finding; full cycle after manual correction | One bounded automatic repair in V1.1 |
| D37 | Slice checkpoints and evidence; one final receipt | No authorizing slice receipts planned |
| D39 | Enforcement only inside Skynex-managed operations | Enforcement of external Git operations |
| D42 | Persist rollback intent and lineage | Automated rollback workflow |

## Open assumptions for the architecture spike

- A1: The selected Go SQLite driver and WAL configuration work reliably across
  supported platforms without an unacceptable binary distribution cost.
- A2: The specified base-tree-plus-worktree temporary-index procedure can be
  implemented without mutating the user's index and can detect concurrent user
  edits reliably.
- A3: Git SHA-1 and SHA-256 repositories can use the same typed OID contract without
  ambiguous serialization.
- A4: The fixed artifact size and retention defaults are operationally sufficient;
  the spike must measure representative diffs, logs, and review evidence.
- A5: Current worker agents can be consolidated behind the result envelope without
  preserving their conflicting return contracts.

## Out of scope for V1

- Parallel mutating slices or reconciliation of writer worktrees.
- Automatic candidate repair.
- Authoritative receipts per slice.
- Receipt enforcement outside operations managed by Skynex.
- Managed workflows and authoritative receipts for non-Git projects.
- Automated post-delivery rollback.
- Configurable canonical database location.
- Linear synchronization or a replacement `/linear` workflow.
- GitHub issue-map synchronization.
- Compatibility aliases for removed orchestrators or commands.
- Hosted or multi-user workflow authority.
- Mandatory full 4R review for every change.
- Treating a receipt as proof that software is universally correct.

## Exit criteria for PRD readiness

The document becomes ready for an implementation PRD only after an architecture
spike demonstrates:

1. Lease expiry, CAS transitions, and idempotent recovery across forced crashes.
2. Temporary-index candidate freeze and drift detection for tracked files,
   untracked files, executable modes, and symlinks.
3. Rejection of stale agent results by attempt and basis candidate.
4. One final receipt that authorizes the frozen tree and fails after any candidate
   drift in a managed delivery path; managed commit and PR operations consume that
   exact frozen tree without a worktree TOCTOU window and reject a broken base-ref
   context seal.
5. Deterministic risk floors and a fully persisted semantic elevation record.
6. Shared SQLite coordination across at least two worktrees of one repository.
7. Breaking installer cleanup that removes only proven Skynex-managed legacy
   assets.
