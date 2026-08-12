package orchestratorsuite_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/cases"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
)

const (
	wantNormativeRequirements = 22
	wantPublicCases           = 19
)

func TestPublicSuiteContractAndDigests(t *testing.T) {
	suiteDir, fixtureRoot := suitePaths(t)
	contract := loadNormativeContract(t, suiteDir)
	loaded, err := cases.LoadSuiteContracts(suiteDir)
	if err != nil {
		t.Fatalf("LoadSuiteContracts: %v", err)
	}
	if len(loaded) != len(contract.cases) {
		t.Fatalf("loaded %d cases, want %d", len(loaded), len(contract.cases))
	}
	if err := validateRequirementGuards(loaded, contract); err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool, len(loaded))
	covered := make(map[string]bool)
	for _, testCase := range loaded {
		wantRequirements, ok := contract.cases[testCase.ID]
		if !ok {
			t.Errorf("unexpected public case %q", testCase.ID)
			continue
		}
		seen[testCase.ID] = true
		if testCase.Suite != "skynex-orchestrator" {
			t.Errorf("%s suite = %q", testCase.ID, testCase.Suite)
		}
		if testCase.Extensions["x-visibility"] != "public" {
			t.Errorf("%s must be explicitly public", testCase.ID)
		}
		if _, claimsHoldout := testCase.Extensions["x-holdout"]; claimsHoldout {
			t.Errorf("%s public case claims holdout status", testCase.ID)
		}
		if !sameStrings(testCase.RequirementIDs, wantRequirements) {
			t.Errorf("%s requirements = %v, want %v", testCase.ID, testCase.RequirementIDs, wantRequirements)
		}

		fixturePath := filepath.Join(fixtureRoot, strings.TrimPrefix(testCase.Fixture.Source, "skynex-orchestrator/"))
		snapshot, digestErr := sandbox.DigestTree(fixturePath, sandbox.SnapshotLimits{})
		if digestErr != nil {
			t.Errorf("%s fixture digest: %v", testCase.ID, digestErr)
		} else if snapshot.Digest != testCase.Fixture.ExpectedDigest {
			t.Errorf("%s fixture digest = %s, want %s", testCase.ID, snapshot.Digest, testCase.Fixture.ExpectedDigest)
		}

		for _, executable := range testCase.Security.AllowedExecutables {
			if executable != "go" {
				t.Errorf("%s grants unexpected executable %q", testCase.ID, executable)
			}
		}
		for _, command := range append(append([]contracts.Command(nil), testCase.Setup.Commands...), testCase.Oracle.Commands...) {
			if command.Argv[0] == "sh" || command.Argv[0] == "bash" {
				t.Errorf("%s uses a shell oracle/setup command: %v", testCase.ID, command.Argv)
			}
		}

		checked := make(map[string]bool)
		for _, check := range testCase.BehaviorChecks {
			if check.Hard == nil || !*check.Hard {
				t.Errorf("%s check %s is not hard", testCase.ID, check.ID)
			}
			if len(check.RequirementIDs) == 0 {
				t.Errorf("%s check %s has no requirement IDs", testCase.ID, check.ID)
			}
			if len(check.EvidenceIDs) == 0 {
				t.Errorf("%s check %s has no evidence IDs", testCase.ID, check.ID)
			}
			if _, legacy := check.Extensions["x-evidence-ids"]; legacy {
				t.Errorf("%s check %s retained legacy x-evidence-ids", testCase.ID, check.ID)
			}
			for _, requirement := range check.RequirementIDs {
				checked[requirement] = true
				covered[requirement] = true
			}
		}
		for _, requirement := range testCase.RequirementIDs {
			if !checked[requirement] {
				t.Errorf("%s requirement %s has no hard check", testCase.ID, requirement)
			}
		}
	}

	for id := range contract.cases {
		if !seen[id] {
			t.Errorf("required case %s is missing", id)
		}
	}
	coveredNormative := 0
	for _, requirement := range contract.requirements {
		if !covered[requirement] {
			t.Errorf("normative requirement %s has no hard-check coverage", requirement)
			continue
		}
		coveredNormative++
	}
	if coveredNormative != wantNormativeRequirements {
		t.Errorf("hard-check coverage = %d/%d normative requirements", coveredNormative, wantNormativeRequirements)
	}
}

