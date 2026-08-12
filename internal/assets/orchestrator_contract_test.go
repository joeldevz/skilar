package assets

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertWorkflowOrchestratorContract(t *testing.T, raw []byte) {
	t.Helper()
	text := strings.ToLower(string(raw))
	required := []string{"skynex workflow start", "skynex workflow run", "skynex workflow review", "skynex workflow deliver", "skynex workflow status", "skynex workflow inspect", "skynex workflow receipt", "skynex workflow approve", "skynex workflow abort", "skynex workflow resume", "skynex workflow retry-verification", "skynex workflow replan", "candidate_frozen", "receipted", "depth 0", "depth 1", "depth 4", "neurox", "--id example --request", "--path", "--check", "--accept"}
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

func assertWorkflowOrchestratorEfficiencyContract(t *testing.T, raw []byte) {
	t.Helper()
	text := strings.ToLower(string(raw))
	required := []string{
		"efficiency and bounded discovery",
		"minimum evidence needed",
		"do not create a second prd",
		"neurox recall is conditional",
		"one targeted query",
		"stop discovery as soon as",
		"do not delegate generic planning",
		"parallelism is for ready execution slices",
		"prefer `simple`",
		"use `planned` only",
		"`discovery` only while a blocking unknown",
		"do not continue searching",
		"never claim that work is running in the background",
		"`--detach` command succeeds and returns durable job evidence",
		"do not invent estimates",
	}
	for _, value := range required {
		if !strings.Contains(text, value) {
			t.Errorf("orchestrator efficiency contract missing %q", value)
		}
	}
}

func assertPRReviewEvidenceContract(t *testing.T, raw []byte) {
	t.Helper()
	text := strings.ToLower(string(raw))
	required := []string{
		"evidence gate",
		"independently executed",
		"tool-observed",
		"author-claimed",
		"hypothesis/unverified",
		"delegated findings are provisional",
		"primary verification",
		"neurox is contextual memory only",
		"this orchestrator may save or update",
		"narrow neurox search to a subagent",
		"baseline",
		"base sha",
	}
	for _, value := range required {
		if !strings.Contains(text, value) {
			t.Errorf("orchestrator PR review contract missing %q", value)
		}
	}
	assertPRReviewEvidenceStructure(t, text)
}

func prReviewEvidenceSection(text string) string {
	const heading = "## pr review evidence gate"
	start := strings.Index(text, heading)
	if start < 0 {
		return ""
	}
	section := text[start+len(heading):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	return section
}

func validPRReviewEvidenceStructure(text string) bool {
	section := prReviewEvidenceSection(text)
	if section == "" {
		return false
	}
	for _, provenance := range []string{"independently executed", "tool-observed", "author-claimed", "hypothesis/unverified"} {
		if !strings.Contains(section, provenance) {
			return false
		}
	}
	positions := []int{
		strings.Index(section, "primary verification"),
		strings.Index(section, "contradiction resolution"),
		strings.Index(section, "drafted verdict"),
		strings.LastIndex(section, "do not persist unverified findings"),
	}
	for index, position := range positions {
		if position < 0 || (index > 0 && position <= positions[index-1]) {
			return false
		}
	}
	return true
}

func assertPRReviewEvidenceStructure(t *testing.T, text string) {
	t.Helper()
	if !validPRReviewEvidenceStructure(text) {
		t.Error("workflow-orchestrator PR review contract must keep all provenance categories inside Evidence Gate and forbid persisting unverified findings before a drafted verdict")
	}
}

func TestPRReviewEvidenceStructureRejectsGlobalMarkersAndEarlySave(t *testing.T) {
	text := `independently executed tool-observed author-claimed hypothesis/unverified
## PR review Evidence Gate
persist a decision to neurox, then perform primary verification, resolve contradictions, and draft the verdict`
	if validPRReviewEvidenceStructure(strings.ToLower(text)) {
		t.Fatal("accepted provenance outside Evidence Gate and neurox.save before verification/synthesis")
	}
}

func TestSourceWorkflowOrchestratorUsesWorkflowV2Contract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "opencode", "agents", "workflow-orchestrator.md"))
	if err != nil {
		t.Fatal(err)
	}
	assertWorkflowOrchestratorContract(t, raw)
	assertWorkflowOrchestratorEfficiencyContract(t, raw)
	assertPRReviewEvidenceContract(t, raw)
	assertDetachedWorkflowContract(t, raw)
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
	raw, err := os.ReadFile(filepath.Join(dest, "agents", "workflow-orchestrator.md"))
	if err != nil {
		t.Fatal(err)
	}
	assertWorkflowOrchestratorContract(t, raw)
	assertWorkflowOrchestratorEfficiencyContract(t, raw)
	assertPRReviewEvidenceContract(t, raw)
	assertDetachedWorkflowContract(t, raw)
}

func assertDetachedWorkflowContract(t *testing.T, raw []byte) {
	t.Helper()
	text := strings.ToLower(string(raw))
	for _, want := range []string{"workflow run workflow_id --detach", "workflow review --id workflow_id --detach", "never create or delegate a subagent", "shell `&`", "`nohup`", "`tmux`", "keep the chat free", "read-only `workflow status`", "`workflow inspect`"} {
		if !strings.Contains(text, want) {
			t.Errorf("orchestrator detached contract missing %q", want)
		}
	}
}

