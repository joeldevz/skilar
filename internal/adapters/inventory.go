package adapters

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/joeldevz/skynex/internal/safefs"
)

type inventory struct {
	Files map[string]string `json:"files"`
}

const inventoryName = ".skynex-manifest.json"
const maxOwnedFileBytes = 4 << 20

func fileDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func loadInventory(target string) inventory {
	root, err := safefs.Open(target)
	if err != nil {
		return inventory{Files: map[string]string{}}
	}
	defer root.Close()
	return loadInventoryFromRoot(root)
}

func loadInventoryFromRoot(root *safefs.Root) inventory {
	value := inventory{Files: map[string]string{}}
	if raw, err := safefs.ReadFileVerified(root, inventoryName, maxOwnedFileBytes); err == nil {
		_ = json.Unmarshal(raw, &value)
	}
	if value.Files == nil {
		value.Files = map[string]string{}
	}
	return value
}

func installOwnedTree(source, target string) error {
	return installOwnedTreeExcluding(source, target, nil, discardReporter())
}

// installOwnedTreeExcluding mirrors source into target through a retained root
// descriptor. Manifest keys are attacker-influenced, so every one of them is
// re-validated as a confined relative path before it is read, written, or
// removed.
func installOwnedTreeExcluding(source, target string, excluded map[string]bool, reporter Reporter) error {
	if reporter == nil {
		reporter = discardReporter()
	}
	root, err := safefs.OpenOrCreate(target, 0o700)
	if err != nil {
		return err
	}
	defer root.Close()
	old := loadInventoryFromRoot(root)
	next := inventory{Files: map[string]string{}}
	err = filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(source, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if excluded[relSlash] {
			return nil
		}
		if d.IsDir() && (rel == "skills" || rel == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if rel == inventoryName {
			return nil
		}
		clean, relErr := safefs.Relative(relSlash)
		if relErr != nil {
			return relErr
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := fileDigest(raw)
		dest := filepath.Join(target, filepath.FromSlash(clean))
		write := true
		info, statErr := root.Lstat(clean)
		switch {
		case statErr == nil:
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("destination is a symlink: %s", dest)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("destination is not a regular file: %s", dest)
			}
			current, readErr := safefs.ReadFileVerified(root, clean, maxOwnedFileBytes)
			if readErr != nil {
				return readErr
			}
			if clean != "opencode.json" {
				if owned, ok := old.Files[clean]; ok {
					if fileDigest(current) != owned {
						reporter.Detail("    Preserving modified managed file: %s", dest)
						write = false
					}
				} else {
					reporter.Detail("    Preserving unknown existing file: %s", dest)
					write = false
				}
			}
		case !errors.Is(statErr, fs.ErrNotExist):
			return statErr
		}
		if write {
			if err = safefs.WriteAtomic(root, clean, raw, 0o600, ".skynex-owned-"); err != nil {
				return err
			}
			next.Files[clean] = digest
		} else if owned, ok := old.Files[clean]; ok {
			next.Files[clean] = owned
		}
		return nil
	})
	if err != nil {
		return err
	}
	for rel, digest := range old.Files {
		if _, still := next.Files[rel]; still {
			continue
		}
		clean, relErr := safefs.Relative(rel)
		if relErr != nil {
			reporter.Warning("ignoring out-of-tree manifest entry %q", rel)
			continue
		}
		info, statErr := root.Lstat(clean)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		raw, readErr := safefs.ReadFileVerified(root, clean, maxOwnedFileBytes)
		if readErr != nil {
			continue
		}
		path := filepath.Join(target, filepath.FromSlash(clean))
		if fileDigest(raw) == digest {
			if err = safefs.Remove(root, clean); err != nil {
				return err
			}
		} else {
			reporter.Detail("    Preserving modified retired file: %s", path)
		}
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return safefs.WriteAtomic(root, inventoryName, raw, 0o600, ".skynex-owned-")
}
