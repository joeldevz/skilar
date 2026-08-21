# Project Conventions

> Cultural and stylistic rules enforced across all agents and skills in this repo.
> Changes here propagate to every system prompt via the orchestrator's skill-resolver.

---

## 1. Cultural disciplina (anti-sycophancy)

### Forbidden phrases (banned in agent outputs and prompts)

These add no value and consume tokens. Reject them in code review:

- "You're absolutely right!"
- "Great question!"
- "Excellent point!"
- "I apologize for the confusion"
- "Of course!" / "Certainly!"
- "I'd be happy to help" (just help)
- Any agradecimiento performativo
- Any sycophantic preamble

**Why banned**: they signal servility, consume context budget, and degrade signal-to-noise ratio of agent responses.

### Required terminology

- **"your human partner"** — when referring to the user in collaborative contexts (replaces "the user", "the human", "the developer")
- **"evidence"** — when discussing verification (replaces "I think", "should work", "looks good")
- **"status: blocked"** — explicit state when an agent cannot proceed (replaces vague "I'm not sure")

### Tone

- **Direct and surgical**: state the change, the rationale, and the verification. No preambles.
- **Bilingual ES/EN**: code, identifiers, technical docs in EN; user-facing prose, decision logs, and team docs can be ES or EN depending on audience. Never mix mid-sentence.
- **No emoji unless requested**: emojis in agent output are noise.
- **No marketing language**: "world-class", "revolutionary", "cutting-edge" are banned.

---

## 2. Skill design rules

Reference: `docs/IMPROVEMENT-PLAN.md` — Compromisos invariantes.

### Hard limits

- **SKILL.md ≤ 120 líneas** (progressive disclosure obligatorio)
- **`references/*.md` ≤ 200 líneas** cada uno
- **Description Trap**: el campo `description:` de un skill dice SOLO **"Use when X"**. NUNCA resume el workflow ni las reglas internas.

### Description examples

✅ **Correcto**:
```yaml
description: Use when the user requests a non-trivial feature before writing a PRD or plan.
```

❌ **Incorrecto** (resume workflow):
```yaml
description: Skill that interviews the user with questions to gather requirements, then generates a design tree document with decisions and assumptions.
```

### File structure

```
opencode/skills/<skill-name>/
├── SKILL.md              # ≤120 líneas, descripción + reglas core
├── references/           # opcional, módulos cargados bajo demanda
│   ├── advanced-X.md     # ≤200 líneas
│   └── examples-Y.md     # ≤200 líneas
└── examples/             # opcional, casos de uso reales
```

---

## 3. Smart-zone awareness

Reference: `opencode/skills/_shared/smart-zone-budget.md`.

- Hard cap: **100K tokens** efectivos
- Warning: **80K tokens** → planear corte limpio
- Estrategias: `/clear` (preferida), surgical compaction, return envelope handoff
- Anti-patrón: `/compact` full sin filtrar

---

## 4. TDD Discipline

Reference: `opencode/skills/tdd-discipline/SKILL.md` y QW3 inyectado en coder.

- **TDD Iron Law**: nunca modificar un test para que pase. Arreglar la implementación.
- **Anti-rationalization table**: aplicar a cada coder/verifier/test-reviewer
- **TDD Cycle Evidence**: el return envelope incluye `red_proof`, `green_proof`, `assertion_quality`
- **Mock Hygiene cap**: si un módulo necesita >6 mocks, es design smell → `status: blocked`

---

## 5. Return envelope

The canonical return envelope, including authoritative field definitions and allowed status values, is defined in `opencode/skills/_shared/return-envelope.md`. All agents and skills must follow that document; do not duplicate its field specification here.

---

## 6. Doc rot prevention

- Toda promesa en README/SPEC/PLAN debe corresponder a archivos reales
- Antes de eliminar/mover archivos: `grep -r` para detectar referencias rotas
- Los commands listados en README solo si existen en `opencode/commands/`

---

## 7. Memory protocol (Neurox)

Reference: `opencode/skills/_shared/neurox-protocol.md`.

- Toda sesión inicia con `neurox_session_start` + `neurox_context`
- Cross-namespace search en discovery (sin filtro de namespace)
- Save inmediato en eventos durables: decisión, bugfix, descubrimiento, patrón, gotcha, config, preference
- Format: `What: / Why: / Where: / Learned:`
- Cierre con `neurox_session_end` con summary Goal/Discoveries/Accomplished/Next

---

## 8. Branching y commits

- Convention: Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, `refactor:`)
- Branch naming: `feat/<scope>-<short-name>`, `fix/<issue>`
- Pre-commit AI gate (cuando GA3 esté implementado): valida diff vs CONVENTIONS.md
- Bypass de emergencia: `SKIP_AI_GATE=1`

---

## Referencias

- `docs/IMPROVEMENT-PLAN.md` — Plan rector y principios destilados
- `opencode/skills/_shared/` — protocols transversales
- `vault://Research/` — análisis comparativos (Matt Pocock, Superpowers, gentle-ai)
