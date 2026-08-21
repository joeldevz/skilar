package skillsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestParseManifestIsReusableCanonicalValidation(t *testing.T) {
	files := []File{{Path: "zeta/skill.md", SHA256: hashBytes([]byte("zeta"))}, {Path: "alpha.md", SHA256: hashBytes([]byte("alpha"))}}
	m := Manifest{Version: ManifestVersion, Source: "opencode/skills", SourceKind: "bundle", BundleVersion: "latest", BundleCommit: "test", Package: "skills", Target: "opencode", GeneratedAt: "2026-08-21T00:00:00Z", Files: files}
	m.TreeSHA256 = canonicalManifestTreeHash(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest(b)
	if err != nil {
		t.Fatalf("canonical manifest parser rejected valid metadata: manifest=%+v err=%v", parsed, err)
	}
	if parsed.Version != m.Version || parsed.Source != m.Source || parsed.SourceKind != m.SourceKind || parsed.BundleVersion != m.BundleVersion || parsed.BundleCommit != m.BundleCommit || parsed.TreeSHA256 != m.TreeSHA256 || parsed.Package != m.Package || parsed.Target != m.Target || parsed.GeneratedAt != m.GeneratedAt {
		t.Fatalf("parser did not preserve canonical fields: got=%+v want=%+v", parsed, m)
	}
	if !reflect.DeepEqual(parsed.Files, files) || parsed.Files[0].Path != "zeta/skill.md" || parsed.Files[0].SHA256 != files[0].SHA256 || parsed.Files[1].Path != "alpha.md" || parsed.Files[1].SHA256 != files[1].SHA256 {
		t.Fatalf("parser did not preserve complete file entries/hashes: got=%+v want=%+v", parsed.Files, files)
	}
	if _, err := ParseManifest([]byte(`{"version":1,"files":[]}`)); err == nil {
		t.Fatal("minimal manifest accepted")
	}
}

func TestParseManifestCanonicalizesTreeHashIndependentOfFileOrder(t *testing.T) {
	first := []File{{Path: "z.md", SHA256: hashBytes([]byte("z"))}, {Path: "a.md", SHA256: hashBytes([]byte("a"))}}
	second := append([]File(nil), first...)
	sort.Slice(second, func(i, j int) bool { return second[i].Path < second[j].Path })
	base := Manifest{Version: ManifestVersion, Source: "opencode/skills", SourceKind: "bundle", BundleVersion: "v1", BundleCommit: "commit", Package: "skills", Target: "opencode"}
	left, right := base, base
	left.Files, right.Files = first, second
	const canonicalBytes = "source\x00opencode/skills\nsourceKind\x00bundle\nbundleVersion\x00v1\nbundleCommit\x00commit\npackage\x00skills\ntarget\x00opencode\na.md\x00ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb\nz.md\x00594e519ae499312b29433b7dd8a97ff068defcba9755b6d5d00e84c524d67b06\n"
	const canonicalHash = "0bbd4989584faa7cc0afc6d780ab3c4b940b80e0de0d8bb408f6e05e9c8f7821"
	if got := string(canonicalManifestTreeBytes(left)); got != canonicalBytes {
		t.Fatalf("unexpected canonical tree bytes: %q", got)
	}
	if got := canonicalManifestTreeHash(left); got != canonicalHash {
		t.Fatalf("unexpected canonical tree hash: %s", got)
	}
	left.TreeSHA256, right.TreeSHA256 = canonicalHash, canonicalHash
	for name, manifest := range map[string]Manifest{"unsorted": left, "sorted": right} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest(mustJSON(t, manifest)); err != nil {
				t.Fatalf("valid manifest rejected: %v", err)
			}
		})
	}
}

