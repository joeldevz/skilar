---
name: Neurox Protocol
description: Memoria persistente entre sesiones.
license: Complete terms in LICENSE.txt
---

> **REGLA FUNDAMENTAL**: Neurox es OBLIGATORIO en todas las skills y agentes.
> Cada skill DEBE consultar Neurox al inicio y guardar descubrimientos relevantes.
> No existe skill que opere sin memoria — Neurox ES la memoria del sistema.

## Cuándo usar Neurox

- **Al inicio de cada tarea**: `neurox_session_start` + `neurox_context` ANTES de cualquier otra acción
- **Búsqueda cross-namespace**: `neurox_recall` SIN namespace para encontrar contexto de OTROS proyectos
- **Búsqueda project-specific**: `neurox_recall` CON namespace para el proyecto actual
- **Durante la tarea**: `neurox_recall` con queries cortos tipo keyword antes de responder preguntas técnicas o modificar código conocido
- **Al encontrar algo durable**: `neurox_save` inmediatamente (no al final — se puede perder)
- **Al terminar**: `neurox_session_end` con resumen Goal/Discoveries/Accomplished/Next

## Protocolo de inicio (obligatorio para TODA skill y agente)

```
1. neurox_session_start(title, directory, namespace="{project}")
2. neurox_context(namespace="{project}")   ← leer ANTES de explorar el repo
3. Búsqueda cross-namespace (global — sin namespace):
   neurox_recall(query="{keywords del task}")
   neurox_recall(query="product decisions {domain}")
   → Esto encuentra contexto de OTROS proyectos que puede ser relevante
4. Búsqueda project-specific:
   neurox_recall(query="{keywords}", namespace="{project}")
5. Si hay preguntas de identidad/preferencias del usuario:
   neurox_recall(query="nombre preferencia usuario", observation_type="preference")
   → Si no hay resultado: 2-3 búsquedas más con variantes antes de rendirse
```

## Mandatory durable-save triggers

Save immediately when verified, durable knowledge fits one of these categories:

| Category | observation_type | topic_key pattern | Required evidence |
| --- | --- | --- | --- |
| Error solution | `bugfix` | `solution/{module}/{error-class}` | root cause, durable solution, regression proof, limits/non-applicability |
| Scope decision or explicit non-goal | `decision` | `scope/{feature}/{decision}` | decision, rationale, in/out boundary |
| Dependency compatibility or deprecation | `discovery` | `dependency/{name}/{version-range}` | affected range, evidence, upgrade/avoidance guidance |
| Explicitly accepted risk | `decision` | `risk/{area}/{risk}` | risk, acceptance rationale, mitigation/follow-up; never invent an approver |
| Recurrent blocker | `gotcha` | `blocker/{root-cause-class}` | recurrence signal, root cause, recovery steps |

Existing durable architecture decisions, patterns, preferences, configuration,
and codebase discoveries remain valid triggers. Do not save every validation or
delivery event.

## Formato de contenido al guardar

```
What: [what was discovered or decided]
Why: [why it matters]
Where: [relevant files or modules]
Evidence: [verification, reference, or recurrence signal]
Learned: [durable guidance for the future]
Limits or follow-up: [non-applicable conditions, mitigation, or next action]
```

## Integración obligatoria en Skills

**TODA skill** debe incluir uso de Neurox. Cuando ejecutes cualquier skill:

1. **Al iniciar**: `neurox_recall(query="{tema de la skill}")` — buscar contexto previo
2. **Cross-namespace**: `neurox_recall(query="{tema}")` sin namespace — inteligencia de otros proyectos
3. **Al descubrir algo**: persist it immediately when the agent has Neurox write
   permission; otherwise route the durable save request to the authorized writer
   — do not wait until the end
4. **Al terminar**: si hubo descubrimientos, guardarlos antes de entregar resultado

Si una skill NO tiene acceso a Neurox tools, debe documentar en su output qué
información valdría la pena guardar para que el orquestador lo haga.

## Critical safety and noise rules

- Never save trivial changes (typos, formatting), raw prompts, large logs/dumps,
  secrets/tokens, or personal/private data.
- Never save information that is only a routine validation or delivery event.
- Use `topic_key` for evolving topics; the same key upserts rather than duplicates.
- Default all project-specific observations to the project namespace.
- Promote only a sanitized, concise, reusable pattern to global memory when it is
  genuinely cross-project; never promote repository-specific/private data.
- Never infer identity or an approver from git history; accepted-risk records must
  state rationale and mitigation without inventing an approver.
- Current evidence wins over memory. Cross-namespace recall remains mandatory
  before product tasks, but recall does not make a finding global.

## Namespace convention

```
proyecto específico:  namespace="{project-dir-name}"
preferencias globales: namespace="default" o sin namespace
cross-project search: neurox_recall sin namespace (busca en TODO)
```
