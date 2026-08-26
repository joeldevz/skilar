# Instalacion

Guia para instalar el paquete `skills`. El asistente sin flags instala OpenCode; `--target claude` instala también los assets de Claude Code.
Las versiones `latest` usan el bundle embebido del binario firmado. `workspace` solo
permite un checkout local. Versiones o refs remotos arbitrarios fallan cerrados hasta
que existan manifiestos de origen firmados.

## Bootstrap y confianza

No uses `curl ... | bash` ni `irm ... | iex`: HTTPS protege el transporte, pero
el contenido remoto todavía se ejecutaría sin revisión. Descarga `install.sh` o
`install.ps1` y su firma desde un release etiquetado, verifica la firma con la
clave pública de `release/trust/`, y ejecuta el archivo local. También puedes
usar Homebrew (confianza delegada explícita) o el binario firmado del release.

## Requisitos previos

| Requisito | Obligatorio | Para que |
|-----------|-------------|----------|
| `git` | Solo workspace local | Leer el commit de un checkout local |
| `python3` | No requerido | El instalador y el CLI son Go |
| `bun`, `pnpm` o `npm` | Solo OpenCode | Instalar dependencias de plugins; se prueban en ese orden |
| `gh` | Opcional | Para usar `/pr` (crear pull requests desde terminal) |
| `opencode` | Solo si usas OpenCode | CLI de OpenCode instalado |
| `claude` | Solo si usas Claude Code | CLI de Claude Code instalado |

## Instalacion automatica (recomendado)

Usa el binario firmado de un release etiquetado o Homebrew. Para desarrollo, ejecuta el CLI desde un checkout local ya obtenido por un canal que tu organización haya verificado.

### Instalar con el CLI unificado (recomendado)

```bash
# Modo interactivo - selecciona paquetes, targets y versiones
skynex install

# Instalar skills para ambos targets
skynex install --non-interactive --package skills --target both --version skills=latest --trust-setup-scripts

# El asistente sin flags instala skills para OpenCode
skynex install --non-interactive --package skills --target opencode
```

`--non-interactive` requiere todos los valores por flags. En modo interactivo, `--yes` conserva la seleccion del asistente y omite solo la confirmacion final.

### Instalar con el script legacy

> **Nota**: `./scripts/setup.sh` es un shim de compatibilidad que ejecuta `skynex install`.

```bash
./scripts/setup.sh [opciones de skynex install]
```

Por defecto, el instalador instala dependencias con `--ignore-scripts`: los scripts de ciclo de vida
de npm/pnpm/Bun no se ejecutan. `--trust-setup-scripts` es un opt-in explícito que muestra una
advertencia y permite ejecutar esos scripts; úsalo solo después de revisar las dependencias.
Los directorios privados creados por el script usan modo `0700` y sus configuraciones/backups
modo `0600`.

### Que hace el instalador

#### Para OpenCode

1. Hace backup de `~/.config/opencode/` si ya existe
2. Copia todo el contenido de `opencode/` a `~/.config/opencode/`
3. Restaura tu API key de Context7 del backup si la tenias configurada
4. Después de confirmar el commit gestionado, instala dependencias de plugins con el primer gestor disponible, en este orden: `bun`, `pnpm`, `npm`, siempre con `--ignore-scripts`
5. Resultado: 12 agentes, 8 commands, skills, templates, evals, y MCPs configurados

La configuración gestionada y el estado se conservan aunque falle la instalación de dependencias
(la instalación queda parcial). Corrige el gestor o la red y reintenta con `skynex deps`; no es
necesario volver a instalar la configuración.

#### Para Claude Code

1. Hace backup de `~/.claude/` si ya existe
2. Renderiza los agentes (`orchestrator`, `skynex-orchestrator`, `advisor`, `coder`, `infrastructure-engineer`, `tech-planner`, `verifier`, `test-reviewer`, `security`, `skill-validator`, `pr-reviewer`) en `~/.claude/agents/`
3. Convierte los 8 commands de OpenCode en skills de Claude Code en `~/.claude/skills/`
4. Copia skills compartidas (`grill-me`, `prd`, `security`, `write-a-skill`, `diagnose`, `triage`) a `~/.claude/skills/`
5. Copia templates a `~/.claude/templates/`
6. Agrega el bloque del workflow a `~/.claude/CLAUDE.md` (sin borrar contenido existente)
7. Registra Neurox como MCP server en `~/.claude.json`
8. Resultado: 12 agentes, 8 skills de comando, skills core (grill-me, prd, security, write-a-skill, diagnose, triage), overlay de CLAUDE.md, y Neurox MCP listo

## Instalacion manual

### OpenCode manual

```bash
# 1. Desde un checkout local verificado

# 2. Copiar config de OpenCode
cp -r opencode/ ~/.config/opencode/

# 3. Instalar dependencias de la instalación gestionada
skynex deps

# 4. Configurar Context7 (opcional)
# Editar ~/.config/opencode/opencode.json y reemplazar SET_IN_LOCAL_CONFIG con tu API key
```

### Claude Code manual

```bash
# 1. Desde un checkout local verificado

# 2. Ejecutar el instalador Go
skynex install

# 3. Agregar overlay a CLAUDE.md
# Copiar el contenido de claude-code/CLAUDE.md y pegarlo en ~/.claude/CLAUDE.md

# 4. Registrar Neurox MCP en ~/.claude.json
# Agregar este bloque dentro de "mcpServers":
```

```json
{
  "mcpServers": {
    "neurox": {
      "type": "stdio",
      "command": "neurox",
      "args": ["mcp"]
    }
  }
}
```

## Verificacion post-instalacion

