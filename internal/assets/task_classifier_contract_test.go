package assets

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var taskClassifierFields = []string{
	"clarification",
	"evidence",
	"reason",
	"risk",
	"route",
	"scope",
	"task_type",
}

func readAsset(t *testing.T, root fs.FS, name string) string {
	t.Helper()
	b, err := fs.ReadFile(root, name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.ToLower(string(b))
}

func assertTaskClassifierContract(t *testing.T, root fs.FS) {
	t.Helper()
	text := readAsset(t, root, "agents/task-classifier.md")

	for _, required := range []string{
		"read-only",
		"classifier",
		"bounded repository discovery",
		"neurox",
		"recall",
		"only when prior context could materially change",
		"material product/behavior ambiguity",
		"direct",
		"tdd",
		"grill-me",
		"human-gate",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("task-classifier contract missing %q", required)
		}
	}

	// Keep the safety boundary structural.  A permissive prose substring is not
	// enough to prevent this agent from becoming an implementation worker.
	for _, heading := range []string{"classification rules", "boundaries"} {
		if !regexp.MustCompile(`(?im)^#+\s*` + regexp.QuoteMeta(heading) + `\s*$`).MatchString(text) {
			t.Errorf("task-classifier contract missing %q section", heading)
		}
	}
	if !strings.Contains(text, "must not implement, edit, write, or execute") {
		t.Error("task-classifier contract lacks explicit no-implementation constraint")
	}

	// Parse the YAML shape rather than matching field-like prose. This keeps the
	// classifier's public response small while allowing nested scope/evidence data.
	heading := regexp.MustCompile(`(?m)^\s*(?:#+\s*)?(?:task-classifier\s+)?(?:output|return)\b[^\n]*`)
	loc := heading.FindStringIndex(text)
	if loc == nil {
		t.Fatal("task-classifier contract has no output/return section")
	}
	section := text[loc[1]:]
	if next := regexp.MustCompile(`(?m)^\s*#+\s+`).FindStringIndex(section); next != nil {
		section = section[:next[0]]
	}
	yamlBlock := regexp.MustCompile("(?s)```yaml\\s*(.*?)```").FindStringSubmatch(section)
	if len(yamlBlock) != 2 {
		t.Fatal("task-classifier output section has no YAML block")
	}
	var output map[string]any
	if err := yaml.Unmarshal([]byte(yamlBlock[1]), &output); err != nil {
		t.Fatalf("parse task-classifier output YAML: %v", err)
	}
	var got []string
	for field := range output {
		got = append(got, field)
	}
	sort.Strings(got)
	want := append([]string(nil), taskClassifierFields...)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("classifier output fields = %v, want exactly %v", got, want)
	}
}

func assertTaskClassifierRegistration(t *testing.T, root string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Agent map[string]struct {
			Mode   string          `json:"mode"`
			Prompt string          `json:"prompt"`
			Tools  map[string]bool `json:"tools"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("decode opencode.json: %v", err)
	}
	classifier, ok := config.Agent["task-classifier"]
	if !ok || classifier.Prompt != "{file:./agents/task-classifier.md}" || classifier.Mode != "subagent" {
		t.Fatalf("task-classifier is not registered as the expected subagent: %+v", classifier)
	}
	if !classifier.Tools["read"] || !classifier.Tools["glob"] || !classifier.Tools["grep"] || classifier.Tools["edit"] || classifier.Tools["write"] || classifier.Tools["bash"] || classifier.Tools["task"] {
		t.Fatalf("task-classifier tool boundary is not read-only/bounded: %+v", classifier.Tools)
	}
}

func assertOrchestratorClassificationContract(t *testing.T, root fs.FS) {
	t.Helper()
	text := readAsset(t, root, "agents/skynex-orchestrator.md")
	for _, required := range []string{
		"task-classifier",
		"before code, config, or infrastructure work",
		"compact brief",
		"returned route",
		"sole agent that asks the human partner",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("skynex-orchestrator classification contract missing %q", required)
		}
	}
}

func assertEmbeddedMatchesSource(t *testing.T, embedded fs.FS, sourceRoot string) {
	t.Helper()
	for _, name := range []string{
		"agents/task-classifier.md",
		"agents/skynex-orchestrator.md",
	} {
		source, err := os.ReadFile(filepath.Join(sourceRoot, name))
		if err != nil {
			t.Fatalf("read source %s: %v", name, err)
		}
		shipped, err := fs.ReadFile(embedded, name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		if string(source) != string(shipped) {
			t.Errorf("embedded asset %s is not synchronized with source", name)
		}
	}
}

func TestSourceAndEmbeddedTaskClassifierContracts(t *testing.T) {
	sourceRoot := filepath.Join("..", "..", "opencode")
	source := os.DirFS(sourceRoot)
	assertTaskClassifierContract(t, source)
	assertOrchestratorClassificationContract(t, source)
	assertTaskClassifierRegistration(t, sourceRoot)

	embedded, err := OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	assertTaskClassifierContract(t, embedded)
	assertOrchestratorClassificationContract(t, embedded)
	assertEmbeddedMatchesSource(t, embedded, sourceRoot)
}
