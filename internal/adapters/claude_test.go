package adapters

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFrontmatter_WithFrontmatter(t *testing.T) {
	input := "---\nname: test\ndescription: A test skill\nagent: coder\n---\n\nBody content here.\n"
	meta, body := parseFrontmatter(input)

	if meta["name"] != "test" {
		t.Errorf("name = %q, want %q", meta["name"], "test")
	}
	if meta["description"] != "A test skill" {
		t.Errorf("description = %q, want %q", meta["description"], "A test skill")
	}
	if meta["agent"] != "coder" {
		t.Errorf("agent = %q, want %q", meta["agent"], "coder")
	}
	if body != "Body content here.\n" {
		t.Errorf("body = %q, want %q", body, "Body content here.\n")
	}
}

func TestRenderAgentsRejectsTraversalName(t *testing.T) {
	source := t.TempDir()
	opencodeDir := filepath.Join(source, "opencode")
	if err := os.MkdirAll(opencodeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"agent":{"../escape":{"prompt":"prompt","description":"description"}}}`
	if err := os.WriteFile(filepath.Join(opencodeDir, "opencode.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	if err := renderAgents(source, target); err == nil {
		t.Fatal("expected traversal agent name to be rejected")
	}
	if _, err := os.Stat(filepath.Join(target, "escape.md")); !os.IsNotExist(err) {
		t.Fatalf("traversal created file outside agents directory: stat error %v", err)
	}
}

func TestRenderAgentsAllowsSafeName(t *testing.T) {
	source := t.TempDir()
	opencodeDir := filepath.Join(source, "opencode")
	if err := os.MkdirAll(opencodeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"agent":{"safe-agent":{"prompt":"safe prompt","description":"safe description"}}}`
	if err := os.WriteFile(filepath.Join(opencodeDir, "opencode.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	if err := renderAgents(source, target); err != nil {
		t.Fatalf("renderAgents failed for safe name: %v", err)
	}
	path := filepath.Join(target, "agents", "safe-agent.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("safe agent file was not created: %v", err)
	}
	if !contains(string(content), "safe prompt") {
		t.Fatalf("safe agent content = %q, want prompt", content)
	}
}

func TestParseFrontmatter_WithoutFrontmatter(t *testing.T) {
	input := "Just plain text without frontmatter.\n"
	meta, body := parseFrontmatter(input)

	if len(meta) != 0 {
		t.Errorf("expected empty meta, got %v", meta)
	}
	if body != input {
		t.Errorf("body = %q, want %q", body, input)
	}
}

func TestParseFrontmatter_Empty(t *testing.T) {
	meta, body := parseFrontmatter("")
	if len(meta) != 0 {
		t.Errorf("expected empty meta for empty input, got %v", meta)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestNormalizeCommandBody_Engram(t *testing.T) {
	input := "Use Engram persistent memory to store stuff."
	got := normalizeCommandBody(input)
	if got == input {
		t.Error("normalizeCommandBody should have replaced Engram references")
	}
	if contains(got, "Engram") {
		t.Errorf("output still contains 'Engram': %q", got)
	}
	if !contains(got, "Neurox") {
		t.Errorf("output does not contain 'Neurox': %q", got)
	}
}

func TestNormalizeCommandBody_Argument(t *testing.T) {
	input := `Do something with "{argument}" value.`
	got := normalizeCommandBody(input)
	if contains(got, `"{argument}"`) {
		t.Errorf("output still contains original argument placeholder: %q", got)
	}
	if !contains(got, `"$ARGUMENTS"`) {
		t.Errorf("output does not contain $ARGUMENTS: %q", got)
	}
}

func TestNormalizeCommandBody_EndsWithNewline(t *testing.T) {
	input := "Some content"
	got := normalizeCommandBody(input)
	if len(got) == 0 || got[len(got)-1] != '\n' {
		t.Errorf("normalizeCommandBody result should end with newline, got: %q", got)
	}
}

func TestAppendMarkedBlock_FirstInsert(t *testing.T) {
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "TARGET.md")
	blockFile := filepath.Join(dir, "block.md")

	os.WriteFile(blockFile, []byte("# Block Content\nSome text.\n"), 0o644)

	err := appendMarkedBlock(targetFile, blockFile, "test-marker")
	if err != nil {
		t.Fatalf("appendMarkedBlock failed: %v", err)
	}

	data, _ := os.ReadFile(targetFile)
	content := string(data)

	if !contains(content, "<!-- BEGIN test-marker -->") {
		t.Error("missing BEGIN marker")
	}
	if !contains(content, "<!-- END test-marker -->") {
		t.Error("missing END marker")
	}
	if !contains(content, "# Block Content") {
		t.Error("missing block content")
	}
}

func TestAppendMarkedBlock_Idempotent(t *testing.T) {
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "TARGET.md")
	blockFile := filepath.Join(dir, "block.md")

	os.WriteFile(blockFile, []byte("# Block Content\n"), 0o644)

	// Insert twice
	appendMarkedBlock(targetFile, blockFile, "test-marker")
	appendMarkedBlock(targetFile, blockFile, "test-marker")

	data, _ := os.ReadFile(targetFile)
	content := string(data)

	// Should only appear once
	count := 0
	for i := 0; i < len(content)-len("<!-- BEGIN test-marker -->"); i++ {
		if content[i:i+len("<!-- BEGIN test-marker -->")] == "<!-- BEGIN test-marker -->" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("BEGIN marker appears %d times, want 1 (idempotent)", count)
	}
}

func TestAppendMarkedBlock_ExistingContent(t *testing.T) {
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "TARGET.md")
	blockFile := filepath.Join(dir, "block.md")

	os.WriteFile(targetFile, []byte("# Existing content\n\nSome text.\n"), 0o644)
	os.WriteFile(blockFile, []byte("# Block\n"), 0o644)

	err := appendMarkedBlock(targetFile, blockFile, "marker")
	if err != nil {
		t.Fatalf("appendMarkedBlock failed: %v", err)
	}

	data, _ := os.ReadFile(targetFile)
	content := string(data)

	if !contains(content, "# Existing content") {
		t.Error("existing content was lost")
	}
	if !contains(content, "<!-- BEGIN marker -->") {
		t.Error("marker block not added")
	}
}

func TestCommandIntro_Coder(t *testing.T) {
	got := commandIntro("implement", "coder")
	if !contains(got, "coder") {
		t.Errorf("commandIntro for coder should mention coder, got: %q", got)
	}
}

func TestCommandIntro_TechPlanner(t *testing.T) {
	got := commandIntro("plan", "tech-planner")
	if !contains(got, "tech-planner") {
		t.Errorf("commandIntro for tech-planner should mention tech-planner, got: %q", got)
	}
}

func TestCommandIntro_Default(t *testing.T) {
	got := commandIntro("run", "manager")
	if got == "" {
		t.Error("commandIntro returned empty string")
	}
}

// helper
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