func TestParseManifestRejectsInvalidCanonicalCases(t *testing.T) {
	valid := Manifest{Version: ManifestVersion, Source: "opencode/skills", SourceKind: "bundle", BundleVersion: "v1", BundleCommit: "commit", Package: "skills", Target: "opencode", Files: []File{{Path: "a.md", SHA256: hashBytes([]byte("a"))}}}
	valid.TreeSHA256 = canonicalManifestTreeHash(valid)
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"unsupported version", func(m map[string]any) { m["version"] = 99 }, "version"},
		{"missing metadata", func(m map[string]any) { delete(m, "source"); m["treeSHA256"] = canonicalManifestTreeHashFromMap(m) }, "source"},
		{"wrong entry type", func(m map[string]any) { m["files"] = []any{"a.md"} }, "entry"},
		{"duplicate path", func(m map[string]any) {
			m["files"] = []any{map[string]any{"path": "a.md", "sha256": valid.Files[0].SHA256}, map[string]any{"path": "a.md", "sha256": valid.Files[0].SHA256}}
			m["treeSHA256"] = canonicalManifestTreeHashFromMap(m)
		}, "duplicate"},
		{"hostile path", func(m map[string]any) {
			m["files"] = []any{map[string]any{"path": "../a.md", "sha256": valid.Files[0].SHA256}}
			m["treeSHA256"] = canonicalManifestTreeHashFromMap(m)
		}, "path"},
		{"invalid hash", func(m map[string]any) { m["files"] = []any{map[string]any{"path": "a.md", "sha256": "not-a-hash"}} }, "hash"},
		{"wrong target", func(m map[string]any) { m["target"] = "other"; m["treeSHA256"] = canonicalManifestTreeHashFromMap(m) }, "target"},
		{"wrong package", func(m map[string]any) { m["package"] = "other"; m["treeSHA256"] = canonicalManifestTreeHashFromMap(m) }, "package"},
		{"tree mismatch", func(m map[string]any) {
			m["treeSHA256"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}, "tree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := manifestMap(valid)
			tc.mutate(m)
			if _, err := ParseManifest(mustJSON(t, m)); err == nil {
				t.Fatalf("invalid manifest accepted: %s", tc.name)
			} else if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("%s returned wrong validation error: %v (want %q)", tc.name, err, tc.want)
			}
		})
	}
}

func canonicalManifestTreeBytes(m Manifest) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "source\x00%s\nsourceKind\x00%s\nbundleVersion\x00%s\nbundleCommit\x00%s\npackage\x00%s\ntarget\x00%s\n", m.Source, m.SourceKind, m.BundleVersion, m.BundleCommit, m.Package, m.Target)
	files := append([]File(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		fmt.Fprintf(&b, "%s\x00%s\n", f.Path, f.SHA256)
	}
	return []byte(b.String())
}

func canonicalManifestTreeHashFromMap(m map[string]any) string {
	b, _ := json.Marshal(m)
	var parsed Manifest
	_ = json.Unmarshal(b, &parsed)
	return hashBytes(canonicalManifestTreeBytes(parsed))
}

