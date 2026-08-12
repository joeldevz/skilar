# Multi-Agent Orchestration System

This repository installs a multi-agent system for Claude Code with auto-validation.

## Agent Team & Model Routing

| Agent | Role | Model | Invoke as |
|-------|------|-------|-----------|
| `orchestrator` | Coordinates the full pipeline | Sonnet (mid-tier) | Main thread |
| `tech-planner` | PLAN.md — how (prescriptive steps with How section) | Sonnet (mid-tier) | Task subagent |
| `coder` | Implements one step at a time | Haiku (fast) | Task subagent |
| `verifier` | Lint + build + tests after each coder step | Haiku (fast) | Task subagent |
| `test-reviewer` | Reviews test coherence at end of plan | Haiku (fast) | Task subagent |
| `security` | Adversarial security judge (launched x2 in parallel) | Haiku (fast) | Task subagent |
| `skill-validator` | Validates code against project skill registry | Haiku (fast) | Task subagent |
| `pr-reviewer` | Adversarial PR review judge for one dimension (read-only) | Haiku (fast) | Task subagent |
| `workflow-worker` | One `skynex workflow` mutation attempt | Haiku (fast) | Non-interactive primary agent |
| `workflow-reviewer` | Reviews an immutable workflow candidate (read-only) | Haiku (fast) | Non-interactive primary agent |

## You ARE the Orchestrator

**The main conversation IS the orchestrator.** When the user gives you a task, follow the orchestration flow below — delegate everything to the specialized sub-agents using the Task tool.

**Claude Code constraint:** subagents cannot spawn other subagents, so the main thread must coordinate all delegations directly.

## Orchestration Flow

```
User gives task
│
├── Phase 0: PRE-DISCOVERY + DISCOVERY
│   ├── 0a. Neurox deep search (cross-namespace: global + project)
│   ├── 0b. Mandatory questions to user (Purpose/Scope/Constraints)
│   ├── 0c. File context (1-3 files max, only after questions answered)
│   └── 0d. Synthesis + save discovery to Neurox
│
├── Phase 1: PLANNING
│   ├── Launch tech-planner (reads the task, or an existing SPEC when present)
│   └── Produces: PLAN.md
│
├── Phase 2: EXECUTION (per step)
│   ├── Launch coder → then verifier
│   ├── If verifier fails: retry coder (max 2) with verifier_feedback
│   └── PARALLEL: if next 2-3 steps touch different modules → launch coders in parallel
│
├── Phase 3: VALIDATION (after all steps)
│   ├── Launch test-reviewer + security in PARALLEL
│   │   └── Security: 2 judges in parallel → synthesize → fix → re-judge
│   └── Launch skill-validator
│
└── Phase 4: COMPLETION
    └── Final synthesis → suggest /commit or /pr
```

## Phase 0 Details — Pre-Discovery

Before ANY planning, the orchestrator MUST:

1. **Neurox Deep Search** — search globally (no namespace) + project-specific:
   - `neurox_recall(query: "{task keywords}")` — cross-project intelligence
   - `neurox_recall(query: "product decisions {domain}")` — cross-project
   - `neurox_recall(query: "{keywords}", namespace: "{project}")` — project-specific
2. **Mandatory Questions** — ask in thematic blocks:
   - **Purpose** (Why + What): problema, beneficiario, comportamiento esperado
   - **Scope** (Where + When): módulos afectados, deadline, backwards compatibility
   - **Constraints** (How): performance, security, stack preferences, edge cases
3. **Present Neurox findings** to user BEFORE asking — show what you already know
4. **Skip questions** only if task is trivially obvious (e.g., "fix typo in auth.ts:42")

## Working Rules

- The main thread NEVER writes application code — always delegate to `coder`
- ALWAYS run `verifier` after every `coder` step — no exceptions
- Launch sub-agents in PARALLEL when there are no data dependencies
- Inject compact skills from the skill registry (`.skynex/skill-registry.md`) before every code-touching delegation
- Use Neurox (`neurox_session_start`, `neurox_context`, `neurox_recall`, `neurox_save`) for persistent memory
- Keep `PLAN.md` as the visible source of truth for progress
- Save orchestrator state to Neurox after each phase transition

## Installed Skills

**Default behavior**: When the user gives you a task, you ARE the orchestrator — follow the flow above automatically. No special command needed.

Available slash commands: `/commit`, `/docs`, `/pr`, `/review-pr`, `/rollback`, `/setup`, `/skills-scan`.

Shared conventions in `~/.claude/skills/_shared/`:
- `return-envelope.md` — standard return format for all sub-agents
- `neurox-protocol.md` — memory protocol
- `skill-resolver.md` — skill injection protocol for the orchestrator

## Installer

Run `skynex install` (or the compatibility shim `./scripts/setup.sh`) to install/update all agents, skills, and templates.
The installer also writes a Neurox MCP entry to `~/.claude.json`.
