# Integración determinista de Skynex y Neurox

## Objetivo

Incorporar ideas de HumanLayer y del enfoque Software Factory sin convertir el
workflow en una colección de convenciones sostenidas únicamente por prompts.

La separación de responsabilidades es:

```text
Prompt = propone.
Neurox = aporta conocimiento durable y reutilizable.
Skynex = valida, congela, ejecuta y obliga.
```

Skynex conserva toda autoridad sobre estado, riesgo, candidatos, evidencia,
aprobaciones y receipts. Neurox nunca puede cambiar retroactivamente un workflow
ni sustituir evidencia observada.

## Integraciones propuestas

| Integración | Sistema | Implementación | Motivo | Beneficio |
|---|---|---|---|---|
| Tipos de slice | Skynex | Campo enum `tracer`, `capability` o `hardening`, validado por Go y persistido en SQLite. | Cada paso persigue una finalidad distinta. | Planes y verificaciones más precisos. |
| Tracer bullet inicial | Skynex | Las rutas `planned` comienzan con un slice vertical ejecutable cuando sea viable. La excepción requiere un código de motivo registrado. | Detecta temprano errores de arquitectura e integración. | Menos reescrituras tardías. |
| Diseño compacto de alto riesgo | Skynex | Campos estructurados para archivos, tipos y firmas, flujo de llamadas, contratos de prueba y decisiones de baja confianza. | Evita decisiones estructurales silenciosas durante la implementación. | Diseño revisable antes de producir un diff grande. |
| Decisiones duraderas | Neurox | El `infrastructure-engineer` guarda únicamente decisiones verificadas y reutilizables después de los checks. | Son conocimiento del proyecto, no estado de un intento. | Los workflows futuros no tienen que redescubrirlas. |
| Referencias a memoria adoptada | Skynex | SQLite guarda ID, versión, hash, artifact congelado, propósito y basis tree de validación. | Neurox puede evolucionar después de iniciar el workflow. | Ejecuciones reproducibles. |
| Consulta de contexto anterior | Neurox + Skynex | El orchestrator realiza una consulta dirigida; Skynex adopta únicamente una memoria identificada y validada. | La búsqueda semántica no es autoridad determinista. | Reutilización segura de conocimiento. |
| Decisiones de baja confianza | Skynex | El plan las liga a riesgos y slices concretos. | Son incertidumbres del intento actual. | El reviewer concentra su esfuerzo donde aporta más valor. |
| Promoción de decisiones confirmadas | Neurox | Tras la verificación, el orchestrator solicita al `infrastructure-engineer` guardar o actualizar el conocimiento. | Evita persistir hipótesis. | Memoria más limpia y fiable. |
| Corrección de memoria | Neurox | `neurox_update` modifica una memoria identificada en vez de crear duplicados contradictorios. | Las decisiones evolucionan. | Menos ambigüedad en futuras consultas. |
| Dependencias entre decisiones y slices | Skynex | `decision_refs` y `depends_on` forman un grafo validado. | El motor debe conocer el impacto exacto de cada decisión. | Invalidación selectiva. |
| Invalidación selectiva | Skynex | Un algoritmo recorre descendientes desde una decisión o slice invalidado. | Un replan completo repite trabajo correcto. | Menos tiempo, tokens y regresiones. |
| Contratos de prueba previos | Skynex | Cada slice declara comportamientos verificables y, cuando corresponda, evidencia roja previa. | Los tests escritos después pueden limitarse a confirmar la implementación. | Pruebas con poder real de detección. |
| Convenciones de pruebas | Neurox | Se guardan patrones confirmados del proyecto, no resultados temporales. | Las convenciones sobreviven a un workflow. | Nuevos agentes aplican patrones existentes. |
| Evidencia funcional por slice | Skynex | Checks tipados: `unit`, `build`, `http`, `cli`, `migration`, `browser` y `custom`. | Compilar no prueba una ruta completa. | Detección de fallos de integración reales. |
| Verificación según slice | Skynex | `tracer` comprueba conectividad, `capability` comportamiento y `hardening` límites, fallos y seguridad. | Una checklist única deja huecos o genera ruido. | Revisiones más eficientes. |
| Continuación automática | Skynex | Transición atómica y creación idempotente del siguiente job. | Los pasos reversibles no necesitan permiso rutinario. | Menos vigilancia manual. |
| Gates humanos por riesgo | Skynex | `waiting_approval` solo para riesgo alto exacto, ambigüedad material o acciones destructivas/externas. | Cuatro gates obligatorios añaden burocracia. | Seguridad sin bloquear tareas normales. |
| Resumen transaccional | Skynex | Estado, slice, findings, evidencia y siguiente acción se persisten en SQLite. | Pertenecen al intento concreto. | Reanudación exacta después de interrupciones. |
| Contexto entre workflows | Neurox | Arquitectura, compatibilidad y restricciones confirmadas se guardan como conocimiento durable. | No pertenecen a un único intento. | Menos redescubrimiento. |
| ADR y documentación | Neurox + repositorio | Neurox conserva memoria consultable; un ADR es opcional cuando la decisión debe ser visible para humanos. | No toda memoria necesita convertirse en archivo. | Documentación útil sin generar planes innecesarios. |
| Detección de worktree existente | Skynex | Git determina root, worktree y basis tree antes de preparar ejecución. | Evita setups redundantes. | Handoffs más rápidos y fiables. |
| Rutas portables | Skynex | Paths relativos normalizados y un `workspace_id` separado de la ubicación física. | Los paths absolutos no sobreviven a otro host. | Workflows reanudables en daemons distintos. |
| Contexto de entornos remotos | Neurox | Se guardan nombres de entornos, herramientas y nombres de variables, nunca secretos. | Son requisitos reutilizables. | Menos fallos de setup. |
| Protección de actualización | Skynex | Registro de PID, identidad, heartbeat y versión; instalación/actualización se bloquea ante jobs vivos. | Reemplazar el binario puede interrumpir ejecuciones. | Menos estados incompletos. |
| Diagnósticos completos | Skynex | Fase, comando, exit code, artifact de stderr, attempt ID e invocation ID. | Un error genérico obliga a investigar otra vez. | Reparaciones y reintentos dirigidos. |
| Soluciones operativas reutilizables | Neurox | Se guarda causa, condiciones y solución general solo después de confirmarla. | El mismo fallo puede reaparecer. | Diagnóstico futuro más rápido. |
| Notificaciones deduplicadas | Skynex | Clave única por workflow, versión de estado y hash del motivo terminal. | Evita avisos terminales repetidos. | Menos ruido y mayor confianza. |
| Precedencia de instrucciones | Skynex | Orden fijo, paths y hashes para petición, `AGENTS.md`, skills y configuración. | Las fuentes pueden contradecirse. | Ejecuciones explicables y reproducibles. |
| Preferencias operativas | Neurox | Se guardan preferencias estables confirmadas, nunca autoridad de workflow. | Algunas reglas de trabajo deben sobrevivir sesiones. | Los agentes no repiten errores conocidos. |
| Exportación de revisión | Skynex | HTML/JSON derivado del candidate tree, diff, checks, findings y receipt congelados. | Facilita revisar fuera del CLI. | Mejor revisión humana sin alterar el candidato. |
| Métricas por fase | Skynex | Inicio, fin, reintentos, modelo y tokens observados por el runtime. | Hace falta medir rendimiento real. | Comparaciones objetivas de workflows. |
| Aprendizajes de rendimiento | Neurox | Solo se persisten conclusiones verificadas, no métricas crudas. | Las conclusiones pueden reutilizarse. | Mejores decisiones futuras. |

