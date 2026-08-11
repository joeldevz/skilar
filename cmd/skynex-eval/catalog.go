package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/cases"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/sandbox"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const maxPublishedSchemaBytes = 4 << 20

type caseDescriptor struct {
	ID             string   `json:"id"`
	Suite          string   `json:"suite"`
	Type           string   `json:"type"`
	Critical       bool     `json:"critical"`
	Runs           int      `json:"runs"`
	SchemaVersion  int      `json:"schema_version"`
	LegacyMigrated bool     `json:"legacy_migrated"`
	RequirementIDs []string `json:"requirement_ids"`
	Digest         string   `json:"digest"`
}

type listResult struct {
	Cases []caseDescriptor `json:"cases"`
	Count int              `json:"count"`
}

func commandList(args []string) (listResult, error) {
	set := newFlagSet("list")
	casesDir := set.String("cases-dir", "eval/cases", "trusted case catalog")
	suite := set.String("suite", "all", "suite selector")
	if err := parseFlagSet(set, args); err != nil {
		return listResult{}, err
	}
	loaded, err := cases.LoadAllContracts(*casesDir)
	if err != nil {
		return listResult{}, invalidf("invalid_cases", "load cases: %v", err)
	}
	selected := selectCases(loaded, *suite, "")
	if len(selected) == 0 {
		return listResult{}, invalidf("empty_selection", "no cases selected for suite %q", *suite)
	}
	result := listResult{Cases: make([]caseDescriptor, 0, len(selected))}
	for _, testCase := range selected {
		digest, digestErr := testCase.Digest()
		if digestErr != nil {
			return listResult{}, invalidf("invalid_case_digest", "case %s: %v", testCase.ID, digestErr)
		}
		result.Cases = append(result.Cases, describeCase(testCase, digest))
	}
	sort.Slice(result.Cases, func(i, j int) bool { return result.Cases[i].ID < result.Cases[j].ID })
	result.Count = len(result.Cases)
	return result, nil
}

type schemaValidation struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Bytes  int64  `json:"bytes"`
}

type fixtureValidation struct {
	Source         string `json:"source"`
	Digest         string `json:"digest"`
	ExpectedDigest string `json:"expected_digest,omitempty"`
	Verified       bool   `json:"verified"`
	LegacyOnly     bool   `json:"legacy_only"`
	FileCount      int    `json:"file_count"`
	TotalBytes     int64  `json:"total_bytes"`
}

type validationResult struct {
	Cases             []caseDescriptor    `json:"cases"`
	Schemas           []schemaValidation  `json:"schemas"`
	Fixtures          []fixtureValidation `json:"fixtures"`
	CaseCount         int                 `json:"case_count"`
	V1Count           int                 `json:"v1_count"`
	LegacyCount       int                 `json:"legacy_migrated_count"`
	CasesDigest       string              `json:"cases_digest"`
	PublicCasesDigest string              `json:"public_cases_digest"`
	CriticalCaseIDs   []string            `json:"critical_case_ids"`
	SchemasDigest     string              `json:"schemas_digest"`
	FixturesDigest    string              `json:"fixtures_digest"`
}

