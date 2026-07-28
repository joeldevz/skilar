package adapters

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyDirRejectsSymlinkedDescendant(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	external := filepath.Join(t.TempDir(), "external")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "payload.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dst, "nested")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := copyDir(src, dst); err == nil {
		t.Fatal("expected copyDir to reject a symlinked destination descendant")
	}
	if _, err := os.Stat(filepath.Join(external, "payload.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside payload was created through symlink: stat error %v", err)
	}
}

func TestCopyFileRejectsSymlinkedDestination(t *testing.T) {
	src := filepath.Join(t.TempDir(), "source.txt")
	external := filepath.Join(t.TempDir(), "sentinel.txt")
	dst := filepath.Join(t.TempDir(), "destination.txt")
	if err := os.WriteFile(src, []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(external, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, dst); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := copyFile(src, dst); err == nil {
		t.Fatal("expected copyFile to reject a symlinked destination")
	}
	content, err := os.ReadFile(external)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "sentinel" {
		t.Fatalf("sentinel content changed: got %q, want %q", content, "sentinel")
	}
}

func TestWriteFileRejectsSymlinkedDestination(t *testing.T) {
	external := filepath.Join(t.TempDir(), "sentinel.txt")
	dst := filepath.Join(t.TempDir(), "destination.txt")
	if err := os.WriteFile(external, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, dst); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := writeFile(dst, "new content"); err == nil {
		t.Fatal("expected writeFile to reject a symlinked destination")
	}
	content, err := os.ReadFile(external)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "sentinel" {
		t.Fatalf("sentinel content changed: got %q, want %q", content, "sentinel")
	}
}

func TestCopyFileRejectsNonCleanDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	candidate := filepath.Join(root, "target")
	sentinel := filepath.Join(root, "sentinel")
	const candidateContent = "candidate must remain unchanged"
	const sentinelContent = "sentinel must remain unchanged"

	if err := os.WriteFile(source, []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte(candidateContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte(sentinelContent), 0o644); err != nil {
		t.Fatal(err)
	}

	rawDestination := root + string(filepath.Separator) + "dir" + string(filepath.Separator) + ".." + string(filepath.Separator) + "target"
	if err := copyFile(source, rawDestination); err == nil {
		t.Fatal("expected copyFile to reject a non-clean destination")
	}

	got, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != candidateContent {
		t.Fatalf("candidate content changed: got %q, want %q", got, candidateContent)
	}
	got, err = os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinelContent {
		t.Fatalf("sentinel content changed: got %q, want %q", got, sentinelContent)
	}
}

func TestWriteFileRejectsNonCleanDestination(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "target")
	sentinel := filepath.Join(root, "sentinel")
	const candidateContent = "candidate must remain unchanged"
	const sentinelContent = "sentinel must remain unchanged"

	if err := os.WriteFile(candidate, []byte(candidateContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte(sentinelContent), 0o644); err != nil {
		t.Fatal(err)
	}

	rawDestination := root + string(filepath.Separator) + "dir" + string(filepath.Separator) + ".." + string(filepath.Separator) + "target"
	if err := writeFile(rawDestination, "new content"); err == nil {
		t.Fatal("expected writeFile to reject a non-clean destination")
	}

	got, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != candidateContent {
		t.Fatalf("candidate content changed: got %q, want %q", got, candidateContent)
	}
	got, err = os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinelContent {
		t.Fatalf("sentinel content changed: got %q, want %q", got, sentinelContent)
	}
}

func TestCopyDirAllowsValidNestedCopy(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	want := "nested content"
	if err := os.MkdirAll(filepath.Join(src, "one", "two"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "one", "two", "payload.txt"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir failed for a valid nested tree: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(dst, "one", "two", "payload.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != want {
		t.Fatalf("copied content = %q, want %q", content, want)
	}
}

func TestValidateInstallDestinationTreeRejectsDescendantSymlink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "install")
	external := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "safe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "safe", "redirect")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := validateInstallDestinationTree(root); err == nil {
		t.Fatal("expected install destination tree with descendant symlink to be rejected")
	}
}

func TestValidateInstallDestinationTreeAllowsSymlinkFreeTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "install")
	if err := os.MkdirAll(filepath.Join(root, "safe", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateInstallDestinationTree(root); err != nil {
		t.Fatalf("symlink-free install tree was rejected: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "missing", "nested")
	if err := validateInstallDestinationTree(missing); err != nil {
		t.Fatalf("missing safe install tree was rejected: %v", err)
	}
}
