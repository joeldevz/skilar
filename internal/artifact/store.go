package artifact

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const MaxArtifact = 16 << 20
const WorkflowQuota = 256 << 20

var ErrLimit = errors.New("artifact: size limit exceeded")
var ErrQuota = errors.New("artifact: workflow quota exceeded")
var ErrProtected = errors.New("artifact: protected by receipt authority")

type Store struct {
	DB       *sql.DB
	Root     string
	Patterns []*regexp.Regexp
}
type Record struct {
	ID, WorkflowID, Kind, Digest, Path string
	Size                               int64
	Authoritative                      bool
	CreatedAt, RetainUntil             time.Time
}

var secrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(token|password|secret|api[_-]?key)\s*[=:]\s*[^\s,;]+`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
}

func (s *Store) redact(raw []byte) []byte {
	out := append([]byte(nil), raw...)
	for _, p := range append(secrets, s.Patterns...) {
		out = p.ReplaceAll(out, []byte("[REDACTED]"))
	}
	return out
}
func (s *Store) Redact(raw []byte) []byte { return s.redact(raw) }
func retention(kind string, now time.Time) time.Time {
	if kind == "log" || kind == "trace" {
		return now.Add(30 * 24 * time.Hour)
	}
	return now.Add(90 * 24 * time.Hour)
}
func (s *Store) Put(workflowID, kind string, raw []byte, authoritative bool) (Record, error) {
	if len(raw) > MaxArtifact {
		return Record{}, ErrLimit
	}
	data := s.redact(raw)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	id := "artifact_" + digest
	var used int64
	if !authoritative {
		_ = s.DB.QueryRow(`SELECT COALESCE(SUM(size),0) FROM artifacts WHERE workflow_id=? AND authoritative=0`, workflowID).Scan(&used)
		if used+int64(len(data)) > WorkflowQuota {
			return Record{}, ErrQuota
		}
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return Record{}, err
	}
	if err := os.Chmod(s.Root, 0o700); err != nil {
		return Record{}, err
	}
	path := filepath.Join(s.Root, digest)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return Record{}, errors.New("artifact: unsafe object path")
		}
	} else if os.IsNotExist(err) {
		tmp, err := os.CreateTemp(s.Root, ".artifact-")
		if err != nil {
			return Record{}, err
		}
		_ = tmp.Chmod(0o600)
		if _, err = tmp.Write(data); err != nil {
			tmp.Close()
			return Record{}, err
		}
		if err = tmp.Close(); err != nil {
			return Record{}, err
		}
		if err = os.Rename(tmp.Name(), path); err != nil {
			return Record{}, err
		}
		_ = os.Chmod(path, 0o600)
	} else {
		return Record{}, err
	}
	now := time.Now().UTC()
	until := retention(kind, now)
	auth := 0
	if authoritative {
		auth = 1
	}
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO artifacts(id,workflow_id,kind,digest,size,path,authoritative,created_at,retain_until) VALUES(?,?,?,?,?,?,?,?,?)`, id, workflowID, kind, digest, len(data), path, auth, now.Format(time.RFC3339Nano), until.Format(time.RFC3339Nano))
	return Record{id, workflowID, kind, digest, path, int64(len(data)), authoritative, now, until}, err
}
func (s *Store) PutLog(workflowID, kind string, raw []byte) ([]Record, error) {
	var out []Record
	for len(raw) > 0 {
		n := len(raw)
		if n > MaxArtifact {
			n = MaxArtifact
		}
		r, err := s.Put(workflowID, kind, raw[:n], false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
		raw = raw[n:]
	}
	if len(out) == 0 {
		r, err := s.Put(workflowID, kind, nil, false)
		if err != nil {
			return nil, err
		}
		out = []Record{r}
	}
	return out, nil
}
func (s *Store) Ref(id, ownerKind, ownerID string) error {
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO artifact_refs(artifact_id,owner_kind,owner_id) VALUES(?,?,?)`, id, ownerKind, ownerID)
	return err
}
func (s *Store) Export(id, destination string, detailed bool) error {
	var path string
	if err := s.DB.QueryRow(`SELECT path FROM artifacts WHERE id=?`, id).Scan(&path); err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if detailed {
		raw = s.redact(raw)
	}
	dir := filepath.Dir(destination)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	if info, e := os.Lstat(destination); e == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("artifact: unsafe export destination")
	}
	tmp, err := os.CreateTemp(dir, ".export-")
	if err != nil {
		return err
	}
	_ = tmp.Chmod(0o600)
	if _, err = tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), destination)
}
func (s *Store) Prune(now time.Time, revokeAuthority bool) (int, error) {
	rows, err := s.DB.Query(`SELECT id,path,authoritative FROM artifacts WHERE retain_until<?`, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type item struct {
		id, path string
		auth     bool
	}
	var items []item
	for rows.Next() {
		var x item
		if err = rows.Scan(&x.id, &x.path, &x.auth); err != nil {
			return 0, err
		}
		items = append(items, x)
	}
	count := 0
	for _, x := range items {
		var protected int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM artifact_refs WHERE artifact_id=? AND owner_kind='receipt_authority'`, x.id).Scan(&protected)
		if x.auth || protected > 0 {
			if !revokeAuthority {
				return count, ErrProtected
			}
			_, _ = s.DB.Exec(`DELETE FROM receipt_authority WHERE workflow_id IN (SELECT owner_id FROM artifact_refs WHERE artifact_id=? AND owner_kind='receipt_authority')`, x.id)
		}
		tx, e := s.DB.Begin()
		if e != nil {
			return count, e
		}
		if _, e = tx.Exec(`DELETE FROM artifact_refs WHERE artifact_id=?`, x.id); e == nil {
			_, e = tx.Exec(`DELETE FROM artifacts WHERE id=?`, x.id)
		}
		if e != nil {
			tx.Rollback()
			return count, e
		}
		if e = tx.Commit(); e != nil {
			return count, e
		}
		_ = os.Remove(x.path)
		count++
	}
	return count, nil
}
func (s *Store) Read(id string) ([]byte, error) {
	var path string
	if err := s.DB.QueryRow(`SELECT path FROM artifacts WHERE id=?`, id).Scan(&path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("artifact read: %w", err)
	}
	return raw, nil
}
