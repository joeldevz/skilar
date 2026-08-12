# Runbook de la suite pública completa de `skynex-orchestrator`

Este documento describe cómo pasar del canario rápido a la evaluación A/B pública
completa de `skynex-orchestrator`.

## Estado operativo actual

> **No ejecutar todavía la suite completa.** A 12 de agosto de 2026, la cápsula
> completa y la imagen standalone existen, pero el launcher standalone admitido sólo
> ofrece `self-test`, `freeze-skynex-canary` y `skynex-canary`. Todavía no ofrece un
> comando A/B público.

Antes de la primera ejecución completa faltan dos condiciones:

1. Obtener un resultado canario sellado con decisión `promote` para el mismo digest
   candidato.
2. Admitir un launcher standalone `public-ab` que implemente las garantías definidas
   en este documento.

No sustituir ese launcher por un comando Podman manual. Tampoco usar
`/home/clasing/skynex-eval-private/podman-v1/run.sh ab`: pertenece a Workflow V2,
usa otra imagen/autoridad y exige holdout.

## Qué significa “suite completa”

La cápsula standalone congela:

- 19 casos públicos;
- 2 repeticiones por caso;
- dos brazos, control y candidato;
- ejecución serializada dentro de cada bloque;
- cero casos holdout.

La población total es:

```text
19 casos × 2 repeticiones × 2 brazos = 76 muestras
```

Cada artefacto final debe contener 38 muestras: 19 casos por 2 repeticiones.

Esto sigue siendo evidencia `development-non-release`. Cubrir todos los casos
públicos no convierte la prueba en evidencia de release ni sustituye un holdout
independiente.

## Tiempo y consumo esperados

La ejecución pública histórica sugiere alrededor de 1 hora y 25 minutos para 76
muestras si el ritmo es estable. Los casos difíciles y sus timeouts pueden ampliar
considerablemente esa cifra.

Planificar una ventana de 6–8 horas:

- la suma de los límites de completion de los 19 casos es aproximadamente 82
  minutos por una pasada de un brazo;
- cuatro pasadas lógicas —dos brazos por dos repeticiones— permiten hasta unas 5
  horas y 28 minutos de completion;
- preflight, readiness, setup, checks, cleanup y reanudaciones añaden margen.

Las 76 muestras no equivalen a 76 llamadas de proveedor. Delegaciones, revisiones y
reintentos pueden producir más inferencias. Con OAuth de suscripción no existe un
coste USD por request autoritativo; se deben vigilar cuota y tokens observados, no
inventar un precio.

## Flujo de decisión

```text
canario sellado: promote
          |
          v
validar mismo candidate digest
          |
          v
launcher public-ab admitido y self-test offline
          |
          v
76 muestras con checkpoint parcial
          |
          +---- interrupción ----> resume estricto
          |
          v
control + candidate + comparison
          |
          v
verificación offline con report
          |
          +---- pass -----------> candidato público aceptable
          +---- regression -----> no adoptar
          +---- invalid/infra ---> evidencia no válida; diagnosticar
```

## Rutas y autoridad

La cápsula inicialmente preparada es:

```text
/home/clasing/skynex-eval-private/capsules/skynex-orchestrator-canary-v1
```

Sus pins relevantes son:

| Campo | Valor |
|---|---|
| Suite | `skynex-orchestrator` |
| Intent | `development` |
| Casos públicos | `19` |
| Runs | `2` |
| Holdout | `0` |
| Control | `sha256:c5e91c499b465b93d31bf6c1d89d8397b55911c21cff924dabfd5ecb0f0bfc39` |
| Candidato | `sha256:ff55f677205a7d0b8d4ef71980ffb3d9f34031096e5ab9dc2beb4f0d39c35c47` |
| Evaluador | `sha256:153c645b039e6c766a495c41257b8bbd4ad79f060a1c4bd371bb2d7b2bb7f7e3` |
| OpenCode | `1.18.16`, SHA `8e4ac80fe535537e6ee03cee1a8e23af3d0da56db1ae0ce3fffad3ea188a1768` |

