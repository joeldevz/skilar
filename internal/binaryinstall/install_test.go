package binaryinstall

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstallRejectsSymlinkAndHardlinkDestinations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hard-link test uses Unix stat metadata")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if err := Install(source, link); err == nil {
		t.Fatal("expected symlink destination rejection")
	}
	hard := filepath.Join(root, "hard")
	if err := os.Link(source, hard); err != nil {
		t.Fatal(err)
	}
	if err := Install(source, hard); err == nil {
		t.Fatal("expected hard-link destination rejection")
	}
}

func TestInstallAtomicAndExecutable(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "nested", "skynex")
	if err := os.WriteFile(source, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Install(source, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 || string(mustRead(t, destination)) != "binary" {
		t.Fatalf("unexpected installed file: mode=%o", info.Mode().Perm())
	}
	if err := os.WriteFile(source, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Install(source, destination); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, destination)) != "replacement" {
		t.Fatal("existing destination was not atomically replaced")
	}
	entries, err := os.ReadDir(filepath.Dir(destination))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) >= len(".skynex-install-") && entry.Name()[:len(".skynex-install-")] == ".skynex-install-" {
			t.Fatalf("temporary install file leaked: %s", entry.Name())
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
