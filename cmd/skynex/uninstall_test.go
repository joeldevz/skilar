package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/safefs"
)

func TestParseArgsFromUninstall(t *testing.T) {
	args := parseArgsFrom([]string{"uninstall", "--dry-run", "--yes", "--state-dir", "/tmp/skynex-state"})
	uninstall := reflect.ValueOf(args).Elem().FieldByName("Uninstall")
	if !uninstall.IsValid() || uninstall.Kind() != reflect.Bool || !uninstall.Bool() || args.ParseError != "" || !args.DryRun || !args.Yes || args.StateDir != "/tmp/skynex-state" {
		t.Fatalf("uninstall flags were not parsed: %#v", args)
	}
}

func TestUninstallStagingNameIsRandomAndUnused(t *testing.T) {
	root, err := safefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	first, err := uninstallStagingName(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	second, err := uninstallStagingName(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, ".skynex-uninstall-") || !strings.HasPrefix(second, ".skynex-uninstall-") {
		t.Fatalf("staging names are not collision-resistant: %q, %q", first, second)
	}
	if _, err := root.Lstat(first); !os.IsNotExist(err) {
		t.Fatalf("staging name already exists: %q, err=%v", first, err)
	}
}

func TestUninstallDryRunIsAnExactOrderedReadOnlyPlan(t *testing.T) {
	home, state := uninstallFixture(t)
	bin := filepath.Join(home, ".local", "bin")
	writeBackupFixture(t, bin)
	before := snapshotPaths(t, home)

	out := runSkynexWithHome(t, home, "uninstall", "--dry-run", "--yes", "--state-dir", state)
	want := []string{
		"remove " + filepath.Join(home, ".config", "opencode", "managed.md"),
		"preserve " + filepath.Join(home, ".config", "opencode", "changed.md"),
		"preserve " + filepath.Join(home, ".config", "opencode", "symlink.md"),
		"preserve " + filepath.Join(home, ".config", "opencode", "workflows"),
		"remove " + filepath.Join(bin, "skynex.backup.1"),
		"remove " + filepath.Join(home, ".config", "opencode", "skills", "owned.md"),
		"preserve " + filepath.Join(state, "owned.md"),
	}
	got := decisionLines(out)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dry-run plan must contain exactly the ordered decisions\nwant=%q\n got=%q\noutput=%s", want, got, out)
	}
	if after := snapshotPaths(t, home); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry-run mutated filesystem\nbefore=%v\nafter=%v", before, after)
	}
}

func TestUninstallRecordsExactOwnershipAndPreservesEveryNonOwnedEntry(t *testing.T) {
	home, state := uninstallFixture(t)
	bin := filepath.Join(home, ".local", "bin")
	writeBackupFixture(t, bin)
	removedPaths := []string{
		filepath.Join(home, ".config", "opencode", "managed.md"),
		filepath.Join(home, ".config", "opencode", "skills", "owned.md"),
		filepath.Join(bin, "skynex.backup.1"),
	}
	preservedPaths := []string{
		filepath.Join(home, ".config", "opencode", "changed.md"),
		filepath.Join(home, ".config", "opencode", "symlink.md"),
		filepath.Join(home, ".config", "opencode", "workflows", "keep.md"),
		filepath.Join(home, ".config", "opencode", "unowned.md"),
		filepath.Join(bin, "skynex.backup.link"),
		filepath.Join(bin, "skynex.backup-dir"),
		filepath.Join(state, "owned.md"),
	}
	before := snapshotPaths(t, preservedPaths...)
	out := runSkynexWithHome(t, home, "uninstall", "--yes", "--state-dir", state)
	wantDecisions := []string{
		"remove " + filepath.Join(home, ".config", "opencode", "managed.md"),
		"preserve " + filepath.Join(home, ".config", "opencode", "changed.md"),
		"preserve " + filepath.Join(home, ".config", "opencode", "symlink.md"),
		"preserve " + filepath.Join(home, ".config", "opencode", "workflows"),
		"remove " + filepath.Join(bin, "skynex.backup.1"),
		"remove " + filepath.Join(home, ".config", "opencode", "skills", "owned.md"),
		"preserve " + filepath.Join(state, "owned.md"),
	}
	if got := decisionLines(out); !reflect.DeepEqual(got, wantDecisions) {
		t.Fatalf("real uninstall must contain exactly the ordered decisions\nwant=%q\n got=%q\noutput=%s", wantDecisions, got, out)
	}
	for _, path := range removedPaths {
		assertMissing(t, path)
		if !strings.Contains(out, "remove "+path) {
			t.Errorf("output lacks exact remove ownership decision for %q: %s", path, out)
		}
	}
	after := snapshotPaths(t, preservedPaths...)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("preserved entries changed\nbefore=%v\nafter=%v", before, after)
	}
	for _, path := range preservedPaths {
		if !strings.Contains(out, "preserve "+path) {
			t.Errorf("output lacks exact preserve ownership decision for %q: %s", path, out)
		}
	}
	assertMissing(t, filepath.Join(home, ".config", "opencode", ".skynex-manifest.json"))
	assertMissing(t, filepath.Join(state, "skills.ownership.json"))
}

