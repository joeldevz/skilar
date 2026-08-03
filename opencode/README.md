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


- lee `PLAN.md`
- ejecuta un solo paso por vez
- delega implementacion a `coder`
- actualiza estados del plan
- obliga a revision humana antes de continuar

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

### Planificacion

- `/onboard`
  - explora un proyecto antes de trabajar
  - resume stack, arquitectura, comandos y convenciones
- `/plan <tarea>`
  - investiga el codebase
  - pregunta lo necesario
  - crea o reemplaza `PLAN.md`
- `/plan-rewrite`
  - revisa y mejora `PLAN.md`
  - rellena huecos y ajusta pasos

### Ejecucion

- `/execute`
  - toma el siguiente paso pendiente o con fixes
  - lo delega a `coder`
  - presenta cambios y pide review humana
- `/apply-feedback <cambios>`
  - toma feedback humano
  - vuelve a delegar correcciones
- `/status`
  - muestra completados, paso actual y siguientes pasos
- `/diff`
  - muestra los cambios del paso actual con anotaciones
  - agrupa por proposito, no solo por archivo
- `/rollback [step]`
  - deshace los cambios del ultimo paso (o del paso indicado)
  - pide confirmacion antes de revertir
  - actualiza PLAN.md a `[ ] pending`
- `/test [modulo]`
  - genera tests para el paso actual o modulo especificado
  - lee tests existentes para seguir los mismos patrones
  - corre los tests y reporta resultados

### Calidad

- `/review`
  - revisa todos los cambios contra CONVENTIONS.md
  - verifica tipos, imports, arquitectura, tests faltantes
  - produce un reporte estructurado (Passing / Warnings / Issues)
  - quality gate antes de `/commit`
- `/estimate`
  - estima el esfuerzo de cada paso del PLAN.md
  - asigna T-shirt sizes (XS a XL) con riesgos
  - util para planificacion de sprint

### Documentacion

- `/docs <libreria> <tema>`
  - busca docs en vivo via Context7 MCP
  - filtra a lo relevante y presenta un resumen practico
  - util cuando integras librerias nuevas o dudas de una API
- `/context [observacion]`
  - guarda descubrimientos y decisiones en Neurox
  - si no se pasa argumento, extrae aprendizajes del trabajo reciente
  - persiste entre sesiones para no repetir descubrimientos

### Git

- `/commit`
  - crea un commit local con Conventional Commits
- `/pr`
  - crea un pull request con `gh`
  - resume todos los cambios de la rama

## Flujo recomendado

```text
/onboard
/plan implementar login con JWT
/estimate
/execute
/diff
/apply-feedback separar DTOs y agregar tests
/execute
/test
/review
/commit
/pr
/context
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

9 golden tests en `evals/golden/` que validan el comportamiento esperado de los 3 agentes y commands clave:

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
