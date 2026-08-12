# Runbook del canario de `skynex-orchestrator`

Este documento explica cómo evaluar rápidamente un cambio en
`skynex-orchestrator` antes de gastar tiempo en la suite pública completa.

El canario es un filtro de desarrollo, no una certificación de release:

1. Ejecuta tres casos públicos fijados contra control y candidato.
2. Produce seis muestras en total, en serie y con parada temprana.
3. Debe terminar normalmente en unos 4–6 minutos.
4. Tiene un límite exterior estricto de 29 minutos y 45 segundos.
5. Sólo una decisión `promote` permite pasar el mismo candidato a la suite
   pública completa.

## Qué evalúa exactamente

Perfil: `skynex-orchestrator-canary-v1`.

Agente evaluado: `skynex-orchestrator` standalone. No evalúa
`workflow-orchestrator` ni Workflow V2. La imagen del canario no contiene el
binario `skynex` ni el plugin `skynex-workflow.ts`.

Los tres casos fijados son:

| Caso | Señal principal |
|---|---|
| `skx_low_direct` | Trabajo LOW directo, sin delegación innecesaria |
| `skx_compaction` | Conservación de contexto, alcance y lineage |
| `skx_no_workflow` | Respeto del límite standalone; no usar Workflow V2 |

Cada caso se ejecuta una vez por brazo: tres muestras de control y tres de
candidato. Un timeout cuenta como rechazo, no como aprobado ni como resultado
omitible.

## Rutas fijas

El launcher y su store privado están separados del sandbox de Workflow V2:

```bash
CANARY_ROOT=/home/clasing/skynex-eval-private/podman-skynex-canary-v1
VARIANT_ROOT=/home/clasing/skynex-eval-private/canary-variants/skynex-orchestrator-v1
CAPSULE_ROOT=/home/clasing/skynex-eval-private/capsules
RESULT_ROOT=/home/clasing/skynex-eval-private/results/skynex-orchestrator-canary-v1
```

No usar `/home/clasing/skynex-eval-private/podman-v1`: ese sandbox pertenece al
perfil Workflow V2 y su imagen tiene una autoridad diferente.

El OAuth dedicado, cuando se autorice una ejecución live, debe estar exactamente
en:

```text
/home/clasing/skynex-eval-private/podman-skynex-canary-v1/secrets/openai-auth.json
```

Nunca imprimir, abrir, versionar ni copiar ese archivo a la cápsula o al directorio
de resultados.

## Flujo rápido

```text
cambio del prompt
      |
      v
variantes control/candidato
      |
      v
self-test offline -> freeze nuevo -> autorización humana
                                      |
                                      v
                                canario 6 muestras
                                      |
                    +-----------------+------------------+
                    |                                    |
                 promote                          reject/inconclusive
                    |                                    |
                    v                                    v
       suite pública completa                  corregir o diagnosticar
       con el mismo digest                     y congelar de nuevo
```

## 1. Preparar control y candidato

El launcher toma las variantes de estas rutas fijas:

```bash
CONTROL="$VARIANT_ROOT/control"
CANDIDATE="$VARIANT_ROOT/candidate"
```

La única diferencia permitida es:

```text
agents/skynex-orchestrator.md
```

El control debe contener el prompt de referencia y el candidato el cambio que se
quiere probar. Todos los demás archivos y sus modos deben ser idénticos. El
launcher rechaza árboles idénticos, symlinks, hardlinks, archivos especiales o
contenido escribible por grupo/otros.

Comprobación previa de lectura:

```bash
cd "$CANARY_ROOT"
diff -qr --exclude=skynex-orchestrator.md "$CONTROL" "$CANDIDATE"

if cmp -s \
  "$CONTROL/agents/skynex-orchestrator.md" \
  "$CANDIDATE/agents/skynex-orchestrator.md"; then
  printf '%s\n' 'ERROR: el tratamiento es idéntico' >&2
  false
fi
```

`diff` no debe imprimir nada y debe devolver `0`; el `if` debe continuar. Si aparece
cualquier otra ruta, no continuar. No copiar el árbol `opencode/` vivo a ciegas:
puede contener dependencias o enlaces que el freeze rechazará. Preparar las
variantes desde un snapshot limpio.

## 2. Ejecutar el self-test offline

Este paso no usa OAuth, red, holdout ni modelos:

```bash
cd "$CANARY_ROOT"
./run.sh self-test
```

Debe terminar con éxito y atestar, entre otras cosas:

