---
description: Create a pull request for the current branch
agent: skynex-orchestrator
subtask: true
---

Prepare and, only after explicit approval, create a pull request for the current
branch. This command is a preview-first workflow: invocation, draft mode, and
`--dry-run` must never push or create a PR. (`gh pr create --dry-run` is not a
safe preview mechanism because it may push.)

Workflow:
1. Inspect the repository without changing it:
   - Run `git status --short` and require a clean working tree, including no
     untracked files. Report the exact blocker and stop if it is not clean.
   - Determine the current branch and a base branch from repository configuration
     or the remote's configured default branch (for example, `git symbolic-ref
     refs/remotes/origin/HEAD` or `gh repo view --json defaultBranchRef`). Never
     assume `main`. Do not target `main` or `master` unless the human partner
     explicitly instructs you to do so.
   - Verify `gh` is available and authenticated, and check for an existing PR
     for the current head branch. Report clear blockers and stop when any
     precondition fails.
2. Gather evidence for the complete branch, not just the latest commit. Run
   the equivalent of `git log <base>..HEAD` and `git diff <base>...HEAD`, and
   inspect the complete output needed to understand all commits and changes.
   Require at least one commit ahead of `<base>`; otherwise report that there is
   nothing to propose.
3. Locate and read `.github/PULL_REQUEST_TEMPLATE.md` when it exists. Use its
   section order and headings as the structure of the draft. If it does not
   exist, use concise `Summary`, `Changes`, `Testing`, and `Notes` sections.
4. Create a temporary draft body file from the gathered evidence. Write a
   factual title and body: describe only changes supported by the commits and
   diff, preserve template placeholders, and use `TBD` when evidence is
   unavailable. Never fabricate an issue link, test/check result, approval,
   risk, or reference. Add `Closes #N` only when the human partner supplied or
   the repository evidence confirms issue `N`.
5. Show the human partner the proposed title, base branch, head branch, and
   full draft body. Then STOP and request explicit confirmation. Do not push or
   create a PR until confirmation is received. `--dry-run` only controls this
   preview and is never confirmation.
6. After explicit confirmation, push normally with `git push` (or
   `git push -u origin <head>` when the branch has no upstream); never force
   push. Create the PR with:
   `gh pr create --base <base> --head <head> --title <title> --body-file <draft-file>`
   Do not combine `--template` with the generated `--body-file`; the repository
   template is only the source used to draft the body. Return the resulting PR
   URL.

Context:
- Working directory: {workdir}
- Current project: {project}

Rules:
- Never invent facts or silently omit significant changes from the full diff.
- Never push or create a PR before explicit human confirmation.
- Do not force push.
- Do not create a PR targeting `main`/`master` without explicit instruction.
- If the branch has no commits ahead of the safely determined base, say so.
