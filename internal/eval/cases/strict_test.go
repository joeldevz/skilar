package cases

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/contracts"
)

const validV1Case = `schema_version: 1
id: strict_case
suite: strict_suite
requirement_ids: [REQ-1]
type: behavior
critical: true
agent:
  name: skynex-orchestrator
  model: openai/gpt-test
fixture:
  source: strict/fixture
  initial_git: true
  expected_digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
  git_seed:
    tracked: []
    staged: []
    untracked: []
    ignored: []
setup:
  commands: []
input: Make the exact requested change.
turns: []
completion:
  max_turns: 3
  timeout: 3m
  unexpected_question: fail
oracle:
  commands:
    - argv: [go, test, ./...]
      timeout: 1m
      expected_exit: [0]
  expected_changes: [src/result.txt]
  forbidden_changes: [go.mod]
  expected_files:
    - path: src/result.txt
      content: done
  require_clean_process_tree: true
behavior_checks:
  - type: no_false_success
    requirement_ids: [REQ-1]
    evidence_ids: [infrastructure, filesystem, acceptance, behavior, claims, security]
security:
  execution_mode: trusted-local
  network: host-unisolated
  package_scripts: false
  allowed_executables: [go]
  allowed_write_roots: [fixture]
  retain_trace: sanitized-on-failure
trace:
  max_bytes: 8388608
  max_events: 10000
  max_event_bytes: 1048576
  quiescence:
    required: true
    quiet_period: 1s
    timeout: 30s
tool_policy:
  allowed_tools: [Read, Edit, Bash]
  forbidden_tools: [github_push]
  fake_mcps: []
runs:
  count: 5
  aggregation: median
gates:
  hard_checks: all
metrics: [tokens_total]
extensions:
  x-owner: eval
`

func TestParseContractV1Strict(t *testing.T) {
	t.Parallel()
	loaded, err := ParseContract([]byte(validV1Case))
	if err != nil {
		t.Fatalf("ParseContract() error = %v", err)
	}
	if loaded.SchemaVersion != contracts.CaseSchemaVersion {
		t.Fatalf("SchemaVersion = %d", loaded.SchemaVersion)
	}
	if loaded.BehaviorChecks[0].ID != "behavior_001_no_false_success" {
		t.Fatalf("normalized check ID = %q", loaded.BehaviorChecks[0].ID)
	}
	if loaded.Oracle.Commands[0].ID != "oracle_001" {
		t.Fatalf("normalized command ID = %q", loaded.Oracle.Commands[0].ID)
	}
	if loaded.Migration != nil {
		t.Fatal("v1 case unexpectedly marked as migrated")
	}
}

func TestParseContractRejectsUnsafeYAML(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"unknown field":  validV1Case + "surprise: true\n",
		"duplicate key":  strings.Replace(validV1Case, "id: strict_case", "id: strict_case\nid: duplicate", 1),
		"alias":          "id: &case_id x\ncopy: *case_id\n",
		"multiple docs":  validV1Case + "---\nid: another\n",
		"missing policy": strings.Replace(validV1Case, "  package_scripts: false\n", "", 1),
		"null critical":  strings.Replace(validV1Case, "critical: true", "critical: null", 1),
		"missing evidence": strings.Replace(validV1Case,
			"    evidence_ids: [infrastructure, filesystem, acceptance, behavior, claims, security]\n", "", 1),
		"timestamp tag": strings.Replace(validV1Case, "x-owner: eval", "x-owner: 2026-08-10", 1),
	}
	for name, input := range tests {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseContract([]byte(input)); err == nil {
				t.Fatal("ParseContract() unexpectedly succeeded")
			}
		})
	}
}

