// Package cases securely loads and normalizes evaluation case YAML.
package cases

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/schemas"
	"gopkg.in/yaml.v3"
)

const (
	MaxCaseBytes = 1 << 20
	MaxYAMLDepth = 64
	MaxYAMLNodes = 50_000
)

// Compatibility aliases keep existing callers source-compatible while new
// callers use contracts.Case through LoadContract.
type Turn = contracts.Turn
type Check = contracts.Check
type LLMJudge = contracts.LLMJudge

// TestCase is the historical flattened view consumed by the current CLI. Its
// Contract field always retains the complete normalized case, so a v1 field is
// never silently discarded by the compatibility API.
type TestCase struct {
	SchemaVersion  int
	Suite          string
	Critical       bool
	RequirementIDs []string
	ID             string
	Item           string
	Type           string
	Agent          string
	Input          string
	Turns          []Turn
	MaxTurns       int
	Fixture        string
	SetupCmd       string
	Checks         []Check
	LLMJudge       *LLMJudge
	NRuns          int
	Aggregation    string
	Metrics        []string
	Contract       contracts.Case
}

// Canonical returns the complete normalized contract. The value is copied;
// callers that mutate slices should clone them first.
func (tc TestCase) Canonical() contracts.Case {
	return tc.Contract
}

// LoadContract loads a schema-v1 case or deliberately migrates a schema-less
// legacy case. YAML aliases, duplicate keys, unknown fields, excess size and
// excess nesting are rejected before semantic validation.
func LoadContract(filePath string) (*contracts.Case, error) {
	data, err := readBoundedRegularFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", filePath, err)
	}
	result, err := ParseContract(data)
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", filePath, err)
	}
	return result, nil
}

// ParseContract parses an in-memory case under the same limits as LoadContract.
func ParseContract(data []byte) (*contracts.Case, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty YAML document")
	}
	if len(data) > MaxCaseBytes {
		return nil, fmt.Errorf("case exceeds %d-byte limit", MaxCaseBytes)
	}
	root, err := inspectYAML(data)
	if err != nil {
		return nil, err
	}
	version, present, err := schemaVersion(root)
	if err != nil {
		return nil, err
	}
	if !present {
		return parseLegacy(data)
	}
	if version != contracts.CaseSchemaVersion {
		return nil, fmt.Errorf("schema_version: unsupported value %d", version)
	}
	if err := validatePublishedV1(root); err != nil {
		return nil, err
	}
	if err := validateV1Presence(root); err != nil {
		return nil, err
	}
	var result contracts.Case
	if err := decodeStrict(data, &result); err != nil {
		return nil, err
	}
	result.Normalize()
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return &result, nil
}

func validatePublishedV1(root *yaml.Node) error {
	var instance any
	if err := root.Decode(&instance); err != nil {
		return fmt.Errorf("decode YAML instance for published schema: %w", err)
	}
	encoded, err := json.Marshal(instance)
	if err != nil {
		return fmt.Errorf("encode YAML instance for published schema: %w", err)
	}
	if err := schemas.ValidateJSON(schemas.EvalCase, encoded); err != nil {
		return fmt.Errorf("published case schema: %w", err)
	}
	return nil
}

// LoadSuiteContracts loads the YAML files directly inside dir in lexical order.
func LoadSuiteContracts(dir string) ([]contracts.Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read directory %s: %w", dir, err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isYAML(entry.Name()) {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(paths)
	return loadContractPaths(paths)
}

// LoadAllContracts recursively loads YAML files in lexical path order.
func LoadAllContracts(baseDir string) ([]contracts.Case, error) {
	paths := make([]string, 0)
	err := filepath.WalkDir(baseDir, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !isYAML(entry.Name()) {
			return nil
		}
		paths = append(paths, filePath)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk directory %s: %w", baseDir, err)
	}
	sort.Strings(paths)
	return loadContractPaths(paths)
}

func loadContractPaths(paths []string) ([]contracts.Case, error) {
	result := make([]contracts.Case, 0, len(paths))
	seen := make(map[string]string, len(paths))
	for _, filePath := range paths {
		loaded, err := LoadContract(filePath)
		if err != nil {
			return nil, err
		}
		if previous, exists := seen[loaded.ID]; exists {
			return nil, fmt.Errorf("duplicate case id %q in %s and %s", loaded.ID, previous, filePath)
		}
		seen[loaded.ID] = filePath
		result = append(result, *loaded)
	}
	return result, nil
}

