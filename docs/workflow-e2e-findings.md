# Workflow E2E findings

End-to-end exercise: build a new `minitodo` Go CLI through a simple, detached
Skynex workflow and continue through semantic review to `receipted`.

## 1. Empty repositories cannot start a workflow

**Failure**

`skynex workflow start` failed on a repository whose initial commit had no
tracked files:

```text
git add -u -- .: exit status 128: error: pathspec '.' did not match any file(s) known to git
```

**Cause**

Candidate sealing invokes `git add -u -- .`; Git rejects that pathspec when the
index has never contained a path.

**Solution**

The E2E test continued after adding a tracked `.gitkeep`. The engine should be
changed so an empty index is valid (for example, skip `git add -u` when there
are no tracked paths, or use an index operation that is a no-op on an empty
repository).

## 2. Invalid start arguments leave a partial workflow

**Failure**

Starting with an explicit `--route simple` but without override attribution
returned:

```text
route override requires --override-actor and --override-reason
```

Despite the error, `status` showed `minitodo-e2e` persisted in `created` at
version 0. Repeating the corrected command then failed with `workflow: already
exists`.

**Cause**

Route-override validation occurs after the initial workflow row is written.

**Solution**

The test continued with a new workflow ID and complete override attribution.
The engine should validate all start arguments before opening a mutation, or
persist the complete start operation in one transaction so validation failure
leaves no workflow row.

## 3. Detached workers launched in an ephemeral Codex sandbox are reaped

**Failure**

The first detached worker received PID 21 and later became:

```text
blocked: detached run worker interrupted
```

A second host worker was also interrupted when its supervising tool call was
manually aborted.

**Cause**

Codex command invocations can run in an ephemeral PID namespace or execution
cgroup. Killing the supervising call may reap descendants even when Skynex
uses an OS-level detached process group. This is not reproduced by the normal
OpenCode notification integration.

**Solution**

Launch the detached worker in the persistent host environment, end the launch
call normally, and observe it from a separate call. Do not abort a tool call
that owns the worker's execution scope. Skynex correctly records interruption
and permits a bounded retry.

## 4. Semantic review was not told its deterministic risk floor

**Failure**

The semantic reviewer returned a lower risk than the deterministic floor and
all retries failed with:

```text
review: semantic assessment cannot lower deterministic risk
```

**Solution implemented**

The semantic prompt now carries the exact deterministic minimum, requires an
equal-or-higher risk, and rejects lower output before checkpointing it.

## 5. Human approval gates consumed technical retries

**Failure**

A valid high-risk review stopped at `approval: exact current approval required`,
but the detached job was recorded as failed and the workflow became blocked.

**Solution implemented**

Approval gates now finish as `waiting_approval`, preserve the workflow state,
do not consume technical retry budget, and expose the exact approval command as
the next action.

## 6. Verification inherits an unwritable Go build cache

**Failure**

The worker successfully produced and locally checked the candidate, but managed
verification failed at `go test ./...` with:

```text
open /home/clasing/.cache/go-build/...: read-only file system
```

**Cause**

The verification subprocess inherited the host `GOCACHE`, while its sandbox
allowed writes only to the candidate workspace and `/tmp`.

**Solution implemented**

Managed verification now provisions a tree-scoped writable `GOCACHE` under
`/tmp` instead of inheriting the host cache. `retry-verification` also accepts
the exact failed acceptance evidence ID, so a genuine environment-only failure
can be retried without forcing a replan.

## 7. Review inherits an unwritable OpenCode data directory

**Failure**

The detached semantic review failed before producing a result:

```text
FileSystem.open (/home/clasing/.local/share/opencode/log/opencode.log)
```

**Cause**

The review subprocess inherited `XDG_DATA_HOME`/the default user data path,
which is read-only inside the managed sandbox even though OpenCode writes its
runtime log there.

**Solution**

Provision writable, invocation-scoped XDG data/cache/state directories under
`/tmp` for OpenCode review subprocesses while retaining the user's read-only
configuration directory. This keeps runtime writes out of the host profile and
inside the sandbox's allowed temporary storage.