Si se utiliza una cápsula posterior, leer estos valores de su manifest en vez de
copiar los anteriores.

## 1. Verificar la promoción del canario

Definir las rutas del canario y de la cápsula:

```bash
CAPSULE=/home/clasing/skynex-eval-private/capsules/skynex-orchestrator-canary-v1
CANARY_RESULT=/home/clasing/skynex-eval-private/results/skynex-orchestrator-canary-v1/screening-001/result.json
```

La comprobación visual mínima es:

```bash
jq -e '
  .kind == "skynex-orchestrator-canary" and
  .profile == "skynex-orchestrator-canary-v1" and
  .authority == "screening-non-release" and
  .suite == "skynex-orchestrator" and
  .decision == "promote" and
  .exit_code == 0 and
  .completed_samples == 6 and
  .skipped_samples == 0 and
  .cleanup_complete == true and
  .holdout_used == false and
  .promotion.full_public_suite_required == true and
  .promotion.digest_reuse_required == true and
  .promotion.release_evidence == false and
  .promotion.candidate_bundle_digest == .candidate_bundle_digest
' "$CANARY_RESULT"

test \
  "$(jq -r '.promotion.candidate_bundle_digest' "$CANARY_RESULT")" = \
  "$(jq -r '.candidate.digest' "$CAPSULE/manifest.json")"
```

Esto no sustituye la verificación del sello. El futuro launcher `public-ab` debe
decodificar el canario con el contrato exacto, vaciar `integrity_digest`, recalcular
el JSON canónico evaluator-owned y exigir la coincidencia antes de montar OAuth o
abrir red. También debe exigir igualdad exacta de `manifest_digest`,
`harness_bundle_digest`, `control_bundle_digest` y `candidate_bundle_digest` entre
el canario y la cápsula.

Si no existe un `promote` íntegro o el digest no coincide, parar. Cualquier cambio
posterior en el candidato obliga a hacer otro freeze y otro canario.

La full suite consume la misma cápsula promocionada. No hacer un segundo freeze
“para el completo”: produciría otra autoridad y rompería el vínculo del canario.

## 2. Contrato del launcher completo que falta admitir

El launcher completo debe vivir en un sandbox standalone separado, por ejemplo:

```text
/home/clasing/skynex-eval-private/podman-skynex-full-v1
```

Esa ruta es el contrato objetivo; no existe todavía y no se debe crear
improvisadamente durante una ejecución live.

### Autoridad obligatoria

El launcher debe:

- tener store, `image.lock`, receipt y estado privados;
- usar una imagen admitida para el perfil
  `skynex-orchestrator-public-ab-v1`;
- fijar el mismo evaluador, OpenCode, harness, control y candidato que la cápsula;
- contener `skynex-eval` y OpenCode, pero no el binario `skynex`, el plugin
  `skynex-workflow.ts` ni Neurox;
- ejecutar como UID no-root con rootfs read-only, capabilities vacías,
  `no-new-privileges`, namespaces privados y `catatonit` pinneado como PID 1;
- montar únicamente cápsula read-only, artefacto canario read-only, resultados
  read-write y OAuth dedicado read-only durante la fase live;
- no montar HOME, XDG, source tree, variantes, sockets ni estado personal del host;
- usar red sólo en preflight/live y `network=none` para self-test y report;
- rechazar `--allow-impure`, plugins, MCP ambient y retención de traces;
- abrir y fijar el artefacto canario dentro de la misma frontera de ejecución —sin
  un precheck por pathname separado— y revalidarlo antes de OAuth/red/modelos;
- exigir `intent=development`, ausencia de la clave `holdout` y
  `holdout_case_count=0` antes de OAuth/red/modelos;
- preservar checkpoints y soportar resume estricto sin repetir coordenadas ya
  aceptadas.

