package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/safefs"
)

// Snapshot describes one retained recovery snapshot. Unknown and corrupt
// snapshots are deliberately visible, but are never eligible for pruning.
type Snapshot struct {
	ID              string
	CreatedAt       time.Time
	Status          string
	Size            int64
	FileCount       int
	Restorable      bool
	EligibleToPrune bool
}

const (
	snapshotStatusCommitted = "committed"
	snapshotStatusRecovery  = "recovery-needed"
	snapshotStatusUnknown   = "unknown"
	maxInventoryEntries     = 10000
)

// ListSnapshots inventories only strict snapshot IDs below stateDir. All
// manifest and size reads happen through retained rooted descriptors.
func ListSnapshots(stateDir string) ([]Snapshot, error) {
	if !isCleanAbsolute(stateDir) {
		return nil, fmt.Errorf("snapshot inventory: invalid state directory %q", stateDir)
	}
	state, err := safefs.Open(stateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer state.Close()
	base, err := state.OpenRoot("snapshots")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer base.Close()
	entries, err := fs.ReadDir(base.FS(), ".")
	if err != nil {
		return nil, err
	}
	if len(entries) > maxInventoryEntries {
		return nil, fmt.Errorf("snapshot inventory exceeds %d entries", maxInventoryEntries)
	}
	result := make([]Snapshot, 0, len(entries))
	for _, entry := range entries {
		if !validSnapshotID(entry.Name()) || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		item := Snapshot{ID: entry.Name(), Status: snapshotStatusUnknown}
		root, openErr := base.OpenRoot(entry.Name())
		if openErr != nil {
			result = append(result, item)
			continue
		}
		manifest, readErr := safefs.ReadFileVerified(root, "manifest.json", maxSnapshotManifestBytes)
		var meta snapshotManifest
		if readErr == nil {
			readErr = json.Unmarshal(manifest, &meta)
		}
		if readErr == nil {
			item.Status = meta.Status
			if item.Status == "" {
				item.Status = snapshotStatusUnknown
			}
			if parsed, parseErr := time.Parse(time.RFC3339Nano, meta.CreatedAt); parseErr == nil {
				item.CreatedAt = parsed
			}
			item.Restorable = meta.Version == 1 && len(meta.Entries) <= maxSnapshotEntries
		}
		item.Size, item.FileCount = snapshotSize(root)
		if item.CreatedAt.IsZero() {
			if info, infoErr := entry.Info(); infoErr == nil {
				item.CreatedAt = info.ModTime()
			}
		}
		item.EligibleToPrune = readErr == nil && item.Restorable && item.Status == snapshotStatusCommitted
		_ = root.Close()
		result = append(result, item)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func validSnapshotID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func snapshotSize(root *safefs.Root) (int64, int) {
	var size int64
	files := 0
	_ = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, statErr := entry.Info()
		if statErr == nil && info.Mode().IsRegular() {
			size += info.Size()
			files++
		}
		return nil
	})
	return size, files
}

// PruneSnapshots removes at most count oldest eligible snapshots, using the
// retained snapshots root and the explicitly selected identity-safe IDs.
func PruneSnapshots(stateDir string, count int) (int, error) {
	if count < 0 {
		return 0, errors.New("snapshot prune count cannot be negative")
	}
	items, err := ListSnapshots(stateDir)
	if err != nil {
		return 0, err
	}
	state, err := safefs.Open(stateDir)
	if err != nil {
		return 0, err
	}
	defer state.Close()
	base, err := state.OpenRoot("snapshots")
	if err != nil {
		return 0, err
	}
	defer base.Close()
	removed := 0
	for _, item := range items {
		if removed >= count {
			break
		}
		if !item.EligibleToPrune {
			continue
		}
		info, statErr := base.Lstat(item.ID)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := base.RemoveAll(item.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
