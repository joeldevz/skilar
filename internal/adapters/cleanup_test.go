package adapters

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/models"
)

func TestDeprecatedManifest(t *testing.T) {
	want := map[string][]string{
		"opencode": {
			"commands/onboard.md", "tools/advisor.ts", "commands/verify-skill.md", "commands/verify-security.md",
			"skills/adversarial-review", "skills/verification-before-completion", "skills/nestjs-patterns",
			"skills/thermo-nuclear-code-quality-review", "skills/typescript-advanced-types",
		},
		"claude": {
			"skills/onboard/SKILL.md", "agents/product-planner.md", "skills/verify-skill/SKILL.md", "skills/verify-security/SKILL.md",
			"skills/adversarial-review", "skills/verification-before-completion", "skills/nestjs-patterns",
			"skills/thermo-nuclear-code-quality-review", "skills/typescript-advanced-types", "skills/plan", "skills/execute",
			"skills/test", "skills/review", "skills/status", "skills/apply-feedback", "skills/context", "skills/diff",
			"skills/estimate", "skills/plan-rewrite",
		},
	}
	if !reflect.DeepEqual(DeprecatedManifest, want) {
		t.Fatalf("deprecated manifest = %#v, want %#v", DeprecatedManifest, want)
	}
}

func TestDeprecatedEntriesAreDisplayedAsTargetRelativePaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "opencode")
	got := formatDeprecatedFileForDisplay(DeprecatedFile{Root: root, Path: filepath.Join(root, "commands", "verify-skill.md"), Target: "opencode"})
	if got != "[opencode] commands/verify-skill.md (deprecated)" {
		t.Fatalf("deprecated display = %q", got)
	}
}

func TestRemoveDeprecatedFilesRenamesRegularFileToBackup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	path := filepath.Join(root, "commands", "onboard.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("deprecated"), 0o600); err != nil {
		t.Fatal(err)
	}
	count, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: path, Root: root, Target: "opencode"}})
	if err != nil || count != 1 {
		t.Fatalf("backup cleanup = %d, %v", count, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("deprecated pathname remains: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".skynex-deprecated-backup-") {
		t.Fatalf("backup not retained: %v, %v", entries, err)
	}
}

func TestRemoveDeprecatedFilesRefusesDirectoriesAndSymlinks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	dir := filepath.Join(root, "skills", "adversarial-review")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: dir, Root: root, Target: "opencode"}}); err == nil {
		t.Fatal("directory cleanup was accepted")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("directory was not preserved: %v", err)
	}
	link := filepath.Join(root, "commands", "verify-security.md")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if count, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: link, Root: root, Target: "opencode"}}); err != nil || count != 1 {
		t.Fatalf("explicitly authorized symlink cleanup = %d, %v", count, err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("authorized symlink was not removed: %v", err)
	}
	unauthorized := filepath.Join(root, "commands", "unlisted-link")
	if err := os.Symlink(outside, unauthorized); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: unauthorized, Root: root, Target: "opencode"}}); err == nil {
		t.Fatal("unauthorized symlink cleanup was accepted")
	}
	if _, err := os.Lstat(unauthorized); err != nil {
		t.Fatalf("unauthorized symlink was not preserved: %v", err)
	}
}

func TestCleanupRejectsTraversalAndUnauthorizedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	candidate := filepath.Join(root, "commands", "onboard.md")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	traversal := root + string(filepath.Separator) + "commands" + string(filepath.Separator) + ".." + string(filepath.Separator) + "commands" + string(filepath.Separator) + "onboard.md"
	if _, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: traversal, Root: root, Target: "opencode"}}); err == nil {
		t.Fatal("path traversal accepted")
	}
	unauthorized := filepath.Join(root, "commands", "user-owned.md")
	if err := os.WriteFile(unauthorized, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: unauthorized, Root: root, Target: "opencode"}}); err == nil {
		t.Fatal("unauthorized file accepted")
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("authorized file changed after rejected traversal: %v", err)
	}
}

func TestCleanupRejectsSymlinkedAncestorAndSurfacesScanErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(root, "skills")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	candidate := filepath.Join(root, "skills", "adversarial-review")
	if err := os.WriteFile(candidate, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: candidate, Root: root, Target: "opencode"}}); err == nil {
		t.Fatal("symlinked ancestor accepted")
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("external candidate was removed: %v", err)
	}

	badHome := t.TempDir()
	t.Setenv("HOME", badHome)
	if err := os.WriteFile(filepath.Join(badHome, ".config"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FindDeprecatedFiles(); err == nil || !strings.Contains(err.Error(), "scan") {
		t.Fatalf("scan error was not surfaced: %v", err)
	}
}

func TestCleanupConsentRequiresExplicitConfirmation(t *testing.T) {
	files := map[string][]DeprecatedFile{"opencode": {{Path: "/tmp/commands/onboard.md", Root: "/tmp", Target: "opencode"}}}
	if PromptCleanupDeprecatedWithIO(files, strings.NewReader("n\n"), &strings.Builder{}) {
		t.Fatal("cleanup consent granted for a negative answer")
	}
	if !PromptCleanupDeprecatedWithIO(files, strings.NewReader("y\n"), &strings.Builder{}) {
		t.Fatal("cleanup consent not granted for an affirmative answer")
	}
}

func TestInstallPreservesDeprecatedEntries(t *testing.T) {
	deprecated := installTestDeprecatedFile(t)
	if err := InstallOpencode(installTestSource(t), &models.InstallRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(deprecated); err != nil {
		t.Fatalf("deprecated entry was removed: %v", err)
	}
}

func TestInstallPreservesExistingNodeModulesBin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := opencodeDir()
	// This is a synthetic npm-style fixture. The real acorn package is not a
	// Skynex dependency; npm creates this relative .bin link during install.
	binTarget := filepath.Join(target, "node_modules", "acorn", "bin", "acorn")
	if err := os.MkdirAll(filepath.Dir(binTarget), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "opencode.json"), []byte(`{"user":true}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binTarget, []byte("acorn"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(target, "node_modules", ".bin", "acorn")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../acorn/bin/acorn", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := InstallOpencode(installTestSource(t), &models.InstallRequest{}); err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if _, err := os.Stat(binTarget); err != nil {
		t.Fatalf("existing acorn was not preserved: %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(target, "opencode.json"))
	if err != nil || !strings.Contains(string(installed), "installed") {
		t.Fatalf("production install did not update the OpenCode artifact/state: %q, %v", installed, err)
	}
	if got, err := os.Readlink(link); err != nil || got != "../acorn/bin/acorn" {
		t.Fatalf("acorn bin link changed: %q, %v", got, err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".skynex-node_modules-quarantine-") {
			t.Fatalf("obsolete node_modules quarantine created: %s", entry.Name())
		}
	}
}

func installTestSource(t *testing.T) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(source, "opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "opencode", "opencode.json"), []byte(`{"installed":true}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return source
}

func installTestDeprecatedFile(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "opencode", "commands", "onboard.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("deprecated"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