func TestBogusBehaviorEvidenceIDInvalidatesCaseContract(t *testing.T) {
	suiteDir, _ := suitePaths(t)
	loaded, err := cases.LoadSuiteContracts(suiteDir)
	if err != nil {
		t.Fatal(err)
	}
	mutant := cloneCases(t, loaded)
	if len(mutant) == 0 || len(mutant[0].BehaviorChecks) == 0 {
		t.Fatal("suite has no behavior check to mutate")
	}
	mutant[0].BehaviorChecks[0].EvidenceIDs = []string{"bogus"}
	if err := mutant[0].Validate(); err == nil || !strings.Contains(err.Error(), "evidence_ids") {
		t.Fatalf("bogus evidence lineage validation = %v, want evidence_ids error", err)
	}
}

// This is a harness mutation test, not a model evaluation. For every
// normative requirement it proves that deleting its mapping or demoting all
// of its checks from hard to advisory makes the trusted suite invalid.
func TestEveryNormativeRequirementHasRemovalAndDemotionMutationGuards(t *testing.T) {
	suiteDir, _ := suitePaths(t)
	contract := loadNormativeContract(t, suiteDir)
	loaded, err := cases.LoadSuiteContracts(suiteDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, requirement := range contract.requirements {
		requirement := requirement
		t.Run(requirement+"/removed", func(t *testing.T) {
			mutant := cloneCases(t, loaded)
			for i := range mutant {
				mutant[i].RequirementIDs = withoutString(mutant[i].RequirementIDs, requirement)
				for j := range mutant[i].BehaviorChecks {
					mutant[i].BehaviorChecks[j].RequirementIDs = withoutString(mutant[i].BehaviorChecks[j].RequirementIDs, requirement)
				}
			}
			if err := validateRequirementGuards(mutant, contract); err == nil {
				t.Fatalf("removing %s was not detected", requirement)
			}
		})
		t.Run(requirement+"/demoted", func(t *testing.T) {
			mutant := cloneCases(t, loaded)
			mutated := false
			for i := range mutant {
				for j := range mutant[i].BehaviorChecks {
					if containsString(mutant[i].BehaviorChecks[j].RequirementIDs, requirement) {
						value := false
						mutant[i].BehaviorChecks[j].Hard = &value
						mutated = true
					}
				}
			}
			if !mutated {
				t.Fatalf("no check was available to mutate for %s", requirement)
			}
			if err := validateRequirementGuards(mutant, contract); err == nil {
				t.Fatalf("demoting all checks for %s was not detected", requirement)
			}
		})
	}
}

func TestDeclaredFakesAreLocalAndScenarioCasesDeclareThem(t *testing.T) {
	suiteDir, _ := suitePaths(t)
	loaded, err := cases.LoadSuiteContracts(suiteDir)
	if err != nil {
		t.Fatal(err)
	}
	requiresFake := map[string]bool{
		"skx_stale_result": true, "skx_worker_failure": true, "skx_retry_lineage": true,
		"skx_neurox_irrelevant": true, "skx_neurox_relevant": true,
		"skx_candidate_drift": true, "skx_review_retry": true,
		"skx_duplicate_validation": true, "skx_late_child": true,
		"skx_neurox_failure": true, "skx_compaction": true, "skx_prompt_injection": true,
	}
	for _, testCase := range loaded {
		if requiresFake[testCase.ID] && len(testCase.ToolPolicy.FakeMCPs) == 0 {
			t.Errorf("%s must declare its local fake", testCase.ID)
		}
		for _, fake := range testCase.ToolPolicy.FakeMCPs {
			if fake.Transport != "stdio" || fake.Command == nil {
				t.Errorf("%s fake %s is not local stdio", testCase.ID, fake.Name)
				continue
			}
			want := []string{"go", "run", "./fake-mcp"}
			if !sameStrings(fake.Command.Argv, want) || fake.URL != "" {
				t.Errorf("%s fake %s command = %v URL=%q", testCase.ID, fake.Name, fake.Command.Argv, fake.URL)
			}
			if fake.Env["SKX_FAKE_SCENARIO"] == "" {
				t.Errorf("%s fake %s has no deterministic scenario", testCase.ID, fake.Name)
			}
		}
	}
}

func TestDirtyWorktreeDeclaresEveryGitState(t *testing.T) {
	suiteDir, _ := suitePaths(t)
	loaded, err := cases.LoadContract(filepath.Join(suiteDir, "skx_dirty_worktree.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	seed := loaded.Fixture.GitSeed
	if len(seed.Tracked) == 0 || len(seed.Staged) == 0 || len(seed.Untracked) == 0 || len(seed.Ignored) == 0 {
		t.Fatalf("dirty-worktree seed is incomplete: %+v", seed)
	}
	wanted := map[string]bool{
		"git_commit": false, "git_add": false, "git_reset": false,
		"git_restore_staged": false, "git_clean": false,
	}
	for _, check := range loaded.BehaviorChecks {
		if check.Type != "tool_not_called" {
			continue
		}
		if _, required := wanted[check.Tool]; required && check.Hard != nil && *check.Hard &&
			containsString(check.EvidenceIDs, "behavior") && containsString(check.RequirementIDs, "SKX-GIT-001") {
			wanted[check.Tool] = true
		}
	}
	for tool, covered := range wanted {
		if !covered {
			t.Errorf("dirty-worktree has no hard absence check for %s", tool)
		}
		if !containsString(loaded.ToolPolicy.ForbiddenTools, tool) {
			t.Errorf("dirty-worktree policy does not forbid %s", tool)
		}
	}
}

func TestTaskCodeFixturesStartRed(t *testing.T) {
	_, fixtureRoot := suitePaths(t)
	for _, name := range []string{"low-button", "low-slugify", "medium-profile", "high-auth", "dirty-worktree"} {
		name := name
		t.Run(name, func(t *testing.T) {
			command := exec.Command("go", "test", "./...")
			command.Dir = filepath.Join(fixtureRoot, name)
			command.Env = append(os.Environ(), "GOCACHE="+t.TempDir(), "GOWORK=off")
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("fixture unexpectedly starts green:\n%s", output)
			}
			if !strings.Contains(string(output), "FAIL") {
				t.Fatalf("fixture did not fail as a Go acceptance test:\n%s", output)
			}
		})
	}
}

func TestCoordinationFakeBuildsOffline(t *testing.T) {
	_, fixtureRoot := suitePaths(t)
	command := exec.Command("go", "test", "./...")
	command.Dir = filepath.Join(fixtureRoot, "coordination")
	command.Env = append(os.Environ(), "GOCACHE="+t.TempDir(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fake MCP: %v\n%s", err, output)
	}
}

func TestCoordinationFakeMCPProtocol(t *testing.T) {
	_, fixtureRoot := suitePaths(t)
	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"worker_result","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"worker_result","arguments":{}}}`,
	}
	command := exec.Command("go", "run", "./fake-mcp")
	command.Dir = filepath.Join(fixtureRoot, "coordination")
	command.Env = append(os.Environ(), "GOCACHE="+t.TempDir(), "GOWORK=off", "SKX_FAKE_SCENARIO=worker_retry")
	command.Stdin = strings.NewReader(strings.Join(requests, "\n") + "\n")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run fake MCP: %v\n%s", err, output)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != len(requests) {
		t.Fatalf("fake MCP returned %d lines, want %d:\n%s", len(lines), len(requests), output)
	}
	type resultEnvelope struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	responses := make([]resultEnvelope, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &responses[i]); err != nil {
			t.Fatalf("decode response %d: %v", i, err)
		}
	}
	if len(responses[1].Result.Tools) != 1 || responses[1].Result.Tools[0].Name != "worker_result" {
		t.Fatalf("unexpected fake tool listing: %+v", responses[1].Result.Tools)
	}
	if len(responses[2].Result.Content) != 1 || !strings.Contains(responses[2].Result.Content[0].Text, `"attempt_id":"attempt-1"`) {
		t.Fatalf("first retry response = %+v", responses[2].Result.Content)
	}
	if len(responses[3].Result.Content) != 1 || !strings.Contains(responses[3].Result.Content[0].Text, `"attempt_id":"attempt-2"`) {
		t.Fatalf("second retry response = %+v", responses[3].Result.Content)
	}
}

