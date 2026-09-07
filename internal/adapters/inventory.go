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
	"strings"

	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/safefs"
	"gopkg.in/yaml.v3"
)

type inventory struct {
	Files map[string]string `json:"files"`
}

const inventoryName = ".skynex-manifest.json"
const maxOwnedFileBytes = 4 << 20

// refreshInventoryDigest records a post-install mutation without replacing
// the manifest non-atomically.
func refreshInventoryDigest(target, path string) error {
	root, err := safefs.Open(target)
	if err != nil {
		return err
	}
	defer root.Close()
	value := loadInventoryFromRoot(root)
	raw, err := safefs.ReadFileVerified(root, path, maxOwnedFileBytes)
	if err != nil {
		return err
	}
	value.Files[path] = fileDigest(raw)
	manifest, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return safefs.WriteAtomic(root, inventoryName, append(manifest, '\n'), 0o600, ".skynex-owned-")
}

func validateCommittedDependencyMetadata(dir, manager string) error {
	root, err := safefs.Open(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	packageRaw, err := safefs.ReadFileVerified(root, "package.json", maxOwnedFileBytes)
	if err != nil {
		return fmt.Errorf("read package.json: %w", err)
	}
	var packageValue struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(packageRaw, &packageValue); err != nil {
		return fmt.Errorf("invalid package.json: %w", err)
	}
	lockName := map[string]string{"bun": "bun.lock", "pnpm": "pnpm-lock.yaml", "npm": "package-lock.json"}[manager]
	lockRaw, err := safefs.ReadFileVerified(root, lockName, maxOwnedFileBytes)
	if err != nil {
		return fmt.Errorf("missing committed %s: %w", lockName, err)
	}
	var locked map[string]string
	switch manager {
	case "bun":
		var value struct {
			Workspaces map[string]struct {
				Dependencies map[string]string `json:"dependencies"`
			} `json:"workspaces"`
		}
		if err := json.Unmarshal(lockRaw, &value); err != nil {
			return fmt.Errorf("invalid %s: %w", lockName, err)
		}
		locked = value.Workspaces[""].Dependencies
	case "pnpm":
		var value struct {
			Importers map[string]struct {
				Dependencies map[string]struct {
					Specifier string `yaml:"specifier"`
					Version   string `yaml:"version"`
				} `yaml:"dependencies"`
			} `yaml:"importers"`
		}
		if err := yaml.Unmarshal(lockRaw, &value); err != nil {
			return fmt.Errorf("invalid %s: %w", lockName, err)
		}
		locked = make(map[string]string)
		for name, dependency := range value.Importers["."].Dependencies {
			// pnpm appends peer-resolution contexts to an exact package version.
			version, _, _ := strings.Cut(dependency.Version, "(")
			if dependency.Specifier != version {
				return fmt.Errorf("mismatched %s dependency %q", lockName, name)
			}
			locked[name] = dependency.Specifier
		}
	case "npm":
		var value struct {
			Packages map[string]struct {
				Dependencies map[string]string `json:"dependencies"`
			} `json:"packages"`
		}
		if err := json.Unmarshal(lockRaw, &value); err != nil {
			return fmt.Errorf("invalid %s: %w", lockName, err)
		}
		locked = value.Packages[""].Dependencies
	}
	if len(locked) != len(packageValue.Dependencies) {
		return fmt.Errorf("%s does not match package.json dependencies", lockName)
	}
	for name, version := range packageValue.Dependencies {
		if locked[name] != version {
			return fmt.Errorf("%s does not match package.json dependency %q", lockName, name)
		}
	}
	return nil
}

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
	skyFiles, err := assets.SkyAgentsShippingFiles(os.DirFS(source))
	if err != nil {
		return err
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
		if !assets.IncludeSkyAgentsPath(relSlash, d.IsDir(), skyFiles) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
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