func canonicalManifestTreeHash(m Manifest) string {
	h := sha256.New()
	fmt.Fprintf(h, "source\x00%s\nsourceKind\x00%s\nbundleVersion\x00%s\nbundleCommit\x00%s\npackage\x00%s\ntarget\x00%s\n", m.Source, m.SourceKind, m.BundleVersion, m.BundleCommit, m.Package, m.Target)
	files := append([]File(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%s\n", f.Path, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func manifestMap(m Manifest) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

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

func TestUpgradeProvenanceFromWorkspaceToLatestEmbedded(t *testing.T) {
	bundle := makeBundle(t, map[string]string{"skill.md": "packaged"})
	target := t.TempDir()
	manifestPath := filepath.Join(t.TempDir(), "ownership.json")
	if err := os.WriteFile(filepath.Join(target, "skill.md"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := []File{{Path: "skill.md", SHA256: hashBytes([]byte("packaged"))}}
	old := Manifest{
		Version:       ManifestVersion,
		Source:        "skills",
		SourceKind:    "bundle",
		BundleVersion: "workspace",
		BundleCommit:  "old-commit",
		Package:       "skills",
		Target:        "opencode",
		Files:         files,
	}
	old.TreeSHA256 = canonicalManifestTreeHash(old)
	if err := os.WriteFile(manifestPath, mustJSON(t, old), 0o600); err != nil {
		t.Fatal(err)
	}

	report := inspectBundle(t, bundle, target, manifestPath)
	entry := findEntry(report, "skill.md")
	if entry.Status != Modified {
		t.Fatalf("expected modified entry for bound decision: %+v", report.Entries)
	}
	metadata := Manifest{Source: "skills", SourceKind: "bundle", BundleVersion: "latest", BundleCommit: "embedded", Package: "skills", Target: "opencode"}
	decision := map[string]Decision{"skill.md": BindDecision(entry.Path, Replace, entry.LocalSHA256, entry.BundleSHA256, entry.BundleTreeSHA256)}
	if err := Apply(os.DirFS(bundle), target, manifestPath, report, decision, metadata); err != nil {
		t.Fatalf("valid workspace ownership upgrade rejected: %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(target, "skill.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != "packaged" {
		t.Fatalf("bound replace did not install inspected bundle: %q", installed)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != metadata.Source || got.SourceKind != metadata.SourceKind || got.BundleVersion != metadata.BundleVersion || got.BundleCommit != metadata.BundleCommit || got.Package != metadata.Package || got.Target != metadata.Target {
		t.Fatalf("manifest did not record upgraded provenance: got=%+v want=%+v", got, metadata)
	}
	if !reflect.DeepEqual(got.Files, files) {
		t.Fatalf("manifest files were not bound to inspected bundle: got=%+v want=%+v", got.Files, files)
	}
	wantTree := metadata
	wantTree.Version = ManifestVersion
	wantTree.Files = files
	wantTree.TreeSHA256 = canonicalManifestTreeHash(wantTree)
	if got.TreeSHA256 != wantTree.TreeSHA256 {
		t.Fatalf("manifest tree hash was not bound to inspected bundle: got=%s want=%s", got.TreeSHA256, wantTree.TreeSHA256)
	}
}

func TestRejectProvenanceStableIdentityMismatch(t *testing.T) {
	base := Manifest{Version: ManifestVersion, Source: "skills", SourceKind: "bundle", BundleVersion: "workspace", BundleCommit: "old-commit", Package: "skills", Target: "opencode"}
	cases := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"source", func(m *Manifest) { m.Source = "different-source" }},
		{"source kind", func(m *Manifest) { m.SourceKind = "different-kind" }},
		{"package", func(m *Manifest) { m.Package = "different-package" }},
		{"target", func(m *Manifest) { m.Target = "different-target" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := makeBundle(t, map[string]string{"skill.md": "packaged"})
			target := t.TempDir()
			manifestPath := filepath.Join(t.TempDir(), "ownership.json")
			old := base
			old.Files = []File{{Path: "skill.md", SHA256: hashBytes([]byte("packaged"))}}
			old.TreeSHA256 = canonicalManifestTreeHash(old)
			if err := os.WriteFile(manifestPath, mustJSON(t, old), 0o600); err != nil {
				t.Fatal(err)
			}
			report := inspectBundle(t, bundle, target, manifestPath)
			metadata := Manifest{Source: "skills", SourceKind: "bundle", BundleVersion: "latest", BundleCommit: "embedded", Package: "skills", Target: "opencode"}
			tc.mutate(&metadata)
			if err := Apply(os.DirFS(bundle), target, manifestPath, report, nil, metadata); err == nil {
				t.Fatalf("%s provenance mismatch was accepted", tc.name)
			} else if !strings.Contains(err.Error(), "provenance") {
				t.Fatalf("%s mismatch returned unrelated error: %v", tc.name, err)
			}
		})
	}
}
