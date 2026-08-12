# Freezing an offline A/B capsule

`skynex-eval freeze` turns three required input trees, plus an optional holdout,
into a content-addressed development capsule. Its data can be copied as a unit, but
execution is intentionally environment-bound: `toolchains_digest` includes canonical
PATH roots and executable targets, and A/B requires the same evaluator, OpenCode
binary and executable closure. It performs no OpenCode probe and no model request.
The command reports `model_calls: 0`; model execution remains a separate, explicitly
authorized `ab --allow-model-calls` step.

## Required input layout

The materialized harness tree owns the public cases and their data inputs and must
contain:

```text
HARNESS/
  cases/
  fixtures/
```

Case contracts and fixture digests are validated before anything is published.
Evaluator-owned oracle or fake data and executable declarations are committed by
those contracts and fixtures. The evaluator implementation, embedded schemas,
judge/gate code and gate policy are not falsely claimed as files in this tree: the
implementation is pinned by the evaluator binary digest and the policy is frozen in
the sibling manifest. The optional external holdout uses the same `cases/` +
`fixtures/` layout, must contain native v1 cases, and must not reuse a public case ID.
Control and candidate are the complete effective OpenCode asset trees for their
respective arms.

Every input must be a real directory. Symlinks, hard links, devices, sockets and
other special entries are rejected rather than followed. Input trees must be
disjoint, and the output must not be within any input. A candidate with the same
canonical tree digest as its control is rejected as an unrealized treatment.

## Command

First capture `/doc` with the read-only `doctor` command, or otherwise obtain its
canonical digest from equivalent trusted evidence. Then freeze without starting
OpenCode:

```bash
skynex-eval freeze \
  --output-dir /absolute/path/to/capsule-v1 \
  --harness /absolute/path/to/harness \
  --control /absolute/path/to/control \
  --candidate /absolute/path/to/candidate \
  --id orchestrator-context-v1 \
  --suite skynex-orchestrator \
  --runs 5 \
  --seed 42 \
  --binary /absolute/path/to/opencode \
  --opencode-version 1.18.16 \
  --opencode-openapi-digest sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

Instead of `--opencode-openapi-digest`, `--doctor-result PATH` accepts a saved,
successful `skynex-eval doctor` JSON envelope. The envelope must attest
`model_calls=0`; its evaluator digest, OpenCode binary digest and exact version must
match the current freeze invocation. `freeze` never invokes `doctor` implicitly.

For a predeclared model treatment, pass both `--control-model` and
`--candidate-model`. The command derives the exact `model` and, when applicable,
`provider` difference from those explicit selections. Bundle treatments use the
comma-separated `--intentional-differences` flag; declaring either `prompt_digest`
or `agent_bundle_digest` commits both because the current producer binds both to the
verified arm tree. Its default is `agent_bundle_digest` (normalized to that pair).
`freeze` rejects toolset or permission-policy differences because it has no distinct
per-arm input that could establish them before paid work. A model-only treatment may
use identical-content, distinct-root arm inputs; any declared bundle treatment must
have different control and candidate tree digests.

## Capsule contract

Publication is all-or-nothing into a new directory:

```text
capsule-v1/
  manifest.json
  bundles/
    harness/
    control/
    candidate/
    holdout/       # only when supplied
```

`manifest.json` is deliberately outside every frozen tree. Its relative roots are
`bundles/harness`, `bundles/control`, `bundles/candidate` and, when present,
`bundles/holdout`. Therefore editing or signing the manifest cannot change the
harness digest, and the manifest can commit the harness digest without a circular
self-reference.

The generated manifest is strict `development` evidence with the local
`trusted-local` / `host-unisolated` / `runtime-readable` boundary. It commits:

- canonical tree digests for all materialized bundles;
- exact public case count, model-neutral public case-set digest and sorted critical
  case IDs;
- optional holdout tree digest and exact case count without printing holdout IDs or
  source paths;
- balanced-blocked AB/BA seed, paired run count and strict development gates;
- the running evaluator digest and selected OpenCode binary digest;
- a canonical executable-closure digest over case setup/oracle/fake commands, the Go
  runtime and Git, for the complete public-plus-holdout population;
- the exact OpenCode version and supplied or cached `/doc` OpenAPI digest.

Git HEAD and dirty-patch provenance are observed before and after materialization.
For harness, control and candidate they are persisted as `source_git_sha` and
`source_dirty_patch_digest` in the manifest and returned in each bundle's
`source_git_provenance` receipt. The copies intentionally exclude `.git`, so their
root-owned `git_sha` fields remain empty and source metadata is not mislabeled as
provenance verifiable inside a copied root. Holdout source Git identifiers and patch
digests are never published; a dirty Git holdout is rejected with a redacted error.
Git is resolved once to an absolute, content-pinned executable and revalidated around
each read-only inspection; the same target must appear in the committed toolchain
closure.

At comparison time, `treatment_realized` requires every predeclared intentional
difference to appear as a real fingerprint mismatch and at least one effective
config, agent, model/provider, toolset or permission-policy change. Thus a differing
but runtime-irrelevant file cannot make a placebo treatment pass.

The command writes and verifies the manifest and every copied tree in a private
staging sibling, then renames that directory to the requested output. It refuses to
overwrite an existing path. Running it twice against unchanged inputs and pins
produces byte-identical manifests.

To consume the result, point `ab` at the sibling manifest and at the harness paths
inside the capsule:

```bash
skynex-eval ab --allow-model-calls \
  --manifest /absolute/path/to/capsule-v1/manifest.json \
  --cases-dir /absolute/path/to/capsule-v1/bundles/harness/cases \
  --fixtures-dir /absolute/path/to/capsule-v1/bundles/harness/fixtures \
  --openai-oauth /absolute/path/to/dedicated-auth.json
```

`ab` keeps a canonical, integrity-digested `<output-prefix>.partial.json` journal
before the first model call and updates it atomically after each accepted sample. If
the command is interrupted, resume only through the explicit partial path:

```bash
skynex-eval ab --allow-model-calls \
  --manifest /absolute/path/to/capsule-v1/manifest.json \
  --cases-dir /absolute/path/to/capsule-v1/bundles/harness/cases \
  --fixtures-dir /absolute/path/to/capsule-v1/bundles/harness/fixtures \
  --openai-oauth /absolute/path/to/new-dedicated-auth.json \
  --resume-partial /absolute/path/to/results/ab-capsule-v1.partial.json
```

Before probe or model work, resume strictly and boundedly loads the journal and
requires the exact experiment ID, manifest digest, intent, authority, randomized
plan and case/variant/repetition subset. It rejects duplicate samples, locks the
partial, and reserves or inode-pins all final paths. Holdout references are translated back to private case
IDs only in memory; every persisted checkpoint remains ordinal and content-redacted.
Handled failures preserve the latest fsynced checkpoint, and the partial is removed
only after the control, candidate and comparison artifacts have been staged and
published successfully without clobbering another file. A hard process or power
failure in the narrow interval after a provider response but before its checkpoint
can require that one uncheckpointed coordinate to run again. A hard crash during
resume can also leave the exclusive `.resume.lock`; an operator must first prove
that no resume process is alive before removing that stale lock. The journal is an
integrity check for the trusted-local workflow, not a cryptographic attestation
against a same-UID writer. If a process dies between final publications, a subsequent
resume reuses only pre-existing finals that exactly match the canonical artifacts
regenerated from the complete partial; mismatches are preserved and rejected.

That later OAuth run consumes plan limits or credits according to the current Codex
plan. `freeze` itself has no provider usage and makes no per-request USD claim for
OAuth.
