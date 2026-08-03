package adapters

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCopyFileRaceReplacingFinalEntryCannotTouchExternalTarget(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	external := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(external, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeFileMutation = func() error {
		if err := os.Remove(destination); err != nil {
			return err
		}
		return os.Symlink(external, destination)
	}
	t.Cleanup(func() { beforeFileMutation = nil })
	if err := copyFile(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(external)
	if err != nil || string(got) != "sentinel" {
		t.Fatalf("external target changed: %q, %v", got, err)
	}
	got, err = os.ReadFile(destination)
	if err != nil || string(got) != "new" {
		t.Fatalf("destination was not replaced atomically: %q, %v", got, err)
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

func TestValidateInstallDestinationTreeValidatesManagedNodeModules(t *testing.T) {
	root := filepath.Join(t.TempDir(), "opencode")
	if err := os.MkdirAll(filepath.Join(root, "node_modules", ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "acorn", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "acorn", "bin", "acorn"), []byte("acorn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "acorn", "bin", "acorn"), filepath.Join(root, "node_modules", ".bin", "acorn")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("missing/unsafe/target", filepath.Join(root, "node_modules", "acorn", "bin", "internal-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := validateInstallDestinationTree(root); err == nil || !strings.Contains(err.Error(), "contains symlink") {
		t.Fatalf("malicious nested node_modules link accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "node_modules", "acorn", "bin", "internal-link")); err != nil {
		t.Fatal(err)
	}
	if err := validateInstallDestinationTree(root); err != nil {
		t.Fatalf("real npm-style node_modules link rejected: %v", err)
	}
}

func TestValidateInstallDestinationTreeRejectsSymlinkRootAndAncestor(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := validateInstallDestinationTree(link); err == nil {
		t.Fatal("expected symlink root to be rejected")
	}
	if err := validateInstallDestinationTree(filepath.Join(link, "child")); err == nil {
		t.Fatal("expected symlink ancestor to be rejected")
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

func TestValidateInstallDestinationTreeRejectsNodeModulesSymlinkAndFile(t *testing.T) {
	base := t.TempDir()
	for _, setup := range []func(string) error{
		func(path string) error { return os.Symlink(filepath.Join(base, "external"), path) },
		func(path string) error { return os.WriteFile(path, []byte("not a directory"), 0o600) },
	} {
		root := filepath.Join(base, t.Name())
		if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		nodeModules := filepath.Join(root, "node_modules")
		if err := setup(nodeModules); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := validateInstallDestinationTree(root); err == nil {
			t.Fatal("unsafe node_modules entry accepted")
		}
		_ = os.RemoveAll(root)
	}
}

func TestValidateInstallDestinationChecksSymlinkBeforeMissingLeaf(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := validateInstallDestination(filepath.Join(link, "missing", "leaf")); err == nil {
		t.Fatal("missing leaf hid symlink ancestor")
	}
}
