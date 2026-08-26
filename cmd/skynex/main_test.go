package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/assets"
)

func TestCompactInstallErrorStripsTerminalControlsAndSuggestsVerbose(t *testing.T) {
	for _, raw := range []string{
		"validate \x1b[?2026h\x1b[1;31mopencode\x1b[0m",
		"validate ^[[?2026h^[[1;31mopencode^[[0m",
		"validate \x1b[?2026;2$y\x1b[?2027;2$y\x1b]11;rgb:1/2/3\x07opencode",
	} {
		err := compactInstallError(messageError(raw), false)
		if err != "Installation failed: validate opencode (rerun with --verbose for details)" {
			t.Fatalf("compact error for %q = %q", raw, err)
		}
	}
}

type messageError string

func (e messageError) Error() string { return string(e) }

func TestNewUpdateInstallRequestPropagatesCleanupDeprecated(t *testing.T) {
	stateDir := t.TempDir()
	packages := []string{"skills"}
	targets := []string{"opencode"}
	versions := map[string]string{"skills": "latest"}

	for _, cleanupDeprecated := range []bool{true, false} {
		t.Run("cleanup="+boolString(cleanupDeprecated), func(t *testing.T) {
			request := newUpdateInstallRequest(packages, targets, versions, stateDir, cleanupDeprecated)
			if request.CleanupDeprecated != cleanupDeprecated {
				t.Fatalf("CleanupDeprecated = %v, want %v", request.CleanupDeprecated, cleanupDeprecated)
			}
			if request.StateDir != stateDir {
				t.Fatalf("StateDir = %q, want %q", request.StateDir, stateDir)
			}
			if request.Interactive {
				t.Fatal("update request must be non-interactive")
			}
		})
	}
}

func TestParseArgsFromDryRun(t *testing.T) {
	args := parseArgsFrom([]string{"install", "--dry-run", "--package", "skills", "--target", "claude"})
	if !args.Install || !args.DryRun {
		t.Fatalf("install/dry-run flags not parsed: %#v", args)
	}
	if len(args.Packages) != 1 || args.Packages[0] != "skills" {
		t.Fatalf("packages = %#v", args.Packages)
	}
}

func TestParseArgsFromVerbose(t *testing.T) {
	args := parseArgsFrom([]string{"install", "--verbose"})
	if !args.Install || !args.Verbose {
		t.Fatalf("verbose flag not parsed: %#v", args)
	}
}

func TestParseArgsFromForce(t *testing.T) {
	args := parseArgsFrom([]string{"install", "--force"})
	if !args.Install || !args.Force {
		t.Fatalf("install/force flags not parsed: %#v", args)
	}
}

func TestParseArgsFromDepsIsFocusedCommand(t *testing.T) {
	args := parseArgsFrom([]string{"deps"})
	if args.ParseError != "" {
		t.Fatalf("deps parse error = %q", args.ParseError)
	}
	deps, ok := reflect.ValueOf(args).Elem().Type().FieldByName("Deps")
	if !ok || deps.Type.Kind() != reflect.Bool {
		t.Fatal("cli args must expose a boolean Deps command selector")
	}
	if !reflect.ValueOf(args).Elem().FieldByIndex(deps.Index).Bool() {
		t.Fatalf("deps command was not parsed: %#v", args)
	}
	if args.Install {
		t.Fatal("deps must not dispatch as install")
	}
}

func TestParseArgsFromDepsDefaultsTrustScriptsOffAndHonorsExplicitTrust(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{name: "default", argv: []string{"deps"}},
		{name: "explicit", argv: []string{"deps", "--trust-setup-scripts"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := parseArgsFrom(tc.argv)
			if args.ParseError != "" {
				t.Fatalf("deps parse error = %q", args.ParseError)
			}
			deps, ok := reflect.ValueOf(args).Elem().Type().FieldByName("Deps")
			if !ok || !reflect.ValueOf(args).Elem().FieldByIndex(deps.Index).Bool() {
				t.Fatalf("deps command selector = %#v, want true", args)
			}
			if args.Install || args.Update {
				t.Fatalf("deps must be separate from install/update: %#v", args)
			}
			if args.TrustScripts != tc.want {
				t.Fatalf("TrustScripts=%v, want %v", args.TrustScripts, tc.want)
			}
		})
	}
}

