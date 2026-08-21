package safefs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRelativeRejectsTraversalAndAbsoluteNames(t *testing.T) {
	for _, name := range []string{"../escape", "a/../../escape", "/absolute", "a//b", ""} {
		if _, err := Relative(name); err == nil {
			t.Errorf("Relative(%q) accepted unsafe path", name)
		}
	}
}

func TestOpenOrCreateRejectsSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOrCreate(link, 0o700); err == nil {
		t.Fatal("expected symlinked root rejection")
	}
}

func TestReadFileVerifiedRejectsHardlink(t *testing.T) {
	rootPath := t.TempDir()
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(rootPath, "source"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(rootPath, "source"), filepath.Join(rootPath, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileVerified(root, "source", 1); err == nil {
		t.Fatal("expected hardlink rejection")
	}
}

func TestRemoveDoesNotDeleteReplacementAfterIdentityValidation(t *testing.T) {
	rootPath := t.TempDir()
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	path := filepath.Join(rootPath, "owned")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspected, err := root.Lstat("owned")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StageVerified(root, "owned", ".", "", func(string) error {
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("replacement"), 0o600)
	}); err == nil {
		t.Fatal("replacement was staged")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "replacement" {
		t.Fatalf("replacement was removed after validation: %q, %v (inspected %v)", got, err, inspected)
	}
}

func TestCopyAtomicInfoRunsAfterRenameAndReturnsRestoredIdentity(t *testing.T) {
	rootPath := t.TempDir()
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	const restored = "restored bytes"
	const attacker = "attacker bytes"
	callbackCalled := false
	var restoredIdentity os.FileInfo
	identity, err := CopyAtomicInfo(root, "target", bytes.NewReader([]byte(restored)), 0o600, ".tmp-", func() error {
		callbackCalled = true
		path := filepath.Join(rootPath, "target")
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if string(got) != restored {
			t.Fatalf("callback saw %q, want restored bytes", got)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("restored mode = %o, want 600", mode)
		}
		restoredIdentity = info
		tmp := filepath.Join(rootPath, ".attacker")
		if err := os.WriteFile(tmp, []byte(attacker), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !callbackCalled {
		t.Fatal("after-rename callback was not called")
	}
	if restoredIdentity == nil || !os.SameFile(identity, restoredIdentity) {
		t.Fatal("CopyAtomicInfo returned the attacker identity instead of the restored identity")
	}
	got, err := os.ReadFile(filepath.Join(rootPath, "target"))
	if err != nil || string(got) != attacker {
		t.Fatalf("final target = %q, %v; want attacker bytes", got, err)
	}
	info, err := os.Stat(filepath.Join(rootPath, "target"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Fatalf("attacker mode = %o, want 644", mode)
	}
}

func TestReadFileVerifiedAcceptsExactLimitAndRejectsOversize(t *testing.T) {
	rootPath := t.TempDir()
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(rootPath, "source"), []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFileVerified(root, "source", 4)
	if err != nil || string(got) != "1234" {
		t.Fatalf("exact-limit read = %q, %v", got, err)
	}
	if _, err := ReadFileVerified(root, "source", 3); err == nil {
		t.Fatal("oversize read was accepted")
	}
}

func TestReadFileVerifiedRejectsInvalidLimit(t *testing.T) {
	root, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := ReadFileVerified(root, "source", 0); err == nil {
		t.Fatal("zero read limit was accepted")
	}
}

func TestValidNestedOperationsAndSymlinkMutation(t *testing.T) {
	rootPath := t.TempDir()
	root, err := Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := Ensure(root, "nested/deeper", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(root, "nested/deeper/file", []byte("safe"), 0o600, ".tmp-"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFileVerified(root, "nested/deeper/file", 4)
	if err != nil || string(got) != "safe" {
		t.Fatalf("read valid nested file: %q, %v", got, err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(rootPath, "nested", "link")); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(root, "nested/link/escaped", []byte("no"), 0o600, ".tmp-"); err == nil {
		t.Fatal("nested symlink mutation was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped")); !os.IsNotExist(err) {
		t.Fatal("nested symlink was followed")
	}
	if err := Remove(root, "nested/deeper/file"); err != nil {
		t.Fatal(err)
	}
}
