package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/experiment"
)

func TestCompareOutputRejectsExistingTarget(t *testing.T) {
	directory, manifest, control, candidate := newCompareOutputInputs(t)
	output := filepath.Join(directory, "comparison.json")
	if err := os.WriteFile(output, []byte("must survive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateCompareOutputLocation(output, manifest, control, candidate, nil); err == nil || !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing output was accepted: %v", err)
	}
	contents, err := os.ReadFile(output)
	if err != nil || string(contents) != "must survive\n" {
		t.Fatalf("existing output changed: %q, %v", contents, err)
	}
}

func TestCompareOutputRejectsManifestOverwriteAndAlias(t *testing.T) {
	directory, manifest, control, candidate := newCompareOutputInputs(t)
	for _, output := range []string{
		manifest,
		filepath.Join(directory, "unused", "..", filepath.Base(manifest)),
	} {
		if _, err := validateCompareOutputLocation(output, manifest, control, candidate, nil); err == nil || !strings.Contains(err.Error(), "manifest") {
			t.Fatalf("manifest output alias %q was accepted: %v", output, err)
		}
	}
	if runtime.GOOS != "windows" {
		alias := filepath.Join(directory, "manifest-alias.json")
		if err := os.Symlink(manifest, alias); err != nil {
			t.Fatal(err)
		}
		if _, err := validateCompareOutputLocation(alias, manifest, control, candidate, nil); err == nil || !strings.Contains(err.Error(), "manifest") {
			t.Fatalf("manifest symlink alias was accepted: %v", err)
		}
	}
	contents, err := os.ReadFile(manifest)
	if err != nil || string(contents) != "manifest\n" {
		t.Fatalf("manifest changed: %q, %v", contents, err)
	}
}

func TestCompareOutputRejectsLocationInsideVerifiedBundle(t *testing.T) {
	directory, manifest, control, candidate := newCompareOutputInputs(t)
	bundleRoot := filepath.Join(directory, "harness")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	bundles := []experiment.VerifiedBundle{{Name: "harness", AbsoluteRoot: bundleRoot}}
	output := filepath.Join(bundleRoot, "results", "comparison.json")
	if _, err := validateCompareOutputLocation(output, manifest, control, candidate, bundles); err == nil || !strings.Contains(err.Error(), "outside verified harness") {
		t.Fatalf("output inside verified bundle was accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(output)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("invalid output validation mutated bundle: %v", err)
	}

	if runtime.GOOS != "windows" {
		alias := filepath.Join(directory, "harness-alias")
		if err := os.Symlink(bundleRoot, alias); err != nil {
			t.Fatal(err)
		}
		aliasedOutput := filepath.Join(alias, "results", "comparison.json")
		if _, err := validateCompareOutputLocation(aliasedOutput, manifest, control, candidate, bundles); err == nil || !strings.Contains(err.Error(), "outside verified harness") {
			t.Fatalf("symlink alias into verified bundle was accepted: %v", err)
		}
	}
}

func TestCompareOutputReservationPublishesCanonicalJSONWithoutClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "comparison.json")
	if err := saveCompareOutputNoClobber(path, map[string]string{"kind": "comparison"}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "{\"kind\":\"comparison\"}\n" {
		t.Fatalf("published comparison = %q", contents)
	}
	if err := saveCompareOutputNoClobber(path, map[string]string{"kind": "replacement"}); err == nil || !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second publication clobbered output: %v", err)
	}
	contents, err = os.ReadFile(path)
	if err != nil || string(contents) != "{\"kind\":\"comparison\"}\n" {
		t.Fatalf("published comparison changed: %q, %v", contents, err)
	}
}

func TestCompareOutputReservationPreservesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "comparison.json")
	reservation, err := reserveCompareOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reservation.Close() }()
	if err := os.WriteFile(path, []byte("external evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Publish(map[string]string{"kind": "replacement"}); err == nil || !errors.Is(err, fs.ErrExist) {
		t.Fatalf("replaced reservation was published: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "external evidence\n" {
		t.Fatalf("external replacement changed: %q, %v", contents, err)
	}
}

func newCompareOutputInputs(t *testing.T) (directory, manifest, control, candidate string) {
	t.Helper()
	directory = t.TempDir()
	manifest = filepath.Join(directory, "manifest.json")
	control = filepath.Join(directory, "control.json")
	candidate = filepath.Join(directory, "candidate.json")
	for path, contents := range map[string]string{
		manifest: "manifest\n", control: "control\n", candidate: "candidate\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory, manifest, control, candidate
}