// LoadCase is the compatibility view of LoadContract.
func LoadCase(filePath string) (*TestCase, error) {
	loaded, err := LoadContract(filePath)
	if err != nil {
		return nil, err
	}
	view := compatibilityView(*loaded)
	return &view, nil
}

// LoadSuite is the compatibility view of LoadSuiteContracts.
func LoadSuite(dir string) ([]TestCase, error) {
	loaded, err := LoadSuiteContracts(dir)
	if err != nil {
		return nil, err
	}
	return compatibilityViews(loaded), nil
}

// LoadAll is the compatibility view of LoadAllContracts.
func LoadAll(baseDir string) ([]TestCase, error) {
	loaded, err := LoadAllContracts(baseDir)
	if err != nil {
		return nil, err
	}
	return compatibilityViews(loaded), nil
}

func compatibilityViews(loaded []contracts.Case) []TestCase {
	result := make([]TestCase, 0, len(loaded))
	for _, contract := range loaded {
		result = append(result, compatibilityView(contract))
	}
	return result
}

func compatibilityView(contract contracts.Case) TestCase {
	turns := append([]Turn(nil), contract.Turns...)
	checks := append([]Check(nil), contract.BehaviorChecks...)
	metrics := append([]string(nil), contract.Metrics...)
	setup := ""
	if contract.Migration != nil {
		setup = contract.Migration.SetupCommand
	} else if len(contract.Setup.Commands) == 1 {
		setup = strings.Join(contract.Setup.Commands[0].Argv, " ")
	}
	item := contract.Suite
	if contract.Migration != nil {
		item = contract.Migration.Item
	}
	return TestCase{
		SchemaVersion:  contract.SchemaVersion,
		Suite:          contract.Suite,
		Critical:       contract.Critical,
		RequirementIDs: append([]string(nil), contract.RequirementIDs...),
		ID:             contract.ID,
		Item:           item,
		Type:           string(contract.Type),
		Agent:          contract.Agent.Name,
		Input:          contract.Input,
		Turns:          turns,
		MaxTurns:       contract.Completion.MaxTurns,
		Fixture:        contract.Fixture.Source,
		SetupCmd:       setup,
		Checks:         checks,
		LLMJudge:       contract.LLMJudge,
		NRuns:          contract.Runs.Count,
		Aggregation:    string(contract.Runs.Aggregation),
		Metrics:        metrics,
		Contract:       contract,
	}
}

type legacyCase struct {
	ID          string        `yaml:"id"`
	Item        string        `yaml:"item"`
	Type        string        `yaml:"type"`
	Agent       string        `yaml:"agent"`
	Input       string        `yaml:"input"`
	Turns       []legacyTurn  `yaml:"turns"`
	MaxTurns    int           `yaml:"max_turns"`
	Fixture     string        `yaml:"fixture"`
	SetupCmd    string        `yaml:"setup_cmd"`
	Checks      []legacyCheck `yaml:"checks"`
	LLMJudge    *LLMJudge     `yaml:"llm_judge"`
	NRuns       int           `yaml:"n_runs"`
	Aggregation string        `yaml:"aggregation"`
	Metrics     []string      `yaml:"metrics"`
}

type legacyTurn struct {
	Answer string `yaml:"answer"`
}

type legacyCheck struct {
	Name     string      `yaml:"name"`
	Type     string      `yaml:"type"`
	Pattern  string      `yaml:"pattern"`
	Patterns []string    `yaml:"patterns"`
	Value    interface{} `yaml:"value"`
	Tool     string      `yaml:"tool"`
}

