# Design: Two-Stage Review (Verifier Split)

> **Status**: Design only. Implementation deferred to Sprint 2 (M3).
> **Source**: Inspired by obra/Superpowers `code-reviewer` agent pattern.
> **Date**: 2026-04-25.

## Problem statement

Our current `verifier` agent runs a single-stage check: lint + build + tests. This catches mechanical errors but misses:

- **Spec compliance**: did the implementation actually meet the acceptance criteria from PLAN.md / SPEC.md?
- **Code quality**: are patterns/idioms followed, is the design coherent, are abstractions appropriate?

A single agent doing both jobs in one prompt risks:
- Context contamination (mechanical noise drowns design signals)
- Verdict averaging (passes mechanical but fails design → unclear status)
- Token bloat (one giant prompt vs two focused ones)

## Proposed split

### Stage 1: `spec-compliance-checker`

**Job**: verify that implementation meets the documented spec.

**Inputs**:
- `spec_path`: SPEC.md or PLAN.md slice section
- `modified_files`: from coder's return envelope
- `acceptance_criteria`: extracted from spec

**Output**:
- `spec_compliance: COMPLIANT | DEVIATION | VIOLATION`
- `findings: [<file>:<line> — <criterion> not met]`

**Tools**: read, glob, grep (no bash, no edit)

**Model**: Haiku (mechanical comparison work)

### Stage 2: `code-quality-reviewer`

**Job**: verify that the code follows good engineering practices regardless of spec.

**Inputs**:
- `modified_files`: same set
- `project_standards`: from skill registry (`.skynex/skill-registry.md`)
- `language`: detected from file extensions

**Output**:
- `quality: SOUND | NEEDS_REFACTOR | DESIGN_SMELL`
- `findings: [<file>:<line> — <pattern> issue]`
- `mocks_count`, `complexity_metric` if applicable

**Tools**: read, glob, grep (no bash, no edit)

**Model**: Sonnet (judgment-heavy work) or Opus for high-stakes

## Pipeline integration

```
coder (returns status:success)
    │
    ▼
┌────────────────────────────────┐
│ Stage 1: spec-compliance       │
│ result: COMPLIANT | DEVIATION  │
└────────────────────────────────┘
    │
    ├─ COMPLIANT ──┐
    │              ▼
    │     ┌────────────────────────────┐
    │     │ Stage 2: code-quality      │
    │     │ result: SOUND | NEEDS_R... │
    │     └────────────────────────────┘
    │              │
    │              ├─ SOUND → status:approved
    │              ├─ NEEDS_REFACTOR → return to coder
    │              └─ DESIGN_SMELL → escalate to human review
    │
    └─ DEVIATION/VIOLATION → return to coder (don't waste Stage 2 tokens)
```

**Key insight**: Stage 2 only runs if Stage 1 passes. This saves ~40% of review tokens on average (Stage 1 catches most issues).

## Synergy with existing agents

- **`security`**: stays as-is (dual-judge already adversarial, different domain)
- **`test-reviewer`**: stays as-is (specific test coherence focus)
- **`skill-validator`**: stays as-is (registry-based)
- **`verifier`** (current): split into the two new agents OR keep as Stage 1 + add Stage 2

### Recommendation: refactor `verifier`

- Rename current `verifier` → `spec-compliance-checker` (its real job is mechanical pass/fail)
- Add NEW `code-quality-reviewer` agent
- Update orchestrator to chain them in sequence
- Update return envelope to carry both verdicts

## Open questions

1. Should `code-quality-reviewer` use Opus or Sonnet?
   - Opus: higher quality, $$$ per call
   - Sonnet: balanced, may miss subtle design issues
   - Decision: probably Sonnet by default, Opus for slices marked `quality_critical:true`

2. Should we run Stage 2 inline in coder for trivial cases?
   - See `docs/lessons-learned/inline-vs-subagent-review.md`
   - Yes: when implementer is in smart zone and task is bounded

3. How to handle disagreement between Stage 1 and Stage 2?
   - Stage 1 fails → never reach Stage 2 (problem doesn't exist)
   - Both pass → proceed to commit
   - Stage 2 fails after Stage 1 passes → coder retry with Stage 2 feedback (max 2 retries, then human review)

4. Should this skill be parameterized like `adversarial-review` (with `domain`)?
   - Maybe in v2; v1 keeps the two stages separate and explicit

## Implementation checklist (for Sprint 2 M3)

- [ ] Create `opencode/skills/spec-compliance-checker/SKILL.md` (≤120 líneas)
- [ ] Create `opencode/skills/code-quality-reviewer/SKILL.md` (≤120 líneas)
- [ ] Add both as agents in `opencode/opencode.json`
- [ ] Update orchestrator prompt to chain them
- [ ] Update return envelope schema with `spec_compliance` + `quality` fields
- [ ] Migrate or retire `verifier` agent
- [ ] Update `docs/IMPROVEMENT-PLAN.md` to mark this design as implemented

## Referencias

- obra/Superpowers — `code-reviewer` agent pattern + RELEASE-NOTES on two-stage review
- `docs/lessons-learned/inline-vs-subagent-review.md` — when to skip stages
- `docs/IMPROVEMENT-PLAN.md` Sprint 2 M3 — implementation slot
