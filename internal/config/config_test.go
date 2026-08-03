package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/joeldevz/skynex/internal/models"
)

func TestSaveConfigHardensAndPreservesState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skills.config.json")
	existing := map[string]interface{}{
		"external": map[string]interface{}{"keep": true},
		"advisor":  map[string]interface{}{"enabled": true, "model": "external"},
		"packages": map[string]interface{}{"old": map[string]interface{}{"version": "1"}},
	}
	req := &models.InstallRequest{Packages: []string{"new"}, Targets: []string{"opencode"}, Versions: map[string]string{"new": "latest"}}
	if err := SaveConfig(path, req, existing); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("permissions = %o, want 600", got)
	}
	cfg, err := LoadOrDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["external"]; !ok {
		t.Fatal("unknown top-level field was not preserved")
	}
	if _, ok := cfg["advisor"]; ok {
		t.Fatal("legacy advisor config was not migrated away")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "skills.config.json" {
		t.Fatalf("temporary files leaked: %v", entries)
	}
}

func TestSaveConfigPreservesNestedUnknownValuesAndSaveLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	existing := map[string]interface{}{"external": map[string]interface{}{"nested": map[string]interface{}{"value": "keep"}}}
	req := &models.InstallRequest{Packages: []string{"pkg"}, Targets: []string{"claude"}, Versions: map[string]string{"pkg": "v1"}}
	if err := SaveConfig(path, req, existing); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadOrDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	external := cfg["external"].(map[string]interface{})["nested"].(map[string]interface{})
	if external["value"] != "keep" {
		t.Fatalf("nested unknown value changed: %#v", external)
	}
	lock := filepath.Join(dir, "lock.json")
	result := &models.InstallResult{PackageID: "pkg", RequestedVersion: "v1", ResolvedVersion: "v1.0.0", Targets: map[string]*models.TargetResult{"claude": {Status: "installed", Artifacts: []string{"skill.md"}}}}
	if err := SaveLock(lock, []*models.InstallResult{result}, req); err != nil {
		t.Fatal(err)
	}
	decoded, err := LoadOrDefault(lock)
	if err != nil {
		t.Fatal(err)
	}
	if decoded["packages"].(map[string]interface{})["pkg"].(map[string]interface{})["resolvedVersion"] != "v1.0.0" {
		t.Fatal("lock did not preserve resolved version")
	}
	if err := SaveLock(filepath.Join(dir, "bad-lock"), []*models.InstallResult{nil}, req); err == nil {
		t.Fatal("nil lock result was accepted")
	}
}

func TestLoadOrDefaultSurfacesMalformedAndReadErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skills.config.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrDefault(path); err == nil {
		t.Fatal("malformed JSON was silently accepted")
	}
	if _, err := LoadOrDefault(filepath.Join(dir, "missing.json")); err != nil {
		t.Fatalf("missing file should load defaults: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "directory.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrDefault(filepath.Join(dir, "directory.json")); err == nil {
		t.Fatal("read error was silently accepted")
	}
}

func TestSaveConfigPropagatesErrorsAndRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	req := &models.InstallRequest{Versions: map[string]string{}}
	blocking := filepath.Join(dir, "blocking")
	if err := os.WriteFile(blocking, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(filepath.Join(blocking, "config.json"), req, nil); err == nil {
		t.Fatal("write error was not propagated")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "real"), link); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(filepath.Join(link, "config.json"), req, nil); err == nil {
		t.Fatal("symlink ancestor was accepted")
	}
}

func TestConcurrentSavesProduceValidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skills.config.json")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := &models.InstallRequest{Packages: []string{"pkg"}, Versions: map[string]string{"pkg": strings.Repeat("v", i+1)}}
			if err := SaveConfig(path, req, map[string]interface{}{}); err != nil {
				t.Errorf("save %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSON after concurrent saves: %v", err)
	}
}

func TestSaveConfigRejectsUncleanPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	req := &models.InstallRequest{}
	if _, err := LoadOrDefault(path + string(os.PathSeparator) + "."); err == nil {
		t.Fatal("unclean path was accepted by load")
	}
	if err := SaveConfig(path+string(os.PathSeparator)+".", req, nil); err == nil {
		t.Fatal("unclean path was accepted")
	}
}

func TestSaveConfigRejectsExternalChangeAfterLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skills.config.json")
	if err := os.WriteFile(path, []byte("{\"version\":1,\"external\":\"before\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	existing, hash, err := LoadOrDefaultWithHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"version\":1,\"external\":\"after\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := &models.InstallRequest{Packages: []string{"pkg"}, Versions: map[string]string{"pkg": "latest"}}
	if err := SaveConfig(path, req, existing, hash); err == nil {
		t.Fatal("external config change was overwritten")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"after"`) {
		t.Fatal("external config was not preserved")
	}
}

func TestSaveConfigPersistsExplicitNeuroxSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skills.config.json")
	req := &models.InstallRequest{NeuroxEnabled: false, NeuroxSelectionSet: true, Versions: map[string]string{}}
	if err := SaveConfig(path, req, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadOrDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	defaults := cfg["defaults"].(map[string]interface{})
	if enabled, ok := defaults["neuroxEnabled"].(bool); !ok || enabled {
		t.Fatalf("neuroxEnabled=%v (%v), want false", enabled, ok)
	}
}