func parseLegacy(data []byte) (*contracts.Case, error) {
	var legacy legacyCase
	if err := decodeStrict(data, &legacy); err != nil {
		return nil, fmt.Errorf("legacy case: %w", err)
	}
	if err := validateLegacy(&legacy); err != nil {
		return nil, err
	}
	setupCommands := []contracts.Command{}
	allowedExecutables := []string{}
	if legacy.SetupCmd != "" {
		argv, err := parseLegacyArgv(legacy.SetupCmd)
		if err != nil {
			return nil, fmt.Errorf("setup_cmd: %w", err)
		}
		setupCommands = append(setupCommands, contracts.Command{
			ID: "legacy_setup_001", Argv: argv, Timeout: "10m", ExpectedExit: []int{0},
		})
		allowedExecutables = append(allowedExecutables, argv[0])
	}
	turns := make([]contracts.Turn, 0, len(legacy.Turns))
	for _, turn := range legacy.Turns {
		turns = append(turns, contracts.Turn{Answer: turn.Answer})
	}
	checks := make([]contracts.Check, 0, len(legacy.Checks))
	for _, check := range legacy.Checks {
		checks = append(checks, contracts.Check{
			ID: check.Name, Name: check.Name, Type: check.Type, Pattern: check.Pattern,
			Patterns: append([]string(nil), check.Patterns...), Value: check.Value, Tool: check.Tool,
		})
	}
	digest := sha256.Sum256(data)
	result := &contracts.Case{
		SchemaVersion:  contracts.CaseSchemaVersion,
		ID:             legacy.ID,
		Suite:          legacy.Item,
		RequirementIDs: []string{},
		Type:           contracts.CaseType(legacy.Type),
		Critical:       false,
		Agent:          contracts.AgentConfig{Name: legacy.Agent},
		Fixture: contracts.FixtureConfig{
			Source: legacy.Fixture, InitialGit: legacy.Fixture != "",
			GitSeed: emptyGitSeed(),
		},
		Setup: contracts.SetupConfig{Commands: setupCommands},
		Input: legacy.Input,
		Turns: turns,
		Completion: contracts.CompletionConfig{
			MaxTurns: legacy.MaxTurns, Timeout: "10m",
			UnexpectedQuestion: contracts.UnexpectedQuestionContinue,
		},
		Oracle: contracts.OracleConfig{
			Commands: []contracts.Command{}, ExpectedChanges: []string{}, ForbiddenChanges: []string{},
			ExpectedFiles: []contracts.ExpectedFile{}, RequireCleanProcessTree: true,
		},
		BehaviorChecks: checks,
		Security: contracts.SecurityConfig{
			ExecutionMode:      contracts.ExecutionTrustedLocal,
			Network:            contracts.NetworkHostUnisolated,
			PackageScripts:     false,
			AllowedExecutables: allowedExecutables,
			AllowedWriteRoots:  []string{"fixture"},
			RetainTrace:        contracts.RetainTraceSanitizedOnFailure,
		},
		Trace: contracts.TraceConfig{
			MaxBytes: 8 << 20, MaxEvents: 10_000, MaxEventBytes: 1 << 20,
			Quiescence: contracts.QuiescenceConfig{Required: true, QuietPeriod: "1s", Timeout: "30s"},
		},
		ToolPolicy: contracts.ToolPolicy{AllowedTools: []string{}, ForbiddenTools: []string{}, FakeMCPs: []contracts.FakeMCP{}},
		Runs:       contracts.RunConfig{Count: legacy.NRuns, Aggregation: contracts.Aggregation(legacy.Aggregation)},
		Gates:      contracts.Gates{HardChecks: "all"},
		LLMJudge:   legacy.LLMJudge,
		Metrics:    append([]string(nil), legacy.Metrics...),
		Migration: &contracts.LegacyMigration{
			SourceDigest: "sha256:" + hex.EncodeToString(digest[:]),
			Item:         legacy.Item, Type: legacy.Type, SetupCommand: legacy.SetupCmd,
		},
	}
	result.Normalize()
	if err := result.Validate(); err != nil {
		return nil, fmt.Errorf("migrate legacy case: %w", err)
	}
	return result, nil
}

func emptyGitSeed() contracts.GitSeed {
	return contracts.GitSeed{
		Tracked: []contracts.GitSeedFile{}, Staged: []contracts.GitSeedFile{},
		Untracked: []contracts.GitSeedFile{}, Ignored: []contracts.GitSeedFile{},
	}
}

