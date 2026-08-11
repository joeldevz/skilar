# Public `skynex-orchestrator` validation suite

This directory contains the 19 public, deterministic validation cases required by
`eval/specs/skynex-orchestrator-contract.md`. These cases are deliberately visible
to prompt authors and **are not secret holdouts**. Release decisions must also use
the separately controlled, content-addressed holdout suite described by the
normative contract.

Every YAML file uses case schema v1, pins its source fixture with the digest returned
by `sandbox.DigestTree`, links every hard check to both normative requirement IDs and
evidence IDs, uses argv-only commands, and explicitly declares tool and executable
authority. All cases are marked `x-visibility: public`.

## Coverage

| Case | Risk / scenario | Normative coverage |
|---|---|---|
| `skx_low_direct` | Direct localized LOW edit, zero children | `SKX-RISK-LOW-001`, `SKX-SCOPE-001`, `SKX-CLAIM-001` |
| `skx_low_tdd` | Direct LOW red/green owner | `SKX-RISK-LOW-002`, `SKX-CLAIM-001` |
| `skx_medium_slice` | One bounded implementation child | `SKX-RISK-MED-001`, `SKX-LINEAGE-001` |
| `skx_high_security` | Targeted security review and attack oracle | `SKX-RISK-HIGH-001`, `SKX-INJECTION-001` |
| `skx_dirty_worktree` | Tracked, staged, untracked, and ignored state | `SKX-SCOPE-001`, `SKX-GIT-001`, `SKX-ADOPTION-001` |
| `skx_stale_result` | Reject mismatched worker envelope | `SKX-LINEAGE-001`, `SKX-STALE-001` |
| `skx_worker_failure` | Preserve failure evidence and recovery | `SKX-FAILURE-001`, `SKX-CLAIM-001`, `SKX-RETURN-001` |
| `skx_retry_lineage` | Stable node and bounded new attempt | `SKX-RETRY-001`, `SKX-LINEAGE-001` |
| `skx_neurox_irrelevant` | Available memory tool remains unused | `SKX-MEMORY-001` |
| `skx_neurox_relevant` | One targeted recall; repository wins | `SKX-MEMORY-001` |
| `skx_no_workflow` | Standalone boundary | `SKX-BOUNDARY-001` |
| `skx_candidate_drift` | Evidence invalidated after freeze drift | `SKX-FREEZE-001`, `SKX-CLAIM-001` |
| `skx_human_gate` | Reversible progress before authority gate | `SKX-HUMAN-001`, `SKX-GIT-001` |
| `skx_review_retry` | One review retry, then honest block | `SKX-RETRY-001`, `SKX-FAILURE-001` |
| `skx_duplicate_validation` | Do not repeat an identical accepted check | `SKX-VALIDATE-001` |
| `skx_late_child` | No completion before quiescence | `SKX-QUIESCE-001`, `SKX-CLAIM-001` |
| `skx_neurox_failure` | Failed/injected memory is non-authoritative | `SKX-MEMORY-002`, `SKX-INJECTION-001` |
| `skx_compaction` | Scope and lineage survive compaction | `SKX-CONTEXT-001`, `SKX-LINEAGE-001`, `SKX-SCOPE-001` |
| `skx_prompt_injection` | Untrusted fixture/tool instructions | `SKX-INJECTION-001`, `SKX-BOUNDARY-001`, `SKX-HUMAN-001` |

Collectively, the table covers all 22 normative requirements. The executable test
in `digest_test.go` fails if a case, requirement, hard-check link, evidence link,
fixture digest, local fake, or dirty-worktree category is missing.

## Deterministic fixtures

The implementation fixtures `low-button`, `low-slugify`, `medium-profile`,
`high-auth`, and `dirty-worktree` intentionally begin with failing acceptance tests.
Their case setup records that red state with `expected_exit: [1]`; their oracle uses
the same argv-only `go test ./...` command and requires exit zero after the task.

Coordination cases do not call real workers or Neurox. They expose frozen local MCP
stdio scenarios from `coordination/fake-mcp`, selected by `SKX_FAKE_SCENARIO`. The
returned worker, memory, retry, and injection text is always untrusted case data.

## Validation

No model or network access is needed:

```bash
GOCACHE=/tmp/skx-suite-go-cache \
  go test -v ./eval/cases/skynex-orchestrator
```

When the CLI validator is available, the equivalent repository-level command is:

```bash
skynex-eval validate --suite skynex-orchestrator
```
