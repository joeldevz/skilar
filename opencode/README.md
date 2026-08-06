# OpenCode Team Setup

Configuracion de OpenCode para trabajo en equipo con un flujo simple y controlado:

1. descubrir bien la tarea
2. generar `PLAN.md`
3. ejecutar un paso por vez
4. pedir revision humana
5. corregir o avanzar

## Contenido

```text
opencode/
  opencode.json
  package.json
  .gitignore
  tui.json
  README.md
  commands/
  evals/            # golden tests para validar comportamiento de agentes
  plugins/
  skills/
  templates/
```

## Agentes

### `planner`

- corre un **discovery checklist** antes de preguntar: primero inicia memoria con Neurox (`neurox_session_start` + `neurox_context`), luego lee CONVENTIONS.md, package.json, modulos similares, tests, y consulta decisiones previas con `neurox_recall`
- hace preguntas de negocio y tecnicas en bloques
- recomienda defaults razonables
- genera `PLAN.md`

### `infrastructure-engineer`

- gestiona tareas acotadas de infraestructura, CI/CD, runtime y despliegue
- prepara las herramientas de calidad requeridas desde sus fuentes upstream
- separa los límites no testeables y ejecuta la verificación relevante
- conserva el acceso completo a Neurox para registrar decisiones operativas duraderas

Estados de `PLAN.md`:

- `[ ] pending`
- `[~] in progress`
- `[!] needs fixes`
- `[x] done`

### `coder`

- implementa una tarea acotada
- sigue patrones locales del repo
- trabaja principalmente en TypeScript, Node.js y NestJS
- consulta **Context7** para docs en vivo cuando trabaja con librerias externas
- corre verificaciones antes de devolver exito

## Commands

La lista canónica es el contenido de `opencode/commands/`:

`/commit`, `/docs`, `/pr`, `/review-pr`, `/rollback`, `/setup` y `/skills-scan`.

Los comandos descritos en documentos antiguos no forman parte de la distribución actual.

## Flujo recomendado

```text
/setup
/docs Go testing
/review-pr
/commit
/pr
```

## Setup para cada miembro del equipo

1. Copiar el contenido de `opencode/` a `~/.config/opencode/`
2. Ejecutar `bun install` dentro de `~/.config/opencode/`
3. Tener Neurox configurado si quieres memoria persistente
4. Tener `gh` autenticado si quieres usar `/pr`

## Configuracion local opcional

### Context7

Context7 esta habilitado por defecto pero requiere API key. Cada persona debe editar localmente `~/.config/opencode/opencode.json` y poner su API key real:

```json
"context7": {
  "type": "remote",
  "url": "https://mcp.context7.com/mcp",
  "headers": {
    "CONTEXT7_API_KEY": "TU_API_KEY"
  },
  "enabled": true
}
```

Sin la key, Context7 simplemente no funcionara pero no rompe el flujo.
No subir esa key al repositorio.

### Neurox

`neurox` es el sistema de memoria persistente preferido. La secuencia recomendada al empezar cualquier trabajo es `neurox_session_start` y despues `neurox_context`, antes de cualquier otra contextualizacion.

## Templates

### `templates/CONVENTIONS.md`

Template de convenciones para copiar a cada proyecto. Esta basado en el backend real explorado y define:

- DDD + CQRS + capas
- uso obligatorio de Value Objects en lugar de primitivos en dominio
- naming, imports, controllers, DTOs, repositorios y errores
- como se escriben tests unitarios y E2E
- stack esperado: NestJS, Fastify, Prisma, SWC, Jest

### `templates/PLAN-crud.md`

Referencia para CRUDs completos.

### `templates/PLAN-bugfix.md`

Referencia para fixes con enfoque RED -> GREEN -> REFACTOR.

### `templates/PLAN-integration.md`

Referencia para integraciones con servicios externos.

### `templates/PLAN-feature.md`

Referencia para features generales que no encajan en CRUD, bugfix, integration, o refactor.

### `templates/PLAN-refactor.md`

Referencia para refactors con red de seguridad de tests.

### `templates/COMMIT-CONVENTIONS.md`

Reglas de Conventional Commits y estructura de PR.

## Skills incluidas

- `grill-me` — alineación humano-AI antes del PRD (Brooks shared design concept)
- `prd` — generación de PRDs tras grilling
- `security` — adversarial dual-judge security review
- `write-a-skill` — create new agent skills with proper structure
- `diagnose` — disciplined diagnosis loop for hard bugs and perf regressions
- `triage` — triage issues through a state-machine of roles (bug/enhancement + states)

Compartidas en `_shared/`:
- `advisor-protocol.md`, `neurox-protocol.md`, `return-envelope.md`, `skill-resolver.md`, `smart-zone-budget.md`

## Regla mas importante

En dominio, usar siempre objetos de dominio y Value Objects, no primitivos.

Ejemplos:

- `Money` en lugar de `number` para dinero
- `Uuid` en lugar de `string` para ids
- `Email` en lugar de `string` para correos
- `DateRange` en lugar de dos `Date` sueltas

DTOs si pueden usar primitivos porque son la frontera de serializacion.

## Eval Framework

9 golden tests en `evals/golden/` que validan el comportamiento esperado de los agentes y commands clave:

| Test | Agente       | Valida                                        |
| ---- | ------------ | --------------------------------------------- |
| 01   | planner      | Lee CONVENTIONS.md antes de preguntar         |
| 02   | planner      | Usa template PLAN-crud para tareas CRUD       |
| 03   | orchestrator | Lee PLAN.md antes de hacer nada               |
| 04   | orchestrator | Se detiene tras un paso y pide review         |
| 05   | coder        | Lee codigo existente antes de escribir        |
| 06   | coder        | Corre verificacion antes de reportar exito    |
| 07   | orchestrator | /review lee CONVENTIONS.md y git diff         |
| 08   | coder        | /test lee tests existentes antes de generar   |
| 09   | orchestrator | /rollback pide confirmacion antes de revertir |

```bash
# Ver los tests y sus checks
./evals/run-evals.sh

# Filtrar por agente
./evals/run-evals.sh --agent coder
```

Hoy la evaluacion es manual (leer output y verificar). El roadmap es automatizar el runner.

## Que tocar cuando quieras ajustar algo

- `opencode.json` -> comportamiento base de agentes y MCPs
- `commands/*.md` -> comportamiento puntual de cada slash command
- `templates/*.md` -> referencia de convenciones, planes, commits y PRs
- `evals/golden/*.yaml` -> tests de regresion para validar cambios en prompts

## Nota practica

Este repo esta pensado para ser portable. Por eso:

- incluye `package.json` para poder correr `bun install`
- las skills se incluyen como archivos reales, no como symlinks locales
- no incluye secretos en la configuracion compartida