## Garantías deterministas

Las garantías críticas no dependen de obediencia del agente.

| Garantía | Mecanismo obligatorio |
|---|---|
| Tipo de slice válido | Enum y validador Go. |
| Grafo válido | Rechazo de ciclos, IDs duplicados y referencias inexistentes. |
| Invalidación | Recorrido determinista del grafo persistido. |
| Gate humano | Tabla de política y máquina de estados. |
| Avance automático | Transacción SQLite e idempotency key. |
| Notificación única | Restricción `UNIQUE` sobre la clave terminal. |
| Path seguro | Normalización, rechazo de paths absolutos, traversal y escapes del workspace. |
| Worktree correcto | Identidad Git y comparación exacta del basis tree. |
| Job vivo | PID, identidad de proceso y heartbeat; stdout silencioso no implica inactividad. |
| Evidencia válida | Exit code y artifacts producidos por el runtime, no afirmaciones del agente. |
| Receipt reproducible | Hashes de candidate tree, policy, evidencia y reviews. |
| Instrucciones reproducibles | Orden, origen y hash de cada fuente efectiva. |
| Memoria adoptada | ID, versión, SHA-256 y copia congelada como artifact. |

El juicio semántico puede elevar riesgo, proponer findings o recomendar un
replan. Nunca puede reducir un suelo de riesgo determinista, fabricar evidencia,
aprobar un gate o autorizar delivery.

