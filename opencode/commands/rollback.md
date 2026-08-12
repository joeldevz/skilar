---
description: Undo the last step's changes and reset PLAN.md status
agent: skynex-orchestrator
---

Undo the changes from the current or last completed step and reset its status in PLAN.md.

Workflow:
1. Read PLAN.md and identify the target step:
   - If there's a `[~] in progress` step → rollback that one
   - If argument specifies a step number → rollback that one
   - Otherwise → rollback the last `[x] done` step
2. Identify the files that were modified in that step (from git diff or step notes)
3. **Ask for confirmation** before proceeding:
   "I'm about to undo Step N: <title>. This will revert: <file list>. Proceed?"
4. Only after human confirms:
   a. Run `git checkout -- <files>` to restore the files (for tracked files)
   b. Run `git clean -f <files>` for newly added files if appropriate
   c. Update PLAN.md: change the step status back to `[ ] pending`
5. Verify the rollback: run `git status` to confirm clean state

Context:
- Working directory: {workdir}
- Current project: {project}
- Target step: {argument}

Important:
- ALWAYS ask for confirmation before reverting
- Do NOT rollback more than one step at a time
- Do NOT use `git reset --hard` — only targeted file restores
- If the step included a database migration, warn that the migration must be reverted manually
- If there are uncommitted changes from OTHER steps, warn and abort unless the user confirms
