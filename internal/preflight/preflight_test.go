package preflight

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureWritableRejectsSymlinkedAncestorWithoutMutation(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	err := ensureWritable(filepath.Join(link, "new-dir"))
	if err == nil {
		t.Fatal("ensureWritable succeeded through a symlinked ancestor")
	}

	if _, statErr := os.Stat(filepath.Join(external, "new-dir")); !os.IsNotExist(statErr) {
		t.Fatalf("external directory was mutated: stat error = %v", statErr)
	}
}

func TestEnsureWritableAllowsNormalMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new-dir")

	if err := ensureWritable(dir); err != nil {
		t.Fatalf("ensureWritable failed: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat new directory: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("new path is not a directory")
	}
}

func TestEnsureWritableRejectsNonCleanPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	rawPath := root + string(filepath.Separator) + "dir" + string(filepath.Separator) + ".." + string(filepath.Separator) + "target"

	if err := ensureWritable(rawPath); err == nil {
		t.Fatal("expected ensureWritable to reject a non-clean path")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("target was created: stat error = %v", err)
	}
}

func TestEnsureWritablePreservesExistingProbeFile(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, ".skynex-preflight-check")
	const sentinel = "sentinel content"
	if err := os.WriteFile(probe, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	if err := ensureWritable(dir); err != nil {
		t.Fatalf("ensureWritable failed: %v", err)
	}

	content, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(content) != sentinel {
		t.Fatalf("sentinel content changed: got %q, want %q", content, sentinel)
	}
}

func TestReadOnlyDoesNotMutateMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing", "destination")
	if err, warning := checkWritable(dir, true); err != nil || warning == "" {
		t.Fatalf("checkWritable = (%v, %q), want warning only", err, warning)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("read-only check mutated directory: %v", err)
	}
}

func TestReadOnlyRejectsSymlinkWithoutMutation(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err, _ := checkWritable(filepath.Join(link, "new"), true); err == nil {
		t.Fatal("read-only check followed symlink")
	}
	if _, err := os.Lstat(filepath.Join(external, "new")); !os.IsNotExist(err) {
		t.Fatalf("external directory was mutated: %v", err)
	}
}
