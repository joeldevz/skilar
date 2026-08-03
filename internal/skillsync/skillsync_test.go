package skillsync

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func makeBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func inspectBundle(t *testing.T, bundle, target, manifest string) Report {
	t.Helper()
	r, err := Inspect(os.DirFS(bundle), target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func applyReport(t *testing.T, bundle, target, manifest string, r Report, decisions map[string]Decision) error {
	t.Helper()
	return Apply(os.DirFS(bundle), target, manifest, r, decisions, Manifest{Source: "skills", SourceKind: "bundle", BundleVersion: "test", Package: "skills", Target: "opencode"})
}

func TestInspectTempDirStatusesAndDeterminism(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"current.md": "same", "outdated.md": "old", "retired.md": "gone", "modified.md": "new", "absent.md": "absent"})
	target := t.TempDir()
	manifest := filepath.Join(t.TempDir(), "skills.ownership.json")
	initial := inspectBundle(t, bundle, target, manifest)
	if err := applyReport(t, bundle, target, manifest, initial, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "outdated.md"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "retired.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "missing.md"), []byte("missing"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"current.md": "same", "outdated.md": "old", "retired.md": "gone", "modified.md": "local", "unknown.md": "user"} {
		if err := os.WriteFile(filepath.Join(target, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := inspectBundle(t, bundle, target, manifest)
	statuses := map[string]Status{}
	for _, e := range r.Entries {
		statuses[e.Path] = e.Status
	}
	for path, want := range map[string]Status{"current.md": Current, "outdated.md": Outdated, "modified.md": Modified, "unknown.md": Unknown, "missing.md": Missing, "retired.md": Retired} {
		if statuses[path] != want {
			t.Fatalf("%s: got %s want %s", path, statuses[path], want)
		}
	}
	if r.BundleTreeSHA256 == "" {
		t.Fatal("missing deterministic bundle tree hash")
	}
	repeated := inspectBundle(t, bundle, target, manifest)
	if !reflect.DeepEqual(r.Entries, repeated.Entries) || r.BundleTreeSHA256 != repeated.BundleTreeSHA256 {
		t.Fatalf("repeated inspection was not deterministic: first=%+v second=%+v", r, repeated)
	}
	for i := 1; i < len(repeated.Entries); i++ {
		if repeated.Entries[i-1].Path >= repeated.Entries[i].Path {
			t.Fatalf("entries are not strictly ordered: %+v", repeated.Entries)
		}
	}
}

func TestLegacyAdoptionAndUnknownPreservation(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"skill.md": "packaged"})
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "skill.md"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := inspectBundle(t, bundle, target, filepath.Join(t.TempDir(), "missing.json"))
	if r.Adoptable || r.Entries[0].Status != Unknown {
		t.Fatalf("different legacy content should be preserved: %+v", r)
	}
	if err := applyReport(t, bundle, target, filepath.Join(t.TempDir(), "missing.json"), r, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(target, "skill.md"))
	if string(b) != "local" {
		t.Fatal("legacy unknown was changed")
	}
	if err := os.WriteFile(filepath.Join(target, "skill.md"), []byte("packaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = inspectBundle(t, bundle, target, filepath.Join(t.TempDir(), "missing.json"))
	if !r.Adoptable {
		t.Fatal("exact legacy bundle should be adoptable")
	}
}

func TestLegacySymlinkAndSpecialAreNotAdoptable(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"skill.md": "packaged"})
	target := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "skill.md")); err != nil {
		t.Fatal(err)
	}
	r := inspectBundle(t, bundle, target, filepath.Join(t.TempDir(), "missing.json"))
	if r.Adoptable || r.Entries[0].Status != Unknown {
		t.Fatalf("legacy symlink became adoptable: %+v", r)
	}
}

