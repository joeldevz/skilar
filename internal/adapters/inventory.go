package adapters

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type inventory struct {
	Files map[string]string `json:"files"`
}

const inventoryName = ".skynex-manifest.json"

func fileDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func loadInventory(root string) inventory {
	value := inventory{Files: map[string]string{}}
	raw, err := os.ReadFile(filepath.Join(root, inventoryName))
	if err == nil {
		_ = json.Unmarshal(raw, &value)
	}
	if value.Files == nil {
		value.Files = map[string]string{}
	}
	return value
}

func installOwnedTree(source, target string) error {
	return installOwnedTreeExcluding(source, target, nil)
}

func installOwnedTreeExcluding(source, target string, excluded map[string]bool) error {
	old := loadInventory(target)
	next := inventory{Files: map[string]string{}}
	err := filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(source, path)
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
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := fileDigest(raw)
		dest := filepath.Join(target, rel)
		write := true
		if current, e := os.ReadFile(dest); e == nil && rel != "opencode.json" {
			currentDigest := fileDigest(current)
			if owned, ok := old.Files[relSlash]; ok {
				if currentDigest != owned {
					fmt.Printf("    Preserving modified managed file: %s\n", dest)
					write = false
				}
			} else {
				fmt.Printf("    Preserving unknown existing file: %s\n", dest)
				write = false
			}
		}
		if write {
			if err = os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				return err
			}
			if err = os.WriteFile(dest, raw, 0o600); err != nil {
				return err
			}
			next.Files[relSlash] = digest
		} else if owned, ok := old.Files[relSlash]; ok {
			next.Files[relSlash] = owned
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
		path := filepath.Join(target, filepath.FromSlash(rel))
		raw, e := os.ReadFile(path)
		if e == nil && fileDigest(raw) == digest {
			_ = os.Remove(path)
		} else if e == nil {
			fmt.Printf("    Preserving modified retired file: %s\n", path)
		}
	}
	raw, _ := json.MarshalIndent(next, "", "  ")
	raw = append(raw, '\n')
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(target, inventoryName), raw, 0o600)
}