La validación del canario no puede limitarse al hash autocontenido: un escritor con
el mismo UID podría modificar el JSON y recalcularlo. El evaluador debe verificar
semánticamente el artefacto completo contra la cápsula: kind/perfil/autoridad,
manifest y bundles, plan exacto, seis muestras únicas con provenance correcta,
resultados y hard checks, telemetría, cleanup, cero holdout, gates, decisión
`promote` y exit `0`.

La admisión debe producir un receipt 0600 que fije al menos imagen, manifest,
evaluador, OpenCode, launcher, init, perfil y resultados de los tests A/B/resume.
El receipt actual de la imagen standalone declara expresamente
`focused tests, not a full-suite claim`; no sirve como admisión de este flujo sin
una readmisión adicional.

### Interfaz requerida

El launcher admitido debe exponer estas operaciones:

```text
run.sh self-test
run.sh public-ab ...
run.sh public-report ...
```

La interfaz live que este runbook presupone es:

```text
run.sh public-ab \
  --capsule ABS_DIR \
  --canary-result FILE \
  --output-dir ABS_DIR \
  --output-prefix NAME \
  --confirm-model-calls YES-I-AUTHORIZE-MODEL-CALLS \
  [--resume-partial NAME.partial.json]
```

No usar estos comandos hasta que aparezcan en un launcher admitido y en su receipt.

### Comando interior obligatorio

Para revisión del implementador, `public-ab` debe acabar invocando dentro del
contenedor el equivalente exacto de:

```bash
skynex-eval ab \
  --allow-model-calls \
  --manifest /capsule/manifest.json \
  --cases-dir /capsule/bundles/harness/cases \
  --fixtures-dir /capsule/bundles/harness/fixtures \
  --binary /usr/local/bin/opencode \
  --openai-oauth /secrets/openai-auth.json \
  --trace-dir /tmp/skynex/traces \
  --output-prefix /outputs/paired
```

El wrapper/evaluador que ejecuta ese comando debe recibir además el artefacto
canario fijado —por ejemplo montado en `/promotion/canary.json` read-only— y completar
su validación semántica antes de llegar a `skynex-eval ab`. El CLI genérico `ab` no
implementa por sí solo esa promoción.

No debe añadir `--require-holdout`, `--retain-trace`, `--allow-impure`,
`--provider-env` ni `--cost-cap`.

Omitir `--require-holdout` no es por sí solo una barrera suficiente: el launcher
debe rechazar de forma independiente cualquier manifest que contenga holdout.

`public-report` debe ejecutar offline, con cápsula y resultados read-only, el
equivalente de:

```bash
skynex-eval report \
  --input /outputs/paired.comparison.json \
  --control /outputs/paired.control.json \
  --candidate /outputs/paired.candidate.json \
  --manifest /capsule/manifest.json
```

Esta operación vuelve a validar los dos artefactos, el manifest y la comparación;
un `jq` aislado no ofrece esa garantía.

## 3. Ejecutar el self-test del launcher completo

Sólo cuando exista y esté admitido:

```bash
FULL_ROOT=/home/clasing/skynex-eval-private/podman-skynex-full-v1
cd "$FULL_ROOT"
./run.sh self-test
```

Debe terminar con exit `0`, `network=none` y `model_calls=0`, y confirmar:

- perfil standalone público correcto;
- 19 casos y 76 coordenadas planificadas;
- mismos digests de evaluador/OpenCode/control/candidato;
- cero holdout;
- ausencia física de `skynex`, Workflow plugin y Neurox;
- pruebas de partial, resume, no-clobber y cleanup aprobadas.

Si falta cualquiera de estas atestaciones, no ejecutar live.

## 4. Preparar el OAuth dedicado

El launcher completo debe tener su propia ruta fija, por ejemplo:

```text
/home/clasing/skynex-eval-private/podman-skynex-full-v1/secrets/openai-auth.json
```

Debe ser una credencial exclusiva y revocable. Verificar sólo metadatos:

```bash
AUTH_FILE="$FULL_ROOT/secrets/openai-auth.json"
stat -c 'mode=%a uid=%u links=%h size=%s' "$AUTH_FILE"
```

Requisitos: archivo regular no-symlink, modo `0600`, propietario del launcher, un
enlace y tamaño entre 1 y 65.536 bytes. No abrir, imprimir ni copiar su contenido.

## 5. Obtener autorización humana

La persona responsable debe declarar:

```text
Autorizo la suite pública completa de 76 muestras
```

El token `YES-I-AUTHORIZE-MODEL-CALLS` del comando es sólo una segunda barrera
técnica. No sustituye la autorización humana.

## 6. Crear un destino nuevo

Elegir un ID nuevo por intento:

```bash
FULL_RESULT_ROOT=/home/clasing/skynex-eval-private/results/skynex-orchestrator-full-v1
RUN_ID=public-YYYYMMDD-01
RESULT_DIR="$FULL_RESULT_ROOT/$RUN_ID"
install -d -m 700 "$RESULT_DIR"
```

Confirmar que está vacío:

```bash
if find "$RESULT_DIR" -mindepth 1 -print -quit | grep -q .; then
  printf '%s\n' 'ERROR: el directorio de resultados no está vacío' >&2
  false
fi
```

No borrar evidencia ni reutilizar un directorio de otra campaña.

## 7. Ejecutar la suite completa

Este comando sólo será válido después de admitir la interfaz del paso 2:

```bash
cd "$FULL_ROOT"

if ./run.sh public-ab \
  --capsule "$CAPSULE" \
  --canary-result "$CANARY_RESULT" \
  --output-dir "$RESULT_DIR" \
  --output-prefix paired \
  --confirm-model-calls YES-I-AUTHORIZE-MODEL-CALLS; then
  PUBLIC_EXIT=0
else
  PUBLIC_EXIT=$?
fi

printf 'public A/B exit=%s\n' "$PUBLIC_EXIT"
```

Un `fail` conductual ordinario no detiene la campaña; queda registrado y contribuye
a los gates finales. Los estados invalid, infra, aborted o budget pueden detenerla
y dejar un partial.

## 8. Monitorizar sin exponer contenido

Mientras la campaña está en curso debe existir:

```text
paired.partial.json
```

Leer sólo agregados:

```bash
PARTIAL="$RESULT_DIR/paired.partial.json"

jq '{
  kind,
  experiment_id,
  intent,
  authority,
  exit_code,
  completed: {
    control: (.control.samples | length),
    candidate: (.candidate.samples | length)
  }
}' "$PARTIAL"
```

No imprimir `.control.samples`, `.candidate.samples`, mensajes, traces, IDs internos
ni telemetría por sesión. El checkpoint se actualiza atómicamente después de cada
muestra aceptada.

## 9. Reanudar una campaña interrumpida

Antes de reanudar, demostrar que no existe launcher, contenedor ni evaluador vivo de
esa campaña. No iniciar dos resumes en paralelo.

El partial debe permanecer intacto en el mismo directorio y el prefijo debe seguir
siendo `paired`:

```bash
cd "$FULL_ROOT"

./run.sh public-ab \
  --capsule "$CAPSULE" \
  --canary-result "$CANARY_RESULT" \
  --output-dir "$RESULT_DIR" \
  --output-prefix paired \
  --resume-partial paired.partial.json \
  --confirm-model-calls YES-I-AUTHORIZE-MODEL-CALLS
```

El resume debe exigir exactamente el mismo manifest, plan, población, bundles,
toolchain, canario y prefix. Las coordenadas ya aceptadas no se vuelven a ejecutar.
Cada resume requiere una nueva autorización humana y una credencial dedicada aún
vigente; nunca sustituir la credencial mientras existe un proceso activo.