- `network=none`;
- `model_calls=0`;
- perfil `skynex-orchestrator-canary-v1`;
- 19 casos públicos disponibles;
- ausencia física del binario `skynex` y del plugin Workflow;
- PID 1 y reaping de procesos correctos.

No ejecutar el canario live si falla el self-test.

## 3. Decidir si hace falta un freeze nuevo

Se puede reutilizar una cápsula únicamente cuando se quiere ejecutar exactamente el
candidato ya congelado en ella.

La cápsula inicial preparada el 12 de agosto de 2026 es:

```text
/home/clasing/skynex-eval-private/capsules/skynex-orchestrator-canary-v1
```

Su candidato congelado tiene este digest:

```text
sha256:ff55f677205a7d0b8d4ef71980ffb3d9f34031096e5ab9dc2beb4f0d39c35c47
```

Si se modifica una sola línea del candidato, hay que crear una cápsula nueva. No se
debe editar una cápsula existente ni reutilizar su resultado.

## 4. Congelar una cápsula nueva

Elegir un nombre e ID nuevos. Los siguientes valores son un ejemplo; sustituir la
fecha y el contador antes de ejecutar:

```bash
CAPSULE_NAME=skynex-orchestrator-canary-YYYYMMDD-01
EXPERIMENT_ID=skynex-orchestrator-canary-YYYYMMDD-01
```

Congelar offline:

```bash
cd "$CANARY_ROOT"
./run.sh freeze-skynex-canary \
  --output-parent "$CAPSULE_ROOT" \
  --output-name "$CAPSULE_NAME" \
  --id "$EXPERIMENT_ID" \
  --seed 1
```

El freeze usa `network=none` y cero modelos. Publica de forma no destructiva: si la
ruta de salida ya existe, falla en lugar de sobrescribirla.

Comprobar el manifest sin mostrar casos ni mensajes:

```bash
CAPSULE="$CAPSULE_ROOT/$CAPSULE_NAME"

jq -e '
  .suite == "skynex-orchestrator" and
  .intent == "development" and
  .public_case_count == 19 and
  (.critical_case_ids | length) == 19 and
  .public_cases_digest == "sha256:f15ce851e7a999e5bd48a611b28c1dd3152effde734a2d22608d42192a243ccb" and
  .runs == 2 and
  .holdout_case_count == 0 and
  (has("holdout") | not) and
  (.control.digest != .candidate.digest)
' "$CAPSULE/manifest.json"

sha256sum "$CAPSULE/manifest.json"
```

El manifest congela los 19 casos públicos, aunque el perfil rápido seleccione sólo
los tres casos del canario. No podar manualmente la cápsula.

## 5. Preparar la credencial dedicada

La ejecución live requiere una credencial OAuth exclusiva, revocable y dedicada a
esta prueba. El launcher exige que el archivo sea regular, no sea symlink, tenga un
solo enlace, pertenezca al usuario del launcher y tenga modo `0600`.

Verificar sólo sus metadatos:

```bash
AUTH_FILE="$CANARY_ROOT/secrets/openai-auth.json"
stat -c 'mode=%a uid=%u links=%h size=%s' "$AUTH_FILE"
```

Valores obligatorios: modo `600`, UID igual a `id -u` —actualmente `1000`—, un
enlace y tamaño entre 1 y 65.536 bytes. No usar una credencial personal compartida
ni continuar si no se puede revocar después de la prueba.

## 6. Obtener autorización humana explícita

Antes de cualquier llamada de modelo, la persona responsable debe autorizar el
gasto de forma separada. La frase acordada es:

```text
Autorizo el canario de 6 muestras
```

El token técnico `YES-I-AUTHORIZE-MODEL-CALLS` del comando no sustituye esa
autorización; sólo impide una ejecución accidental.

## 7. Crear un destino de resultados nuevo

Elegir un identificador no usado:

```bash
RUN_ID=screening-YYYYMMDD-01
RESULT_DIR="$RESULT_ROOT/$RUN_ID"
install -d -m 700 "$RESULT_DIR"
```

Comprobar que está vacío:

```bash
if find "$RESULT_DIR" -mindepth 1 -print -quit | grep -q .; then
  printf '%s\n' 'ERROR: el directorio de resultados no está vacío' >&2
  false
fi
```

Nunca reutilizar ni vaciar un directorio que contenga un resultado anterior. Crear
otro `RUN_ID`.

## 8. Ejecutar el canario live

Para la cápsula recién creada:

