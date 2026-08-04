# Agent Evaluation Framework

Tests mínimos para validar que los agentes principales se comportan como esperamos.

## Golden Tests (15 tests)

| Test | Agente | Qué valida |
|------|--------|------------|
| 01-planner-reads-conventions | planner | Lee CONVENTIONS.md antes de preguntar |
| 02-planner-uses-template | planner | Usa template PLAN-crud para tareas CRUD |
| 05-coder-reads-before-writing | coder | Lee código existente antes de escribir |
| 06-coder-runs-verification | coder | Corre tsc/build/test antes de reportar éxito |
| 08-test-follows-existing-patterns | coder | Lee tests existentes antes de generar nuevos |
| 10-workflow-state-recovery | orchestrator | Reanuda un workflow de forma idempotente desde estado persistido |
| 11-proportional-risk-depth | orchestrator | La profundidad de review sigue el riesgo efectivo |
| 12-candidate-drift-invalidation | orchestrator | El drift del candidate invalida la autoridad de review |
| 13-stale-result-rejection | orchestrator | Un resultado de worker obsoleto es solo auditoría |
| 14-receipt-exact-tree-gate | orchestrator | La entrega exige receipt vigente y árbol exacto |
| 15-orchestrator-workflow-v2-audit | orchestrator | Audita un workflow simple de bajo riesgo sin editar |
| 16-orchestrator-pr-review-evidence | orchestrator | Verifica findings y preserva la procedencia de la evidencia |
| 17-orchestrator-detached-background | orchestrator | Usa el workflow detached gestionado para trabajo en background |
| 18-agents-git-risk-policy | agentes | Aplican la política Git proporcional al riesgo |
| 19-orchestrator-continuous-correction | orchestrator | Continúa una corrección accionable sin pedir permiso en bucle |

## Plugin tests

`skynex-workflow.test.ts` cubre el plugin de estado de OpenCode
(`plugins/skynex-workflow.ts`) y está escrito sobre el runner `node:test`, así
que corre con Bun o con Node desde `opencode/`.

## Cómo correr

```bash
# Todos los golden tests
./evals/run-evals.sh

# Un test específico
./evals/run-evals.sh golden/01-planner-reads-conventions.yaml

# Solo tests de un agente
./evals/run-evals.sh --agent planner
```

## Formato de test YAML

```yaml
id: unique-id
name: "Nombre legible"
description: |
  Qué valida este test.

prompt: |
  Lo que se le envía al agente.

setup:
  files:
    - path: relativo/al/tmpdir
      content: |
        contenido del archivo

checks:
  must_read:           # Archivos que DEBE leer
    - CONVENTIONS.md
  must_read_any:       # Al menos uno de estos
    - file-a.ts
    - file-b.ts
  must_not:            # Cosas que NO debe hacer
    - "Write code directly"
  must_run_any:        # Comandos que debe ejecutar
    - "npx tsc --noEmit"
  reads_before_writes: # true = las lecturas deben ocurrir antes que las escrituras
  must_delegate_to:    # Subagente al que debe delegar
  expect_in_output:    # Strings que deben aparecer en la respuesta
    - "review"

timeout: 120000
```

## Evaluación

Los tests son **declarativos** — definen expectativas sobre el comportamiento del agente.
La evaluación hoy es manual (leer el output y verificar contra los checks).

Roadmap:
- [ ] Runner automático que parsea los YAML y ejecuta con opencode CLI
- [ ] Evaluadores que comparan tool calls contra los checks
- [ ] Resultados en JSON para tracking de regresión
- [ ] CI integration

## Cuándo agregar tests

- Cuando cambias el prompt de un agente
- Cuando agregas un nuevo command
- Cuando un agente se comporta mal y quieres evitar regresión