## Adopción determinista de memoria Neurox

Neurox no participa directamente en transiciones críticas. La búsqueda puede
proponer memorias, pero Skynex utiliza únicamente una versión adoptada y
congelada:

```text
1. Neurox devuelve memorias candidatas.
2. El orchestrator propone una memoria concreta.
3. Skynex exige ID y versión exactos.
4. Obtiene el contenido exacto y calcula SHA-256.
5. Guarda una copia inmutable como artifact.
6. Valida el esquema y la contrasta con el repositorio actual.
7. Liga la decisión a slices concretos.
8. Los workers reciben la copia congelada, no acceso directo a Neurox.
9. Un cambio posterior en Neurox requiere una nueva adopción y posible replan.
```

Ejemplo de referencia congelada:

```json
{
  "memory_id": "mem_tls_policy",
  "memory_version": 4,
  "content_sha256": "abc123",
  "artifact_id": "artifact_frozen_memory",
  "validated_at_basis_tree": "80db311",
  "decision_type": "security_constraint",
  "affected_slices": ["tls", "proxy"]
}
```

Ejemplo de decisión estructurada:

```json
{
  "id": "decision_public_transport",
  "kind": "security_constraint",
  "status": "validated",
  "subject": "public_http_transport",
  "rule": "tls_required",
  "scope": ["cmd/shortd", "internal/httpapi"],
  "source": {
    "type": "neurox",
    "id": "mem_tls_policy",
    "version": 4,
    "sha256": "abc123"
  },
  "validated_by": ["README.md", "internal/httpapi/server.go"]
}
```

Los campos de control (`kind`, `status`, `rule`, referencias y scopes) son
estructurados y validados. El texto explicativo nunca controla transiciones.

## Autoridad y flujo

```text
Neurox recall dirigido
        │
        ▼
Orchestrator propone una decisión
        │
        ▼
Skynex valida repositorio, esquema y hash
        │
        ▼
Skynex congela artifact + referencia en SQLite
        │
        ▼
Slices ejecutan con contexto inmutable
        │
        ▼
Checks y reviewer confirman o rechazan
        │
        ▼
Infrastructure-engineer actualiza Neurox si el conocimiento es durable
```

## Lo que nunca debe quedar solo en prompts

- Cuándo avanza el workflow.
- Qué suelo de riesgo aplica.
- Si se requiere aprobación.
- Qué slice queda invalidado.
- Si un check pasó.
- Si un proceso está vivo.
- Qué versión de memoria se utilizó.
- Si puede actualizarse el binario.
- Si puede entregarse un candidato.
- Si una notificación ya fue emitida.
- Qué instrucciones tuvieron precedencia.

Los prompts se reservan para proponer arquitectura, identificar incertidumbre,
interpretar fallos, redactar planes, revisar semánticamente y sugerir qué
conocimiento merece persistirse.

## Elementos que no se integrarán

- Cuatro gates obligatorios para toda feature.
- Aprobación humana después de cada slice.
- Umbrales de riesgo basados únicamente en líneas modificadas.
- `00-status.md` como autoridad paralela.
- PRD, arquitectura y diseño obligatorios en rutas `simple`.
- Acceso directo de workers o reviewers a Neurox.
- Búsqueda semántica de Neurox como condición para una transición crítica.

## Orden recomendado de implementación

### Fase 1: seguridad operativa

1. Protección de instalación y actualización ante jobs vivos.
2. Diagnósticos completos.
3. Notificaciones terminales deduplicadas.
4. Precedencia e inventario de instrucciones.

### Fase 2: slices y evidencia

1. Tipos de slice.
2. Tracer bullet inicial.
3. Checks funcionales tipados.
4. Política de verificación según tipo de slice.

### Fase 3: decisiones y memoria

1. Esquema de decisiones estructuradas.
2. Referencias Neurox versionadas y congeladas.
3. Grafo entre decisiones y slices.
4. Invalidación selectiva.

### Fase 4: portabilidad y revisión

1. Detección de worktree existente.
2. Paths portables y `workspace_id`.
3. Exportación HTML/JSON de revisión.
4. Métricas por fase y promoción selectiva de aprendizajes a Neurox.

Cada fase debe introducir migraciones compatibles, pruebas unitarias de
validadores, pruebas de máquina de estados y un workflow end-to-end que demuestre
recuperación tras interrupción. Ninguna fase debe depender únicamente de que un
modelo siga correctamente un prompt.