Resume no significa “reintentar fallos”: una coordenada ya guardada se salta aunque
su status sea `fail` o `budget_exhausted`. El `exit_code` transitorio del partial por
sí solo tampoco es la decisión final; mandan la población preservada y, si se llega
a completar, la comparación final.

No editar el partial, eliminar muestras, cambiar un timeout por `fail` ni crear una
copia sin su sello. Si el partial ya contiene `invalid`, `infra_error` o
`budget_exhausted`, terminar las muestras restantes puede aportar diagnóstico, pero
no puede convertir esa campaña en evidencia `pass`. Para una decisión válida se
necesita corregir la causa y crear una campaña congelada nueva.

Si queda un `.resume.lock`, no borrarlo hasta demostrar que ningún proceso o
contenedor de esa campaña está vivo. La eliminación manual sin esa prueba permite
dos escritores sobre el mismo checkpoint.

Una caída en la estrecha ventana posterior a una respuesta del proveedor y anterior
al fsync del checkpoint puede repetir esa única coordenada al reanudar. El operador
debe contar esa posibilidad al revisar consumo y cuota.

## 10. Comprobar la finalización

Una publicación completa produce:

```text
paired.control.json
paired.candidate.json
paired.comparison.json
```

El partial debe desaparecer sólo después de publicar correctamente los tres
artefactos.

Un exit no-cero no implica automáticamente que haya que reanudar. Primero comprobar
los archivos: si existen los tres finales y ya no existe el partial, la campaña
terminó y su decisión puede ser `regression`, `inconclusive` o `infra_error`. Sólo se
reanuda cuando existe un partial íntegro y no existe otra ejecución viva.

Comprobar población y nombres sin mostrar muestras:

```bash
CONTROL_RESULT="$RESULT_DIR/paired.control.json"
CANDIDATE_RESULT="$RESULT_DIR/paired.candidate.json"
COMPARISON_RESULT="$RESULT_DIR/paired.comparison.json"

jq -e '
  .kind == "skynex-eval-baseline" and
  .suite == "skynex-orchestrator" and
  .label == "control" and
  (.samples | length) == 38
' "$CONTROL_RESULT"

jq -e '
  .kind == "skynex-eval-baseline" and
  .suite == "skynex-orchestrator" and
  .label == "candidate" and
  (.samples | length) == 38
' "$CANDIDATE_RESULT"

test \
  "$(jq -r '.fingerprint.agent_bundle_digest' "$CANDIDATE_RESULT")" = \
  "$(jq -r '.candidate.digest' "$CAPSULE/manifest.json")"

test ! -e "$PARTIAL"
```

## 11. Verificar y leer la comparación

Primero ejecutar la verificación offline del launcher admitido:

```bash
cd "$FULL_ROOT"

if ./run.sh public-report \
  --capsule "$CAPSULE" \
  --output-dir "$RESULT_DIR" \
  --output-prefix paired; then
  REPORT_EXIT=0
else
  REPORT_EXIT=$?
fi

printf 'verified report exit=%s\n' "$REPORT_EXIT"
```

Después mostrar el resumen:

```bash
jq '{
  kind,
  intent,
  authority,
  manifest_digest,
  decision: .report.decision,
  reliability: .report.reliability,
  metrics: [
    .report.metrics[] |
    {
      name,
      unit,
      scope,
      control_median: .control.median,
      candidate_median: .candidate.median,
      paired: .paired
    }
  ],
  gates: [
    .report.decision.results[] |
    {
      name,
      status,
      reason,
      paired
    }
  ]
}' "$COMPARISON_RESULT"
```

### Decisión final