func TestRetiredIntactRemovedModifiedNeedsReplace(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"retired.md": "old", "kept.md": "new"})
	target := t.TempDir()
	manifest := filepath.Join(t.TempDir(), "ownership.json")
	if err := os.WriteFile(filepath.Join(target, "retired.md"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	seed := inspectBundle(t, bundle, target, manifest)
	if err := applyReport(t, bundle, target, manifest, seed, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "retired.md")); err != nil {
		t.Fatal(err)
	}
	retired := inspectBundle(t, bundle, target, manifest)
	retiredEntry := findEntry(retired, "retired.md")
	if retiredEntry.Status != Retired {
		t.Fatalf("expected retired status: %+v", retired.Entries)
	}
	entry := findEntry(retired, "retired.md")
	if err := applyReport(t, bundle, target, manifest, retired, map[string]Decision{"retired.md": BindDecision(entry.Path, Replace, entry.LocalSHA256, entry.BundleSHA256, entry.BundleTreeSHA256)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "retired.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("intact retired file not removed")
	}
	if err := os.WriteFile(filepath.Join(target, "retired.md"), []byte("user"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRetiredModifiedRequiresReplaceDecision(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"retired.md": "packaged"})
	target := t.TempDir()
	manifest := filepath.Join(t.TempDir(), "ownership.json")
	seed := inspectBundle(t, bundle, target, manifest)
	if err := applyReport(t, bundle, target, manifest, seed, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundle, "retired.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "retired.md"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := inspectBundle(t, bundle, target, manifest)
	if findEntry(r, "retired.md").Status != Modified {
		t.Fatalf("expected modified retired entry: %+v", r.Entries)
	}
	entry := findEntry(r, "retired.md")
	if err := applyReport(t, bundle, target, manifest, r, map[string]Decision{"retired.md": BindDecision(entry.Path, Replace, entry.LocalSHA256, entry.BundleSHA256, entry.BundleTreeSHA256)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "retired.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replace decision should retire modified file")
	}
}

func TestModifiedReplaceBindsSourceDestinationHashes(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"skill.md": "packaged"})
	target := t.TempDir()
	manifest := filepath.Join(t.TempDir(), "ownership.json")
	initial := inspectBundle(t, bundle, target, manifest)
	if err := applyReport(t, bundle, target, manifest, initial, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "skill.md"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := inspectBundle(t, bundle, target, manifest)
	if err := os.WriteFile(filepath.Join(bundle, "skill.md"), []byte("changed source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyReport(t, bundle, target, manifest, r, map[string]Decision{"skill.md": Replace}); !errors.Is(err, ErrChangedSinceDecision) {
		t.Fatalf("source change: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "skill.md"), []byte("packaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = inspectBundle(t, bundle, target, manifest)
	if err := os.WriteFile(filepath.Join(target, "skill.md"), []byte("changed destination"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyReport(t, bundle, target, manifest, r, map[string]Decision{"skill.md": Replace}); !errors.Is(err, ErrChangedSinceDecision) {
		t.Fatalf("destination change: %v", err)
	}
}

func TestModifiedKeepPreservesLocalContent(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"skill.md": "packaged"})
	target := t.TempDir()
	manifest := filepath.Join(t.TempDir(), "ownership.json")
	seed := inspectBundle(t, bundle, target, manifest)
	if err := applyReport(t, bundle, target, manifest, seed, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "skill.md"), []byte("edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := inspectBundle(t, bundle, target, manifest)
	if findEntry(r, "skill.md").Status != Modified {
		t.Fatal("expected modified skill")
	}
	if err := applyReport(t, bundle, target, manifest, r, map[string]Decision{"skill.md": Keep}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, "skill.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "edited" {
		t.Fatalf("keep changed local content to %q", data)
	}
}

func TestNestedSymlinkCannotEscapeWriteOrDelete(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"nested/skill.md": "safe"})
	target := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(target, "nested")); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(t.TempDir(), "ownership.json")
	r := inspectBundle(t, bundle, target, manifest)
	if err := applyReport(t, bundle, target, manifest, r, nil); err == nil {
		t.Fatal("write followed nested symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "skill.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("write escaped through symlink")
	}
}

func TestDeleteSymlinkDoesNotTouchTarget(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"skill.md": "packaged"})
	target := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "skill.md")); err != nil {
		t.Fatal(err)
	}
	r := inspectBundle(t, bundle, target, filepath.Join(t.TempDir(), "missing.json"))
	if err := applyReport(t, bundle, target, filepath.Join(t.TempDir(), "missing.json"), r, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "untouched" {
		t.Fatal("symlink target was modified")
	}
}

func TestApplyRaceReplacingFinalEntryCannotTouchExternalTarget(t *testing.T) {
	bundle := t.TempDir()
	target := t.TempDir()
	external := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(filepath.Join(bundle, "skill.md"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(external, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial := inspectBundle(t, bundle, target, filepath.Join(t.TempDir(), "manifest.json"))
	beforeSkillsMutation = func() error {
		return os.Symlink(external, filepath.Join(target, "skill.md"))
	}
	t.Cleanup(func() { beforeSkillsMutation = nil })
	if err := applyReport(t, bundle, target, filepath.Join(t.TempDir(), "manifest.json"), initial, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(external)
	if err != nil || string(got) != "sentinel" {
		t.Fatalf("external target changed: %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(target, "skill.md"))
	if err != nil || string(got) != "new" {
		t.Fatalf("destination was not replaced atomically: %q, %v", got, err)
	}
}

func TestManifestValidationAndPermissions(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"skill.md": "safe"})
	target := t.TempDir()
	manifest := filepath.Join(t.TempDir(), "ownership.json")
	r := inspectBundle(t, bundle, target, manifest)
	if err := applyReport(t, bundle, target, manifest, r, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty manifest")
	}
	if err := os.WriteFile(manifest, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(os.DirFS(bundle), target, manifest); err == nil {
		t.Fatal("future malformed manifest accepted")
	}
}
