package skillsync

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

//go:embed testdata/embedbundle
var embeddedBundle embed.FS

// Release binaries ship the skills bundle as an embed.FS, whose FileInfo values
// carry neither a link count nor an inode identity. Verified reads must still
// succeed for them or every install reapplies and snapshots.
func TestEmbeddedBundleIsReadableAndInstallable(t *testing.T) {
	bundle, err := fs.Sub(embeddedBundle, "testdata/embedbundle")
	if err != nil {
		t.Fatal(err)
	}
	files, err := bundleFiles(bundle)
	if err != nil {
		t.Fatalf("embedded bundle rejected: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%v", files)
	}
	target := filepath.Join(t.TempDir(), "skills")
	manifest := filepath.Join(t.TempDir(), "skills.ownership.json")
	session, err := NewSession(bundle, target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	report, err := session.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Apply(report, nil, Manifest{Source: "opencode/skills", SourceKind: "bundle", BundleVersion: "latest", BundleCommit: "embedded", Package: "skills", Target: "opencode"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(target, "nested", "child.md"))
	if err != nil || string(raw) != "nested body\n" {
		t.Fatalf("installed=%q err=%v", raw, err)
	}
	second, err := Inspect(bundle, target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, entry := range second.Entries {
		if entry.Status != Current {
			t.Fatalf("reinstall is not a no-op: %+v", entry)
		}
	}
}