Los instaladores de releases verifican el SHA-256 exacto antes de abrir un archivo. También
validan todos los miembros del archivo, rechazan traversal/enlaces/archivos especiales y
reemplazan el binario mediante una raíz temporal privada y reemplazo atómico. Si PowerShell
está disponible, las pruebas de sintaxis pueden ejecutarse localmente; en otros entornos se
ejecuta al menos `bash -n` para los scripts POSIX.

### OpenCode

```bash
# Verificar que los archivos estan en su lugar
ls ~/.config/opencode/opencode.json
ls ~/.config/opencode/commands/
ls ~/.config/opencode/skills/
ls ~/.config/opencode/templates/

# Abrir OpenCode y probar
opencode
# Dentro de OpenCode, probar un command disponible en `opencode/commands/`.
```

### Claude Code

```bash
# Verificar agentes
ls ~/.claude/agents/
# Deberia mostrar también: orchestrator.md  skynex-orchestrator.md  advisor.md  coder.md  infrastructure-engineer.md  tech-planner.md  verifier.md  test-reviewer.md  security.md  skill-validator.md  pr-reviewer.md

# Verificar skills
ls ~/.claude/skills/
# Deberia mostrar: commit/  docs/  linear/  pr/  review-pr/  rollback/  setup/  skills-scan/  grill-me/  prd/  security/  write-a-skill/  diagnose/  triage/

# Verificar templates
ls ~/.claude/templates/

# Verificar overlay en CLAUDE.md
grep "skills-repo" ~/.claude/CLAUDE.md

# Verificar Neurox MCP
grep "neurox" ~/.claude.json

# Abrir Claude Code y probar
claude
# Dentro de Claude, probar una skill instalada.
```

## Configuracion opcional

### Context7 (solo OpenCode)

Context7 provee documentacion en vivo de librerias externas. Esta habilitado por defecto pero requiere API key.

Editar `~/.config/opencode/opencode.json`:

```json
"context7": {
  "type": "remote",
  "url": "https://mcp.context7.com/mcp",
  "headers": {
    "CONTEXT7_API_KEY": "TU_API_KEY_REAL"
  },
  "enabled": true
}
```

Sin la key, Context7 simplemente no funciona pero no rompe el flujo.

### CONVENTIONS.md (recomendado para cada proyecto)

Copiar el template de convenciones a la raiz de cada proyecto donde uses Skills:

```bash
cp ~/.config/opencode/templates/CONVENTIONS.md ./CONVENTIONS.md
# O desde el repo clonado:
cp skills/opencode/templates/CONVENTIONS.md ./CONVENTIONS.md
```

Editar el archivo para ajustarlo al stack real del proyecto. Esto hace que los agentes sean mucho mas consistentes.

## Actualizacion

```bash
skynex update
```

Esto actualiza todos los paquetes instalados a la última versión. Para actualizar un paquete específico: `skynex update skills`

El instalador hace backup automatico antes de sobreescribir, asi que es seguro correr multiples veces.

### Confianza del manifiesto de Skills

El manifiesto de propiedad de `skills` autentica su propia lista de archivos y
los metadatos de origen mediante `TreeSHA256`; no exige que el manifiesto
anterior coincida con el bundle actual. Esto permite actualizar desde una
version anterior sin tratarla como corrupcion. La frontera de amenaza es el
binario y el bundle que se esta ejecutando: si el directorio de estado y el
binario estan bajo control del atacante, el manifiesto no es una raiz de
confianza. Los archivos retirados o modificados nunca se eliminan sin una
decision explicita enlazada a la observacion inspeccionada.

## Diagnostico

```bash
# Ver estado completo del entorno
skynex status

# Diagnostico detallado de dependencias
skynex doctor
```

## Desinstalacion

```bash
# OpenCode
rm -rf ~/.config/opencode/

# Claude Code: restaura el backup creado por skynex (evita borrar assets ajenos).
ls -dt ~/.claude.backup.* | head -1
# Después de revisar el contenido, restaura el backup correspondiente:
# cp -r ~/.claude.backup.XXXXXXXX-XXXXXX/ ~/.claude/
```

## Troubleshooting

| Problema | Solucion |
|----------|----------|
| `neurox: command not found` | Instalar neurox y asegurar que esta en PATH |
| `Error: opencode/ directory not found` | Ejecutar desde la raiz del repo clonado |
| Context7 no funciona | Verificar API key en `opencode.json`. Sin key, se ignora silenciosamente |
| Skills no aparecen en Claude | Verificar que `~/.claude/skills/` tiene los directorios. Reiniciar Claude Code |
| Agentes no aparecen en Claude | Verificar que `~/.claude/agents/` tiene los `.md`. Reiniciar Claude Code |
| `bun: command not found` | El instalador prueba `pnpm` y después `npm`; instala uno de ellos desde un release/package manager verificado o ejecuta `skynex deps` tras instalarlo |
| Backup no se creo | El backup solo se crea si el directorio destino ya existia |
# Transaction safety

Install transactions retain private recovery snapshots under the configured
state directory. Only the OpenCode path `EBWebView/Default/Code Cache` is
excluded from snapshots; `node_modules` and all other files remain subject to
the snapshot protection and limits. If dependency installation fails after the
managed commit, the installation is partial: managed configuration and state
are preserved. Fix the problem and retry with `skynex deps`; do not rerun the
full install just for dependencies. At most five snapshots are retained; when
that limit is reached, recover or remove one explicitly before starting another
transaction. Deprecated managed entries are reported and preserved. Explicit
file cleanup, where supported, renames a regular file to a recovery backup and
never removes directories recursively.
