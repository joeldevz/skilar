package adapters

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallJSDepsRejectsDirectoryReplacementBeforeSubprocess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor-backed install cwd is Linux-only")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	replacement := filepath.Join(root, "replacement")
	external := filepath.Join(root, "external")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatal(err)
	}
	externalMarker := filepath.Join(external, "marker")
	if err := os.WriteFile(externalMarker, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(root, "started")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	npm := filepath.Join(bin, "npm")
	if err := os.WriteFile(npm, []byte("#!/bin/sh\nprintf started > \"$STARTED\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("STARTED", started)

	beforeInstallCwdOpen = func() error {
		if err := os.Rename(target, replacement); err != nil {
			return err
		}
		return os.Symlink(external, target)
	}
	t.Cleanup(func() {
		beforeInstallCwdOpen = nil
		_ = os.Remove(target)
		_ = os.Rename(replacement, target)
	})

	err := installJSDeps(target)
	if err == nil {
		t.Fatal("expected replaced install directory to be rejected")
	}
	if _, statErr := os.Stat(started); !os.IsNotExist(statErr) {
		t.Fatalf("subprocess started: stat(%q) = %v", started, statErr)
	}
	data, readErr := os.ReadFile(externalMarker)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "untouched" {
		t.Fatalf("external marker = %q, want untouched", data)
	}
}

func TestInstallCwdUsesRetainedDescriptor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := openInstallCwd(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer cwd.Close()
	cmd := exec.Command("sh", "-c", "test -f marker")
	if err := runWithInstallCwd(cwd, cmd); err != nil {
		t.Fatalf("descriptor-backed command failed: %v", err)
	}
	if !strings.HasPrefix(cmd.Dir, "/proc/self/fd/") {
		t.Fatalf("command cwd = %q, want descriptor path", cmd.Dir)
	}
	if err := cwd.verify(dir); err != nil {
		t.Fatalf("directory identity verification failed: %v", err)
	}
}

func TestMergeOpencodeConfig_PreservesUserMCP(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "opencode.json")

	// Installed config (what we just copied)
	installed := map[string]interface{}{
		"mcp": map[string]interface{}{
			"neurox": map[string]interface{}{
				"command": []string{"neurox", "mcp"},
				"enabled": true,
				"type":    "local",
			},
			"context7": map[string]interface{}{
				"command": []string{"context7"},
				"enabled": true,
			},
		},
	}
	data, _ := json.MarshalIndent(installed, "", "  ")
	os.WriteFile(configPath, append(data, '\n'), 0o644)

	// Backup config (user had a custom MCP server)
	backup := map[string]json.RawMessage{}
	userMCP := map[string]interface{}{
		"my-custom-server": map[string]interface{}{
			"command": []string{"my-server"},
			"enabled": true,
		},
		"neurox": map[string]interface{}{
			"command": []string{"old", "command"},
			"enabled": false,
		},
	}
	userMCPJSON, _ := json.Marshal(userMCP)
	backup["mcp"] = userMCPJSON

	err := mergeOpencodeConfig(configPath, backup)
	if err != nil {
		t.Fatalf("mergeOpencodeConfig failed: %v", err)
	}

	// Read result
	result, _ := os.ReadFile(configPath)
	var merged map[string]interface{}
	json.Unmarshal(result, &merged)

	mcp, ok := merged["mcp"].(map[string]interface{})
	if !ok {
		t.Fatal("mcp field missing or wrong type after merge")
	}

	// User's custom server should be preserved
	if _, ok := mcp["my-custom-server"]; !ok {
		t.Error("user's custom MCP server was lost after merge")
	}

	// Neurox should be forced to correct shape (installed wins)
	neurox, ok := mcp["neurox"].(map[string]interface{})
	if !ok {
		t.Fatal("neurox MCP entry missing after merge")
	}

	// neurox.enabled should be true (installed version wins)
	if enabled, _ := neurox["enabled"].(bool); !enabled {
		t.Error("neurox.enabled should be true (installed config wins)")
	}
}

func TestMergeOpencodeConfig_NilBackup(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "opencode.json")

	installed := map[string]interface{}{
		"mcp": map[string]interface{}{
			"neurox": map[string]interface{}{"enabled": true},
		},
	}
	data, _ := json.MarshalIndent(installed, "", "  ")
	os.WriteFile(configPath, append(data, '\n'), 0o644)

	// Should not panic with nil backup
	err := mergeOpencodeConfig(configPath, nil)
	if err != nil {
		t.Fatalf("mergeOpencodeConfig with nil backup failed: %v", err)
	}
}

func TestMergeOpencodeConfig_ForceNeuroxEntry(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "opencode.json")

	// Config without neurox MCP
	installed := map[string]interface{}{
		"mcp": map[string]interface{}{},
	}
	data, _ := json.MarshalIndent(installed, "", "  ")
	os.WriteFile(configPath, append(data, '\n'), 0o644)

	err := mergeOpencodeConfig(configPath, nil)
	if err != nil {
		t.Fatalf("mergeOpencodeConfig failed: %v", err)
	}

	result, _ := os.ReadFile(configPath)
	var merged map[string]interface{}
	json.Unmarshal(result, &merged)

	mcp := merged["mcp"].(map[string]interface{})
	if _, ok := mcp["neurox"]; !ok {
		t.Error("neurox entry should be forced even when not in installed config")
	}
}