| Estado / exit | Significado | Acción |
|---|---|---|
| `pass`, `0` | Todos los gates públicos pasan | Candidato aceptable para la siguiente fase de desarrollo; no es release |
| `regression`, `1` | Algún hard gate o umbral empeora | No adoptar; revisar por caso y cambiar el candidato |
| `invalid`, `2` | Evidencia, compatibilidad o contrato inválidos | Reparar harness/runtime y repetir; no juzga al candidato |
| `inconclusive`, `3` | Evidencia estadística insuficiente | No adoptar como mejora demostrada; aumentar runs exige otro freeze/canario |
| `aborted`, `4` | Interrupción | Usar sólo el resume estricto si el partial es válido |
| `infra_error`, `5` | Fallo de infraestructura o muestra no evaluable | Reparar infraestructura; no convertirlo en fallo conductual |
| `budget_exhausted`, `6` | Una muestra agotó su presupuesto | El partial queda preservado; no reetiquetar como `fail` |

Un launcher/preflight que devuelve `125` no ha evaluado al candidato.

## Gates congelados de esta cápsula

| Gate | Umbral |
|---|---:|
| Pass rate de casos críticos | `1.0` |
| Regresiones pass→fail | `0` |
| Violaciones de scope | `0` |
| Falsos éxitos | `0` |
| Ratio parent peak input | `≤ 0.70` |
| Ratio tree input | `≤ 1.00` |
| Ratio wall time | `≤ 1.10` |
| Ratio retry rate | `≤ 1.00` |
| Confianza | `0.95` |
| Pares mínimos | `2` |

Los 19 casos son críticos. Por tanto, un solo `fail` candidato puede romper el gate
de pass rate crítico. Con billing por suscripción, `tree_cost_usd` no es una métrica
autoritaria y debe aparecer como no aplicable en vez de inventar un coste.

## Qué hacer después

### Si pasa

Guardar juntos y sin modificar:

- cápsula completa;
- canario `promote`;
- `image.lock` y receipt del launcher completo;
- artefactos control, candidate y comparison;
- salida verificada de `public-report`.

El resultado sólo permite decir que el candidato superó la evaluación pública de
desarrollo. Un proceso de release requiere un experimento separado, más repeticiones,
holdout y una frontera de credencial/proveedor adecuada.

### Si hay regression

Usar los gates y checks públicos para mejorar el prompt. Después:

1. actualizar únicamente el candidato;
2. crear otra cápsula;
3. ejecutar otro canario;
4. ejecutar la suite completa sólo si vuelve a dar `promote`.

No mezclar muestras de dos digests candidatos.

### Si es invalid, inconclusive o infra

No interpretarlo como “Skynex es malo”. La evidencia no soporta una decisión limpia.
Corregir el evaluador, runtime, telemetría o población; cualquier cambio pinneado
obliga a readmitir el launcher y normalmente a congelar de nuevo.

## Checklist corta

- [ ] Existe un canario sellado `promote` de seis muestras.
- [ ] Su candidate digest coincide exactamente con el manifest.
- [ ] Existe un launcher standalone `public-ab` admitido; no es `podman-v1`.
- [ ] El self-test offline pasa con `model_calls=0` y plan de 76 muestras.
- [ ] La cápsula declara 19 casos, `runs=2` y cero holdout.
- [ ] OAuth dedicado: regular, `0600`, propietario correcto, un enlace, revocable.
- [ ] Existe autorización humana para 76 muestras.
- [ ] El directorio de resultados es nuevo y vacío.
- [ ] No existe otra ejecución o resume concurrente.
- [ ] Un partial se reanuda sin editarlo y con el mismo prefix.
- [ ] La finalización produce 38 muestras por brazo y elimina el partial.
- [ ] `public-report` verifica offline los tres artefactos y el manifest.
- [ ] Sólo `pass` permite avanzar; incluso entonces sigue siendo non-release.

## Referencias

- Canario previo: [`docs/skynex-orchestrator-canary-runbook.md`](skynex-orchestrator-canary-runbook.md)
- Contrato normativo: [`eval/specs/skynex-orchestrator-contract.md`](../eval/specs/skynex-orchestrator-contract.md)
- Freeze y resume A/B: [`docs/skynex-eval-freeze.md`](skynex-eval-freeze.md)
- Modelo de seguridad: [`docs/security-model.md`](security-model.md)