func TestParseContractRejectsSemanticHazards(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"traversal":           strings.Replace(validV1Case, "source: strict/fixture", "source: ../fixture", 1),
		"absolute":            strings.Replace(validV1Case, "source: strict/fixture", "source: /tmp/fixture", 1),
		"empty argv":          strings.Replace(validV1Case, "argv: [go, test, ./...]", "argv: []", 1),
		"unlisted executable": strings.Replace(validV1Case, "allowed_executables: [go]", "allowed_executables: [npm]", 1),
		"trusted scripts":     strings.Replace(validV1Case, "package_scripts: false", "package_scripts: true", 1),
		"unknown requirement": strings.Replace(validV1Case, "    requirement_ids: [REQ-1]\n    evidence_ids:", "    requirement_ids: [REQ-2]\n    evidence_ids:", 1),
		"bogus evidence":      strings.Replace(validV1Case, "evidence_ids: [infrastructure, filesystem, acceptance, behavior, claims, security]", "evidence_ids: [bogus]", 1),
		"overlong timeout":    strings.Replace(validV1Case, "timeout: 3m", "timeout: 25h", 1),
	}
	for name, input := range tests {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseContract([]byte(input)); err == nil {
				t.Fatal("ParseContract() unexpectedly succeeded")
			}
		})
	}
}

func TestParseContractBoundsSizeAndDepth(t *testing.T) {
	t.Parallel()
	if _, err := ParseContract(make([]byte, MaxCaseBytes+1)); err == nil {
		t.Fatal("oversized case unexpectedly succeeded")
	}
	var nested strings.Builder
	for i := 0; i < MaxYAMLDepth+2; i++ {
		nested.WriteString(strings.Repeat("  ", i))
		nested.WriteString("x:\n")
	}
	if _, err := ParseContract([]byte(nested.String())); err == nil {
		t.Fatal("over-deep case unexpectedly succeeded")
	}
}

func TestLoadAllContractsMigratesAllHistoricalCasesLosslessly(t *testing.T) {
	root := getProjectRoot()
	if root == "" {
		t.Fatal("project root not found")
	}
	loaded, err := LoadAllContracts(filepath.Join(root, "eval", "cases"))
	if err != nil {
		t.Fatalf("LoadAllContracts() error = %v", err)
	}
	legacyCount := 0
	for _, testCase := range loaded {
		if testCase.Migration != nil {
			legacyCount++
			if testCase.Migration.SourceDigest == "" {
				t.Errorf("case %s lacks explicit migration provenance", testCase.ID)
			}
			if testCase.Migration.Item != testCase.Suite {
				t.Errorf("case %s lost item during migration", testCase.ID)
			}
		}
		if _, err := testCase.Digest(); err != nil {
			t.Errorf("case %s digest: %v", testCase.ID, err)
		}
	}
	if legacyCount != 42 {
		t.Fatalf("migrated %d historical cases, want 42 (total loaded: %d)", legacyCount, len(loaded))
	}
}

func TestLoadAllContractsRejectsDuplicateCaseIDs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"one.yaml", "two.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(validV1Case), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadAllContracts(dir); err == nil || !strings.Contains(err.Error(), "duplicate case id") {
		t.Fatalf("LoadAllContracts() error = %v, want duplicate ID", err)
	}
}

func TestLoadContractRejectsSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	link := filepath.Join(dir, "link.yaml")
	if err := os.WriteFile(target, []byte(validV1Case), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := LoadContract(link); err == nil {
		t.Fatal("symlink case unexpectedly succeeded")
	}
}

func TestLoadContractRejectsSchemaInvalidAllowedExecutable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "invalid-executable.yaml")
	invalid := strings.Replace(validV1Case, "allowed_executables: [go]", "allowed_executables: [go!]", 1)
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadContract(path); err == nil || !strings.Contains(err.Error(), "allowed_executables") {
		t.Fatalf("LoadContract() error = %v, want published allowed_executables rejection", err)
	}
}

func FuzzParseContract(f *testing.F) {
	f.Add([]byte(validV1Case))
	f.Add([]byte("id: legacy\nitem: test\ntype: positive\nagent: verifier\ninput: hi\n"))
	f.Add([]byte("a: &a [*a]\n"))
	f.Add([]byte("schema_version: 999\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxCaseBytes+1 {
			data = data[:MaxCaseBytes+1]
		}
		_, _ = ParseContract(data)
	})
}
