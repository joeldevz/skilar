package assets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertWorkflowOrchestratorContract(t *testing.T, raw []byte) {
	t.Helper()
	text := strings.ToLower(string(raw))
	required := []string{"skynex workflow start", "skynex workflow run", "skynex workflow review", "skynex workflow deliver", "skynex workflow status", "skynex workflow inspect", "skynex workflow receipt", "skynex workflow approve", "skynex workflow abort", "candidate_frozen", "receipted", "depth 0", "depth 1", "depth 4", "neurox"}
	for _, value := range required {
		if !strings.Contains(text, value) {
			t.Errorf("orchestrator contract missing %q", value)
		}
	}
	forbidden := []string{"research-orchestrator", "plan.md", "security ×2", "security x2", "red gate", "skynex workflow discover", "skynex workflow plan", "skynex workflow execute", "skynex workflow verify", "skynex workflow validate"}
	for _, value := range forbidden {
		if strings.Contains(text, value) {
			t.Errorf("orchestrator contract contains legacy/invented protocol %q", value)
		}
	}
}

func TestSourceOrchestratorUsesWorkflowV2Contract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "opencode", "agents", "orchestrator.md"))
	if err != nil {
		t.Fatal(err)
	}
	assertWorkflowOrchestratorContract(t, raw)
}

func TestEmbeddedInstallPreservesWorkflowV2Contract(t *testing.T) {
	sub, err := OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err = ExtractTo(sub, dest); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "agents", "orchestrator.md"))
	if err != nil {
		t.Fatal(err)
	}
	assertWorkflowOrchestratorContract(t, raw)
}
