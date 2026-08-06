# Skynex — Improvement Plan

> **Status**: Active · **Owner**: Christopher · **Last revision**: 2026-05-18
> **Source**: Destilación de Matt Pocock + obra/Superpowers + Gentleman/gentle-ai + harnesses Gentle (mayo 2026)
> **Companion file**: `docs/plan.json` (backlog ejecutable estructurado)

---

## Filosofía (invariante)

**No copiamos código de otros proyectos.** Destilamos los principios validados y diseñamos NUESTRAS piezas con todo el conocimiento acumulado, encajando en nuestro stack (OpenCode + Claude Code + Neurox + return envelope + 9 agentes coordinados).

Las menciones a Matt Pocock, obra/Superpowers o Gentleman/gentle-ai son **referencias de procedencia del principio**, no instrucciones de copia.

---

## Compromisos invariantes (norte arquitectónico)

1. **Smart-zone hard cap 100K tokens** → reset automático
2. **SKILL.md ≤120 líneas** + `references/*.md` (≤200 c/u) — progressive disclosure
3. **HITL default; AFK opt-in** explícito por slice tag
4. **TDD Iron Law** solo cuando `slice.tdd=true` (no fricciona bugfixes triviales)
5. **Return envelope** con `slice_id` + `mode: hitl|afk` en todos los agentes
6. **Doc rot prohibido** — toda promesa en README/SPEC/PLAN debe corresponder a archivos reales
7. **Description Trap rule** — descripción de skill = SOLO "Use when X", NUNCA resume el workflow
8. **Disciplina cultural** — ban "You're absolutely right!" / agradecimientos performativos; usar "your human partner"

---

## 📊 Estado actual — lo que TENEMOS

### ✅ Fase 0 — Quick Wins (HECHO)

| # | Item | Estado |
|---|---|---|
| QW1 | README limpio de commands ficticios | ✅ DONE |
| QW2 | Skill `grill-me` (67L) | ✅ DONE |
| QW3 | TDD Iron Law + anti-rationalization en coder | ✅ DONE |
| QW4 | `_shared/smart-zone-budget.md` | ✅ DONE |
| QW5 | advisor.ts en `opencode/tools/` | ⛔ RETIRADO — el Advisor se eliminó del bundle |
| QW6 | Skill `verification-before-completion` (89L) | ✅ DONE |

### ✅ Fase 1 — Simplificación (HECHO)

3 SKILLs grandes eliminados: `typescript-advanced-types` (724L), `nestjs-patterns` (607L), `clasing-ui-v2-beta` (656L). Total: 1987 líneas borradas.

### ✅ Fase 1.5 — Sprint 0.5 (HECHO)

| # | Item | Estado |
|---|---|---|
| IL1 | Description Trap audit (todas las skills) | ✅ DONE |
| IL2 | Skill `tdd-discipline` (94L) | ✅ DONE |
| IL3 | = QW6 | ✅ DONE |
| IL4 | "your human partner" + CONVENTIONS.md | ✅ DONE |
| IL5 | Anti-rationalization tables en coder/verifier/test-reviewer/security | ✅ DONE |
| IL6 | SUBAGENT-STOP gate | ✅ DONE |
| IL7 | `_shared/dispatching-parallel-agents.md` | ✅ DONE |
| IL8 | `docs/lessons-learned/inline-vs-subagent-review.md` | ✅ DONE |
| IL9 | `opencode/hooks/session-start.sh` | ✅ DONE |
| IL10 | `docs/design/two-stage-review.md` | ✅ DONE |

### ✅ Fase 1.6 — Sprint 0.5b (HECHO)

| # | Item | Estado |
|---|---|---|
| GA1 | `.github/` ISSUE_TEMPLATE + PR template + workflows | ✅ DONE |
| GA2 | Skill `adversarial-review` (109L) generalizado | ✅ DONE |
| GA3 | Pre-commit AI gate en TypeScript | ⏳ pendiente (no hay `scripts/precommit-ai-gate.ts`) |
| GA4 | `/skills-scan` físico | ✅ DONE (`opencode/commands/skills-scan.md`) |

### 🟢 Fortalezas propias activas

- 10 agentes con responsabilidades claras (orchestrator, tech-planner, coder, verifier, test-reviewer, security, skill-validator, pr-reviewer, workflow-worker, workflow-reviewer)
- Return envelope estandarizado con campo `skill_resolution` (detecta context compaction)
- Dual-judge adversarial security con re-judgment
- Neurox con kinds + types + 4D scoring + brain power
- 15 golden eval tests en `opencode/evals/golden/`
- skilar CLI Go con profiles + doctor + adapters claude/opencode

---

## 🎯 Lo que DEBEMOS TENER — pendiente