func commandValidate(args []string) (validationResult, error) {
	set := newFlagSet("validate")
	casesDir := set.String("cases-dir", "eval/cases", "trusted case catalog")
	fixtureDir := set.String("fixtures-dir", "eval/fixtures", "fixture root")
	schemasDir := set.String("schemas-dir", "schemas", "schema root")
	suite := set.String("suite", "all", "suite selector")
	if err := parseFlagSet(set, args); err != nil {
		return validationResult{}, err
	}

	loaded, err := cases.LoadAllContracts(*casesDir)
	if err != nil {
		return validationResult{}, invalidf("invalid_cases", "load cases: %v", err)
	}
	loaded = selectCases(loaded, *suite, "")
	if len(loaded) == 0 {
		return validationResult{}, invalidf("empty_selection", "no cases selected for suite %q", *suite)
	}
	result := validationResult{Cases: make([]caseDescriptor, 0, len(loaded))}
	caseDigests := make([]struct {
		ID     string `json:"id"`
		Digest string `json:"digest"`
	}, 0, len(loaded))
	for _, testCase := range loaded {
		digest, digestErr := testCase.Digest()
		if digestErr != nil {
			return validationResult{}, invalidf("invalid_case_digest", "case %s: %v", testCase.ID, digestErr)
		}
		result.Cases = append(result.Cases, describeCase(testCase, digest))
		caseDigests = append(caseDigests, struct {
			ID     string `json:"id"`
			Digest string `json:"digest"`
		}{ID: testCase.ID, Digest: digest})
		if testCase.Critical {
			result.CriticalCaseIDs = append(result.CriticalCaseIDs, testCase.ID)
		}
		if testCase.Migration == nil {
			result.V1Count++
		} else {
			result.LegacyCount++
		}
	}
	sort.Slice(result.Cases, func(i, j int) bool { return result.Cases[i].ID < result.Cases[j].ID })
	sort.Slice(caseDigests, func(i, j int) bool { return caseDigests[i].ID < caseDigests[j].ID })
	sort.Strings(result.CriticalCaseIDs)
	result.CaseCount = len(result.Cases)
	result.CasesDigest, err = contracts.CanonicalDigest(caseDigests)
	if err != nil {
		return validationResult{}, invalidf("invalid_case_digest", "%v", err)
	}
	result.PublicCasesDigest, err = publicCaseSetDigest(loaded)
	if err != nil {
		return validationResult{}, invalidf("invalid_case_digest", "%v", err)
	}

	result.Schemas, result.SchemasDigest, err = validateSchemas(*schemasDir)
	if err != nil {
		return validationResult{}, invalidf("invalid_schemas", "%v", err)
	}
	if err := validateV1CasesAgainstPublishedSchema(*schemasDir, loaded); err != nil {
		return validationResult{}, invalidf("invalid_case_schema", "%v", err)
	}
	result.Fixtures, result.FixturesDigest, err = validateFixtures(*fixtureDir, loaded)
	if err != nil {
		return validationResult{}, invalidf("invalid_fixtures", "%v", err)
	}
	return result, nil
}

func describeCase(testCase contracts.Case, digest string) caseDescriptor {
	return caseDescriptor{
		ID: testCase.ID, Suite: testCase.Suite, Type: string(testCase.Type), Critical: testCase.Critical,
		Runs: testCase.Runs.Count, SchemaVersion: testCase.SchemaVersion, LegacyMigrated: testCase.Migration != nil,
		RequirementIDs: append([]string(nil), testCase.RequirementIDs...), Digest: digest,
	}
}

func selectCases(all []contracts.Case, suite, caseID string) []contracts.Case {
	selected := make([]contracts.Case, 0, len(all))
	for _, testCase := range all {
		if suite != "" && suite != "all" && testCase.Suite != suite {
			continue
		}
		if caseID != "" && testCase.ID != caseID {
			continue
		}
		selected = append(selected, testCase)
	}
	return selected
}

func validateSchemas(root string) ([]schemaValidation, string, error) {
	required := []string{"eval-case.schema.json", "eval-result.schema.json", "eval-experiment.schema.json"}
	result := make([]schemaValidation, 0, len(required))
	for _, name := range required {
		path, err := resolveWithin(root, name)
		if err != nil {
			return nil, "", err
		}
		var document map[string]any
		if err := baseline.LoadJSON(path, &document, baseline.IOOptions{MaxBytes: maxPublishedSchemaBytes}); err != nil {
			return nil, "", fmt.Errorf("schema %s: %w", name, err)
		}
		if document["$schema"] != "https://json-schema.org/draft/2020-12/schema" || document["type"] != "object" {
			return nil, "", fmt.Errorf("schema %s is not a draft-2020-12 object schema", name)
		}
		if _, err := compilePublishedSchema(path); err != nil {
			return nil, "", fmt.Errorf("schema %s does not compile as draft 2020-12: %w", name, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("read schema %s: %w", name, err)
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("schema %s is not a regular file", name)
		}
		result = append(result, schemaValidation{Name: name, Digest: digestBytes(data), Bytes: int64(len(data))})
	}
	digest, err := contracts.CanonicalDigest(result)
	return result, digest, err
}

// validateV1CasesAgainstPublishedSchema makes the published Draft 2020-12
// contract executable. Semantic Go validation remains useful for invariants
// that JSON Schema cannot express, but neither layer is allowed to silently
// substitute for the other.
func validateV1CasesAgainstPublishedSchema(schemaRoot string, loaded []contracts.Case) error {
	path, err := resolveWithin(schemaRoot, "eval-case.schema.json")
	if err != nil {
		return err
	}
	schema, err := compilePublishedSchema(path)
	if err != nil {
		return fmt.Errorf("compile eval-case.schema.json: %w", err)
	}
	for _, testCase := range loaded {
		if testCase.Migration != nil {
			continue
		}
		raw, err := json.Marshal(testCase)
		if err != nil {
			return fmt.Errorf("case %s: encode canonical instance: %w", testCase.ID, err)
		}
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("case %s: decode canonical instance: %w", testCase.ID, err)
		}
		if err := schema.Validate(instance); err != nil {
			return fmt.Errorf("case %s violates eval-case.schema.json: %w", testCase.ID, err)
		}
	}
	return nil
}

