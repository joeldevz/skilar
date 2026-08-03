package adapters

import (
	"database/sql"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallOwnedTreeFreshUpdateRetireAndPreserveModified(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	_ = os.MkdirAll(src, 0o700)
	_ = os.WriteFile(filepath.Join(src, "managed.md"), []byte("v1"), 0o600)
	if err := installOwnedTree(src, dst); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(src, "managed.md"), []byte("v2"), 0o600)
	if err := installOwnedTree(src, dst); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dst, "managed.md"))
	if string(raw) != "v2" {
		t.Fatalf("update=%q", raw)
	}
	_ = os.WriteFile(filepath.Join(dst, "managed.md"), []byte("user edit"), 0o600)
	_ = os.Remove(filepath.Join(src, "managed.md"))
	if err := installOwnedTree(src, dst); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(dst, "managed.md"))
	if string(raw) != "user edit" {
		t.Fatalf("modified retired file lost=%q", raw)
	}
}

func TestLegacyWorkflowDatabaseActiveStopsInactiveArchives(t *testing.T) {
	target := t.TempDir()
	create := func(path, state string) {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`CREATE TABLE workflows(state TEXT); INSERT INTO workflows(state) VALUES(?)`, state)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	active := filepath.Join(target, "workflow.sqlite")
	create(active, "executing")
	if err := archiveInactiveLegacyWorkflowDB(target); err == nil {
		t.Fatal("active legacy workflow accepted")
	}
	_ = os.Remove(active)
	inactive := filepath.Join(target, "workflows.sqlite")
	create(inactive, "delivered")
	if err := archiveInactiveLegacyWorkflowDB(target); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(inactive + ".archive-*")
	if len(matches) != 1 {
		t.Fatalf("archives=%v", matches)
	}
	if info, _ := os.Stat(matches[0]); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}
func TestLegacyExactDigestPreservesModified(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".config", "opencode")
	path := filepath.Join(root, "commands", "linear.md")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte("custom"), 0o600)
	removed, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: path, Root: root, Target: "opencode", ExpectedDigest: legacyExactDigests["commands/linear.md"]}})
	if err != nil || removed != 0 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("modified legacy file removed")
	}
}