func assertWorkflowWorkerContract(t *testing.T, root string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "agents", "workflow-worker.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(raw))
	for _, required := range []string{"primary agent", "non-interactive", "allowed paths", "checks", "do not commit", "do not push", "do not create a pr", "do not review", "do not deliver", "do not delegate", "skynex_result_file"} {
		if !strings.Contains(text, required) {
			t.Errorf("workflow-worker missing %q", required)
		}
	}
	config, err := os.ReadFile(filepath.Join(root, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Agent map[string]struct {
			Mode   string `json:"mode"`
			Prompt string `json:"prompt"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(config, &parsed); err != nil {
		t.Fatal(err)
	}
	worker, ok := parsed.Agent["workflow-worker"]
	if !ok || worker.Mode == "subagent" || !strings.Contains(worker.Prompt, "workflow-worker.md") {
		t.Fatalf("workflow-worker is not primary: %+v", worker)
	}
}

func TestWorkflowWorkerIsPrimaryInSourceAndEmbeddedAssets(t *testing.T) {
	assertWorkflowWorkerContract(t, filepath.Join("..", "..", "opencode"))
	sub, err := OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := ExtractTo(sub, dest); err != nil {
		t.Fatal(err)
	}
	assertWorkflowWorkerContract(t, dest)
}

func assertWorkflowReviewerContract(t *testing.T, root string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "agents", "workflow-reviewer.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(raw))
	for _, want := range []string{"primary agent", "read", "do not modify", "skynex_result_file", "do not commit", "do not delegate", "exactly one json"} {
		if !strings.Contains(text, want) {
			t.Errorf("workflow-reviewer missing %q", want)
		}
	}
	config, err := os.ReadFile(filepath.Join(root, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Agent map[string]struct {
			Mode   string `json:"mode"`
			Prompt string `json:"prompt"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(config, &parsed); err != nil {
		t.Fatal(err)
	}
	reviewer, ok := parsed.Agent["workflow-reviewer"]
	if !ok || reviewer.Mode != "primary" || !strings.Contains(reviewer.Prompt, "workflow-reviewer.md") {
		t.Fatalf("workflow-reviewer invalid: %+v", reviewer)
	}
}

func TestWorkflowReviewerIsPrimaryInSourceAndEmbeddedAssets(t *testing.T) {
	assertWorkflowReviewerContract(t, filepath.Join("..", "..", "opencode"))
	sub, err := OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := ExtractTo(sub, dest); err != nil {
		t.Fatal(err)
	}
	assertWorkflowReviewerContract(t, dest)
}

func assertGitRiskPolicy(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, "agents", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(raw))
		// Every agent carries the shared baseline; the ladder above it is
		// proportional to what the role is allowed to mutate.
		for _, want := range []string{"read-only git", "do not delegate", "untracked", "force push", "reset --hard", "clean -fd"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing git policy %q", entry.Name(), want)
			}
		}
		if strings.Contains(text, "stricter read-only boundary") {
			// Roles that inspect an immutable candidate must prohibit every
			// mutation outright instead of documenting bounded local actions.
			for _, want := range []string{"do not stage paths", "do not run `git restore`", "commit, push, or open a pr", "prohibited outright"} {
				if !strings.Contains(text, want) {
					t.Errorf("%s missing read-only git policy %q", entry.Name(), want)
				}
			}
			if strings.Contains(text, "may be executed directly") {
				t.Errorf("%s grants bounded mutations despite a read-only boundary", entry.Name())
			}
			continue
		}
		for _, want := range []string{"git status", "git restore --staged", "stage exact paths", "do not ask the user to run", "git restore --worktree", "exact paths and impact", "commit, push, and pr"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing git policy %q", entry.Name(), want)
			}
		}
	}
}

func TestAllSourceAndEmbeddedAgentsUseRiskBasedGitPolicy(t *testing.T) {
	assertGitRiskPolicy(t, filepath.Join("..", "..", "opencode"))
	sub, err := OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := ExtractTo(sub, dest); err != nil {
		t.Fatal(err)
	}
	assertGitRiskPolicy(t, dest)
}

func assertContinuousWorkflowPolicy(t *testing.T, raw []byte) {
	t.Helper()
	text := strings.ToLower(string(raw))
	for _, want := range []string{"continuous execution", "do not ask", "candidate_frozen", "workflow review --id", "--detach", "replan_required", "verify the evidence", "workflow replan", "same workflow id", "idempotency key", "technical job failure", "retry", "retries are exhausted", "human gate", "destructive", "real ambiguity", "do not auto-approve", "do not auto-deliver", "commit, push, or pr", "receipt-driven development is disabled", "kill switch",
		// Recovery must reach both correction paths: resume for a durable
		// blocker, retry-verification for one wrong check on the same candidate.
		"workflow resume workflow_id --blocker-id", "workflow retry-verification --id workflow_id --check-id", "--replacement", "--actor", "--reason", "instead of rerunning the coder", "preserves the same immutable candidate"} {
		if !strings.Contains(text, want) {
			t.Errorf("continuous workflow policy missing %q", want)
		}
	}
}

func TestSourceAndEmbeddedWorkflowOrchestratorContinueWithoutPermissionLoops(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "opencode", "agents", "workflow-orchestrator.md"))
	if err != nil {
		t.Fatal(err)
	}
	assertContinuousWorkflowPolicy(t, raw)
	sub, err := OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := ExtractTo(sub, dest); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(dest, "agents", "workflow-orchestrator.md"))
	if err != nil {
		t.Fatal(err)
	}
	assertContinuousWorkflowPolicy(t, raw)
}
