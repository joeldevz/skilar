# PLAN: Advisor Strategy Integration (historical migration record)

> Historical plan retained for traceability. The original advisor migration is
> complete; current work is tracked in issues/reviews rather than this stale plan.

## Goal

Add the Advisor Strategy pattern to the skills system: a tool that lets worker agents (coder, tech-planner) consult a larger model for strategic guidance when stuck. Uses OpenCode's internal providers via `session.prompt()` — no external SDK needed.

## Steps

- [x] **Step 1 — Advisor agent + coder/tech-planner wiring in `opencode.json`**
  - Add `advisor` agent: model `anthropic/claude-opus-4-6`, `tools: {}` empty, mode `subagent`
  - System prompt: pure thinking, <100 words, enumerated steps, no code, no tools
  - Add `"advisor": true` to coder and tech-planner tools maps
  - Append compact advisor protocol reference (3-4 lines) to coder and tech-planner prompts

- [x] **Step 2 — Advisor tool `opencode/tools/advisor.ts`** (moved from `plugins/` in commit 23a7cb7 to fix tool registry collision; registers as `advisor_consult`)
  - Tool file uses `export const consult = tool({...})` direct export (not plugin Hooks.tool)
  - Registers `advisor_consult` tool with args: `question` (string)
  - Reads full transcript via `client.session.messages({ path: { id: ctx.sessionID } })`
  - Smart truncation: if transcript > 100K tokens (~400K chars), keep first 5 messages + last 50K tokens
  - Creates temp session with `advisor` agent via `client.session.create()`
  - Passes transcript + question via `client.session.prompt()` with model override
  - Waits for completion, reads response from session messages
  - Fallback: try/catch returns "Advisor unavailable, continue with your best judgment"
  - Counter: max 3 advisor calls per session (in-memory Map)
  - Secret sanitization: regex strip API keys/tokens before sending

- [x] **Step 3 — Advisor protocol `opencode/skills/_shared/advisor-protocol.md`**
  - 4 triggers: before substantive work, when done, when stuck, before pivoting
  - Treatment of advice: serious weight, reconcile call on conflict
  - Hierarchy: advisor = strategy (WHAT), tech-planner = tactics (HOW)
  - Fallback: if unavailable, continue without blocking
  - Max uses: 3 per task, circuit breaker after 2 consecutive without progress

- [x] **Step 4 — Claude Code support** (historical; renderer moved into Go)
  - The deleted `install_claude_assets.py` was replaced by the Go asset renderer.
  - Update `CLAUDE.md`: add advisor section — when coder returns blocked, orchestrator consults advisor as Task subagent

- [x] **Step 5 — Schema update for advisor config**
  - Add `advisor` section to `skills.config.schema.json`: enabled, model, max_uses
  - Add `advisor` to `skills.lock.schema.json`

## Verification
- `opencode.json` is valid JSON with advisor agent, coder and tech-planner have advisor tool
- `advisor.ts` compiles (TypeScript syntax valid)
- `advisor-protocol.md` is consistent with other _shared/ protocols
- The current Go renderer renders advisor agent to `~/.claude/agents/advisor.md`
- Schemas validate with example config
