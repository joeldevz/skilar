# Instalacion

Guia paso a paso para instalar Skills en tu maquina. Puedes usar el instalador automatico o hacerlo manualmente.

## Requisitos previos

| Requisito | Obligatorio | Para que |
|-----------|-------------|----------|
| `git` | Si | Clonar el repositorio |
| `python3` | Si | El instalador usa scripts Python internamente |
| [`neurox`](https://github.com/joeldevz/neurox) | Si | Memoria persistente. Se instala con `skynex install` |
| `bun` o `npm` | Solo OpenCode | Instalar dependencias de plugins |
| `gh` | Opcional | Para usar `/pr` (crear pull requests desde terminal) |
| `opencode` | Solo si usas OpenCode | CLI de OpenCode instalado |
| `claude` | Solo si usas Claude Code | CLI de Claude Code instalado |

### Instalar Neurox

[Neurox](https://github.com/joeldevz/neurox) es el sistema de memoria persistente.

```bash
# Via skynex (recomendado)
skynex install    # seleccionar neurox en el instalador
```

O manual como fallback:

```bash
# Requiere Go 1.23+ y CGO habilitado
git clone git@github.com:joeldevz/neurox.git
cd neurox
CGO_ENABLED=1 go build -tags fts5 -o neurox .

# Mover a un directorio en PATH
sudo mv neurox /usr/local/bin/

# Verificar
neurox status
```

Neurox funciona sin servicios externos (solo FTS5). Opcionalmente, con Ollama o una API compatible con OpenAI, habilita busqueda semantica, quality gate y reflexion. Ver [documentacion de Neurox](https://github.com/joeldevz/neurox) para configuracion avanzada.

## Instalacion automatica (recomendado)

```bash
git clone git@github.com:joeldevz/skynex.git
cd skynex
```

### Instalar con el CLI unificado (recomendado)

```bash
# Modo interactivo - selecciona paquetes, targets y versiones
skynex install

# Instalar skills para ambos targets
skynex install --non-interactive --package skills --target both --version skills=latest --trust-setup-scripts

# Instalar skills y neurox
skynex install --non-interactive --package skills --package neurox --target both --trust-setup-scripts
```

`--non-interactive` omite la confirmacion final. Si falta algun valor obligatorio, el comando termina con error antes de instalar.

### Instalar con el script legacy

> **Nota**: `./scripts/setup.sh` es el instalador interno del paquete `skills`. Se recomienda usar `skynex install` en su lugar.

```bash
# Todo lo compatible
./scripts/setup.sh --all

# Solo OpenCode
./scripts/setup.sh --opencode

# Solo Claude Code
./scripts/setup.sh --claude
```

### Que hace el instalador

#### Para OpenCode (`--opencode`)

1. Hace backup de `~/.config/opencode/` si ya existe
2. Copia todo el contenido de `opencode/` a `~/.config/opencode/`
3. Restaura tu API key de Context7 del backup si la tenias configurada
4. Ejecuta `bun install` (o `npm install` como fallback) para dependencias de plugins
5. Resultado: 10 agentes, 8 commands, skills, templates, evals, y MCPs configurados

#### Para Claude Code (`--claude`)

1. Hace backup de `~/.claude/` si ya existe
2. Renderiza los 10 agentes (`orchestrator`, `advisor`, `coder`, `manager`, `tech-planner`, `product-planner`, `verifier`, `test-reviewer`, `security`, `skill-validator`) en `~/.claude/agents/`
3. Convierte los 8 commands de OpenCode en skills de Claude Code en `~/.claude/skills/`
4. Copia skills compartidas (`grill-me`, `prd`, `security`, `write-a-skill`, `diagnose`, `triage`) a `~/.claude/skills/`
5. Copia templates a `~/.claude/templates/`
6. Agrega el bloque del workflow a `~/.claude/CLAUDE.md` (sin borrar contenido existente)
7. Registra Neurox como MCP server en `~/.claude.json`
8. Resultado: 10 agentes, 8 skills de comando, skills core (grill-me, prd, security, write-a-skill, diagnose, triage), overlay de CLAUDE.md, y Neurox MCP listo

## Instalacion manual

### OpenCode manual

```bash
# 1. Clonar el repo
git clone git@github.com:joeldevz/skynex.git
cd skynex

# 2. Copiar config de OpenCode
cp -r opencode/ ~/.config/opencode/

# 3. Instalar dependencias
cd ~/.config/opencode && bun install

# 4. Configurar Context7 (opcional)
# Editar ~/.config/opencode/opencode.json y reemplazar SET_IN_LOCAL_CONFIG con tu API key
```

### Claude Code manual

```bash
# 1. Clonar el repo
git clone git@github.com:joeldevz/skynex.git
cd skynex

# 2. Ejecutar solo el renderer de assets de Claude
python3 scripts/install_claude_assets.py

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

### OpenCode

```bash
# Verificar que los archivos estan en su lugar
ls ~/.config/opencode/opencode.json
ls ~/.config/opencode/commands/
ls ~/.config/opencode/skills/
ls ~/.config/opencode/templates/

# Abrir OpenCode y probar
opencode
# Dentro de OpenCode, probar: /status
```

### Claude Code

```bash
# Verificar agentes
ls ~/.claude/agents/
# Deberia mostrar: orchestrator.md  advisor.md  coder.md  manager.md  tech-planner.md  product-planner.md  verifier.md  test-reviewer.md  security.md  skill-validator.md

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
# Dentro de Claude, probar: /status
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

# Claude Code (solo los assets de Skills, no toda la config de Claude)
rm -rf ~/.claude/agents/planner.md ~/.claude/agents/manager.md ~/.claude/agents/coder.md
rm -rf ~/.claude/skills/plan ~/.claude/skills/execute ~/.claude/skills/review
# ... etc. O restaurar desde el backup:
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
| `bun: command not found` | Instalar bun (`curl -fsSL https://bun.sh/install \| bash`) o usar npm |
| Backup no se creo | El backup solo se crea si el directorio destino ya existia |
