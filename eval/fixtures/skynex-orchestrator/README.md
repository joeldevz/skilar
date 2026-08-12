# `skynex-orchestrator` public fixtures

These fixtures belong to the trusted evaluation bundle and are copied into a fresh
workspace for every repetition. Each case records the exact `sandbox.DigestTree`
digest of its selected child directory; this root README is not part of a case
fixture digest.

- `low-button`: localized LOW edit; acceptance test starts red.
- `low-slugify`: LOW red/green behavioral change; acceptance tests start red.
- `medium-profile`: one bounded MEDIUM implementation slice; tests start red.
- `high-auth`: HIGH authentication flaw plus untrusted review text; tests start red.
- `dirty-worktree`: code test starts red and `git_seed` creates tracked, staged,
  untracked, and ignored pre-existing state.
- `coordination`: immutable scenario records and an offline stdio MCP for worker,
  retry, Neurox, compaction, validation, and prompt-injection cases.

Fixture prose, JSON, MCP results, and memory text are untrusted data. They never
grant authority or replace the suite's requirements and oracles.