```bash
CAPSULE="$CAPSULE_ROOT/$CAPSULE_NAME"
```

Para ejecutar la cápsula ya preparada sin cambios:

```bash
CAPSULE=/home/clasing/skynex-eval-private/capsules/skynex-orchestrator-canary-v1
```

Después de la autorización humana, ejecutar:

```bash
cd "$CANARY_ROOT"

if ./run.sh skynex-canary \
  --capsule "$CAPSULE" \
  --output-dir "$RESULT_DIR" \
  --output-name result \
  --confirm-model-calls YES-I-AUTHORIZE-MODEL-CALLS; then
  CANARY_EXIT=0
else
  CANARY_EXIT=$?
fi

printf 'canary exit=%s\n' "$CANARY_EXIT"
```

No lanzar otro canario en paralelo. El launcher usa sólo:

- cápsula read-only;
- salida vacía read-write;
- OAuth dedicado read-only;
- contenedor rootless endurecido;
- límite exterior de 29m45s y limpieza forzada posterior.

## 9. Leer el resultado

El artefacto esperado es:

```bash
RESULT_FILE="$RESULT_DIR/result.json"
```

Mostrar únicamente el resumen agregado:

```bash
jq '{
  schema_version,
  kind,
  profile,
  suite,
  authority,
  decision,
  reasons,
  exit_code,
  population: {
    planned: .planned_samples,
    completed: .completed_samples,
    skipped: .skipped_samples
  },
  cleanup_complete,
  gates,
  promotion,
  exit_code,
  integrity_digest
}' "$RESULT_FILE"
```

Comprobar el gate mínimo de promoción:

```bash
jq -e '
  .kind == "skynex-orchestrator-canary" and
  .profile == "skynex-orchestrator-canary-v1" and
  .suite == "skynex-orchestrator" and
  .authority == "screening-non-release" and
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
' "$RESULT_FILE"

test \
  "$(jq -r '.promotion.candidate_bundle_digest' "$RESULT_FILE")" = \
  "$(jq -r '.candidate.digest' "$CAPSULE/manifest.json")"

sha256sum "$RESULT_FILE"
```

Este `jq` verifica identidad y gates, pero no recalcula el sello canónico. El
`sha256sum` sirve para custodia desde ese momento; tampoco sustituye la verificación
de `integrity_digest`. No editar, reetiquetar ni reconstruir manualmente el JSON.

Antes de aceptar la promoción en una suite posterior, el launcher de esa suite debe
decodificar el artefacto con el contrato `skynex-orchestrator-canary`, poner
`integrity_digest` a vacío, recalcular el JSON canónico evaluator-owned y exigir que
el SHA-256 coincida. Si el launcher posterior no implementa esa verificación, parar:
no basta con que el selector `jq` devuelva `0`.

Si el proceso falla antes de publicar el artefacto, conservar el exit y el envelope
mostrado en stdout; no fabricar un `result.json`.

### Interpretación

| Decisión / salida | Significado | Acción |
|---|---|---|
| `promote`, exit `0` | Las seis muestras y todos los gates pasaron | Ejecutar la suite pública completa usando exactamente el digest promocionado |
| `reject`, exit `1` | Falló una muestra, un hard check, el límite de contexto o hubo timeout | Corregir el candidato, crear un freeze nuevo y repetir el canario |
| `invalid`, exit `2` | Contrato, evidencia o población inválidos | Corregir el harness/runtime; no juzga la calidad del candidato |
| `inconclusive`, exit `3` | Evidencia o telemetría insuficiente/incompatible | Diagnosticar y repetir con una cápsula nueva si cambió algo |
| `aborted`, exit `4` | Ejecución interrumpida | Comprobar limpieza y volver a ejecutar en un destino nuevo |
| `infra_error`, exit `5` | Fallo de infraestructura o cleanup | Reparar infraestructura; no convertirlo en fallo del candidato |
| `budget_exhausted`, exit `6` | Clasificación genérica de presupuesto fuera del contrato canario | Investigar; el launcher convierte sus propios timeouts en `reject`/exit `1` |

Un fallo de preflight/configuración del launcher devuelve `125`. Las salidas por
señal son `129` (HUP), `130` (INT) y `143` (TERM). Ninguna de ellas juzga al
candidato ni permite promoción.

La promoción exige, como mínimo:

- seis de seis muestras `pass`;
- cero timeout, invalid o error de infraestructura;
- todos los hard checks y controles de seguridad aprobados;
- telemetría completa;
- runtime compatible entre brazos;
- cleanup atestado;
- tokens de entrada del árbol candidato no superiores a 1,15 veces el control.

