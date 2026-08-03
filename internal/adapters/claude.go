package adapters

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/joeldevz/skynex/internal/models"
	"github.com/joeldevz/skynex/internal/safefs"
)

const neuroxSkillBlock = `
## Neurox Memory (obligatorio)

Esta skill DEBE usar Neurox para memoria persistente:
- **Al iniciar**: ` + "`" + `neurox_recall(query="{tema relevante}")` + "`" + ` — buscar contexto previo
- **Cross-namespace**: ` + "`" + `neurox_recall(query="{tema}")` + "`" + ` sin namespace — inteligencia de otros proyectos
- **Al descubrir algo**: ` + "`" + `neurox_save(...)` + "`" + ` inmediatamente — no esperar al final
- Si no tienes acceso a Neurox tools, documenta en tu output qué información guardar.
`

var skillMap = map[string][]string{
	"orchestrator":    {"security"},
	"advisor":         {},
	"product-planner": {"prd"},
	"tech-planner":    {"prd", "nestjs-patterns", "typescript-advanced-types"},
	"coder":           {"nestjs-patterns", "typescript-advanced-types"},
	"verifier":        {},
	"test-reviewer":   {},
	"security":        {"security"},
	"skill-validator": {},
	"manager":         {},
}

// InstallClaude installs all Claude Code assets from srcDir.
// srcDir is the checkout/workspace root (contains opencode/, claude-code/, etc.)
// req contains cleanup preference from CLI flags.
func InstallClaude(srcDir string, req *models.InstallRequest) error {
	return InstallClaudeWithReporter(srcDir, req, discardReporter())
}

func InstallClaudeWithReporter(srcDir string, req *models.InstallRequest, reporter Reporter) error {
	target := claudeDir()
	if err := validateInstallDestinationTree(target); err != nil {
		return fmt.Errorf("validate Claude install destination: %w", err)
	}
	targetRoot, err := safefs.OpenOrCreate(target, 0o700)
	if err != nil {
		return fmt.Errorf("create claude dir: %w", err)
	}
	if err := safefs.ChmodRoot(targetRoot, 0o700); err != nil {
		_ = targetRoot.Close()
		return fmt.Errorf("tighten claude dir: %w", err)
	}
	_ = targetRoot.Close()

	reporter.Detail("Rendering agents...")
	if err := renderAgents(srcDir, target); err != nil {
		return fmt.Errorf("render agents: %w", err)
	}

	reporter.Detail("Copying shared skills...")
	if err := copyDir(
		filepath.Join(srcDir, "opencode", "skills"),
		filepath.Join(target, "skills"),
	); err != nil {
		return fmt.Errorf("copy skills: %w", err)
	}

	reporter.Detail("Copying templates...")
	templatesSrc := filepath.Join(srcDir, "opencode", "templates")
	if _, err := os.Stat(templatesSrc); err == nil {
		if err := copyDir(templatesSrc, filepath.Join(target, "templates")); err != nil {
			return fmt.Errorf("copy templates: %w", err)
		}
	}

	reporter.Detail("Rendering command skills...")
	if err := renderCommandSkills(srcDir, target); err != nil {
		return fmt.Errorf("render command skills: %w", err)
	}

	reporter.Detail("Updating CLAUDE.md...")
	if err := appendMarkedBlock(
		filepath.Join(target, "CLAUDE.md"),
		filepath.Join(srcDir, "claude-code", "CLAUDE.md"),
		"skills-repo",
	); err != nil {
		return fmt.Errorf("update CLAUDE.md: %w", err)
	}

	reporter.Detail("Configuring Neurox MCP...")
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home directory: %w", err)
	}
	if err := validateInstallDestination(filepath.Join(home, ".claude.json")); err != nil {
		return fmt.Errorf("validate Claude config destination: %w", err)
	}
	if err := configureClaudeNeuroxMCPWithReporter(reporter); err != nil {
		return fmt.Errorf("configure neurox mcp: %w", err)
	}

	// Deprecated entries are informational only. Never delete them implicitly.
	deprecated, err := FindDeprecatedFiles()
	if err != nil {
		return fmt.Errorf("discover deprecated Claude files: %w", err)
	}
	if len(deprecated) > 0 && deprecated["claude"] != nil {
		reporter.Detail("Deprecated files detected and preserved (no recursive cleanup is performed):")
		notifyDeprecatedFiles(reporter, "claude", deprecated["claude"])
	}

	reporter.Detail("Claude Code assets installed at %s", target)
	return nil
}

