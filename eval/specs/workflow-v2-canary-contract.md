# Workflow V2 canary contract

`workflow-v2-canary-v1` is a fast, non-release screening profile. Its only
positive conclusion is that the exact frozen candidate is eligible for the
full public Workflow V2 suite. It is not statistical evidence and never
authorizes release or use of a holdout.

## Command

```bash
skynex-eval canary \
  --allow-model-calls \
  --profile workflow-v2-canary-v1 \
  --manifest /absolute/path/to/experiment.json \
  --openai-oauth /absolute/path/to/auth.json \
  --binary /absolute/path/to/opencode \
  --workflow-plugin /usr/local/share/skynex-eval/skynex-workflow.ts \
  --output /absolute/path/to/workflow-v2-canary.json
```

`--output` and `--binary` have safe defaults. `--workflow-plugin` is accepted
only at the fixed evaluator installation path shown above; the file and every
ancestor must be root-owned and not group- or world-writable. The other flags
are explicit and mandatory. The evaluator itself must run unprivileged, making
that installation path outside its write authority. The command writes one integrity-sealed artifact with authority
`screening-non-release`.

The controlled plugin must byte-for-byte equal
`internal/assets`' embedded `plugins/skynex-workflow.ts`. Only its SHA-256 digest
is published; its host path is never included in the artifact. Ambient plugins,
including Neurox, remain disabled.

The regular experiment schema still requires at least two planned A/B runs.
The canary does not weaken that contract: it takes only repetition 1 from each
case's frozen balanced plan and independently fixes `runs_per_arm=1`. The
manifest continues to pin the population, ordering seed, runtime, toolchains,
control and candidate; its remaining repetitions belong to the later suite.

## Frozen population

The command rejects the capsule before any model call unless all of these are
true:

- suite is exactly `workflow-v2-canary`;
- intent is `development` and authority is non-release;
- exactly three public cases exist and all three are critical;
- their IDs, complete canonical case digests, fixture digests, and mandatory
  hard checks exactly match the evaluator-owned profile;
- there is no holdout bundle and `holdout_case_count` is zero;
- both arms use the same model selected by each case;
- execution is serialized with a clean OpenAI OAuth profile;
- every case uses `workflow-orchestrator`, has `runs.count: 1`, and has a
  completion timeout no greater than four minutes;
- every case declares `x-visibility: public` and
  `x-canary-profile: workflow-v2-canary-v1`;
- every case contains exactly one `x-workflow-driver-v1` object:

```yaml
x-workflow-driver-v1:
  mode: managed-detach # or foreground
  workflow_id: stable-case-id
  terminal_state: candidate_frozen
  autonomous_turns: 2 # 0..2
```

`workflow_id` must equal the case ID. Fake MCPs and Neurox tools are forbidden.
The local `skynex` executable, OpenCode executable, evaluator, cases, fixtures,
agent bundles and toolchain closure are content-pinned and checked again after
execution.

## Fixed budget

The profile has no tuning flags:

| Segment | Limit |
|---|---:|
| Preflight | 1 minute |
| Six samples (three cases × two arms × one run) | 24 minutes |
| Workflow cleanup reserve | 1 minute |
| Gate evaluation and artifact sealing reserve | 4 minutes |
| Hard wall-clock total | 30 minutes |

Each sample is capped at four minutes. Execution is serial and fail-fast. No
new sample may start after a non-pass result. A timeout or the global deadline
is a failed canary, not a skipped or successful sample. The executor must stop
detached work and report cleanup complete before promotion is possible.

## Deterministic promotion gate

The decision is one of `promote`, `reject`, or `inconclusive`.

`promote` requires all of the following:

- all six committed sample coordinates completed and returned `pass`;
- every evaluator-owned declared hard check and each mechanical
  infrastructure, filesystem, acceptance, behavior, claim-consistency and
  security category passed;
- zero control-pass to candidate-fail regressions;
- each case's control/candidate pair observed the same provider/model,
  execution mode/network, authorization-bound toolset digest, and effective tool
  catalog digest;
- complete evaluator-authoritative token telemetry with positive input tokens
  and at least one observed session in every sample;
- candidate/control `tree_sum_input_tokens` is at most `1.15`;
- Workflow cleanup completed and every frozen input retained its digest.

The Workflow executor must account for nested worker/reviewer model invocations
in the reported tree telemetry. Root-session-only telemetry is incomplete and
therefore cannot promote.

`reject` covers a behavioral failure, any timeout, or an input-token ratio over
1.15. `inconclusive` covers incomplete telemetry/population, runtime
compatibility mismatch, cancellation, an invalid result, infrastructure failure,
or incomplete cleanup. Exit codes keep
their normal machine meanings, except timeout is deliberately returned as the
ordinary failed-screen exit (`1`).

Fail-fast artifacts list planned, completed and skipped coordinates; skipped
samples are never fabricated as run results.

## Promotion handoff

A promoted artifact records the candidate bundle digest and
`digest_reuse_required: true`. The full public suite must consume that exact
digest. Any candidate edit, binary rebuild, prompt/plugin change, or fixture
change invalidates the promotion and requires a new canary.

The canary never reads the secret holdout. Holdout evidence remains reserved
for the separate frozen release experiment.