### 🔴 Deuda crítica (no estaba en el plan original)

| # | Item | Razón | Esfuerzo |
|---|---|---|---|
| ~~QW8~~ | ✅ DONE 2026-05-18 — Fix grill-me + migración orchestrator a `opencode/agents/orchestrator.md` | — | — |
| **D1** | Plantilla `.gitmessage` con secciones Why + Rejected alternatives | Resuelve "¿por qué X vs Y?" sin sistema de trazas paralelo | 15 min |

> **Decisión 2026-05-18**: QW7 (banner visual) eliminado. Análisis demostró que era decoración sin problema real — git commit + Neurox ya cubren auditoría y decisiones. Reemplazado por D1 (disciplina de commits) que es 10x más útil con menor costo.

### 🟡 Sprint 1 — Fundamentos (pendiente)

| # | Item | Archivo | Esfuerzo |
|---|---|---|---|
| M1 | `templates/PLAN-feature.md` → vertical slices | `opencode/templates/PLAN-feature.md` (hoy 82L con 8 fases horizontales) | medio |
| M4 | PRD slim (eliminar "Phase 1: Interrogate", ≤120L) | `opencode/skills/prd/SKILL.md` (hoy 167L) | bajo |
| A2 | Skill `tdd-red-green-refactor` formal | `opencode/skills/tdd-red-green-refactor/SKILL.md` (NUEVO) | bajo |
| A11 | Golden test Iron Law | `opencode/evals/golden/iron-law.md` | bajo |
| A12 | Golden test vertical slice | `opencode/evals/golden/vertical-slice.md` | medio |
| E1 | Deduplicar `skills/prd/` (existe duplicado del de `opencode/`) | `skills/prd/` | 5 min |

### 🟡 Sprint 0.5b backlog — gaps de harnesses Gentle (NUEVO, mayo 2026)

| # | Item | Razón (harness Gentle) | Esfuerzo |
|---|---|---|---|
| **GA12** | Skill `review-workload` (PR >400 líneas → estrategia partir) | Review Workload Harness #18 | 2h |
| **GA13** | Extender `/pr` con `--chain` y `--feature-track` | Chain Strategy + Delivery Strategy #19/20 | 3h |
| **GA14** | Diseño `docs/design/compaction-recovery.md` | Session Summary + Compaction Recovery #30 | 2h |
| GA3 | Pre-commit AI gate en TypeScript | Pre-commit AI gate (gentle-ai) | 4h |

### 🟢 Sprint 1 backlog (GA5-GA8, ya planeado)

- **GA5** — Profiles switchables (3 archivos `opencode/profiles/{explore,balanced,production}.json`) · 4-6h
- **GA6** — Skill creator meta-skill · 2h
- **GA7** — Neurox interop study · 1 día
- ~~**GA8** — Cross-provider advisor (Codex GPT-5.3)~~ · cancelado con el retiro del Advisor

### 🟢 Sprint 2 — Arquitectura (pendiente)

| # | Item | Archivo | Esfuerzo |
|---|---|---|---|
| A3 | Skill `improve-codebase-architecture` (Ousterhout deep modules) | NUEVO | medio |
| A9 | Agente `architect` (Opus, post-PRD pre-tech-planner) | `opencode/opencode.json` | medio |
| M3 | ~~Manager con HITL/AFK rutearing~~ — el `manager` fue retirado; los gates HITL viven en `skynex workflow` | — | — |
| M14 | Orchestrator Opus + task_budget | `opencode/opencode.json` | bajo + $$ |
| M10 | Test-reviewer Iron Law audit step | `opencode/opencode.json` | bajo |
| A6 | Command `/grill` | `opencode/commands/grill.md` (NUEVO) | bajo |
| A20 | Command `/calibrate` (post-onboard, genera `.skynex/project-config.yaml`) | NUEVO — **harness SDD Init #3** | 2h |

### 🟢 Sprint 3 — Escalamiento (pendiente)

- A4 · AFK Ralph loop docker
- A15 · Security cross-lab (Phase 5 cuando dual-judge contradice)
- A13 · Golden test cross-provider
- A7 · Command `/afk-run`
- ~~Cross-provider advisor wiring real (Codex GPT-5.3)~~ · cancelado con el retiro del Advisor

### 🟢 Backlog P2 (post-publicación comunidad)

- GA9 · Persona system opt-in bilingüe · 2-3h
- GA10 · Backup automático antes de cada `opencode/skills/` write · 4h
- GA11 · Commands `/contribute`, `/propose-skill` · 2h

---

## 🗺️ Mapeo: 30 Agent Harnesses (Gentle mayo 2026) vs Skynex

