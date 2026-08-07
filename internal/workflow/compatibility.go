package workflow

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
)

const CurrentSchemaVersion = 19

// CompatibilityError is safe for both humans and automation. Commands may
// inspect its fields with errors.As instead of parsing SQLite's error text.
type CompatibilityError struct {
	DatabasePath   string   `json:"database_path"`
	DatabaseSchema int      `json:"database_schema"`
	SupportedMin   int      `json:"binary_supported_schema_min"`
	SupportedMax   int      `json:"binary_supported_schema_max"`
	MissingObjects []string `json:"missing_objects,omitempty"`
	Hint           string   `json:"hint"`
}

func (e *CompatibilityError) Error() string {
	raw, _ := json.Marshal(e)
	return "workflow database compatibility error: " + string(raw)
}

func compatibilityError(path string, version int, missing []string) error {
	hint := fmt.Sprintf("use a Skynex binary compatible with workflow schema %d", version)
	if version < CurrentSchemaVersion {
		hint = fmt.Sprintf("run a writable workflow command with this Skynex binary to migrate schema %d to %d", version, CurrentSchemaVersion)
	} else if version > CurrentSchemaVersion {
		hint = fmt.Sprintf("upgrade Skynex or use the binary that created workflow schema %d", version)
	} else if len(missing) != 0 {
		hint = "use the binary that created this database or restore/migrate it; the declared schema does not match its required objects"
	}
	return &CompatibilityError{DatabasePath: filepath.Clean(path), DatabaseSchema: version, SupportedMin: CurrentSchemaVersion, SupportedMax: CurrentSchemaVersion, MissingObjects: missing, Hint: hint}
}

var currentSchemaObjects = []string{
	"execution_contract_revisions",
	"replan_revisions",
	"verification_contract_revisions",
	"verification_run_history",
}

func validateCurrentSchema(db *sql.DB, path string, version int) error {
	if version != CurrentSchemaVersion {
		return compatibilityError(path, version, nil)
	}
	missing := make([]string, 0)
	for _, name := range currentSchemaObjects {
		var found string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&found)
		if err == sql.ErrNoRows {
			missing = append(missing, name)
			continue
		}
		if err != nil {
			return err
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return compatibilityError(path, version, missing)
	}
	return nil
}

// CompatibilityJSON preserves the typed diagnostic when a caller wants a
// machine-readable report without opening the database for mutation.
func CompatibilityJSON(err error) ([]byte, bool) {
	var compatibility *CompatibilityError
	if !errors.As(err, &compatibility) {
		return nil, false
	}
	raw, marshalErr := json.MarshalIndent(compatibility, "", "  ")
	return raw, marshalErr == nil
}
