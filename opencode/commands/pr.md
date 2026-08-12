---
description: Create a pull request for the current branch
agent: skynex-orchestrator
subtask: true
---

Create a pull request for the current branch.

Workflow:
1. Run `git status` to check if there are uncommitted changes (warn if so)
2. Run `git log main..HEAD` (or appropriate base branch) to see all commits in this branch
3. Run `git diff main...HEAD` to understand the full scope of changes
4. Push the branch to remote if not already pushed
5. Create the PR using `gh pr create` with:
   - Title following conventional commit format
   - Body with Summary, Changes, Testing, and Notes sections
6. Return the PR URL

Context:
- Working directory: {workdir}
- Current project: {project}

Rules:
- Do not force push
- Do not create PR to main/master without explicit user instruction
- If the branch has no commits ahead of base, say so
- Include all significant changes in the PR body, not just the latest commit
