---
name: tdd-discipline
description: Use when the user opts into TDD or the task requires writing or modifying tests.
---

# TDD Discipline — Iron Law + Cycle Evidence

> **SUBAGENT-STOP gate**: if you are running as a subagent invoked by another subagent, STOP. Return `status: blocked` with reason `nested-subagent-loop-detected`. TDD discipline must be applied at the implementer level, not recursively.

> **Principios destilados** (obra/Superpowers Iron Law + Gentleman/gentle-ai Cycle Evidence + nuestro return envelope): la disciplina TDD requiere reglas explícitas, anti-rationalization activa y evidencia estructurada en el output. No basta con afirmar "tests pasan".

## The Iron Law (7 rules)

1. **NEVER modify a test to make it pass** — fix the implementation instead
2. **WRITE THE TEST FIRST** (red phase) when the task requires a new test
3. **Confirm the test fails for the EXPECTED REASON** before implementing
4. Implement minimal code to pass (green phase)
5. Refactor only after green
6. If a pre-existing test fails after your change, the implementation is wrong — do NOT touch the test
7. If no failing test exists and task requires one → `status: blocked`

## Anti-rationalization table

Reject these excuses immediately:

| Excuse                                          | Reality                                           |
|-------------------------------------------------|---------------------------------------------------|
| "The test was wrong"                            | Fix the spec, then the test, then the impl       |
| "It's just a small adjustment to the assert"    | That IS modifying the test. Stop.                |
| "The implementation is correct, test is flaky"  | Prove it: run 10x. If flaky, fix setup not assert |
| "Adding `.skip()` temporarily"                  | Never skip. Block and report.                    |
| "Updating snapshot to match new output"         | Only if spec changed. Otherwise impl is wrong.   |

## Tests derive from the spec (SDD link)

When a PLAN.md exists, the test cases are NOT invented — they come directly from the plan's **Dado / Cuando / Entonces** requirements:

- Each `Dado/Cuando/Entonces` requirement → at least one test
- The test name should trace to the requirement it covers
- A test that does not map to any requirement is suspect — flag it, do not silently keep it
- This gives double validation: the implementation passes the tests AND the tests trace back to the agreed spec

This traceability is what makes a wrong test catchable: if a test contradicts the plan, the plan is the source of truth.

## TDD Cycle Evidence (mandatory in return envelope)

When `slice.tdd=true`, the return envelope MUST include:

```yaml
tdd_evidence:
  red_proof: <test name + failure reason captured BEFORE impl>
  green_proof: <test runner output snippet showing pass>
  assertion_quality: high | medium | low
  mocks_used: <count>
  spec_trace: <which Dado/Cuando/Entonces requirement(s) each test covers>
```

### `assertion_quality` rubric

- **high**: specific values asserted, error messages matched, real behavior verified
- **medium**: types checked, partial structure asserted, happy path only
- **low**: `toBeTruthy`, `toBeDefined`, empty collections without context, ghost loops → consider `status: blocked` for redesign

### Mock Hygiene (max 6)

If `mocks_used > 6` → design smell. Either:
- Refactor to extract pure functions (preferred)
- Return `status: blocked` with reason `mock-overload-design-smell`
- Document why exception applies in `risks` field

## Banned Assertion Patterns

These are detectable smell signs. Reject in code review:

| Pattern | Why banned |
|---|---|
| `expect(true).toBe(true)` | Tautology — always passes |
| `expect(arr).toBeDefined()` after creating it | No-op assertion |
| `for (let i=0; i<arr.length; i++) { expect(...) }` empty | Ghost loop — passes for empty arrays |
| `expect(() => ...).not.toThrow()` only | Smoke test without behavior verification |
| `expect(spy).toHaveBeenCalled()` without args check | Doesn't verify correct call |

## Exceptions

- **Trivial bugfixes** (typo, null check, rename) — TDD optional
- **Non-code tasks** (docs, configs, README) — TDD not applicable
- **Spec changes** — require explicit user approval BEFORE touching tests

## Integration with the verifier

This skill runs the cycle (red → green → refactor). The `verifier` agent runs the evidence checks (build, test runner output, types, lint) after each step. Both must be satisfied before `status: completed`.

## Cultural rules

- Address the user as "your human partner"
- Banned phrases: sycophantic preambles, "should work", "looks good"

## Referencias

- obra/Superpowers — Iron Law + anti-rationalization
- Gentleman/gentle-ai — TDD Cycle Evidence + Banned Assertion Patterns + Mock Hygiene cap
- Kent Beck — *Test-Driven Development: By Example*
