package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/models"
)

func TestConfigV1FixtureMatchesSaveContract(t *testing.T) {
	fixturePath := fixtureFile(t, "skills.config.json")
	existing, err := LoadOrDefault(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	want := decodeJSONFile(t, fixturePath)
	assertVersionOne(t, want)

	path := filepath.Join(t.TempDir(), "skills.config.json")
	req := &models.InstallRequest{
		Packages:           []string{"skills"},
		Targets:            []string{"claude", "opencode"},
		Versions:           map[string]string{"skills": "latest"},
		Interactive:        false,
		NeuroxEnabled:      true,
		NeuroxSelectionSet: true,
	}
	if err := SaveConfig(path, req, existing); err != nil {
		t.Fatal(err)
	}
	got := decodeJSONFile(t, path)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config v1 output drifted\n got: %#v\nwant: %#v", got, want)
	}
}

func TestLockV1FixtureMatchesSaveContract(t *testing.T) {
	fixturePath := fixtureFile(t, "skills.lock.json")
	want := decodeJSONFile(t, fixturePath)
	assertVersionOne(t, want)

	installedAt := "2026-01-02T03:04:05Z"
	result := &models.InstallResult{
		PackageID:        "skills",
		RequestedVersion: "latest",
		ResolvedVersion:  "latest",
		ResolvedRef:      "",
		Commit:           "embedded",
		Targets: map[string]*models.TargetResult{
			"claude":   {Status: "installed", InstalledAt: installedAt, Artifacts: []string{"agents", "skills"}},
			"opencode": {Status: "installed", InstalledAt: installedAt, Artifacts: []string{"opencode.json", "plugins"}},
		},
	}
	path := filepath.Join(t.TempDir(), "skills.lock.json")
	if err := SaveLock(path, []*models.InstallResult{result}, &models.InstallRequest{}); err != nil {
		t.Fatal(err)
	}
	got := decodeJSONFile(t, path)
	if _, err := time.Parse(time.RFC3339, got["generatedAt"].(string)); err != nil {
		t.Fatalf("generatedAt is not RFC3339: %v", err)
	}
	got["generatedAt"] = want["generatedAt"]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lock v1 output drifted\n got: %#v\nwant: %#v", got, want)
	}
}

func TestV1FixturesSatisfyPublishedSchemaContract(t *testing.T) {
	configFixture := decodeJSONFile(t, fixtureFile(t, "skills.config.json"))
	configSchema := decodeJSONFile(t, schemaFile(t, "skills.config.schema.json"))
	assertSchemaVersion(t, configFixture, configSchema)
	assertRequiredFields(t, "config", configFixture, configSchema)
	configProperties := objectValue(t, "config schema properties", configSchema["properties"])
	defaultsSchema := objectValue(t, "config defaults schema", configProperties["defaults"])
	assertRequiredFields(t, "config.defaults", objectValue(t, "config defaults", configFixture["defaults"]), defaultsSchema)
	defaultsProperties := objectValue(t, "config defaults schema properties", defaultsSchema["properties"])
	if _, ok := defaultsProperties["neuroxEnabled"]; !ok {
		t.Fatal("published config v1 schema does not describe the runtime-owned neuroxEnabled field")
	}
	packageSchema := objectValue(t, "config package schema", objectValue(t, "config packages schema", configProperties["packages"])["additionalProperties"])
	for name, value := range objectValue(t, "config packages", configFixture["packages"]) {
		assertRequiredFields(t, "config.packages."+name, objectValue(t, "config package", value), packageSchema)
	}

	lockFixture := decodeJSONFile(t, fixtureFile(t, "skills.lock.json"))
	lockSchema := decodeJSONFile(t, schemaFile(t, "skills.lock.schema.json"))
	assertSchemaVersion(t, lockFixture, lockSchema)
	assertRequiredFields(t, "lock", lockFixture, lockSchema)
	lockProperties := objectValue(t, "lock schema properties", lockSchema["properties"])
	lockPackageSchema := objectValue(t, "lock package schema", objectValue(t, "lock packages schema", lockProperties["packages"])["additionalProperties"])
	lockPackageProperties := objectValue(t, "lock package properties", lockPackageSchema["properties"])
	targetsSchema := objectValue(t, "lock targets schema", lockPackageProperties["targets"])
	targetSchema := firstObjectValue(t, "lock target pattern", objectValue(t, "lock target patterns", targetsSchema["patternProperties"]))
	for name, value := range objectValue(t, "lock packages", lockFixture["packages"]) {
		item := objectValue(t, "lock package", value)
		assertRequiredFields(t, "lock.packages."+name, item, lockPackageSchema)
		commit, ok := item["commit"].(string)
		if !ok || !schemaAllowsString(lockPackageProperties["commit"], commit) {
			t.Fatalf("lock.packages.%s.commit %q is rejected by the published v1 schema", name, commit)
		}
		for target, targetValue := range objectValue(t, "lock targets", item["targets"]) {
			assertRequiredFields(t, "lock.packages."+name+".targets."+target, objectValue(t, "lock target", targetValue), targetSchema)
		}
	}
}

func decodeJSONFile(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func assertVersionOne(t *testing.T, value map[string]interface{}) {
	t.Helper()
	if version, ok := value["version"].(float64); !ok || version != 1 {
		t.Fatalf("fixture version = %#v, want 1", value["version"])
	}
}

func fixtureFile(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "v1", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func schemaFile(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func assertSchemaVersion(t *testing.T, fixture, schema map[string]interface{}) {
	t.Helper()
	properties := objectValue(t, "schema properties", schema["properties"])
	versionSchema := objectValue(t, "version schema", properties["version"])
	if !reflect.DeepEqual(fixture["version"], versionSchema["const"]) {
		t.Fatalf("fixture version %#v does not match schema const %#v", fixture["version"], versionSchema["const"])
	}
}

func assertRequiredFields(t *testing.T, path string, value, schema map[string]interface{}) {
	t.Helper()
	required, ok := schema["required"].([]interface{})
	if !ok {
		t.Fatalf("%s schema has no required field list", path)
	}
	for _, raw := range required {
		name, ok := raw.(string)
		if !ok {
			t.Fatalf("%s schema contains a non-string required field", path)
		}
		if _, exists := value[name]; !exists {
			t.Errorf("%s fixture is missing schema-required field %q", path, name)
		}
	}
}

func objectValue(t *testing.T, label string, value interface{}) map[string]interface{} {
	t.Helper()
	object, ok := value.(map[string]interface{})
	if !ok {
		t.Fatalf("%s is not an object: %#v", label, value)
	}
	return object
}

func firstObjectValue(t *testing.T, label string, values map[string]interface{}) map[string]interface{} {
	t.Helper()
	for _, value := range values {
		return objectValue(t, label, value)
	}
	t.Fatalf("%s is empty", label)
	return nil
}

func schemaAllowsString(raw interface{}, value string) bool {
	schema, ok := raw.(map[string]interface{})
	if !ok {
		return false
	}
	if pattern, ok := schema["pattern"].(string); ok {
		matched, err := regexp.MatchString(pattern, value)
		return err == nil && matched
	}
	if values, ok := schema["enum"].([]interface{}); ok {
		for _, candidate := range values {
			if candidate == value {
				return true
			}
		}
	}
	if options, ok := schema["anyOf"].([]interface{}); ok {
		for _, option := range options {
			if schemaAllowsString(option, value) {
				return true
			}
		}
	}
	return false
}