// renderAgents reads opencode.json and generates ~/.claude/agents/{name}.md
func renderAgents(srcDir, target string) error {
	configPath := filepath.Join(srcDir, "opencode", "opencode.json")
	data, err := safefs.ReadFileAbsoluteVerified(configPath, 4<<20)
	if err != nil {
		return err
	}

	var config map[string]json.RawMessage
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}

	var agents map[string]map[string]json.RawMessage
	if err := json.Unmarshal(config["agent"], &agents); err != nil {
		return err
	}
	for name := range agents {
		if err := validateAgentName(name); err != nil {
			return fmt.Errorf("reject agent %q: %w", name, err)
		}
	}

	agentsDir := filepath.Join(target, "agents")
	if err := validateInstallDestinationTree(agentsDir); err != nil {
		return err
	}
	agentsRoot, err := safefs.OpenOrCreate(agentsDir, 0o700)
	if err != nil {
		return err
	}
	if err := safefs.ChmodRoot(agentsRoot, 0o700); err != nil {
		_ = agentsRoot.Close()
		return err
	}
	_ = agentsRoot.Close()

	for name, agent := range agents {
		var prompt, description string
		if err := json.Unmarshal(agent["prompt"], &prompt); err != nil {
			continue
		}
		if err := json.Unmarshal(agent["description"], &description); err != nil {
			description = name
		}

		skills := skillMap[name]
		var sb strings.Builder
		sb.WriteString("---\n")
		sb.WriteString(fmt.Sprintf("name: %s\n", yamlScalar(name)))
		sb.WriteString(fmt.Sprintf("description: %s\n", yamlScalar(description)))
		sb.WriteString("model: inherit\n")
		sb.WriteString("memory: local\n")
		if len(skills) > 0 {
			sb.WriteString("skills:\n")
			for _, s := range skills {
				sb.WriteString(fmt.Sprintf("  - %s\n", s))
			}
		}
		sb.WriteString("---\n\n")
		sb.WriteString(strings.TrimSpace(prompt))
		sb.WriteString("\n")

		outPath := filepath.Join(agentsDir, name+".md")
		rel, err := filepath.Rel(agentsDir, outPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return fmt.Errorf("reject agent %q: output path escapes agents directory", name)
		}
		if err := writeFile(outPath, sb.String()); err != nil {
			return fmt.Errorf("write agent %s: %w", name, err)
		}
	}
	return nil
}

