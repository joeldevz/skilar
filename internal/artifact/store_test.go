package artifact

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/workflow"
)

func artifactFixture(t *testing.T) (*Store, *workflow.SQLiteStore) {
	t.Helper()
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
	db, err := workflow.OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	return &Store{DB: db.Database(), Root: filepath.Join(repo, "objects")}, db
}
func TestRedactionBeforePersistenceAndSecureModes(t *testing.T) {
	s, db := artifactFixture(t)
	defer db.Close()
	secret := "token=raw-secret password=hunter2 api_key=abcdef\n-----BEGIN PRIVATE KEY-----\nrawkey\n-----END PRIVATE KEY-----"
	r, err := s.Put("wf", "log", []byte(secret), false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(r.Path)
	if strings.Contains(string(raw), "raw-secret") || strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "rawkey") {
		t.Fatalf("secret persisted: %s", raw)
	}
	var blob string
	_ = db.Database().QueryRow(`SELECT path||digest FROM artifacts WHERE id=?`, r.ID).Scan(&blob)
	if strings.Contains(blob, "raw-secret") {
		t.Fatal("secret stored in db")
	}
	if info, _ := os.Stat(s.Root); info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode=%o", info.Mode().Perm())
	}
	if info, _ := os.Stat(r.Path); info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode=%o", info.Mode().Perm())
	}
}
func TestLimitsChunkingAndQuota(t *testing.T) {
	s, db := artifactFixture(t)
	defer db.Close()
	if _, err := s.Put("wf", "diff", make([]byte, MaxArtifact+1), false); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit=%v", err)
	}
	chunks, err := s.PutLog("wf", "log", make([]byte, MaxArtifact+1))
	if err != nil || len(chunks) != 2 {
		t.Fatalf("chunks=%d err=%v", len(chunks), err)
	}
	_, _ = db.Database().Exec(`INSERT INTO artifacts(id,workflow_id,kind,digest,size,path,authoritative,created_at,retain_until) VALUES('quota','full','diff','x',?,?,?,?,?)`, WorkflowQuota, s.Root+"/x", 0, time.Now().Format(time.RFC3339Nano), time.Now().Add(time.Hour).Format(time.RFC3339Nano))
	if _, err = s.Put("full", "diff", []byte("x"), false); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota=%v", err)
	}
}
func TestRetentionPruneGuardRevocationAndSymlinkExport(t *testing.T) {
	s, db := artifactFixture(t)
	defer db.Close()
	r, err := s.Put("wf", "log", []byte("safe"), false)
	if err != nil {
		t.Fatal(err)
	}
	if result, e := db.Database().Exec(`UPDATE artifacts SET retain_until=? WHERE id=?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), r.ID); e != nil {
		t.Fatal(e)
	} else if n, _ := result.RowsAffected(); n != 1 {
		t.Fatalf("updated=%d", n)
	}
	if err = s.Ref(r.ID, "receipt_authority", "wf"); err != nil {
		t.Fatal(err)
	}
	var refs int
	_ = db.Database().QueryRow(`SELECT COUNT(*) FROM artifact_refs WHERE artifact_id=?`, r.ID).Scan(&refs)
	if refs != 1 {
		t.Fatalf("refs=%d", refs)
	}
	if _, err = s.Prune(time.Now(), false); !errors.Is(err, ErrProtected) {
		t.Fatalf("guard=%v", err)
	}
	if _, err = s.Prune(time.Now(), true); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Put("wf", "log", []byte("export"), false)
	dest := filepath.Join(t.TempDir(), "out")
	target := filepath.Join(t.TempDir(), "target")
	_ = os.WriteFile(target, []byte("keep"), 0o600)
	_ = os.Symlink(target, dest)
	if err = s.Export(r.ID, dest, true); err == nil {
		t.Fatal("symlink export accepted")
	}
	raw, _ := os.ReadFile(target)
	if !bytes.Equal(raw, []byte("keep")) {
		t.Fatal("symlink target changed")
	}
}