func TestUninstallSnapshotReadFailurePropagatesUnchangedWithoutMutation(t *testing.T) {
	cases := []struct {
		name       string
		targetPath func(home, state string) string
	}{
		{"OpenCode manifest", func(home, state string) string {
			return filepath.Join(home, ".config", "opencode", ".skynex-manifest.json")
		}},
		{"skills ownership state", func(home, state string) string {
			return filepath.Join(state, "skills.ownership.json")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, state := uninstallFixture(t)
			target := tc.targetPath(home, state)
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			before := snapshotPaths(t, home, state)
			_, readErr := os.ReadFile(target)
			if readErr == nil {
				t.Fatal("directory read unexpectedly succeeded")
			}

			out, err := runSkynexResult(t, home, "uninstall", "--yes", "--state-dir", state)
			if err == nil {
				t.Errorf("snapshot read failure must fail the command: output=%q", out)
			}
			if got := strings.TrimSpace(string(out)); got != readErr.Error() {
				t.Errorf("snapshot read failure was not propagated unchanged: want exact output=%q, got=%q", readErr.Error(), got)
			}
			if after := snapshotPaths(t, home, state); !reflect.DeepEqual(before, after) {
				t.Fatalf("snapshot read failure mutated filesystem\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func TestUninstallRejectsSemanticMalformedManifestsWithoutMutation(t *testing.T) {
	cases := []struct {
		name string
		path func(home, state string) string
		json any
		want string
	}{
		{"OpenCode wrong top-level type", func(home, state string) string {
			return filepath.Join(home, ".config", "opencode", ".skynex-manifest.json")
		}, []any{"bad"}, "top-level"},
		{"OpenCode missing version", func(home, state string) string {
			return filepath.Join(home, ".config", "opencode", ".skynex-manifest.json")
		}, map[string]any{"files": map[string]string{}}, "version"},
		{"OpenCode unsupported version", func(home, state string) string {
			return filepath.Join(home, ".config", "opencode", ".skynex-manifest.json")
		}, map[string]any{"version": 2, "files": map[string]string{}}, "version"},
		{"OpenCode missing files field", func(home, state string) string {
			return filepath.Join(home, ".config", "opencode", ".skynex-manifest.json")
		}, map[string]any{"version": 1}, "files"},
		{"OpenCode invalid digest length", func(home, state string) string {
			return filepath.Join(home, ".config", "opencode", ".skynex-manifest.json")
		}, map[string]any{"version": 1, "files": map[string]any{"managed.md": strings.Repeat("a", 63)}}, "digest"},
		{"OpenCode invalid digest characters", func(home, state string) string {
			return filepath.Join(home, ".config", "opencode", ".skynex-manifest.json")
		}, map[string]any{"version": 1, "files": map[string]any{"managed.md": strings.Repeat("g", 64)}}, "digest"},
		{"OpenCode invalid entry type", func(home, state string) string {
			return filepath.Join(home, ".config", "opencode", ".skynex-manifest.json")
		}, map[string]any{"version": 1, "files": map[string]any{"managed.md": []string{"bad"}}}, "entry"},
		{"skills wrong top-level type", func(home, state string) string { return filepath.Join(state, "skills.ownership.json") }, []any{"bad"}, "top-level"},
		{"skills minimal version/files", func(home, state string) string { return filepath.Join(state, "skills.ownership.json") }, map[string]any{"version": 1, "files": []any{}}, "source"},
		{"skills missing files field", func(home, state string) string { return filepath.Join(state, "skills.ownership.json") }, canonicalOwnershipWithoutFiles(), "files"},
		{"skills invalid digest length", func(home, state string) string { return filepath.Join(state, "skills.ownership.json") }, canonicalOwnershipJSON([]manifestFile{{Path: "owned.md", SHA256: strings.Repeat("a", 63)}}), "hash"},
		{"skills invalid entry type", func(home, state string) string { return filepath.Join(state, "skills.ownership.json") }, map[string]any{"version": 1, "source": "opencode/skills", "sourceKind": "bundle", "bundleVersion": "latest", "package": "skills", "target": "opencode", "treeSHA256": strings.Repeat("a", 64), "files": []any{"bad"}}, "entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, state := uninstallFixture(t)
			writeUninstallJSON(t, tc.path(home, state), tc.json)
			before := snapshotPaths(t, home, state)
			out, err := runSkynexResult(t, home, "uninstall", "--yes", "--state-dir", state)
			if err == nil || !strings.Contains(strings.ToLower(string(out)), tc.want) {
				t.Fatalf("must reject %s, err=%v output=%s", tc.name, err, out)
			}
			if after := snapshotPaths(t, home, state); !reflect.DeepEqual(before, after) {
				t.Fatalf("rejection mutated state\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func TestUninstallRejectsInvalidCanonicalSkillsOwnershipWithoutMutation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"wrong target", func(m map[string]any) { m["target"] = "other"; m["treeSHA256"] = canonicalTreeHash(m) }, "target"},
		{"wrong package", func(m map[string]any) { m["package"] = "other"; m["treeSHA256"] = canonicalTreeHash(m) }, "package"},
		{"duplicate paths", func(m map[string]any) {
			m["files"] = []map[string]string{{"path": "owned.md", "sha256": digest([]byte("owned skill"))}, {"path": "owned.md", "sha256": digest([]byte("owned skill"))}}
			m["treeSHA256"] = canonicalTreeHash(m)
		}, "duplicate"},
		{"tree hash mismatch", func(m map[string]any) { m["treeSHA256"] = strings.Repeat("a", 64) }, "tree"},
		{"unclean path", func(m map[string]any) {
			m["files"] = []map[string]string{{"path": "../owned.md", "sha256": digest([]byte("owned skill"))}}
			m["treeSHA256"] = canonicalTreeHash(m)
		}, "path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home, state := uninstallFixture(t)
			manifest := canonicalOwnershipMap([]manifestFile{{Path: "owned.md", SHA256: digest([]byte("owned skill"))}})
			tc.mutate(manifest)
			writeUninstallJSON(t, filepath.Join(state, "skills.ownership.json"), manifest)
			before := snapshotPaths(t, home, state)
			out, err := runSkynexResult(t, home, "uninstall", "--yes", "--state-dir", state)
			if err == nil || !strings.Contains(strings.ToLower(string(out)), tc.want) {
				t.Fatalf("must reject %s, err=%v output=%s", tc.name, err, out)
			}
			if after := snapshotPaths(t, home, state); !reflect.DeepEqual(before, after) {
				t.Fatalf("rejection mutated state\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func TestUninstallRejectsAbsoluteAndTraversalEntriesWithoutExternalMutation(t *testing.T) {
	for _, name := range []string{"absolute", "traversal"} {
		t.Run(name, func(t *testing.T) {
			home, state := uninstallFixture(t)
			opencode := filepath.Join(home, ".config", "opencode")
			external := filepath.Join(filepath.Dir(home), "outside-skynex-target")
			if name == "traversal" {
				external = filepath.Join(filepath.Dir(opencode), "outside-skynex-target")
			}
			if err := os.WriteFile(external, []byte("do not delete"), 0o600); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(external)
			entry := external
			if name == "traversal" {
				entry = "../../outside-skynex-target"
			}
			writeUninstallJSON(t, filepath.Join(opencode, ".skynex-manifest.json"), map[string]any{"files": map[string]string{entry: digest([]byte("do not delete"))}})
			before := snapshotPaths(t, home, state, external)
			out, err := runSkynexResult(t, home, "uninstall", "--yes", "--state-dir", state)
			pathReason := "absolute paths are not allowed"
			if name == "traversal" {
				pathReason = "path traversal is not allowed"
			}
			wantError := fmt.Sprintf("invalid manifest path %q: %s", entry, pathReason)
			if err == nil || !strings.Contains(string(out), wantError) {
				t.Fatalf("hostile path accepted: err=%v output=%s", err, out)
			}
			if strings.Contains(string(out), "remove "+external) || strings.Contains(string(out), "preserve "+external) {
				t.Fatalf("hostile path was processed as an internal candidate: %s", out)
			}
			if after := snapshotPaths(t, home, state, external); !reflect.DeepEqual(before, after) {
				t.Fatalf("hostile entry mutated filesystem\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func TestUninstallRejectsSymlinkedIntermediateDirectory(t *testing.T) {
	home, state := uninstallFixture(t)
	opencode := filepath.Join(home, ".config", "opencode")
	external := t.TempDir()
	if err := os.RemoveAll(filepath.Join(opencode, "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(opencode, "skills")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(external, "owned.md")
	if err := os.WriteFile(target, []byte("owned skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotPaths(t, home, state, external)
	linkBefore, err := os.Readlink(filepath.Join(opencode, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := runSkynexResult(t, home, "uninstall", "--yes", "--state-dir", state)
	if err == nil || !strings.Contains(string(out), "symlink") {
		t.Fatalf("symlinked ancestor must be rejected: err=%v output=%s", err, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("external target was touched: %v", err)
	}
	if got, err := os.Readlink(filepath.Join(opencode, "skills")); err != nil || got != linkBefore {
		t.Fatalf("skills-root symlink identity/target changed: got=%q err=%v want=%q", got, err, linkBefore)
	}
	if after := snapshotPaths(t, home, state, external); !reflect.DeepEqual(before, after) {
		t.Fatalf("symlinked skills-root rejection changed surrounding filesystem\nbefore=%v\nafter=%v", before, after)
	}
}

func TestUninstallRejectionRetainsAllStateBackupsAndDirectories(t *testing.T) {
	home, state := uninstallFixture(t)
	bin := filepath.Join(home, ".local", "bin")
	writeBackupFixture(t, bin)
	blocked := filepath.Join(home, ".config", "opencode", "managed.md")
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "child"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runSkynexResult(t, home, "uninstall", "--yes", "--state-dir", state)
	wantPreserve := "preserve " + blocked
	if err != nil || !strings.Contains(string(out), wantPreserve) {
		t.Fatalf("directory entry must be preserved: want=%q err=%v output=%s", wantPreserve, err, out)
	}
	for _, later := range []string{
		filepath.Join(home, ".config", "opencode", "skills", "owned.md"),
		filepath.Join(home, ".config", "opencode", "changed.md"),
		filepath.Join(home, ".config", "opencode", "symlink.md"),
		filepath.Join(home, ".config", "opencode", "workflows"),
		filepath.Join(bin, "skynex.backup.1"),
		filepath.Join(state, "owned.md"),
	} {
		if !strings.Contains(string(out), "remove "+later) && !strings.Contains(string(out), "preserve "+later) {
			t.Fatalf("candidate was not processed: %q output=%s", later, out)
		}
	}
	assertMissing(t, filepath.Join(home, ".config", "opencode", "skills", "owned.md"))
	beforeState, readErr := os.ReadFile(filepath.Join(state, "owned.md"))
	afterState, afterErr := os.ReadFile(filepath.Join(state, "owned.md"))
	if readErr != nil || afterErr != nil || !reflect.DeepEqual(beforeState, afterState) {
		t.Fatalf("state ownership collision changed: before=%q after=%q", beforeState, afterState)
	}
}

func TestUninstallConfirmationRejectionRetainsCompleteFilesystemState(t *testing.T) {
	home, state := uninstallFixture(t)
	writeBackupFixture(t, filepath.Join(home, ".local", "bin"))
	before := snapshotPaths(t, home, state)
	out, err := runSkynexInput(t, home, "n\n", "uninstall", "--state-dir", state)
	const wantRejection = "Uninstall cancelled. No changes were made."
	if err == nil {
		t.Errorf("confirmation rejection must fail the command: output=%s", out)
	}
	if !strings.Contains(string(out), wantRejection) {
		t.Errorf("confirmation rejection must report %q: output=%s", wantRejection, out)
	}
	if after := snapshotPaths(t, home, state); !reflect.DeepEqual(before, after) {
		t.Fatalf("confirmation rejection mutated filesystem\nbefore=%v\nafter=%v", before, after)
	}
}

func TestUninstallNestedStateOwnershipPreservesRelativePath(t *testing.T) {
	parent := t.TempDir()
	owned := filepath.Join(parent, "state")
	control := filepath.Join(parent, "control")
	if err := os.MkdirAll(filepath.Join(owned, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "sub", "file"), []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "file"), []byte("sibling"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := uninstallCandidate{path: filepath.Join(owned, "sub", "file"), root: owned, stageOutside: true}
	if err := uninstallCommit([]uninstallCandidate{candidate}, t.TempDir(), control, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(owned, "sub", "file")); !os.IsNotExist(err) {
		t.Fatalf("nested owned file was not removed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(owned, "file")); err != nil || string(got) != "sibling" {
		t.Fatalf("state/file sibling was touched: %q, %v", got, err)
	}
}

func TestUninstallRacedBackupReplacementIsNotStaged(t *testing.T) {
	bin := t.TempDir()
	path := filepath.Join(bin, "skynex.backup.raced")
	originalContents := []byte("original")
	if err := os.WriteFile(path, originalContents, 0o600); err != nil {
		t.Fatal(err)
	}
	preserved := filepath.Join(bin, "original-preserved")
	var originalInfo os.FileInfo
	uninstallHooks.BeforeStage = func(stagedPath string) error {
		var err error
		originalInfo, err = os.Lstat(stagedPath)
		if err != nil {
			return err
		}
		if err := os.Rename(stagedPath, preserved); err != nil {
			return err
		}
		target := filepath.Join(t.TempDir(), "replacement")
		if err := os.WriteFile(target, []byte("replacement"), 0o600); err != nil {
			return err
		}
		return os.Symlink(target, stagedPath)
	}
	defer func() { uninstallHooks.BeforeStage = nil }()
	if err := uninstallCommit([]uninstallCandidate{{path: path, root: bin, backup: true}}, t.TempDir(), t.TempDir(), nil); err == nil {
		t.Fatal("raced backup replacement was staged")
	}
	replacementInfo, err := os.Lstat(path)
	if err != nil || replacementInfo.Mode()&os.ModeSymlink == 0 || os.SameFile(originalInfo, replacementInfo) {
		t.Fatalf("replacement symlink identity was not preserved: original=%v replacement=%v err=%v", originalInfo, replacementInfo, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "replacement" {
		t.Fatalf("replacement content was not preserved: %q, %v", got, err)
	}
	if got, err := os.ReadFile(preserved); err != nil || string(got) != string(originalContents) {
		t.Fatalf("original backup was not preserved: %q, %v", got, err)
	}
}

func TestUninstallMetadataRacedReplacementIsNotRemoved(t *testing.T) {
	opencode := t.TempDir()
	path := filepath.Join(opencode, ".skynex-manifest.json")
	originalContents := []byte(`{"version":1,"files":{}}`)
	if err := os.WriteFile(path, originalContents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readUninstallManifest(path); err != nil {
		t.Fatalf("original metadata was not valid: %v", err)
	}
	preserved := filepath.Join(opencode, ".skynex-manifest.original.json")
	replacementContents := []byte(`{"version":1,"files":{"replacement.md":"different"}}`)
	var originalInfo os.FileInfo
	uninstallHooks.BeforeStage = func(stagedPath string) error {
		var err error
		originalInfo, err = os.Lstat(stagedPath)
		if err != nil {
			return err
		}
		if err := os.Rename(stagedPath, preserved); err != nil {
			return err
		}
		return os.WriteFile(stagedPath, replacementContents, 0o600)
	}
	defer func() { uninstallHooks.BeforeStage = nil }()
	control := t.TempDir()
	if err := uninstallCommit([]uninstallCandidate{{path: path, root: opencode}}, opencode, control, nil); err == nil {
		t.Fatal("metadata replacement was staged")
	}
	afterInfo, err := os.Lstat(path)
	if err != nil || os.SameFile(originalInfo, afterInfo) {
		t.Fatalf("metadata replacement identity was not preserved: original=%v replacement=%v err=%v", originalInfo, afterInfo, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(replacementContents) {
		t.Fatalf("metadata replacement was not preserved: %q, %v", got, err)
	}
	if got, err := os.ReadFile(preserved); err != nil || string(got) != string(originalContents) {
		t.Fatalf("original metadata was not preserved: %q, %v", got, err)
	}
}

func TestUninstallManifestReadIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".skynex-manifest.json")
	data := append([]byte(`{"version":1,"files":{},"padding":"`), []byte(strings.Repeat("a", 16<<20))...)
	data = append(data, []byte(`"}`)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readUninstallManifest(path); err == nil {
		t.Fatal("oversize manifest was accepted")
	}
}

func TestUninstallRollbackPropagatesFailureAndDoesNotOverwriteReplacement(t *testing.T) {
	rootPath := t.TempDir()
	path := filepath.Join(rootPath, "owned")
	stage := filepath.Join(rootPath, ".stage")
	if err := os.WriteFile(stage, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := safefs.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = uninstallRollback([]uninstallMove{{c: uninstallCandidate{path: path}, root: root, backupRoot: root, rel: "owned", backup: ".stage"}})
	if err == nil || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("rollback failure was not propagated: %v", err)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "replacement" {
		t.Fatalf("rollback overwrote replacement: %q, %v", got, readErr)
	}
}

func uninstallFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	opencode, state := filepath.Join(home, ".config", "opencode"), filepath.Join(home, ".config", "skynex")
	if err := os.MkdirAll(filepath.Join(opencode, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string][]byte{"managed.md": []byte("managed"), "changed.md": []byte("changed"), "symlink-target.md": []byte("target"), "unowned.md": []byte("keep")} {
		if err := os.WriteFile(filepath.Join(opencode, name), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(opencode, "skills", "owned.md"), []byte("owned skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(opencode, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opencode, "workflows", "keep.md"), []byte("workflow"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("symlink-target.md", filepath.Join(opencode, "symlink.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "owned.md"), []byte("owned skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeUninstallJSON(t, filepath.Join(opencode, ".skynex-manifest.json"), map[string]any{"version": 1, "files": map[string]string{"managed.md": digest([]byte("managed")), "changed.md": digest([]byte("original")), "symlink.md": digest([]byte("target")), "workflows": digest([]byte("not-a-directory-digest"))}})
	writeUninstallJSON(t, filepath.Join(state, "skills.ownership.json"), canonicalOwnershipMap([]manifestFile{{Path: "owned.md", SHA256: digest([]byte("owned skill"))}}))
	return home, state
}

func writeBackupFixture(t *testing.T, bin string) {
	t.Helper()
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "skynex.backup.1"), []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("skynex.backup.1", filepath.Join(bin, "skynex.backup.link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bin, "skynex.backup-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
}
func digest(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }

type manifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func canonicalOwnershipJSON(files []manifestFile) map[string]any {
	return canonicalOwnershipMap(files)
}

func canonicalOwnershipWithoutFiles() map[string]any {
	m := canonicalOwnershipMap(nil)
	delete(m, "files")
	return m
}

func canonicalOwnershipMap(files []manifestFile) map[string]any {
	m := map[string]any{"version": 1, "source": "opencode/skills", "sourceKind": "bundle", "bundleVersion": "latest", "bundleCommit": "test", "package": "skills", "target": "opencode", "files": files}
	m["treeSHA256"] = canonicalTreeHash(m)
	return m
}

func canonicalTreeHash(m map[string]any) string {
	var files []manifestFile
	data, _ := json.Marshal(m["files"])
	_ = json.Unmarshal(data, &files)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	h := sha256.New()
	fmt.Fprintf(h, "source\x00%s\nsourceKind\x00%s\nbundleVersion\x00%s\nbundleCommit\x00%s\npackage\x00%s\ntarget\x00%s\n", m["source"], m["sourceKind"], m["bundleVersion"], m["bundleCommit"], m["package"], m["target"])
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%s\n", f.Path, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func decisionLines(output string) []string {
	var decisions []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "remove ") || strings.HasPrefix(line, "preserve ") {
			decisions = append(decisions, line)
		}
	}
	return decisions
}

func writeUninstallJSON(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
func runSkynex(t *testing.T, args ...string) string { return runSkynexWithHome(t, "", args...) }
func runSkynexWithHome(t *testing.T, home string, args ...string) string {
	t.Helper()
	out, err := runSkynexResult(t, home, args...)
	if err != nil {
		t.Fatalf("skynex %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}
func runSkynexResult(t *testing.T, home string, args ...string) ([]byte, error) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "skynex")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		return out, err
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "HOME="+home, "GOPATH=/home/clasing/go", "GOCACHE=/home/clasing/.cache/go-build")
	return cmd.CombinedOutput()
}
func runSkynexInput(t *testing.T, home, input string, args ...string) ([]byte, error) {
	t.Helper()
	binaryPath := filepath.Join(t.TempDir(), "skynex")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		return out, err
	}
	cmd := exec.Command(binaryPath, args...)
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "HOME="+home, "GOPATH=/home/clasing/go", "GOCACHE=/home/clasing/.cache/go-build")
	cmd.Stdin = strings.NewReader(input)
	return cmd.CombinedOutput()
}
func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists, err=%v", path, err)
	}
}

type fileSnapshot struct {
	mode         fs.FileMode
	digest, link string
}

func snapshotPaths(t *testing.T, roots ...string) map[string]fileSnapshot {
	t.Helper()
	got := map[string]fileSnapshot{}
	for _, root := range roots {
		info, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		if info.IsDir() {
			if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				addSnapshot(t, got, path, entry.Type())
				if entry.IsDir() && path != root {
					return nil
				}
				return nil
			}); err != nil {
				t.Fatalf("snapshot walk %s: %v", root, err)
			}
		} else {
			addSnapshot(t, got, root, info.Mode())
		}
	}
	return got
}
func addSnapshot(t *testing.T, got map[string]fileSnapshot, path string, mode fs.FileMode) {
	t.Helper()
	s := fileSnapshot{mode: mode}
	if mode&os.ModeSymlink != 0 {
		s.link = mustReadlink(t, path)
	} else if mode.IsRegular() {
		s.digest = digest(readFile(t, path))
	}
	got[path] = s
}
func mustReadlink(t *testing.T, path string) string {
	t.Helper()
	value, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