func validateAgentName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("name must be a non-empty managed segment")
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\\`) || filepath.Base(name) != name {
		return fmt.Errorf("name must be a single relative managed segment")
	}
	for _, char := range name {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return fmt.Errorf("name must use lowercase letters, digits, and hyphens")
		}
	}
	return nil
}

// renderCommandSkills transforms opencode/commands/*.md → ~/.claude/skills/{name}/SKILL.md
func renderCommandSkills(srcDir, target string) error {
	commandsDir := filepath.Join(srcDir, "opencode", "commands")
	commandRoot := filepath.Join(target, "skills")

	commandsRoot, err := safefs.Open(commandsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer commandsRoot.Close()
	entries, err := fs.ReadDir(commandsRoot.FS(), ".")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// Sort for deterministic output
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".md")
		if err := validateCommandBasename(name); err != nil {
			return fmt.Errorf("reject command %q: %w", entry.Name(), err)
		}
		content, err := safefs.ReadFileAbsoluteVerified(filepath.Join(commandsDir, entry.Name()), 4<<20)
		if err != nil {
			return err
		}

		metadata, body := parseFrontmatter(string(content))
		description := metadata["description"]
		if description == "" {
			description = "Run /" + name
		}
		description = strings.ReplaceAll(description, "Engram persistent memory", "Neurox persistent memory")
		agentName := metadata["agent"]
		if agentName == "" {
			agentName = "manager"
		}

		transformed := normalizeCommandBody(body)

		var sb strings.Builder
		sb.WriteString("---\n")
		sb.WriteString(fmt.Sprintf("name: %s\n", yamlScalar(name)))
		sb.WriteString(fmt.Sprintf("description: %s\n", yamlScalar(description)))
		sb.WriteString("disable-model-invocation: true\n")
		sb.WriteString("---\n\n")
		sb.WriteString(commandIntro(name, agentName))
		sb.WriteString("\n")
		sb.WriteString(transformed)
		sb.WriteString(neuroxSkillBlock)

		outPath := filepath.Join(commandRoot, name, "SKILL.md")
		if err := writeFile(outPath, sb.String()); err != nil {
			return fmt.Errorf("write skill %s: %w", name, err)
		}
	}
	return nil
}

func validateCommandBasename(name string) error {
	return validateAgentName(name)
}

// yamlScalar emits a JSON double-quoted scalar, which is also a strict YAML scalar.
// This keeps repository-controlled metadata on one syntactic YAML value.
func yamlScalar(value string) string {
	return strconv.Quote(value)
}

func parseFrontmatter(text string) (map[string]string, string) {
	meta := make(map[string]string)
	if !strings.HasPrefix(text, "---\n") {
		return meta, text
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return meta, text
	}
	raw := text[4 : end+4]
	body := text[end+9:]
	for _, line := range strings.Split(raw, "\n") {
		if idx := strings.Index(line, ":"); idx >= 0 {
			k := strings.TrimSpace(line[:idx])
			v := strings.TrimSpace(line[idx+1:])
			meta[k] = v
		}
	}
	return meta, strings.TrimLeft(body, "\n")
}

func normalizeCommandBody(body string) string {
	replacements := [][2]string{
		{`"{argument}"`, `"$ARGUMENTS"`},
		{`{argument}`, `$ARGUMENTS`},
		{`{workdir}`, `the current working directory`},
		{`{project}`, `the current project`},
		{`Engram memory (` + "`" + `mem_search` + "`" + `)`, `Neurox memory (` + "`" + `neurox_recall` + "`" + `)`},
		{`Engram persistent memory`, `Neurox persistent memory`},
		{`Engram`, `Neurox`},
		{"`mem_search`", "`neurox_recall`"},
		{`~/.config/opencode/templates/`, `~/.claude/templates/`},
		{`Use ` + "`" + `topic_key` + "`" + ` for evolving topics so they update instead of duplicating`, `Prefer updating an existing memory note when the topic already exists`},
	}
	result := body
	for _, r := range replacements {
		result = strings.ReplaceAll(result, r[0], r[1])
	}
	return strings.TrimRight(result, "\n") + "\n"
}

func commandIntro(commandName, agentName string) string {
	switch agentName {
	case "planner", "tech-planner":
		return fmt.Sprintf("Use the `tech-planner` subagent for `/%s` unless the task is too small to justify delegation.\nKeep the final answer concise and action-oriented.\n", commandName)
	case "coder":
		return fmt.Sprintf("Use the `coder` subagent for `/%s` whenever code or tests must be written or updated.\nKeep the work bounded to the requested scope.\n", commandName)
	default:
		return fmt.Sprintf("Run `/%s` from the main conversation following the orchestrator workflow.\nImportant: Claude subagents cannot spawn other subagents, so keep orchestration in the main thread.\nDelegate bounded code changes to `coder`, planning to `tech-planner`, and reviews to specialized agents.\n", commandName)
	}
}

// appendMarkedBlock idempotently merges blockFile content into targetFile
// using <!-- BEGIN marker --> / <!-- END marker --> delimiters
func appendMarkedBlock(targetFile, blockFile, marker string) error {
	blockContent, err := safefs.ReadFileAbsoluteVerified(blockFile, 4<<20)
	if err != nil {
		return err
	}

	start := fmt.Sprintf("<!-- BEGIN %s -->", marker)
	end := fmt.Sprintf("<!-- END %s -->", marker)
	wrapped := start + "\n" + strings.TrimRight(string(blockContent), "\n") + "\n" + end + "\n"

	var existing string
	if data, err := safefs.ReadFileAbsoluteVerified(targetFile, 4<<20); err == nil {
		existing = string(data)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing Claude file securely: %w", err)
	}

	var updated string
	if strings.Contains(existing, start) && strings.Contains(existing, end) {
		before := existing[:strings.Index(existing, start)]
		after := existing[strings.Index(existing, end)+len(end):]
		updated = strings.TrimRight(before, "\n")
		if updated != "" {
			updated += "\n\n"
		}
		updated += wrapped
		tail := strings.TrimSpace(after)
		if tail != "" {
			updated += "\n\n" + tail + "\n"
		}
	} else {
		updated = strings.TrimRight(existing, "\n")
		if updated != "" {
			updated += "\n\n"
		}
		updated += wrapped
	}

	return writeFile(targetFile, updated)
}

// configureClaudeNeuroxMCP merges neurox MCP into ~/.claude.json
func configureClaudeNeuroxMCP() error {
	return configureClaudeNeuroxMCPWithReporter(discardReporter())
}

func configureClaudeNeuroxMCPWithReporter(reporter Reporter) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home directory: %w", err)
	}
	claudeJSON := filepath.Join(home, ".claude.json")

	var data map[string]json.RawMessage
	var rawBackup []byte
	if raw, err := safefs.ReadFileAbsoluteVerified(claudeJSON, 1<<20); err == nil {
		rawBackup = raw
		if err := json.Unmarshal(raw, &data); err != nil {
			// File exists but can't be parsed — save backup
			reporter.Warning("existing .claude.json is malformed, preserving as .bak")
			backupPath := claudeJSON + ".bak"
			if err := writeFile(backupPath, string(rawBackup)); err != nil {
				reporter.Warning("could not save backup: %v", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read Claude config securely: %w", err)
	}
	if data == nil {
		data = make(map[string]json.RawMessage)
	}

	var mcpServers map[string]json.RawMessage
	if raw, ok := data["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &mcpServers); err != nil {
			reporter.Warning("could not parse mcpServers: %v", err)
		}
	}
	if mcpServers == nil {
		mcpServers = make(map[string]json.RawMessage)
	}

	neuroxEntry := map[string]interface{}{
		"type":    "stdio",
		"command": "neurox",
		"args":    []string{"mcp"},
	}
	neuroxJSON, _ := json.Marshal(neuroxEntry)
	mcpServers["neurox"] = neuroxJSON

	mcpJSON, _ := json.Marshal(mcpServers)
	data["mcpServers"] = mcpJSON

	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(claudeJSON, string(out)+"\n")
}

func claudeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Return a fallback path that will likely fail gracefully downstream.
		return "~/.claude"
	}
	return filepath.Join(home, ".claude")
}
