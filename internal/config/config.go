package config

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/joeldevz/skynex/internal/models"
	"github.com/joeldevz/skynex/internal/safefs"
)

var processWriteMu sync.Mutex

// maxConfigFileBytes bounds user-editable configuration before JSON parsing.
const maxConfigFileBytes int64 = 1 << 20

// LoadOrDefault loads config from path. Only a missing file uses the default.
func LoadOrDefault(path string) (map[string]interface{}, error) {
	cfg, _, err := LoadOrDefaultWithHash(path)
	return cfg, err
}

// LoadOrDefaultWithHash returns the exact bytes observed while loading. The
// hash is used by installer transactions to detect an external edit between
// load and commit.
func LoadOrDefaultWithHash(path string) (map[string]interface{}, string, error) {
	if err := validatePath(path); err != nil {
		return nil, "", err
	}
	parent, err := safefs.Open(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return defaultConfig(), "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read config %q: %w", path, err)
	}
	defer parent.Close()
	data, err := safefs.ReadFileVerified(parent, filepath.Base(path), maxConfigFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return defaultConfig(), "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, "", fmt.Errorf("parse config %q: %w", path, err)
	}
	if cfg == nil {
		return nil, "", fmt.Errorf("parse config %q: JSON object required", path)
	}
	return cfg, hashBytes(data), nil
}

func hashBytes(data []byte) string { sum := sha256.Sum256(data); return fmt.Sprintf("%x", sum[:]) }

func defaultConfig() map[string]interface{} {
	return map[string]interface{}{
		"version":  1,
		"defaults": map[string]interface{}{},
		"packages": map[string]interface{}{},
	}
}

// SaveConfig writes the config file with the user's install request.
func SaveConfig(path string, req *models.InstallRequest, existing map[string]interface{}, expectedHash ...string) error {
	if req == nil {
		return errors.New("save config: nil install request")
	}
	cfg := cloneMap(existing)
	cfg["version"] = 1
	defaults := cloneMap(asMap(cfg["defaults"]))
	defaults["interactive"] = req.Interactive
	defaults["targets"] = req.Targets
	cfg["defaults"] = defaults
	pkgs := cloneMap(asMap(cfg["packages"]))
	for _, pkgID := range req.Packages {
		pkg := cloneMap(asMap(pkgs[pkgID]))
		pkg["version"] = req.Versions[pkgID]
		pkg["targets"] = req.Targets
		pkgs[pkgID] = pkg
	}
	cfg["packages"] = pkgs
	if req.Advisor != nil {
		advisor := cloneMap(asMap(cfg["advisor"]))
		advisor["enabled"] = req.Advisor.Enabled
		advisor["model"] = req.Advisor.Model
		advisor["maxUses"] = req.Advisor.MaxUses
		cfg["advisor"] = advisor
	}
	return atomicWrite(path, cfg, expectedHash...)
}

// SaveLock writes the lock file with resolved install results.
func SaveLock(path string, results []*models.InstallResult, req *models.InstallRequest) error {
	if req == nil {
		return errors.New("save lock: nil install request")
	}
	lock := map[string]interface{}{
		"version":     1,
		"generatedAt": time.Now().UTC().Format(time.RFC3339),
		"packages":    map[string]interface{}{},
	}
	pkgs := lock["packages"].(map[string]interface{})
	for _, r := range results {
		if r == nil {
			return errors.New("save lock: nil install result")
		}
		targets := map[string]interface{}{}
		for target, tr := range r.Targets {
			if tr == nil {
				return fmt.Errorf("save lock: nil target result for %q", target)
			}
			targets[target] = map[string]interface{}{
				"status": tr.Status, "installedAt": tr.InstalledAt, "artifacts": tr.Artifacts,
			}
		}
		pkgs[r.PackageID] = map[string]interface{}{
			"requestedVersion": r.RequestedVersion, "resolvedVersion": r.ResolvedVersion,
			"resolvedRef": r.ResolvedRef, "commit": r.Commit, "targets": targets,
		}
	}
	if req.Advisor != nil && req.Advisor.Enabled {
		lock["advisor"] = map[string]interface{}{
			"enabled": true, "model": req.Advisor.Model,
			"installedAt": time.Now().UTC().Format(time.RFC3339),
		}
	}
	return atomicWrite(path, lock)
}

func cloneMap(src map[string]interface{}) map[string]interface{} {
	dst := make(map[string]interface{}, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func asMap(value interface{}) map[string]interface{} {
	if result, ok := value.(map[string]interface{}); ok {
		return result
	}
	return nil
}

func validatePath(path string) error {
	if path == "" || path != filepath.Clean(path) {
		return fmt.Errorf("invalid config path %q: path must be clean and non-empty", path)
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing symlink in config path %q", current)
			}
			if current == path && !info.Mode().IsRegular() {
				return fmt.Errorf("config target %q is not a regular file", path)
			}
			if current == path {
				if !safefs.SingleLink(info) {
					return fmt.Errorf("refusing hard-linked config target %q", path)
				}
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("validate config path %q: %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}

func atomicWrite(path string, data map[string]interface{}, expectedHash ...string) error {
	processWriteMu.Lock()
	defer processWriteMu.Unlock()
	if err := validatePath(path); err != nil {
		return err
	}
	root, err := safefs.OpenOrCreate(filepath.Dir(path), 0o700)
	if err != nil {
		return fmt.Errorf("open config root: %w", err)
	}
	defer root.Close()
	lockName := filepath.Base(path) + ".lock"
	var lock *os.File
	for attempt := 0; attempt < 100; attempt++ {
		lock, err = root.OpenFile(lockName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create config lock: %w", err)
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("create config lock: timed out")
	}
	if err := lock.Close(); err != nil {
		_ = root.Remove(lockName)
		return fmt.Errorf("close config lock: %w", err)
	}
	defer root.Remove(lockName)
	if len(expectedHash) > 0 {
		current, readErr := safefs.ReadFileVerified(root, filepath.Base(path), maxConfigFileBytes)
		currentHash := ""
		if readErr == nil {
			currentHash = hashBytes(current)
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("check config identity: %w", readErr)
		}
		if currentHash != expectedHash[0] {
			return errors.New("config changed after load; aborting installation")
		}
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	return safefs.WriteAtomic(root, filepath.Base(path), append(b, '\n'), 0o600, ".skynex-config-")
}
