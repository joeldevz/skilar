//go:build windows

package adapters

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The Windows implementation must retain a real directory handle.  Referencing
// file here is intentional: the !linux fallback must not satisfy this contract.
func requireWindowsInstallCwdHandle(t *testing.T, cwd *installCwd) *os.File {
	t.Helper()
	if cwd == nil || cwd.file == nil {
		t.Fatal("openInstallCwd returned no live directory handle")
	}
	return cwd.file
}

func TestOpenInstallCwdWindowsUsesExactPathAndLiveHandle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := openInstallCwd(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer cwd.Close()
	handle := requireWindowsInstallCwdHandle(t, cwd)

	cmd := exec.Command("cmd.exe", "/c", "if exist marker (exit /b 0) else (exit /b 1)")
	if err := runWithInstallCwd(cwd, cmd); err != nil {
		t.Fatalf("descriptor-backed command failed: %v", err)
	}
	if cmd.Dir != dir {
		t.Fatalf("command cwd = %q, want exact validated path %q", cmd.Dir, dir)
	}
	if _, err := handle.Stat(); err != nil {
		t.Fatalf("directory handle was not live through subprocess: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shared"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("directory handle did not share WRITE: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "shared")); err != nil || string(data) != "ok" {
		t.Fatalf("directory handle did not share READ: data=%q err=%v", data, err)
	}
	if err := cwd.verify(dir); err != nil {
		t.Fatalf("post-subprocess identity verification failed: %v", err)
	}
}

func TestOpenInstallCwdWindowsDoesNotShareDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := openInstallCwd(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requireWindowsInstallCwdHandle(t, cwd).Stat(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-replacement"); err == nil {
		t.Fatal("directory rename succeeded while install cwd handle was live; FILE_SHARE_DELETE must be absent")
	}
	if err := cwd.Close(); err != nil {
		t.Fatal(err)
	}
	if cwd.file != nil {
		t.Fatal("Close left the directory handle retained")
	}
	if err := os.Rename(dir, dir+"-replacement"); err != nil {
		t.Fatalf("directory could not be renamed after Close: %v", err)
	}
}

func TestOpenInstallCwdWindowsRejectsIdentityReplacement(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "target")
	replacement := filepath.Join(root, "replacement")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, dir); err != nil {
		t.Fatal(err)
	}
	if cwd, err := openInstallCwd(dir, identity); err == nil {
		_ = cwd.Close()
		t.Fatal("openInstallCwd accepted a replaced directory identity")
	}
}

func TestOpenInstallCwdWindowsRejectsReparsePointComponent(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(root, "external")
	link := filepath.Join(root, "junction")
	target := filepath.Join(link, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	// Replace the ordinary component with a junction.  Some restricted Windows
	// environments disallow creating reparse points; that limitation is not the
	// behavior under test.
	if err := os.RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, external).Run(); err != nil {
		t.Skipf("cannot create junction for reparse-point test: %v", err)
	}
	if err := os.Mkdir(filepath.Join(external, "target"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if cwd, err := openInstallCwd(target, identity); err == nil {
		_ = cwd.Close()
		t.Fatal("openInstallCwd accepted a path containing a reparse-point component")
	}
}
