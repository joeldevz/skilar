package installer

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/safefs"
)

func TestApplyRestoresExistingAndRemovesMissing(t *testing.T) {
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(claude, "existing.txt")
	if err := os.WriteFile(existing, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := &Plan{destinations: Destinations{ClaudeDir: claude, StateDir: state}}
	err := Apply(plan, func() error {
		if err := os.WriteFile(existing, []byte("after"), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(claude, "new.txt"), []byte("new"), 0o600); err != nil {
			return err
		}
		return errors.New("mutation failed")
	})
	if err == nil {
		t.Fatal("expected mutation failure")
	}
	got, readErr := os.ReadFile(existing)
	if readErr != nil || string(got) != "before" {
		t.Fatalf("existing = %q, %v", got, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(claude, "new.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("new file remains: %v", statErr)
	}
}

func TestRollbackRestoresMultipleStateRootsAfterMutation(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	claude := filepath.Join(root, "claude")
	sentinel := errors.New("sentinel failure")
	files := map[string]string{
		"skills.config.json":    "config-before",
		"skills.lock.json":      "lock-before",
		"skills.ownership.json": "ownership-before",
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(state, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	err := Apply(&Plan{destinations: Destinations{ClaudeDir: claude, StateDir: state}}, func() error {
		for name := range files {
			if err := os.WriteFile(filepath.Join(state, name), []byte("mutated"), 0o600); err != nil {
				return err
			}
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback result = %v, want sentinel failure", err)
	}
	if strings.Contains(err.Error(), "refusing rollback of replaced root") {
		t.Errorf("rollback reported its own replaced root: %v", err)
	}
	for name, want := range files {
		got, readErr := os.ReadFile(filepath.Join(state, name))
		if readErr != nil || !bytes.Equal(got, []byte(want)) {
			t.Errorf("%s = %q, %v; want original bytes %q", name, got, readErr, want)
		}
	}
}

func TestRollbackRejectsAtomicReplacementAfterRestoringRegularFileRoot(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	config := filepath.Join(state, "skills.config.json")
	sentinel := errors.New("mutation sentinel")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	hookCalled := false
	snapshotHooks.AfterRestore = func(path string) error {
		if path != config {
			return nil
		}
		hookCalled = true
		tmp := filepath.Join(state, ".attacker-replacement")
		if err := os.WriteFile(tmp, []byte("attacker replacement"), 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	t.Cleanup(func() { snapshotHooks.AfterRestore = nil })

	err := Apply(&Plan{destinations: Destinations{StateDir: state}}, func() error {
		if err := os.WriteFile(config, []byte("mutated"), 0o600); err != nil {
			return err
		}
		return sentinel
	})
	if !hookCalled {
		t.Fatal("after-restore replacement hook was not called for the canonical file root")
	}
	if err == nil || !strings.Contains(err.Error(), "replaced root") {
		t.Fatalf("rollback result = %v, want replaced-root integrity error", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback result = %v, want primary sentinel discoverable", err)
	}
	got, readErr := os.ReadFile(config)
	if readErr != nil || string(got) != "attacker replacement" {
		t.Fatalf("attacker replacement = %q, %v; rollback must not overwrite or delete it", got, readErr)
	}
}

func TestRollbackRejectsReplacementBeforePostRenamePathOperation(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	config := filepath.Join(state, "skills.config.json")
	sentinel := errors.New("mutation sentinel")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	hookCalled := false
	const attackerBytes = "attacker replacement"
	snapshotHooks.AfterRestoreRename = func(path string) error {
		if path != config {
			return nil
		}
		hookCalled = true
		tmp := filepath.Join(state, ".attacker-replacement")
		if err := os.WriteFile(tmp, []byte(attackerBytes), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	t.Cleanup(func() { snapshotHooks.AfterRestoreRename = nil })

	err := Apply(&Plan{destinations: Destinations{StateDir: state}}, func() error {
		if err := os.WriteFile(config, []byte("mutated"), 0o600); err != nil {
			return err
		}
		return sentinel
	})
	if !hookCalled {
		t.Fatal("after-rename replacement hook was not called for the canonical file root")
	}
	if err == nil || !strings.Contains(err.Error(), "replaced root") {
		t.Fatalf("rollback result = %v, want replaced-root integrity error", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback result = %v, want primary sentinel discoverable", err)
	}
	got, readErr := os.ReadFile(config)
	if readErr != nil || string(got) != attackerBytes {
		t.Fatalf("attacker replacement = %q, %v; rollback must preserve attacker content", got, readErr)
	}
	info, statErr := os.Stat(config)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Fatalf("attacker replacement mode = %o, want 644; post-rename chmod must not affect attacker", mode)
	}
}

func TestApplyRejectsSymlinkAncestorBeforeCallback(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	called := false
	err := Apply(&Plan{destinations: Destinations{ClaudeDir: filepath.Join(link, "child"), StateDir: filepath.Join(root, "state")}}, func() error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("symlink ancestor accepted: err=%v called=%v", err, called)
	}
}

func TestApplyPanicRollsBack(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "claude", "x")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
		got, err := os.ReadFile(file)
		if err != nil || string(got) != "old" {
			t.Fatalf("file not restored: %q %v", got, err)
		}
	}()
	_ = Apply(&Plan{destinations: Destinations{ClaudeDir: filepath.Dir(file), StateDir: filepath.Join(root, "state")}}, func() error {
		_ = os.WriteFile(file, []byte("new"), 0o600)
		panic("boom")
	})
}

func TestRollbackStopsWhenRootChangesBetweenEntries(t *testing.T) {
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	opencode := filepath.Join(root, "opencode")
	for _, dir := range []string{claude, opencode} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(claude, "old"), []byte("claude-old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opencode, "old"), []byte("opencode-old"), 0o600); err != nil {
		t.Fatal(err)
	}
	swapped := false
	snapshotHooks.BeforeRestore = func(path string) error {
		if swapped || !strings.HasPrefix(path, opencode) {
			return nil
		}
		swapped = true
		replacement := filepath.Join(root, "replacement")
		if err := os.Mkdir(replacement, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(replacement, "sentinel"), []byte("untouched"), 0o600); err != nil {
			return err
		}
		if err := os.RemoveAll(opencode); err != nil {
			return err
		}
		return os.Rename(replacement, opencode)
	}
	t.Cleanup(func() { snapshotHooks.BeforeRestore = nil })
	err := Apply(&Plan{destinations: Destinations{ClaudeDir: claude, OpencodeDir: opencode, StateDir: filepath.Join(root, "state")}}, func() error {
		if err := os.WriteFile(filepath.Join(claude, "new"), []byte("new"), 0o600); err != nil {
			return err
		}
		return errors.New("force rollback")
	})
	if err == nil || !strings.Contains(err.Error(), "replaced root") {
		t.Fatalf("rollback race result: %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(opencode, "sentinel"))
	if readErr != nil || string(got) != "untouched" {
		t.Fatalf("replacement root changed: %q, %v", got, readErr)
	}
}

func TestApplyRestoresSymlinkAndPermissions(t *testing.T) {
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(claude, "file")
	if err := os.WriteFile(file, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(claude, "link")
	if err := os.Symlink("file", link); err != nil {
		t.Fatal(err)
	}
	err := Apply(&Plan{destinations: Destinations{ClaudeDir: claude, StateDir: filepath.Join(root, "state")}}, func() error {
		if err := os.Remove(link); err != nil {
			return err
		}
		if err := os.WriteFile(file, []byte("new"), 0o600); err != nil {
			return err
		}
		return errors.New("stop")
	})
	if err == nil {
		t.Fatal("expected rollback error")
	}
	info, statErr := os.Stat(file)
	if statErr != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, err=%v", info.Mode(), statErr)
	}
	got, readErr := os.ReadFile(file)
	if readErr != nil || string(got) != "old" {
		t.Fatalf("restored content = %q, err=%v", got, readErr)
	}
	target, readErr := os.Readlink(link)
	if readErr != nil || target != "file" {
		t.Fatalf("link = %q, err=%v", target, readErr)
	}
}

func TestApplyPreservesNodeModulesInPlace(t *testing.T) {
	root := t.TempDir()
	opencode := filepath.Join(root, "opencode")
	if err := os.MkdirAll(filepath.Join(opencode, "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(opencode, "node_modules", "old")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	err := Apply(&Plan{destinations: Destinations{OpencodeDir: opencode, StateDir: state}}, func() error {
		if err := os.MkdirAll(filepath.Join(opencode, "node_modules"), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(opencode, "node_modules", "new"), []byte("new"), 0o600); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("old node_modules not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(opencode, "node_modules", "new")); err != nil {
		t.Fatalf("node_modules update was lost: %v", err)
	}
	if err := Apply(&Plan{destinations: Destinations{OpencodeDir: opencode, StateDir: state}}, func() error {
		if err := os.MkdirAll(filepath.Join(opencode, "node_modules"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(opencode, "node_modules", "fresh"), []byte("fresh"), 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(opencode, "node_modules", "fresh")); err != nil {
		t.Fatalf("successful mutation lost: %v", err)
	}
}

func TestApplyRejectsOverlapAndStateInsideTarget(t *testing.T) {
	root := t.TempDir()
	if err := Apply(&Plan{destinations: Destinations{ClaudeDir: filepath.Join(root, "x"), ClaudeConfigFile: filepath.Join(root, "x", "config"), StateDir: filepath.Join(root, "state")}}, func() error { return nil }); err == nil {
		t.Fatal("overlap accepted")
	}
	if err := Apply(&Plan{destinations: Destinations{ClaudeDir: filepath.Join(root, "x"), StateDir: filepath.Join(root, "x", "state")}}, func() error { return nil }); err == nil {
		t.Fatal("state inside target accepted")
	}
}

func TestSnapshotRootsFollowSelectedTargetOperations(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	claude := filepath.Join(root, "claude")
	opencode := filepath.Join(root, "opencode")
	for _, test := range []struct {
		name, target string
		want         []string
	}{
		{"opencode only", "opencode", []string{opencode, filepath.Join(state, "skills.ownership.json"), filepath.Join(state, "skills.config.json"), filepath.Join(state, "skills.lock.json")}},
		{"claude only", "claude", []string{claude, filepath.Join(state, "skills.ownership.json"), filepath.Join(state, "skills.config.json"), filepath.Join(state, "skills.lock.json")}},
		{"both", "both", []string{claude, opencode, filepath.Join(state, "skills.ownership.json"), filepath.Join(state, "skills.config.json"), filepath.Join(state, "skills.lock.json")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var operations []Operation
			if test.target == "claude" || test.target == "both" {
				operations = append(operations, Operation{Kind: InstallTarget, Target: "claude", Destination: claude})
			}
			if test.target == "opencode" || test.target == "both" {
				operations = append(operations, Operation{Kind: InstallTarget, Target: "opencode", Destination: opencode})
			}
			operations = append(operations,
				Operation{Kind: WriteState, Destination: filepath.Join(state, "skills.config.json")},
				Operation{Kind: WriteState, Destination: filepath.Join(state, "skills.lock.json")},
			)
			plan := &Plan{Operations: operations, destinations: Destinations{
				ClaudeDir: claude, ClaudeConfigFile: filepath.Join(claude, "config"), OpencodeDir: opencode,
				StateDir: state, OwnershipManifest: filepath.Join(state, "skills.ownership.json"),
			}}
			got, _, err := snapshotRoots(plan)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("roots=%v, want %v", got, test.want)
			}
		})
	}
}

func TestOpenCodeSnapshotDoesNotStatUnselectedClaudePath(t *testing.T) {
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	file, err := os.OpenFile(claude, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxSnapshotRegularBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	opencode := filepath.Join(root, "opencode")
	state := filepath.Join(root, "state")
	plan := &Plan{
		Operations: []Operation{
			{Kind: InstallTarget, Target: "opencode", Destination: opencode},
			{Kind: WriteState, Destination: filepath.Join(state, "skills.config.json")},
			{Kind: WriteState, Destination: filepath.Join(state, "skills.lock.json")},
		},
		destinations: Destinations{ClaudeDir: claude, OpencodeDir: opencode, StateDir: state},
	}
	if err := Apply(plan, func() error { return nil }); err != nil {
		t.Fatalf("unselected Claude path affected OpenCode snapshot: %v", err)
	}
}

func TestSnapshotLimitErrorNamesSelectedTargetOnly(t *testing.T) {
	root := t.TempDir()
	opencode := filepath.Join(root, "opencode")
	if err := os.MkdirAll(opencode, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(opencode, "large"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxSnapshotRegularBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	claude := filepath.Join(root, "claude")
	plan := &Plan{
		Operations:   []Operation{{Kind: InstallTarget, Target: "opencode", Destination: opencode}},
		destinations: Destinations{ClaudeDir: claude, OpencodeDir: opencode, StateDir: filepath.Join(root, "state")},
	}
	err = Apply(plan, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), opencode) || strings.Contains(err.Error(), claude) {
		t.Fatalf("snapshot limit error=%v, want selected OpenCode target only", err)
	}
}

func TestSnapshotArtifactsHavePrivateModes(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := Apply(&Plan{destinations: Destinations{ClaudeDir: filepath.Join(root, "claude"), StateDir: state}}, func() error { return errors.New("rollback") }); err == nil {
		t.Fatal("expected rollback")
	}
	entries, err := os.ReadDir(filepath.Join(state, "snapshots"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshots = %v, %v", entries, err)
	}
	info, err := entries[0].Info()
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("snapshot mode = %v, err=%v", info.Mode(), err)
	}
	manifest := filepath.Join(state, "snapshots", entries[0].Name(), "manifest.json")
	manifestInfo, err := os.Stat(manifest)
	if err != nil || manifestInfo.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %v, err=%v", manifestInfo.Mode(), err)
	}
}

func TestSnapshotCapacityRefusesWithoutPruningRetention(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	plan := &Plan{destinations: Destinations{ClaudeDir: filepath.Join(root, "claude"), StateDir: state}}
	for i := 0; i < snapshotCap; i++ {
		if err := Apply(plan, func() error { return errors.New("keep recovery") }); err == nil {
			t.Fatal("expected mutation failure")
		}
	}
	entries, err := os.ReadDir(filepath.Join(state, "snapshots"))
	if err != nil || len(entries) != snapshotCap {
		t.Fatalf("retained snapshots = %d, %v", len(entries), err)
	}
	if err := Apply(plan, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "snapshot capacity reached") {
		t.Fatalf("capacity error = %v", err)
	}
	entries, err = os.ReadDir(filepath.Join(state, "snapshots"))
	if err != nil || len(entries) != snapshotCap {
		t.Fatalf("capacity attempt changed retention: %d, %v", len(entries), err)
	}
}

func TestPreparationFailureRemovesPartialSnapshotAndPayload(t *testing.T) {
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	sensitive := filepath.Join(claude, "sensitive.txt")
	if err := os.WriteFile(sensitive, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshotHooks.BeforePayloadCopy = func() error { return errors.New("injected preparation failure") }
	t.Cleanup(func() { snapshotHooks.BeforePayloadCopy = nil })

	if err := Apply(&Plan{destinations: Destinations{ClaudeDir: claude, StateDir: state}}, func() error {
		t.Fatal("mutation callback ran after preparation failure")
		return nil
	}); err == nil || !strings.Contains(err.Error(), "injected preparation failure") {
		t.Fatalf("error = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(state, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial recovery snapshot was not cleaned: %v", entries)
	}
	got, err := os.ReadFile(sensitive)
	if err != nil || string(got) != "secret" {
		t.Fatalf("sensitive source = %q, %v", got, err)
	}
}

func TestPreparationRejectsOversizedRegularFileBeforeMutation(t *testing.T) {
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "large"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(claude, "large"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxSnapshotRegularBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	called := false
	err = Apply(&Plan{destinations: Destinations{ClaudeDir: claude, StateDir: filepath.Join(root, "state")}}, func() error {
		called = true
		return nil
	})
	if err == nil || called || !strings.Contains(err.Error(), "snapshot limits exceeded") {
		t.Fatalf("oversized snapshot result: err=%v called=%v", err, called)
	}
}

func TestPreparationRejectsTooManyRegularFiles(t *testing.T) {
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxSnapshotRegularFiles; i++ {
		if err := os.WriteFile(filepath.Join(claude, fmt.Sprintf("file-%d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Apply(&Plan{destinations: Destinations{ClaudeDir: claude, StateDir: filepath.Join(root, "state")}}, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "snapshot limits exceeded") {
		t.Fatalf("file count limit result: %v", err)
	}
}

func TestPreparationRejectsMetadataOnlyEntryLimit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	parent, err := safefs.Open(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	payload, err := safefs.OpenOrCreate(filepath.Join(t.TempDir(), "payload"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	defer payload.Close()
	_, err = capture(&rootedPath{parent: parent, name: filepath.Base(root), path: root}, root, false, false, payload, &snapshotLimits{entries: maxSnapshotEntries - 1})
	if err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("metadata-only entry limit result: %v", err)
	}
}

func TestSnapshotCountsRootSymlinkAgainstEntryLimit(t *testing.T) {
	root := t.TempDir()
	parent, err := safefs.Open(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	payload, err := safefs.OpenOrCreate(filepath.Join(t.TempDir(), "payload"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	defer payload.Close()
	link := filepath.Join(filepath.Dir(root), "root-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	linkParent, err := safefs.Open(filepath.Dir(link))
	if err != nil {
		t.Fatal(err)
	}
	defer linkParent.Close()
	if _, err := capture(&rootedPath{parent: linkParent, name: filepath.Base(link), path: link}, link, false, false, payload, &snapshotLimits{entries: maxSnapshotEntries}); err == nil {
		t.Fatal("root symlink over entry boundary was accepted")
	}
}

func TestSnapshotStreamingBudgetIncludesBoundaryByte(t *testing.T) {
	payload, err := safefs.OpenOrCreate(filepath.Join(t.TempDir(), "payload"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	defer payload.Close()
	if _, err := savePayloadReaderLimited(bytes.NewReader(bytes.Repeat([]byte{'x'}, int(maxSnapshotRegularBytes))), payload, "exact", &snapshotLimits{}); err != nil {
		t.Fatalf("exact byte budget rejected: %v", err)
	}
	if _, err := savePayloadReaderLimited(bytes.NewReader([]byte{'x'}), payload, "over", &snapshotLimits{regularBytes: maxSnapshotRegularBytes}); err == nil {
		t.Fatal("byte budget overrun accepted")
	}
}

func TestApplyRejectsInstallTargetDestinationBeforeFilesystemAccess(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	external := filepath.Join(root, "external")
	file, err := os.OpenFile(external, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxSnapshotRegularBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	called := false
	err = Apply(&Plan{
		Version:      1,
		Operations:   []Operation{{Kind: InstallTarget, Target: "opencode", Destination: external}},
		destinations: Destinations{OpencodeDir: filepath.Join(root, "opencode"), StateDir: state},
	}, func() error {
		called = true
		return nil
	})
	if err == nil || called || !strings.Contains(err.Error(), "destination") {
		t.Fatalf("arbitrary destination result: err=%v called=%v", err, called)
	}
}

func TestApplyRejectsMissingOrUnknownInstallTargetBeforeFilesystemAccess(t *testing.T) {
	root := t.TempDir()
	for _, target := range []string{"", "other"} {
		t.Run(fmt.Sprintf("target-%q", target), func(t *testing.T) {
			called := false
			err := Apply(&Plan{
				Version:      1,
				Operations:   []Operation{{Kind: InstallTarget, Target: target, Destination: filepath.Join(root, "opencode")}},
				destinations: Destinations{OpencodeDir: filepath.Join(root, "opencode"), StateDir: filepath.Join(root, "state")},
			}, func() error {
				called = true
				return nil
			})
			if err == nil || called || !strings.Contains(err.Error(), "target") {
				t.Fatalf("invalid target result: err=%v called=%v", err, called)
			}
		})
	}
}

func TestApplyRejectsStateOperationOutsideCanonicalStateFilesBeforeFilesystemAccess(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	external := filepath.Join(root, "state-escape")
	called := false
	err := Apply(&Plan{
		Version: 1,
		Operations: []Operation{
			{Kind: InstallTarget, Target: "opencode", Destination: filepath.Join(root, "opencode")},
			{Kind: WriteState, Destination: external},
		},
		destinations: Destinations{OpencodeDir: filepath.Join(root, "opencode"), StateDir: state},
	}, func() error {
		called = true
		return nil
	})
	if err == nil || called || !strings.Contains(err.Error(), "state") {
		t.Fatalf("state escape result: err=%v called=%v", err, called)
	}
}

func TestApplyRejectsRelativeStateOperationBeforeFilesystemAccess(t *testing.T) {
	root := t.TempDir()
	called := false
	err := Apply(&Plan{
		Version: 1,
		Operations: []Operation{
			{Kind: InstallTarget, Target: "opencode", Destination: filepath.Join(root, "opencode")},
			{Kind: WriteState, Destination: "state/skills.config.json"},
		},
		destinations: Destinations{OpencodeDir: filepath.Join(root, "opencode"), StateDir: filepath.Join(root, "state")},
	}, func() error {
		called = true
		return nil
	})
	if err == nil || called || !strings.Contains(err.Error(), "state") {
		t.Fatalf("relative state result: err=%v called=%v", err, called)
	}
}