func suitePaths(t *testing.T) (string, string) {
	t.Helper()
	suiteDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	fixtureRoot, err := filepath.Abs(filepath.Join("..", "..", "fixtures", "skynex-orchestrator"))
	if err != nil {
		t.Fatal(err)
	}
	return suiteDir, fixtureRoot
}

func sameStrings(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}

type normativeContract struct {
	requirements []string
	cases        map[string][]string
}

func loadNormativeContract(t *testing.T, suiteDir string) normativeContract {
	t.Helper()
	specPath := filepath.Join(suiteDir, "..", "..", "specs", "skynex-orchestrator-contract.md")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read normative spec: %v", err)
	}
	contract, err := parseNormativeContract(string(raw))
	if err != nil {
		t.Fatalf("parse normative spec: %v", err)
	}
	if len(contract.requirements) != wantNormativeRequirements {
		t.Fatalf("normative spec contains %d requirement IDs, want %d", len(contract.requirements), wantNormativeRequirements)
	}
	if len(contract.cases) != wantPublicCases {
		t.Fatalf("normative spec contains %d public case mappings, want %d", len(contract.cases), wantPublicCases)
	}
	return contract
}

func parseNormativeContract(markdown string) (normativeContract, error) {
	contract := normativeContract{cases: make(map[string][]string)}
	normativeIDs := make(map[string]struct{})
	section := ""
	for _, line := range strings.Split(markdown, "\n") {
		switch line {
		case "## Normative behavior":
			section = "requirements"
			continue
		case "## Required case traceability":
			section = "cases"
			continue
		}
		if strings.HasPrefix(line, "## ") {
			section = ""
			continue
		}
		switch section {
		case "requirements":
			match := normativeRowPattern.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			id := match[1]
			if _, duplicate := normativeIDs[id]; duplicate {
				return normativeContract{}, fmt.Errorf("duplicate normative requirement ID %s", id)
			}
			normativeIDs[id] = struct{}{}
			contract.requirements = append(contract.requirements, id)
		case "cases":
			match := caseRowPattern.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			caseID := match[1]
			if _, duplicate := contract.cases[caseID]; duplicate {
				return normativeContract{}, fmt.Errorf("duplicate case traceability row %s", caseID)
			}
			for _, requirementMatch := range requirementReferencePattern.FindAllStringSubmatch(match[2], -1) {
				requirement := requirementMatch[1]
				if _, declared := normativeIDs[requirement]; !declared {
					return normativeContract{}, fmt.Errorf("case %s references undeclared normative requirement %s", caseID, requirement)
				}
				contract.cases[caseID] = append(contract.cases[caseID], requirement)
			}
			if len(contract.cases[caseID]) == 0 {
				return normativeContract{}, fmt.Errorf("case %s has no normative requirements", caseID)
			}
		}
	}
	if len(contract.requirements) == 0 {
		return normativeContract{}, fmt.Errorf("Normative behavior table has no requirement IDs")
	}
	if len(contract.cases) == 0 {
		return normativeContract{}, fmt.Errorf("Required case traceability table has no case rows")
	}
	return contract, nil
}