| # | Harness | Estado en Skynex |
|---|---|---|
| 1 | SDD Orchestrator (puro, no ejecuta) | ✅ filosofía base |
| 2 | Delegation | ✅ implícito en arquitectura |
| 3 | SDD Init (calibrar repo) | ⏳ A20 propuesto (`/calibrate`) |
| 4 | Execution Mode (interactive/auto) | ⏳ M3 Sprint 2 (HITL/AFK) |
| 5 | Artifact Store (chat NO source of truth) | ✅ PLAN.md + SPEC.md + Neurox |
| 6 | Phase DAG | ✅ vertical slices con `blocks:` (M1 Sprint 1) |
| 7 | Artifact Dependency | ✅ implícito en `blocks:` de slices |
| 8 | Result Contract (envelope) | ✅ `_shared/return-envelope.md` |
| 9 | SDD Artifact Grammar | ❌ **NO adoptar** — vertical slices son superiores |
| 10 | Engram Memory | ✅ Neurox (más rico que Engram) |
| 11 | Strict TDD | ✅ `tdd-discipline` + Iron Law en coder |
| 12 | Verify | ✅ verifier agent + `verification-before-completion` |
| 13 | TaskList Continuity | ⚠️ PLAN.md con states (parcial) |
| 14 | Skill Registry | ✅ `_shared/skill-resolver.md` + GA4 `/skills-scan` |
| 15 | Skill Digestion (Compact Rules) | ✅ skill-resolver inyecta Project Standards |
| 16 | Skill Resolution Feedback | ✅ campo `skill_resolution` en envelope |
| 17 | Subagent Isolation | ✅ `mode: subagent` + IL6 SUBAGENT-STOP |
| 18 | Review Workload | ⏳ **GA12 propuesto** |
| 19 | Delivery Strategy | ⏳ **GA13 propuesto** |
| 20 | Chain Strategy | ⏳ **GA13 propuesto** |
| 21 | Model Routing | ⏳ GA5 profiles backlog |
| 22 | Profile Isolation | ✅ skilar profiles + tui.json |
| 23 | Permission Security | ⚠️ dual-judge cubre review, no runtime guards |
| 24 | MCP Injection | ❌ no urgente |
| 25 | Backup | ✅ adapters Go backup-before-overwrite |
| 26 | Rollback | ✅ `opencode/commands/rollback.md` |
| 27 | Component Dependency | ✅ implícito en `blocks:` de slices |
| 28 | Command Wrapper | ❌ no urgente |
| 29 | Per-Agent Adapter | ✅ skilar adapters (claude/opencode) |
| 30 | Session Summary / Compaction Recovery | ⏳ **GA14 propuesto** (diseño) |

**Resultado**: 21/30 cubiertos · 5 propuestos nuevos · 4 decisiones conscientes de NO implementar.

---

## Decisiones tomadas (no revisitar sin razón fuerte)

### Filosofía
- ✅ Plan principle-driven, no copy-driven
- ✅ Compromisos invariantes #1-#8 son contrato

### Simplificación
- ✅ Eliminar `typescript-advanced-types`, `nestjs-patterns`, `clasing-ui-v2-beta`
- ⛔ Plugin advisor.ts retirado: agente, plugin y protocolo `_shared/advisor-protocol.md` ya no se distribuyen

### Disciplina (de Superpowers)
- ✅ TDD Iron Law con anti-rationalization table en coder
- ✅ smart-zone-budget con cap 100K, warning 80K
- ✅ NO importar repo entero de Superpowers — destilar patrones
- ✅ NO adoptar SessionStart hook tal cual — diseñar propio
- ✅ NO adoptar Visual Companion / brainstorm-server — overengineering
- ✅ Disciplina cultural: "your human partner"

### Operación (de gentle-ai)
- ✅ NO importar repo entero de gentle-ai — destilar, no migrar a Go
- ✅ NO adoptar SDD lineal de 9 fases — son ortogonales a vertical slices
- ✅ NO copiar `strict-tdd.md` literal — destilar en `tdd-discipline`
- ✅ NO copiar `judgment-day` literal — generalizar `security` con `domain`
- ⛔ Pre-commit AI gate en TypeScript reusando `advisor_consult` — descartado con el retiro del Advisor
- ✅ Skill-registry físico generado por `/skills-scan`

### Harnesses Gentle (mayo 2026)
- ✅ SÍ añadir: review-workload, chain strategy, compaction recovery, banner visual, `/calibrate`
- ✅ NO añadir: SDD 9 fases, persistence modes múltiples, TUI separado, MCP injection gating, command wrapper

---

## Riesgos y mitigaciones

