<div align="center">

# Skills

**Dale a tu agente de IA un equipo de sub-agentes especializados y un flujo de trabajo profesional.**

Un solo instalador. Funciona con Claude Code y OpenCode.

[Quick Start](#quick-start) · [Como funciona](#como-funciona) · [Commands](#commands) · [Herramientas soportadas](#herramientas-soportadas) · [Modos de trabajo](#modos-de-trabajo) · [Instalacion](docs/installation.md) · [Docs](#documentacion)

</div>

---

## El Problema

Los asistentes de IA para codigo son potentes, pero fallan con features complejas:

- **Sobrecarga de contexto** — Conversaciones largas llevan a compresion, detalles perdidos, alucinaciones
- **Sin estructura** — "Implementa login con JWT" produce resultados impredecibles
- **Sin gate de revision** — El codigo se escribe antes de que alguien acuerde que construir
- **Sin memoria** — Las decisiones viven en el historial del chat que desaparece

## La Solucion

**Skills** es un workflow portable de planificacion y ejecucion donde un orquestador liviano delega todo el trabajo real a agentes especializados. Cada agente arranca con contexto fresco, ejecuta una tarea acotada, y devuelve un resultado estructurado.

```
TU: "Quiero agregar export CSV a la app"

ORQUESTADOR (delega, contexto minimo):
  → lanza PLANNER        → retorna: discovery + PLAN.md
  → te muestra el plan, vos apruevas
  → lanza CODER          → retorna: paso 1 implementado
  → te muestra el diff, vos revisas
  → lanza CODER          → retorna: paso 2 implementado
  → ...hasta completar el plan
  → /review              → quality gate final
  → /commit + /pr        → entrega limpia
```

**La clave**: el orquestador NUNCA hace trabajo real directamente. Delega a sub-agentes, trackea estado en `PLAN.md`, y sintetiza resumenes. Esto mantiene el hilo principal liviano y estable.

**Neurox** como sistema de memoria persistente entre sesiones — las decisiones, patrones y descubrimientos sobreviven entre conversaciones.

## Como funciona

Tres conceptos:

1. **Arquitectura delegate-first** — Tu agente principal se convierte en orquestador y delega todo a sub-agentes especializados. Cada uno recibe contexto fresco, hace trabajo acotado, y devuelve solo un resumen. [Ver agentes →](#agentes)

2. **Command-driven workflow** — Un flujo estructurado con comandos de discovery, validacion y entrega. Cada fase es un skill que cualquier agente puede correr. [Ver commands →](#commands)

3. **Memoria persistente** — Neurox guarda decisiones, bugs, patrones y preferencias. El orquestador los consulta automaticamente al empezar cada sesion. [Ver memoria →](#memoria-persistente)

## Quick Start

### macOS / Linux

```bash
# Download install.sh and install.sh.sig from a tagged release, verify the
# signature with release/trust/skynex-release-signing-key.pub, then run locally.
./install.sh
```

### Windows (PowerShell)

```powershell
# Download install.ps1 and its signature from a tagged release, verify first,
# then run the local script.
./install.ps1
```

### Homebrew (macOS / Linux, explicit delegated trust)

```bash
brew tap joeldevz/tap
brew install skynex
```

---

Una vez instalado:

```bash
# Interactivo (recomendado la primera vez)
skynex install

# Solo Claude Code
skynex install --package skills --target claude

# Solo OpenCode
skynex install --package skills --target opencode

# Ambos targets (skills; Neurox es opcional)
skynex install --package skills --target both
```

El setup hace backup de tu configuracion existente antes de escribir.

Para instalacion manual, verificacion post-instalacion, y troubleshooting, ver [docs/installation.md](docs/installation.md).

## Skynex CLI

`skynex` es el CLI unificado para instalar, configurar y lanzar tu entorno de agentes.

### Comandos principales

| Comando | Que hace |
|---------|----------|
| `skynex install` | Instalador interactivo (TUI) |
| `skynex update [package]` | Actualiza paquetes instalados a la ultima version |
| `skynex status` | Dashboard: paquetes instalados, perfiles, herramientas |
| `skynex doctor` | Diagnostico del entorno y dependencias |
| `skynex up [profile]` | Lanza OpenCode con un perfil de modelos |
| `skynex backup list\|prune` | Inspecciona y purga los snapshots de recuperacion del instalador |
| `skynex workflow <cmd>` | Inspecciona o controla workflows gestionados ([docs](docs/workflow-commands.md)) |

### Perfiles de modelos

Los perfiles permiten lanzar OpenCode con diferentes configuraciones de modelos sin tocar `opencode.json`.

```bash
# Listar perfiles disponibles
skynex profile list

# Crear un perfil custom
skynex profile create

# Editar un perfil
skynex profile backend edit

# Eliminar un perfil
skynex profile backend delete

# Establecer perfil por defecto
skynex profile backend default

# Lanzar con perfil
skynex up                    # usa el perfil por defecto
skynex up cheap              # tier builtin: todo Haiku
skynex up premium            # tier builtin: Opus + Sonnet
skynex up backend            # tu perfil custom
skynex up backend --web      # modo web UI
```

**Tiers builtin:**

| Tier | Descripcion |
|------|-------------|
| `cheap` | Haiku para todo — rapido y barato |
| `balanced` | Sonnet para planificacion, Haiku para ejecucion (default) |
| `premium` | Opus para planificacion, Sonnet para ejecucion |

### Autocompletado de shell

```bash
# Bash
eval "$(skynex completion bash)"
# O permanente:
skynex completion bash > /etc/bash_completion.d/skynex

# Zsh
eval "$(skynex completion zsh)"

# Fish
skynex completion fish > ~/.config/fish/completions/skynex.fish
```

## Herramientas soportadas

| Herramienta | Sub-agentes | Setup |
|-------------|-------------|-------|
| Claude Code | Full (Agent tool) | `skynex install` |
| OpenCode | Full (delegate/task) | `skynex install` |

> **Full** = el orquestador delega a sub-agentes con contexto independiente.

## Agentes

La lista canonica es la clave `agent` de `opencode/opencode.json`; cada prompt vive en `opencode/agents/`.

| Agente | Rol | Que hace |
|--------|-----|----------|
| `orchestrator` | Coordinacion | Delega todo el trabajo real a sub-agentes y sintetiza resultados |
| `tech-planner` | Discovery y planificacion | Inicia memoria con Neurox, lee convenciones, explora el codebase, hace preguntas, genera `PLAN.md` |
| `coder` | Implementacion acotada | Implementa una tarea, sigue patrones locales, consulta Context7 para docs, verifica antes de entregar |
| `verifier` | Verificacion | Corre lint, build y tests despues de cada paso del coder |
| `test-reviewer` | Coherencia de tests | Clasifica cada archivo de test como SOUND, WEAK o MISLEADING |
| `security` | Juez de seguridad | Revision adversarial (se lanza x2 en paralelo) |
| `skill-validator` | Convenciones | Valida el codigo contra el registro de skills del proyecto |
| `pr-reviewer` | Review de PR | Juez adversarial de una sola dimension (R0–R4), solo lectura |
| `workflow-worker` | Ejecucion de slice | Agente primario no interactivo de un intento de mutacion de `skynex workflow` |
| `workflow-reviewer` | Review de candidate | Agente primario no interactivo que revisa el candidate inmutable, solo lectura |

### Estados de PLAN.md

| Estado | Significado |
|--------|-------------|
| `[ ]` | Pendiente |
| `[~]` | En progreso |
| `[!]` | Necesita fixes |
| `[x]` | Completado |

## Commands

> **Nota**: solo se listan los commands realmente disponibles en `opencode/commands/`. Las matrices anteriores prometían commands ficticios (doc rot eliminado en QW1).

### Exploracion

| Command | Que hace |
|---------|----------|
| `/docs <lib> <tema>` | Busca docs en vivo via Context7 MCP |

### Calidad y verificacion

| Command | Que hace |
|---------|----------|
| `/review-pr [scope]` | Revisa en profundidad un PR o diff actual con jueces en paralelo |
| `/rollback [step]` | Deshace el ultimo paso (pide confirmacion) |
| `/setup` | Configura el proyecto para OpenCode |
| `/skills-scan` | Regenera el registro físico de skills |

### Git

| Command | Que hace |
|---------|----------|
| `/commit` | Crea commit con Conventional Commits |
| `/pr` | Abre pull request con `gh` |

### Backlog (commands planeados, aún no implementados)

Los comandos no incluidos en la tabla no forman parte del inventario actual; consulta `opencode/commands/` para la lista canónica.

## Flujo recomendado

```text
/review-pr                      # revisar el PR o diff actual en profundidad
/commit                         # commit con Conventional Commits
/pr                             # abrir pull request
```

## Memoria persistente

**Neurox** es el sistema de memoria que conecta sesiones de trabajo:

- `session_start` — Inicia una sesion con titulo, directorio y branch
- `context` — Carga memorias relevantes al namespace actual
- `recall` — Busca decisiones, patrones o bugs previos
- `save` — Guarda descubrimientos con tags y archivos relacionados
- `session_end` — Cierra la sesion con un resumen

El setup configura Neurox automaticamente en ambas herramientas.

## Estructura del proyecto

```text
skills/
├── cmd/skynex/                # CLI entry point
├── internal/
│   ├── adapters/              # instalacion (claude, opencode)
│   ├── completion/            # autocompletado bash/zsh/fish
│   ├── profiles/              # CRUD de perfiles de modelos
│   ├── runner/                # lanzador de OpenCode con perfiles
│   └── ...
├── claude-code/
│   └── CLAUDE.md              # overlay para el orquestador en Claude Code
├── opencode/
│   ├── opencode.json          # configuracion base de agentes y MCPs
│   ├── commands/              # 7 slash commands reales
│   ├── skills/                # grill-me, prd, security, write-a-skill, diagnose, triage + _shared
│   ├── templates/             # convenciones, commits, 5 tipos de plan
│   ├── evals/                 # 15 golden tests de regresion
│   └── plugins/
├── skills/
│   └── prd/                   # skill compartida de PRD
└── scripts/
    └── setup.sh               # compatibility shim
```

## Que instala en cada herramienta

### Claude Code

- **7 slash skills** en `~/.claude/skills` con los mismos nombres operativos
- **Overlay de `CLAUDE.md`** para mantener el mismo workflow
- **Neurox MCP** configurado en `~/.claude.json`

### OpenCode

- **10 agentes** con roles claros en `opencode.json`
- **7 commands** para todo el ciclo
- **Neurox + Context7 MCP** como sistemas externos
- **Templates** para convenciones, commits/PRs, y 5 tipos de plan
- **Skills** de PRD, TypeScript avanzado, y patrones NestJS DDD+CQRS
- **Eval framework** con 15 golden tests

## Eval Framework

15 golden tests en `opencode/evals/golden/` validan el comportamiento de los
agentes. El inventario canónico esta en
[`opencode/evals/README.md`](opencode/evals/README.md).

```bash
./opencode/evals/run-evals.sh
./opencode/evals/run-evals.sh --agent coder
```

## Documentacion

| Tema | Descripcion |
|------|-------------|
| [Instalacion](docs/installation.md) | Guia completa: requisitos, setup automatico/manual, verificacion, troubleshooting |
| [Workflow commands](docs/workflow-commands.md) | `skynex workflow`: ejecucion detached, recuperacion y gates de entrega |
| [Modelo de seguridad](docs/security-model.md) | Confianza del instalador: binding de commits firmados y escrituras en filesystem |
| [Firma de releases](docs/release-signing.md) | Claves, verificacion de firmas y procedimiento del mantenedor |
| [OpenCode setup](opencode/README.md) | Configuracion detallada de OpenCode |
| [Claude Code setup](claude-code/CLAUDE.md) | Overlay y reglas para Claude Code |

## Claude Code: nota importante

Claude Code no permite que un subagente lance otro subagente. Para mantener el mismo comportamiento:

- El hilo principal de Claude hace de orquestador
- `planner` y `coder` se usan como subagentes
- La orquestacion multi-agente se queda en el hilo principal

## Recomendacion

En cada proyecto nuevo, copia `opencode/templates/CONVENTIONS.md` a la raiz del repo y ajustalo al stack real. Eso hace que los agentes sean mucho mas consistentes.

## Licencia

MIT

---

<div align="center">
  <sub>Built by <a href="https://github.com/joeldevz">joeldevz</a></sub>
</div>
