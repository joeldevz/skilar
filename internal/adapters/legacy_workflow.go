package adapters

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

func archiveInactiveLegacyWorkflowDB(target string) error {
	for _, name := range []string{"workflow.sqlite", "workflows.sqlite"} {
		path := filepath.Join(target, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return err
		}
		var active int
		err = db.QueryRow(`SELECT COUNT(*) FROM workflows WHERE state NOT IN ('delivered','aborted','failed')`).Scan(&active)
		_ = db.Close()
		if err != nil {
			return fmt.Errorf("inspect legacy workflow database %s: %w", path, err)
		}
		if active > 0 {
			return fmt.Errorf("active legacy workflow detected in %s; complete or abort it, or export it in human-readable form before upgrade", path)
		}
		archive := path + ".archive-" + time.Now().UTC().Format("20060102T150405Z")
		if err = os.Rename(path, archive); err != nil {
			return err
		}
		if err = os.Chmod(archive, 0o600); err != nil {
			return err
		}
		fmt.Printf("    Archived inactive legacy workflow database: %s\n", archive)
	}
	return nil
}
