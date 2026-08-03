package installer

import (
	"bytes"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/joeldevz/skynex/internal/models"
)

func testCatalog() *models.Catalog {
	return &models.Catalog{Packages: map[string]*models.PackageDefinition{
		"skills": {ID: "skills", SupportedTargets: []string{"claude", "opencode"}},
	}}
}

func TestBuildIsDeterministicAndUsesOperations(t *testing.T) {
	destinations := Destinations{ClaudeDir: t.TempDir() + "/claude", OpencodeDir: t.TempDir() + "/opencode", StateDir: t.TempDir()}
	req := &models.InstallRequest{Packages: []string{"skills"}, Targets: []string{"opencode", "claude"}}
	first, err := Build(req, testCatalog(), destinations)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(req, testCatalog(), destinations)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("plans differ: %#v != %#v", first, second)
	}
	want := []Operation{
		{Kind: InstallTarget, PackageID: "skills", Target: "claude", Destination: destinations.ClaudeDir},
		{Kind: InstallTarget, PackageID: "skills", Target: "opencode", Destination: destinations.OpencodeDir},
		{Kind: WriteState, Destination: filepath.Join(destinations.StateDir, "skills.config.json")},
		{Kind: WriteState, Destination: filepath.Join(destinations.StateDir, "skills.lock.json")},
	}
	if !reflect.DeepEqual(first.Operations, want) {
		t.Fatalf("unexpected operations: %#v, want %#v", first.Operations, want)
	}
}

func TestBuildCleanupIsConditional(t *testing.T) {
	destinations := Destinations{StateDir: t.TempDir()}
	without, err := Build(&models.InstallRequest{Packages: []string{"skills"}, Targets: []string{"claude"}}, testCatalog(), destinations)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Build(&models.InstallRequest{Packages: []string{"skills"}, Targets: []string{"claude"}, CleanupDeprecated: true}, testCatalog(), destinations)
	if err != nil {
		t.Fatal(err)
	}
	if len(with.Operations) != len(without.Operations)+1 || with.Operations[1].Kind != CleanupDeprecated {
		t.Fatalf("cleanup operation not conditional: %#v", with.Operations)
	}
}

func TestBuildRejectsUnknownPackageAndTarget(t *testing.T) {
	if _, err := Build(&models.InstallRequest{Packages: []string{"missing"}}, testCatalog(), Destinations{}); err == nil {
		t.Fatal("unknown package accepted")
	} else if err.Error() != "unknown package: missing" {
		t.Fatalf("unknown package error = %q", err)
	}
	if _, err := Build(&models.InstallRequest{Packages: []string{"skills"}, Targets: []string{"missing"}}, testCatalog(), Destinations{}); err == nil {
		t.Fatal("unknown target accepted")
	} else if err.Error() != "package skills does not support target missing" {
		t.Fatalf("unknown target error = %q", err)
	}
}

func TestRenderTextStable(t *testing.T) {
	plan, err := Build(&models.InstallRequest{Packages: []string{"skills"}, Targets: []string{"claude"}}, testCatalog(), Destinations{ClaudeDir: "/claude", StateDir: "/state"})
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := plan.RenderText(&rendered); err != nil {
		t.Fatal(err)
	}
	want := "Plan v1\n" +
		"1. install-target package=skills target=claude destination=/claude\n" +
		"2. write-state destination=/state/skills.config.json\n" +
		"3. write-state destination=/state/skills.lock.json\n"
	if rendered.String() != want {
		t.Fatalf("render = %q, want %q", rendered.String(), want)
	}
}