The same isolation is now applied to implementation workers. A clean rerun
revealed that `opencode run` also attempted to open the host log at
`~/.local/share/opencode/log/opencode.log`. Each worker invocation now receives
writable `XDG_DATA_HOME`, `XDG_CACHE_HOME`, and `XDG_STATE_HOME` directories and
only the minimal OpenCode identity files needed for provider authentication.

## 8. Review watchdog treats stdout silence as process inactivity

**Failure**

After OpenCode started successfully, semantic review was killed with:

```text
review: OpenCode produced no output before idle timeout
```

The managed process and workflow job were still heartbeating; only stdout was
silent while the provider request was in flight.

**Cause**

The default idle watchdog used a fixed two-minute stdout timer. It did not have
a runtime-activity signal and therefore treated a quiet but live provider call
as a stalled process.

**Solution implemented**

When no explicit idle policy is configured, stdout silence no longer expires
before the authoritative review timeout. The process heartbeat remains visible
for diagnostics. A future runtime adapter can safely restore a shorter idle
limit when it can observe provider/process activity independently of stdout.

## 9. The live provider did not return during the bounded E2E window

**Failure**

After repairing the sandbox environment, the real OpenCode provider remained
alive but returned no semantic result within the E2E observation window.

**Solution**

The workflow engine portion of the E2E uses a deterministic local reviewer that
implements the same `$SKYNEX_RESULT_FILE` protocol: it preserves the high-risk
floor and emits empty finding sets for all required lenses. Candidate creation,
verification, approval binding, receipt issuance, and all workflow transitions
remain real. This isolates engine correctness from external provider latency;
provider integration should be exercised separately with network/service SLOs.

## 10. Terminal workflow status still displays an obsolete failed job

**Failure**

After the recovered foreground review issued a receipt, workflow status correctly
reported `receipted` but also displayed the previous failed detached review job
and its stale error. That can make a completed gate look failed.

**Cause**

`status` always prints the most recently created job, even when a later
foreground operation advanced the authoritative workflow state without creating
a replacement job.

**Solution**

Terminal workflow state must take precedence over obsolete job state. Status and
notifications should either suppress superseded job errors or label them as
historical whenever their terminal state/version predates the workflow's current
authoritative transition.

## 11. Detached children can be interrupted by a PID-isolated test harness

**Failure**

The clean rerun's detached worker was interrupted while its OpenCode invocation
was still recorded as running. A later detached attempt remained queued because
the harness ended the command namespace before the orphaned child could attach.

**Cause**

This execution harness does not preserve arbitrary orphaned processes between
tool calls. That differs from a normal terminal or the OpenCode notification
integration for which detached workflow jobs were designed.

**Solution used for the E2E**

The workflow was still queued with `--detach`, producing durable job evidence.
The same persisted job was then attached to the public managed
`workflow worker ... --job ...` entry point in the foreground. No state or
receipt was edited directly. A deterministic contract-compatible worker and
reviewer removed external provider latency from this engine test.

## Final outcome

The clean E2E workflow `minitodo-e2e-v2` reached `receipted` at state version 23.
Its immutable candidate tree is `d87a9a54c30ee57f449c816990c7a626e1682029`
and its receipt is
`rcpt_cf255543a1eb01fb761a35066d807226dfc555878e14466361fcbbea0343fff4`.
No delivery, commit, push, or pull request was performed.

After implementing all discovered engine fixes, a second truly empty-repository
workflow, `minitodo-fixed-v2`, reached `receipted` at state version 11. Its
candidate tree is `cad7fd08252346ec9d3125b22eb6cdd5a3ade0bd`, its effective
risk is `high`, and the exact approval gate was recorded before receipt
issuance. Independent acceptance checks passed: `go test ./...`, `go build`,
`add`, `list`, and `done`. The final CLI showed task 1 first as `pending` and
then as `completed`. No delivery, commit, push, or pull request was performed.