## 10. Pasar a la suite pública completa

`promote` significa “buen candidato para una evaluación mayor”; no significa que el
cambio sea mejor ni que esté listo para release.

Antes de la suite completa, guardar el digest exacto:

```bash
PROMOTED_DIGEST="$(jq -r '.promotion.candidate_bundle_digest' "$RESULT_FILE")"
printf 'candidate=%s\n' "$PROMOTED_DIGEST"
```

Reglas de promoción:

1. Usar la misma cápsula y el mismo `candidate_bundle_digest`.
2. No modificar el prompt, bundle, modelo, toolchain ni imagen entre ambas pruebas.
3. Si cambia cualquier byte, la promoción queda anulada y se comienza con otro
   freeze y otro canario.
4. Ejecutar después la suite pública completa de 19 casos; el canario no usa
   holdout.
5. Tratar el resultado como `screening-non-release` hasta completar la evaluación
   pública y, si corresponde, un proceso de release separado.

El launcher dedicado descrito aquí sólo ejecuta el canario. En este momento no hay
un comando full-suite admitido dentro de él. Para esa fase hay que usar o construir
un launcher público **standalone** que admita esta misma cápsula, valide el sello
canario y consuma exactamente el digest anterior.

No usar `/home/clasing/skynex-eval-private/podman-v1` para hacerlo: es el sandbox de
Workflow V2, tiene otros pins y otra autoridad. Tampoco pasarle esta cápsula al
launcher A/B histórico sin una readmisión explícita de imagen, evaluador, perfil y
digest.

## Errores comunes

### `Skynex orchestrator treatment is identical`

Control y candidato tienen el mismo prompt. Aplicar el cambio real al candidato y
repetir la comprobación de variantes del paso 1.

### `variants differ outside agents/skynex-orchestrator.md`

Se cambió otra ruta o un modo de archivo. Restaurar igualdad completa fuera del
prompt; el perfil actual no permite otro tipo de tratamiento.

### `freeze output exists` o `output exists`

No borrar ni sobrescribir evidencia. Elegir un nombre nuevo para la cápsula o el
`RUN_ID`.

### `dedicated OAuth file is unsafe or missing`

Corregir la ruta, propietario, modo `0600` o número de enlaces. No relajar el
launcher y no inspeccionar el contenido durante el diagnóstico.

### `wrong standalone profile` o image mismatch

Se está usando un lock, imagen o sandbox incorrectos. Volver a
`$CANARY_ROOT` y ejecutar `./run.sh self-test`. No reutilizar el sandbox Workflow V2.

### Timeout

Un timeout del canario es `reject`. No cambiarlo manualmente a `fail`, no omitir la
muestra y no reanudar el mismo artefacto. Simplificar/corregir el candidato y crear
una ejecución nueva.

Si vence el timeout exterior, el launcher devuelve exit `1` y un envelope con
`error.kind=outer_timeout`, pero puede no existir un `result.json` sellado. Eso
bloquea la promoción; no se debe inventar una decisión `reject` dentro de un
artefacto manual.

## Checklist corta

- [ ] Sólo cambia `agents/skynex-orchestrator.md`.
- [ ] La comprobación del paso 1 confirma que no existe ninguna otra diferencia.
- [ ] `./run.sh self-test` pasa offline con `model_calls=0`.
- [ ] Se creó una cápsula nueva después del último cambio del candidato.
- [ ] El manifest declara 19 casos, `runs=2` y cero holdout.
- [ ] OAuth dedicado: regular, `0600`, propietario correcto, un enlace, revocable.
- [ ] Existe autorización humana: `Autorizo el canario de 6 muestras`.
- [ ] El directorio de resultados es nuevo, privado y vacío.
- [ ] El canario se ejecuta una sola vez y sin concurrencia.
- [ ] El JSON no se edita ni se reetiquetan timeouts.
- [ ] Sólo `promote` habilita la suite completa con el mismo digest.

## Referencias

- Contrato normativo: [`eval/specs/skynex-orchestrator-contract.md`](../eval/specs/skynex-orchestrator-contract.md)
- Contrato general de freeze: [`docs/skynex-eval-freeze.md`](skynex-eval-freeze.md)
- Canario Workflow V2, que es un perfil distinto:
  [`eval/specs/workflow-v2-canary-contract.md`](../eval/specs/workflow-v2-canary-contract.md)