| Riesgo | Mitigación |
|---|---|
| Costo orchestrator Sonnet → Opus | task_budget threshold; Sonnet fallback |
| AFK runaway loop | Hard cap iteraciones + budget cap + sandbox docker |
| Refactors rompen URLs internas | Verificar con `grep -r` antes de borrar |
| Evals invalidados con vertical slices | Re-baselinar golden tests antes de M1 |
| Migration shock | P0 en feature branch, validar evals, mergear atómico |

---

## Métricas de éxito

- [ ] 0 SKILL.md > 120 líneas (hoy: prd 167L, skills/prd 167L)
- [ ] 100% slices verticales en PLAN-feature (hoy: 8 fases horizontales)
- [ ] ≥ 3 golden tests cubren Iron Law / vertical / cross-provider
- [ ] `.gitmessage` template activo con Why + Rejected alternatives
- [ ] 0 duplicación entre `skills/` y `opencode/skills/`
- [ ] grill-me invocado vía delegación, no inline en orchestrator

---

## Next steps (orden ejecutivo)

### HOY — Deuda crítica + disciplina ✅ COMPLETADO
- ✅ ~~QW8~~ — DONE (fix grill-me + migración orchestrator a archivo separado)
- ✅ ~~D1~~ — DONE (`.gitmessage` template con Why + Rejected alternatives)
- ✅ ~~E1~~ — DONE (eliminado `skills/prd/` legacy, canónico = `opencode/skills/prd/`)
- ✅ Bonus: 10 MCPs unificados al repo con env var placeholders + `docs/setup-mcps.md`

### Backlog nuevo derivado (post-QW8)
- **GA15** (P2) — Migración oportunista de los 9 agentes restantes a `opencode/agents/*.md` (cuando se editen, no big-bang)
- **GA16** (P2) — Script `scripts/setup-git.sh` para que nuevos clones autoconfiguren `commit.template`

### Esta semana (~7h) — Cierre Sprint 0.5b + harnesses Gentle nuevos
- **GA12** — Skill `review-workload` (2h)
- **GA13** — `/pr --chain --feature-track` (3h)
- **GA14** — Diseño `compaction-recovery.md` (2h)

### Próxima semana — Sprint 1 kickoff
- **M1** — PLAN-feature.md → vertical slices
- **M4** — PRD slim a ≤120L
- **A2** — `tdd-red-green-refactor` skill
- **A11**, **A12** — Golden tests Iron Law + vertical-slice
- **A20** — Command `/calibrate` (harness SDD Init)

### Después — Sprint 2 → Sprint 3
- Re-baseline evals con golden tests nuevos
- Mergear Sprint 1 atómico
- Iniciar Sprint 2 (architect + HITL/AFK + Opus orchestrator)

---

## Referencias

### Vault notes (síntesis investigada)
- `vault://Research/Matt-Pocock-AI-Software-Engineering-Workshop.md`
- `vault://Research/Matt-Pocock-Community-Validation.md`
- `vault://Research/Skills-Repo-vs-Matt-Pocock.md`
- `vault://Research/Skills-Repo-Improvement-Plan-v2.md`
- `vault://Research/Superpowers-vs-Clasing-Skills.md`
- `vault://Research/Gentle-AI-vs-Clasing-Skills.md`

### Neurox memory (handoffs durables)
- `01KQFT5TW6ZR9N69P7TFEC2RPS` — bug grill-me duplicación (forensic)
- `01KMYVRJK7253CKCT8MVWWTGEB` — gentle-ai SDD pipeline patterns
- `01KQFEDD554DSWWW2D67V97NE6` — mattpocock/skills deep analysis
- `01KQ2DZ6DGYAZS2W16BN79W5FM` — gentle-ai hard data verification
- `01KQ2DX98CVBFZBF4K46Y6T3WS` — Skynex vs Matt Pocock verdict
- `01KRWZVEJ5FA2MHZ5AN6MY6W4Z` — 30 harnesses Gentle (mayo 2026) gap analysis

### Bibliografía rectora
- Brooks — *The Design of Design* (shared design concept)
- Ousterhout — *A Philosophy of Software Design* (deep modules)
- Pragmatic Programmer — Tracer bullets / vertical slices
- Chroma research — context rot benchmarks empíricos
- Anthropic Advisor Strategy (abr 2026)

### Repos de referencia
- [obra/superpowers](https://github.com/obra/superpowers) — disciplina adversarial
- [Gentleman-Programming/gentle-ai](https://github.com/Gentleman-Programming/gentle-ai) — operación industrial
- [Gentleman-Programming/engram](https://github.com/Gentleman-Programming/engram) — memoria persistente Go
- [Gentleman-Programming/gentleman-guardian-angel](https://github.com/Gentleman-Programming/gentleman-guardian-angel) — pre-commit AI gate
