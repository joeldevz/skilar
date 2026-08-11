package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/contracts"
)

func TestExecutableSearchPathRejectsAmbientCWDResolution(t *testing.T) {
	absolute := t.TempDir()
	for _, value := range []string{
		"", ".", "relative" + string(os.PathListSeparator) + absolute,
		absolute + string(os.PathListSeparator), string(os.PathListSeparator) + absolute,
	} {
		if err := ValidateExecutableSearchPath(value); err == nil {
			t.Fatalf("unsafe PATH %q was accepted", value)
		}
	}
	if err := ValidateExecutableSearchPath(absolute); err != nil {
		t.Fatalf("absolute PATH rejected: %v", err)
	}
}

func TestExecutableClosureBindsResolvedPathsAndContentsAndRevalidates(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"setup-tool", "oracle-tool", "fake-tool", "allowed-tool"} {
		writeNativeTestExecutable(t, filepath.Join(directory, name), name)
	}
	t.Setenv("PATH", directory)
	testCase := contracts.Case{
		Setup:  contracts.SetupConfig{Commands: []contracts.Command{{Argv: []string{"setup-tool"}}}},
		Oracle: contracts.OracleConfig{Commands: []contracts.Command{{Argv: []string{"oracle-tool"}}}},
		Security: contracts.SecurityConfig{AllowedExecutables: []string{
			"setup-tool", "oracle-tool", "fake-tool", "allowed-tool",
		}},
		ToolPolicy: contracts.ToolPolicy{FakeMCPs: []contracts.FakeMCP{{
			Name: "fake", Command: &contracts.Command{Argv: []string{"fake-tool"}},
		}}},
	}
	closure, err := ResolveExecutableClosure([]contracts.Case{testCase})
	if err != nil {
		t.Fatal(err)
	}
	if closure.Digest() == "" {
		t.Fatal("closure digest is empty")
	}
	for _, declaration := range testCase.Security.AllowedExecutables {
		path, pathErr := closure.PathFor(declaration)
		if pathErr != nil || !filepath.IsAbs(path) || filepath.Dir(path) != directory {
			t.Fatalf("resolved %q = %q, %v", declaration, path, pathErr)
		}
	}
	mapped := mapCommands(testCase.Setup.Commands, closure)
	if len(mapped) != 1 || mapped[0].Argv[0] != filepath.Join(directory, "setup-tool") {
		t.Fatalf("setup did not execute the captured path: %#v", mapped)
	}
	if err := closure.Revalidate(); err != nil {
		t.Fatalf("unchanged closure rejected: %v", err)
	}

	// Atomic replacement models the relevant TOCTOU race without introducing a
	// Go data race. Revalidation compares both path identity and file bytes.
	replacement := filepath.Join(directory, "replacement")
	writeNativeTestExecutable(t, replacement, "replacement")
	if err := os.Rename(replacement, filepath.Join(directory, "oracle-tool")); err != nil {
		t.Fatal(err)
	}
	if err := closure.Revalidate(); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("atomic executable replacement was accepted: %v", err)
	}
}

func TestExecutableResolutionRejectsFinalSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	link := filepath.Join(directory, "tool")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	if _, err := ResolveExecutableSnapshot("tool"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink executable was accepted: %v", err)
	}
}

func TestExecutableClosureRejectsPATHDrift(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	tool := filepath.Join(first, "tool")
	writeNativeTestExecutable(t, tool, "path-drift")
	t.Setenv("PATH", first)
	closure, err := ResolveExecutableClosure(nil, "tool")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", second)
	if err := closure.Revalidate(); err == nil || !strings.Contains(err.Error(), "PATH drifted") {
		t.Fatalf("PATH drift was accepted: %v", err)
	}
}

func TestExecutableClosureRejectsCanonicalPATHRetarget(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "bin-a")
	second := filepath.Join(directory, "bin-b")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	selection := filepath.Join(directory, "bin")
	if err := os.Symlink(first, selection); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", selection)
	closure, err := ResolveExecutableClosure(nil)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(directory, "bin-new")
	if err := os.Symlink(second, replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, selection); err != nil {
		t.Fatal(err)
	}
	if err := closure.Revalidate(); err == nil || !strings.Contains(err.Error(), "canonical PATH") {
		t.Fatalf("canonical PATH retarget was accepted: %v", err)
	}
}

func TestExecutableClosureTracksAndRevalidatesSymlinkSelectionAndTarget(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "target-a")
	second := filepath.Join(directory, "target-b")
	selection := filepath.Join(directory, "tool")
	writeNativeTestExecutable(t, first, "target-a")
	writeNativeTestExecutable(t, second, "target-b")
	if err := os.Symlink(first, selection); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	closure, err := ResolveExecutableClosure(nil, "tool")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := closure.PathFor("tool")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != first {
		t.Fatalf("closure executes %q, want canonical target %q", resolved, first)
	}
	if got := closure.byDeclaration["tool"]; got.SelectionPath != selection || got.Path != first {
		t.Fatalf("closure did not bind selection and target: %+v", got)
	}

	replacementLink := filepath.Join(directory, "tool-new")
	if err := os.Symlink(second, replacementLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementLink, selection); err != nil {
		t.Fatal(err)
	}
	if err := closure.Revalidate(); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("symlink retarget was accepted: %v", err)
	}
}

func TestExecutableClosureRejectsUnpinnedScriptInterpreter(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "tool"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	if _, err := ResolveExecutableClosure(nil, "tool"); err == nil || !strings.Contains(err.Error(), "interpreter closure is not pinned") {
		t.Fatalf("script executable was accepted without its interpreter: %v", err)
	}
}

func writeNativeTestExecutable(t *testing.T, path, suffix string) {
	t.Helper()
	if err := os.WriteFile(path, append([]byte("\x7fELF"), []byte(suffix)...), 0o700); err != nil {
		t.Fatal(err)
	}
}
