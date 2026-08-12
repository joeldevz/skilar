---
description: Create a conventional commit for the current changes
agent: skynex-orchestrator
subtask: true
---

Create a commit for the current staged changes following the project's commit conventions.

Workflow:
1. Run `git status` and `git diff --staged` to understand what changed
2. Determine the appropriate commit type (feat, fix, refactor, test, docs, chore, etc.)
3. Determine the scope from the affected context or module
4. Write a commit message following Conventional Commits format: `<type>(<scope>): <description>`
5. If the change is complex, add a body explaining why
6. Stage any unstaged related files if needed
7. Create the commit

Context:
- Working directory: {workdir}
- Current project: {project}

Rules:
- First line max 72 characters
- Use imperative present tense in English: "add", "fix", "remove"
- Do not end with a period
- Do not commit files that contain secrets (.env, credentials, etc.)
- If there are no changes to commit, say so

Do NOT push to remote. Only create the local commit.