func validateLegacy(legacy *legacyCase) error {
	if legacy.ID == "" {
		return fmt.Errorf("field 'id' is required and cannot be empty")
	}
	if legacy.Item == "" {
		return fmt.Errorf("field 'item' is required and cannot be empty")
	}
	if legacy.Agent == "" {
		return fmt.Errorf("field 'agent' is required and cannot be empty")
	}
	if strings.TrimSpace(legacy.Input) == "" {
		return fmt.Errorf("field 'input' is required and cannot be empty")
	}
	if legacy.Type != string(contracts.CaseTypeLegacyPositive) && legacy.Type != string(contracts.CaseTypeLegacyNegative) {
		return fmt.Errorf("type: legacy value must be positive or negative")
	}
	if legacy.MaxTurns == 0 {
		legacy.MaxTurns = 10
	}
	if legacy.NRuns == 0 {
		legacy.NRuns = 1
	}
	if legacy.Aggregation == "" {
		legacy.Aggregation = string(contracts.AggregationMin)
	}
	if legacy.Fixture != "" {
		if err := contracts.ValidateRelativePath(legacy.Fixture); err != nil {
			return fmt.Errorf("fixture: %w", err)
		}
	}
	seenChecks := make(map[string]struct{}, len(legacy.Checks))
	for i, check := range legacy.Checks {
		if check.Name == "" {
			return fmt.Errorf("checks[%d].name: must not be empty", i)
		}
		if _, exists := seenChecks[check.Name]; exists {
			return fmt.Errorf("checks[%d].name: duplicate id %q", i, check.Name)
		}
		seenChecks[check.Name] = struct{}{}
	}
	return nil
}

func parseLegacyArgv(command string) ([]string, error) {
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return nil, fmt.Errorf("must not be empty")
	}
	for i, arg := range argv {
		if !legacyArgPattern.MatchString(arg) {
			return nil, fmt.Errorf("argument %d contains quoting or shell syntax; migrate it to argv explicitly", i)
		}
	}
	return argv, nil
}

func inspectYAML(data []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("YAML document root must be a mapping")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("multiple YAML documents are not allowed")
		}
		return nil, fmt.Errorf("parse trailing YAML: %w", err)
	}
	count := 0
	if err := inspectNode(document.Content[0], 1, &count, "$"); err != nil {
		return nil, err
	}
	return document.Content[0], nil
}

func inspectNode(node *yaml.Node, depth int, count *int, location string) error {
	*count++
	if *count > MaxYAMLNodes {
		return fmt.Errorf("YAML exceeds %d-node limit", MaxYAMLNodes)
	}
	if depth > MaxYAMLDepth {
		return fmt.Errorf("YAML exceeds depth limit %d at %s", MaxYAMLDepth, location)
	}
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return fmt.Errorf("YAML aliases and anchors are not allowed at %s", location)
	}
	switch node.Kind {
	case yaml.MappingNode:
		if len(node.Content)%2 != 0 {
			return fmt.Errorf("invalid YAML mapping at %s", location)
		}
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			value := node.Content[i+1]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fmt.Errorf("mapping keys must be strings at %s", location)
			}
			if key.Value == "<<" {
				return fmt.Errorf("YAML merge keys are not allowed at %s", location)
			}
			if _, exists := seen[key.Value]; exists {
				return fmt.Errorf("duplicate YAML key %q at %s", key.Value, location)
			}
			seen[key.Value] = struct{}{}
			if err := inspectNode(value, depth+1, count, location+"."+key.Value); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			if err := inspectNode(child, depth+1, count, fmt.Sprintf("%s[%d]", location, i)); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str", "!!int", "!!bool", "!!float", "!!null":
		default:
			return fmt.Errorf("unsupported YAML tag %q at %s", node.Tag, location)
		}
		if node.Tag == "!!float" {
			lower := strings.ToLower(node.Value)
			if strings.Contains(lower, ".nan") || strings.Contains(lower, ".inf") {
				return fmt.Errorf("non-finite YAML number is not allowed at %s", location)
			}
		}
	default:
		return fmt.Errorf("unsupported YAML node at %s", location)
	}
	return nil
}

func decodeStrict(data []byte, target interface{}) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode YAML with known fields: %w", err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple YAML documents are not allowed")
		}
		return fmt.Errorf("decode trailing YAML: %w", err)
	}
	return nil
}

func schemaVersion(root *yaml.Node) (int, bool, error) {
	node, ok := mappingValue(root, "schema_version")
	if !ok {
		return 0, false, nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return 0, true, fmt.Errorf("schema_version: must be an integer")
	}
	version, err := strconv.Atoi(node.Value)
	if err != nil {
		return 0, true, fmt.Errorf("schema_version: invalid integer: %w", err)
	}
	return version, true, nil
}

