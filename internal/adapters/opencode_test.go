package adapters

import (
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/models"
)

func writeDependencyFixture(t *testing.T, target, manager string, packageName string) {
	t.Helper()
	packageJSON := `{"name":"` + packageName + `","private":true,"dependencies":{"@opencode-ai/plugin":"1.2.27"}}`
	if err := os.WriteFile(filepath.Join(target, "package.json"), []byte(packageJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	var lockName, lock string
	switch manager {
	case "bun":
		lockName = "bun.lock"
		lock = "{\n  \"lockfileVersion\": 1,\n  \"configVersion\": 1,\n  \"workspaces\": {\n    \"\": {\n      \"dependencies\": {\n        \"@opencode-ai/plugin\": \"1.2.27\"\n      }\n    }\n  },\n  \"packages\": {}\n}\n"
	case "pnpm":
		lockName = "pnpm-lock.yaml"
		lock = "lockfileVersion: '9.0'\nimporters:\n  .:\n    dependencies:\n      '@opencode-ai/plugin':\n        specifier: 1.2.27\n        version: 1.2.27\npackages: {}\n"
	case "npm":
		lockName = "package-lock.json"
		lock = `{"name":"` + packageName + `","lockfileVersion":3,"requires":true,"packages":{"":{"name":"` + packageName + `","private":true,"dependencies":{"@opencode-ai/plugin":"1.2.27"}}}}` + "\n"
	default:
		t.Fatalf("unknown manager %q", manager)
	}
	if err := os.WriteFile(filepath.Join(target, lockName), []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
}

func installManagerStub(t *testing.T, root, manager string) string {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, manager+"-started")
	if err := os.WriteFile(filepath.Join(bin, manager), []byte("#!/bin/sh\nprintf started > \"$MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("MARKER", marker)
	return marker
}

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
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			t.Errorf("cleanup target: %v", err)
		}
		if err := os.Rename(replacement, target); err != nil {
			t.Errorf("cleanup replacement: %v", err)
		}
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

func TestInstallJSDepsPrefersBunWhenBothPackageManagersAvailable(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "package")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, manager := range []string{"bun", "pnpm", "npm"} {
		writeDependencyFixture(t, target, manager, "selection-target")
	}
	markers := map[string]string{
		"bun":  filepath.Join(root, "bun-selected"),
		"pnpm": filepath.Join(root, "pnpm-selected"),
		"npm":  filepath.Join(root, "npm-selected"),
	}
	for name, marker := range markers {
		script := "#!/bin/sh\nprintf 'selected|%s' \"$*\" > \"$MARKER_" + name + "\"\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MARKER_"+name, marker)
	}
	t.Setenv("PATH", bin)

	if err := installJSDeps(target); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(markers["bun"]); err != nil || string(got) != "selected|install --frozen-lockfile --silent --ignore-scripts" {
		t.Fatalf("bun was not selected: %q, %v", got, err)
	}
	for _, name := range []string{"pnpm", "npm"} {
		if _, err := os.Stat(markers[name]); !os.IsNotExist(err) {
			t.Fatalf("%s was selected despite bun being available: %v", name, err)
		}
	}
}

func TestInstallJSDepsPrefersPnpmBeforeNpm(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "package")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, manager := range []string{"pnpm", "npm"} {
		writeDependencyFixture(t, target, manager, "selection-target")
	}
	selected := filepath.Join(root, "selected")
	for _, name := range []string{"pnpm", "npm"} {
		script := "#!/bin/sh\nprintf '%s|%s' '" + name + "' \"$*\" > \"$SELECTED\"\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("SELECTED", selected)

	if err := installJSDeps(target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(selected)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pnpm|install --frozen-lockfile --silent --ignore-scripts" {
		t.Fatalf("selected manager and args = %q, want pnpm|install --frozen-lockfile --silent --ignore-scripts", got)
	}
}

func TestInstallJSDepsUsesSafeArgsUnlessScriptsAreTrusted(t *testing.T) {
	root := t.TempDir()
	for _, manager := range []string{"bun", "pnpm", "npm"} {
		installArgs := "install --frozen-lockfile --silent --ignore-scripts"
		trustedArgs := "install --frozen-lockfile --silent"
		if manager == "npm" {
			installArgs = "ci --silent --ignore-scripts"
			trustedArgs = "ci --silent"
		}
		for _, test := range []struct {
			name  string
			trust bool
			want  string
		}{
			{name: "untrusted", want: installArgs},
			{name: "trusted", trust: true, want: trustedArgs},
		} {
			t.Run(manager+"/"+test.name, func(t *testing.T) {
				bin := filepath.Join(root, manager, test.name, "bin")
				if err := os.MkdirAll(bin, 0o700); err != nil {
					t.Fatal(err)
				}
				args := filepath.Join(root, manager, test.name, "args")
				script := "#!/bin/sh\nprintf '%s' \"$*\" > \"$ARGS\"\n"
				if err := os.WriteFile(filepath.Join(bin, manager), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin)
				t.Setenv("ARGS", args)
				target := filepath.Join(root, manager, test.name, "package")
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
				writeDependencyFixture(t, target, manager, "trusted-target")
				if err := installJSDepsWithReporter(target, test.trust, discardReporter()); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(args)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != test.want {
					t.Fatalf("%s args = %q, want %q", manager, got, test.want)
				}
			})
		}
	}
}

func TestInstallJSDepsRequiresManagerLockBeforeSubprocess(t *testing.T) {
	for _, manager := range []struct {
		name string
		lock string
	}{
		{name: "bun", lock: "bun.lock"},
		{name: "pnpm", lock: "pnpm-lock.yaml"},
		{name: "npm", lock: "package-lock.json"},
	} {
		t.Run(manager.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "package")
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "package.json"), []byte(`{"name":"locked-target","private":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, wrong := range []string{"bun", "pnpm", "npm"} {
				if map[string]string{"bun": "bun.lock", "pnpm": "pnpm-lock.yaml", "npm": "package-lock.json"}[wrong] != manager.lock {
					writeDependencyFixture(t, target, wrong, "locked-target")
				}
			}
			started := filepath.Join(root, "started")
			if err := os.WriteFile(filepath.Join(bin, manager.name), []byte("#!/bin/sh\nprintf started > \"$STARTED\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("STARTED", started)

			if err := installJSDeps(target); err == nil {
				t.Fatal("expected missing committed lockfile to be rejected")
			}
			if _, err := os.Stat(started); !os.IsNotExist(err) {
				t.Fatalf("manager started without %s: %v", manager.lock, err)
			}
		})
	}
}

func TestInstallJSDepsRejectsMismatchedPackageMetadataBeforeSubprocess(t *testing.T) {
	for _, manager := range []string{"bun", "pnpm", "npm"} {
		t.Run(manager, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "package")
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			writeDependencyFixture(t, target, manager, "committed-target")
			lock := map[string]string{"bun": "bun.lock", "pnpm": "pnpm-lock.yaml", "npm": "package-lock.json"}[manager]
			contents, err := os.ReadFile(filepath.Join(target, lock))
			if err != nil {
				t.Fatal(err)
			}
			contents = []byte(strings.Replace(string(contents), "1.2.27", "9.9.9", 1))
			if err := os.WriteFile(filepath.Join(target, lock), contents, 0o600); err != nil {
				t.Fatal(err)
			}
			started := installManagerStub(t, root, manager)
			if err := installJSDeps(target); err == nil {
				t.Fatal("expected mismatched package metadata to be rejected")
			}
			if _, err := os.Stat(started); !os.IsNotExist(err) {
				t.Fatalf("%s started with mismatched lock: %v", manager, err)
			}
		})
	}
}

func TestValidateManagedOpenCodeRejectsSelfAttestedDependencyMetadata(t *testing.T) {
	target := t.TempDir()
	bundle, err := assets.OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	if err := assets.ExtractTo(bundle, target); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"package.json", "bun.lock"} {
		contents, err := os.ReadFile(filepath.Join(target, path))
		if err != nil {
			t.Fatal(err)
		}
		// The target metadata is internally consistent, but differs from the
		// authenticated metadata shipped in the embedded OpenCode bundle.
		contents = []byte(strings.ReplaceAll(string(contents), "1.2.27", "9.9.9"))
		if err := os.WriteFile(filepath.Join(target, path), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := inventory{Files: map[string]string{}}
	if err := filepath.WalkDir(target, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(target, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == inventoryName {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifest.Files[rel] = fileDigest(contents)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, inventoryName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ValidateManagedOpenCode(target); err == nil {
		t.Fatal("expected forged self-consistent target metadata to be rejected")
	}
}

func TestValidateManagedOpenCodeRefreshesInventoryAfterMCPMerge(t *testing.T) {
	sourceRoot, target := t.TempDir(), t.TempDir()
	bundle, err := assets.OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceRoot, "opencode")
	if err := assets.ExtractTo(bundle, source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "opencode.json"), []byte(`{"mcp":{"custom":{"command":["custom-mcp"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	req := &models.InstallRequest{NeuroxEnabled: true, StateDir: filepath.Join(sourceRoot, "state")}
	if err := InstallOpencodeWithReporterAndOptions(sourceRoot, req, discardReporter(), InstallOptions{OpencodeDir: target, Input: strings.NewReader(""), Output: io.Discard}); err != nil {
		t.Fatal(err)
	}
	merged, err := os.ReadFile(filepath.Join(target, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := os.ReadFile(filepath.Join(target, inventoryName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest inventory
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if got, want := manifest.Files["opencode.json"], fileDigest(merged); got != want {
		t.Fatalf("manifest digest = %q, want post-merge digest %q", got, want)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(merged, &config); err != nil {
		t.Fatal(err)
	}
	var mcp map[string]json.RawMessage
	if err := json.Unmarshal(config["mcp"], &mcp); err != nil {
		t.Fatal("MCP merge removed mcp configuration")
	}
	if _, ok := mcp["custom"]; !ok {
		t.Fatal("MCP merge removed the user's custom server")
	}
	var neurox struct {
		Type    string   `json:"type"`
		Command []string `json:"command"`
		Enabled bool     `json:"enabled"`
	}
	if err := json.Unmarshal(mcp["neurox"], &neurox); err != nil {
		t.Fatalf("invalid Neurox MCP entry: %v", err)
	}
	if neurox.Type != "local" || len(neurox.Command) != 2 || neurox.Command[0] != "neurox" || neurox.Command[1] != "mcp" || !neurox.Enabled {
		t.Fatalf("Neurox MCP entry = %+v, want local [neurox mcp] enabled", neurox)
	}
	if err := ValidateManagedOpenCode(target); err != nil {
		t.Fatalf("managed target should validate after install and MCP merge: %v", err)
	}

	if err := os.WriteFile(filepath.Join(target, "opencode.json"), []byte(`{"mcp":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateManagedOpenCode(target); err == nil {
		t.Fatal("expected post-install tampering to invalidate managed target")
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
	data, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

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
	userMCPJSON, err := json.Marshal(userMCP)
	if err != nil {
		t.Fatal(err)
	}
	backup["mcp"] = userMCPJSON

	err = mergeOpencodeConfig(configPath, backup)
	if err != nil {
		t.Fatalf("mergeOpencodeConfig failed: %v", err)
	}

	// Read result
	result, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var merged map[string]interface{}
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatal(err)
	}

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
	data, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	// Should not panic with nil backup
	err = mergeOpencodeConfig(configPath, nil)
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
	data, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	err = mergeOpencodeConfig(configPath, nil)
	if err != nil {
		t.Fatalf("mergeOpencodeConfig failed: %v", err)
	}

	result, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var merged map[string]interface{}
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatal(err)
	}

	mcp := merged["mcp"].(map[string]interface{})
	if _, ok := mcp["neurox"]; !ok {
		t.Error("neurox entry should be forced even when not in installed config")
	}
}

func TestMergeOpencodeConfigDisablesNeuroxWithoutDeletingUserServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := map[string]json.RawMessage{"mcp": json.RawMessage(`{"custom":{"type":"local","command":["custom"]},"neurox":{"type":"local","command":["neurox","mcp"],"enabled":true}}`)}
	if err := mergeOpencodeConfigForNeurox(path, backup, false, discardReporter()); err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	mcp := got["mcp"].(map[string]interface{})
	if _, ok := mcp["custom"]; !ok {
		t.Fatal("custom MCP lost")
	}
	if _, ok := mcp["neurox"]; ok {
		t.Fatal("Neurox MCP retained while disabled")
	}
}

func TestMergeOpencodeConfigPreservesDiagnosticGateway(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")
	installed := `{"mcp":{"diagnostic":{"type":"local","command":["skynex","diagnostic-gateway"],"enabled":true}}}`
	if err := os.WriteFile(path, []byte(installed), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := map[string]json.RawMessage{"mcp": json.RawMessage(`{"custom":{"type":"local","command":["custom"]}}`)}
	if err := mergeOpencodeConfigForNeurox(path, backup, false, discardReporter()); err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCP map[string]struct {
			Command []string `json:"command"`
			Enabled bool     `json:"enabled"`
			Type    string   `json:"type"`
		} `json:"mcp"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	diagnostic, ok := got.MCP["diagnostic"]
	if !ok || !diagnostic.Enabled || diagnostic.Type != "local" || len(diagnostic.Command) != 2 || diagnostic.Command[0] != "skynex" || diagnostic.Command[1] != "diagnostic-gateway" {
		t.Fatalf("diagnostic gateway was not preserved: %+v", got.MCP)
	}
	if _, ok := got.MCP["custom"]; !ok {
		t.Fatal("custom MCP was not preserved")
	}
}

func TestMergeOpencodeConfigPreservesCustomNeuroxWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"neurox":{"type":"local","command":["neurox","mcp"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := map[string]json.RawMessage{"mcp": json.RawMessage(`{"neurox":{"type":"remote","url":"https://example.test"}}`)}
	if err := mergeOpencodeConfigForNeurox(path, backup, false, discardReporter()); err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["mcp"].(map[string]interface{})["neurox"]; !ok {
		t.Fatal("custom Neurox MCP entry was removed")
	}
}

func TestInstallOwnedTreeRetiresUnmodifiedConditionalPluginAndPreservesModified(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plugins", "neurox.ts"), []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installOwnedTree(source, target); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	if err := installOwnedTree(empty, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "plugins", "neurox.ts")); !os.IsNotExist(err) {
		t.Fatalf("unchanged plugin not retired: %v", err)
	}

	if err := os.WriteFile(filepath.Join(source, "plugins", "neurox.ts"), []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installOwnedTree(source, target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "plugins", "neurox.ts"), []byte("user modified"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installOwnedTree(empty, target); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(target, "plugins", "neurox.ts"))
	if err != nil || string(raw) != "user modified" {
		t.Fatalf("modified plugin not preserved: %q %v", raw, err)
	}
}