func TestDispatchDepsInvokesOnlyDependencyHandler(t *testing.T) {
	var events []string
	args := parseArgsFrom([]string{"deps", "--trust-setup-scripts"})
	err := dispatch(args, dispatchDependencies{
		install: func() error { events = append(events, "install"); return nil },
		update:  func() error { events = append(events, "update"); return nil },
		deps:    func() error { events = append(events, "deps"); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"deps"}) {
		t.Fatalf("dispatch events = %v, want [deps]", events)
	}
}

func TestRunDepsRejectsMissingAndInvalidManagedTargetsWithoutSetup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T) string
		want    string
	}{
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }, "managed OpenCode installation not found; run `skynex install` first\n"},
		{"empty", func(t *testing.T) string { return t.TempDir() }, "managed OpenCode installation is invalid; run `skynex install` first\n"},
		{"unmarked", func(t *testing.T) string {
			path := t.TempDir()
			if err := os.WriteFile(filepath.Join(path, "opencode.json"), []byte(`{"name":"unmarked"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, "managed OpenCode installation is invalid; run `skynex install` first\n"},
		{"forged-marker", func(t *testing.T) string {
			path := t.TempDir()
			forged := []byte("arbitrary forged content\n")
			if err := os.WriteFile(filepath.Join(path, "arbitrary.txt"), forged, 0o600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(forged)
			manifest := `{"files":{"arbitrary.txt":"` + hex.EncodeToString(digest[:]) + `"}}`
			if err := os.WriteFile(filepath.Join(path, ".skynex-manifest.json"), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, "managed OpenCode installation is invalid; run `skynex install` first\n"},
		{"invalid-manifest", func(t *testing.T) string {
			path := t.TempDir()
			if err := os.WriteFile(filepath.Join(path, ".skynex-manifest.json"), []byte("not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, "managed OpenCode installation is invalid; run `skynex install` first\n"},
		{"wrong-digest", func(t *testing.T) string {
			return managedOpenCodeFixtureWithManifest(t, `{"version":1,"files":{"opencode.json":"`+strings.Repeat("0", 64)+`"}}`, true)
		}, "managed OpenCode installation is invalid; run `skynex install` first\n"},
		{"missing-owned-file", func(t *testing.T) string {
			path := t.TempDir()
			if err := os.WriteFile(filepath.Join(path, ".skynex-manifest.json"), []byte(`{"version":1,"files":{"opencode.json":"`+strings.Repeat("0", 64)+`"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, "managed OpenCode installation is invalid; run `skynex install` first\n"},
		{"wrong-version", func(t *testing.T) string {
			return managedOpenCodeFixtureWithManifest(t, `{"version":2,"files":{}}`, false)
		}, "managed OpenCode installation is invalid; run `skynex install` first\n"},
		{"file", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "opencode")
			if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, "managed OpenCode installation is invalid; run `skynex install` first\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			setupCalls := 0
			err := runDeps(&cliArgs{}, dependencyDependencies{
				opencodeDir: func() string { return tc.prepare(t) }, output: &out,
				setup: func(string, bool) error { setupCalls++; return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			if setupCalls != 0 {
				t.Fatalf("dependency setup calls=%d, want 0 for invalid managed target", setupCalls)
			}
			if out.String() != tc.want {
				t.Fatalf("output=%q, want=%q", out.String(), tc.want)
			}
		})
	}
}

func TestRunDepsRetriesOnlyDependenciesAndForwardsTrust(t *testing.T) {
	target := managedOpenCodeFixture(t)
	var out strings.Builder
	var gotTarget string
	var gotTrust []bool
	setup := func(target string, trust bool) error {
		gotTarget, gotTrust = target, append(gotTrust, trust)
		return nil
	}
	for _, tc := range []struct {
		name  string
		trust bool
	}{{"default", false}, {"trusted", true}} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			gotTrust = nil
			args := &cliArgs{TrustScripts: tc.trust}
			if err := runDeps(args, dependencyDependencies{opencodeDir: func() string { return target }, setup: setup, output: &out}); err != nil {
				t.Fatal(err)
			}
			if gotTarget != target || !reflect.DeepEqual(gotTrust, []bool{tc.trust}) {
				t.Fatalf("setup target/trust=%q/%v", gotTarget, gotTrust)
			}
			if out.String() != "✓ OpenCode dependencies installed.\n" {
				t.Fatalf("output=%q", out.String())
			}
		})
	}
}

func TestRunDepsFailureIsDeterministicRetryMessage(t *testing.T) {
	for _, trust := range []bool{false, true} {
		t.Run("trust="+boolString(trust), func(t *testing.T) {
			target := managedOpenCodeFixture(t)
			var out strings.Builder
			setupCalls := 0
			var gotTarget string
			err := runDeps(&cliArgs{TrustScripts: trust}, dependencyDependencies{
				opencodeDir: func() string { return target }, output: &out,
				setup: func(got string, gotTrust bool) error {
					setupCalls++
					gotTarget = got
					if gotTrust != trust {
						t.Fatalf("trust=%v, want %v", gotTrust, trust)
					}
					return errors.New("bun unavailable")
				},
			})
			if err == nil {
				t.Fatal("expected dependency failure")
			}
			if got := err.Error(); got != "dependency setup failed: bun unavailable" {
				t.Fatalf("error=%q", got)
			}
			if setupCalls != 1 || gotTarget != target {
				t.Fatalf("setup calls/target=%d/%q, want 1/%q", setupCalls, gotTarget, target)
			}
			if out.String() != "Dependency setup failed: bun unavailable\nRetry with: skynex deps\n" {
				t.Fatalf("output=%q", out.String())
			}
		})
	}
}

// managedOpenCodeFixture materializes the same complete bundle and
// authenticated dependency metadata as an installed managed target.
func managedOpenCodeFixture(t *testing.T) string {
	t.Helper()
	target := t.TempDir()
	bundle, err := assets.OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	if err := assets.ExtractTo(bundle, target); err != nil {
		t.Fatal(err)
	}
	manifest := struct {
		Files map[string]string `json:"files"`
	}{Files: map[string]string{}}
	if err := fs.WalkDir(bundle, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := fs.ReadFile(bundle, path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(contents)
		manifest.Files[filepath.ToSlash(path)] = hex.EncodeToString(digest[:])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".skynex-manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return target
}

func managedOpenCodeFixtureWithManifest(t *testing.T, manifest string, withConfig bool) string {
	t.Helper()
	target := t.TempDir()
	if withConfig {
		if err := os.WriteFile(filepath.Join(target, "opencode.json"), []byte(`{"name":"managed"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, ".skynex-manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return target
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestAdvisorModelFlagIsNotPublicCLIState(t *testing.T) {
	args := parseArgsFrom([]string{"install", "--advisor-model", "legacy/model"})
	if args.ParseError == "" {
		t.Fatal("retired --advisor-model flag was silently accepted")
	}
}

func TestSnapshotsToPruneKeepsTotalRetentionBound(t *testing.T) {
	for _, test := range []struct {
		retained int
		keep     int
		want     int
	}{
		{retained: 5, keep: 3, want: 2},
		{retained: 3, keep: 3, want: 0},
		{retained: 2, keep: 3, want: 0},
	} {
		if got := snapshotsToPrune(test.retained, test.keep); got != test.want {
			t.Errorf("snapshotsToPrune(%d, %d) = %d, want %d", test.retained, test.keep, got, test.want)
		}
	}
}