func validateRequirementGuards(loaded []contracts.Case, contract normativeContract) error {
	seen := make(map[string]bool, len(loaded))
	covered := make(map[string]bool)
	for _, testCase := range loaded {
		want, exists := contract.cases[testCase.ID]
		if !exists {
			return fmt.Errorf("unexpected case %s", testCase.ID)
		}
		seen[testCase.ID] = true
		if !sameStrings(testCase.RequirementIDs, want) {
			return fmt.Errorf("case %s requirement mapping changed", testCase.ID)
		}
		for _, check := range testCase.BehaviorChecks {
			if check.Hard == nil || !*check.Hard {
				continue
			}
			for _, requirement := range check.RequirementIDs {
				covered[requirement] = true
			}
		}
		for _, requirement := range testCase.RequirementIDs {
			if !coveredByHardCheck(testCase, requirement) {
				return fmt.Errorf("case %s requirement %s has no hard check", testCase.ID, requirement)
			}
		}
	}
	for id := range contract.cases {
		if !seen[id] {
			return fmt.Errorf("required case %s is missing", id)
		}
	}
	for _, requirement := range contract.requirements {
		if !covered[requirement] {
			return fmt.Errorf("normative requirement %s has no hard-check coverage", requirement)
		}
	}
	return nil
}

var (
	normativeRowPattern         = regexp.MustCompile("^\\| `(SKX-[A-Z0-9-]+)` \\|")
	caseRowPattern              = regexp.MustCompile("^\\| `([a-z0-9_-]+)` \\| (.+) \\|$")
	requirementReferencePattern = regexp.MustCompile("`(SKX-[A-Z0-9-]+)`")
)

func coveredByHardCheck(testCase contracts.Case, requirement string) bool {
	for _, check := range testCase.BehaviorChecks {
		if check.Hard != nil && *check.Hard && containsString(check.RequirementIDs, requirement) {
			return true
		}
	}
	return false
}

func cloneCases(t *testing.T, source []contracts.Case) []contracts.Case {
	t.Helper()
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var result []contracts.Case
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func withoutString(values []string, removed string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != removed {
			result = append(result, value)
		}
	}
	return result
}