func compilePublishedSchema(path string) (*jsonschema.Schema, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("schema is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPublishedSchemaBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPublishedSchemaBytes {
		return nil, fmt.Errorf("schema exceeds %d bytes", maxPublishedSchemaBytes)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const resource = "urn:skynex:published-schema"
	if err := compiler.AddResource(resource, document); err != nil {
		return nil, err
	}
	return compiler.Compile(resource)
}

func validateFixtures(root string, loaded []contracts.Case) ([]fixtureValidation, string, error) {
	type declaration struct {
		expected   string
		legacyOnly bool
	}
	declarations := make(map[string]declaration)
	emptyLegacyFixture := false
	for _, testCase := range loaded {
		if testCase.Fixture.Source == "" {
			emptyLegacyFixture = emptyLegacyFixture || testCase.Migration != nil
			continue
		}
		current, exists := declarations[testCase.Fixture.Source]
		if !exists {
			current.legacyOnly = true
		}
		if testCase.Migration == nil {
			current.legacyOnly = false
			if current.expected != "" && current.expected != testCase.Fixture.ExpectedDigest {
				return nil, "", fmt.Errorf("fixture %s has conflicting expected digests", testCase.Fixture.Source)
			}
			current.expected = testCase.Fixture.ExpectedDigest
		}
		declarations[testCase.Fixture.Source] = current
	}
	paths := make([]string, 0, len(declarations))
	for path := range declarations {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	result := make([]fixtureValidation, 0, len(paths))
	for _, relative := range paths {
		absolute, err := resolveWithin(root, relative)
		if err != nil {
			return nil, "", fmt.Errorf("fixture %s: %w", relative, err)
		}
		snapshot, err := sandbox.DigestTree(absolute, sandbox.DefaultSnapshotLimits())
		if err != nil {
			return nil, "", fmt.Errorf("fixture %s: %w", relative, err)
		}
		declaration := declarations[relative]
		verified := declaration.expected != "" && declaration.expected == snapshot.Digest
		if declaration.expected != "" && !verified {
			return nil, "", fmt.Errorf("fixture %s digest mismatch: got %s, expected %s", relative, snapshot.Digest, declaration.expected)
		}
		result = append(result, fixtureValidation{
			Source: relative, Digest: snapshot.Digest, ExpectedDigest: declaration.expected,
			Verified: verified, LegacyOnly: declaration.legacyOnly, FileCount: snapshot.FileCount, TotalBytes: snapshot.TotalBytes,
		})
	}
	if emptyLegacyFixture {
		emptyDigest, digestErr := contracts.CanonicalDigest([]sandbox.Entry{})
		if digestErr != nil {
			return nil, "", digestErr
		}
		result = append(result, fixtureValidation{
			Source: "_legacy-empty", Digest: emptyDigest, Verified: false, LegacyOnly: true,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Source < result[j].Source })
	digest, err := contracts.CanonicalDigest(result)
	return result, digest, err
}

func resolveWithin(root, relative string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("root is required")
	}
	if err := contracts.ValidateRelativePath(filepath.ToSlash(relative)); err != nil {
		return "", err
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved := filepath.Clean(filepath.Join(absoluteRoot, filepath.FromSlash(relative)))
	rel, err := filepath.Rel(absoluteRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes root")
	}
	return resolved, nil
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}