func validateV1Presence(root *yaml.Node) error {
	if err := requireKeys(root, "$", "schema_version", "id", "suite", "requirement_ids", "type", "critical", "agent", "fixture", "setup", "input", "turns", "completion", "oracle", "behavior_checks", "security", "trace", "tool_policy", "runs", "gates"); err != nil {
		return err
	}
	agent, _ := mappingValue(root, "agent")
	if err := requireKeys(agent, "agent", "name", "model"); err != nil {
		return err
	}
	fixture, _ := mappingValue(root, "fixture")
	if err := requireKeys(fixture, "fixture", "source", "initial_git", "expected_digest", "git_seed"); err != nil {
		return err
	}
	gitSeed, _ := mappingValue(fixture, "git_seed")
	if err := requireKeys(gitSeed, "fixture.git_seed", "tracked", "staged", "untracked", "ignored"); err != nil {
		return err
	}
	setup, _ := mappingValue(root, "setup")
	if err := requireKeys(setup, "setup", "commands"); err != nil {
		return err
	}
	completion, _ := mappingValue(root, "completion")
	if err := requireKeys(completion, "completion", "max_turns", "timeout", "unexpected_question"); err != nil {
		return err
	}
	oracle, _ := mappingValue(root, "oracle")
	if err := requireKeys(oracle, "oracle", "commands", "expected_changes", "forbidden_changes", "expected_files", "require_clean_process_tree"); err != nil {
		return err
	}
	security, _ := mappingValue(root, "security")
	if err := requireKeys(security, "security", "execution_mode", "network", "package_scripts", "allowed_executables", "allowed_write_roots", "retain_trace"); err != nil {
		return err
	}
	trace, _ := mappingValue(root, "trace")
	if err := requireKeys(trace, "trace", "max_bytes", "max_events", "max_event_bytes", "quiescence"); err != nil {
		return err
	}
	quiescence, _ := mappingValue(trace, "quiescence")
	if err := requireKeys(quiescence, "trace.quiescence", "required", "quiet_period", "timeout"); err != nil {
		return err
	}
	toolPolicy, _ := mappingValue(root, "tool_policy")
	if err := requireKeys(toolPolicy, "tool_policy", "allowed_tools", "forbidden_tools", "fake_mcps"); err != nil {
		return err
	}
	runs, _ := mappingValue(root, "runs")
	if err := requireKeys(runs, "runs", "count", "aggregation"); err != nil {
		return err
	}
	gates, _ := mappingValue(root, "gates")
	if err := requireKeys(gates, "gates", "hard_checks"); err != nil {
		return err
	}
	for _, container := range []struct {
		path string
		node *yaml.Node
	}{
		{"setup.commands", mustMappingValue(setup, "commands")},
		{"oracle.commands", mustMappingValue(oracle, "commands")},
	} {
		if container.node.Kind != yaml.SequenceNode {
			return fmt.Errorf("%s: must be an array", container.path)
		}
		for i, command := range container.node.Content {
			if err := requireKeys(command, fmt.Sprintf("%s[%d]", container.path, i), "argv", "timeout", "expected_exit"); err != nil {
				return err
			}
		}
	}
	checks, _ := mappingValue(root, "behavior_checks")
	if checks.Kind != yaml.SequenceNode {
		return fmt.Errorf("behavior_checks: must be an array")
	}
	for i, check := range checks.Content {
		if err := requireKeys(check, fmt.Sprintf("behavior_checks[%d]", i), "type", "evidence_ids"); err != nil {
			return err
		}
	}
	return nil
}

func requireKeys(node *yaml.Node, location string, keys ...string) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: must be an object", location)
	}
	for _, key := range keys {
		value, ok := mappingValue(node, key)
		if !ok {
			return fmt.Errorf("%s.%s: required field is missing", location, key)
		}
		if value.Tag == "!!null" {
			return fmt.Errorf("%s.%s: required field must not be null", location, key)
		}
	}
	return nil
}

func mappingValue(mapping *yaml.Node, key string) (*yaml.Node, bool) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1], true
		}
	}
	return nil, false
}

func mustMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	value, _ := mappingValue(mapping, key)
	return value
}

func readBoundedRegularFile(filePath string) ([]byte, error) {
	info, err := os.Lstat(filePath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("case must be a regular file (symlinks are rejected)")
	}
	if info.Size() > MaxCaseBytes {
		return nil, fmt.Errorf("case exceeds %d-byte limit", MaxCaseBytes)
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("case identity changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxCaseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxCaseBytes {
		return nil, fmt.Errorf("case exceeds %d-byte limit", MaxCaseBytes)
	}
	return data, nil
}

func isYAML(name string) bool {
	extension := strings.ToLower(filepath.Ext(name))
	return extension == ".yaml" || extension == ".yml"
}

var legacyArgPattern = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)
