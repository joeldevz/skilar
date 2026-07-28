package adapters

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/models"
)

func TestDeprecatedManifest(t *testing.T) {
	// Verify manifest structure is correct
	if len(DeprecatedManifest) != 2 {
		t.Fatalf("expected 2 targets in manifest, got %d", len(DeprecatedManifest))
	}

	if _, ok := DeprecatedManifest["opencode"]; !ok {
		t.Error("missing 'opencode' target in manifest")
	}
	if _, ok := DeprecatedManifest["claude"]; !ok {
		t.Error("missing 'claude' target in manifest")
	}

	if len(DeprecatedManifest["opencode"]) < 1 {
		t.Error("opencode target should have deprecated files")
	}
	if len(DeprecatedManifest["claude"]) < 1 {
		t.Error("claude target should have deprecated files")
	}
}

func TestDeprecatedFileDisplayIncludesTargetAndRelativePath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "opencode")
	tests := []struct {
		name string
		file DeprecatedFile
		want []string
	}{
		{
			name: "opencode command",
			file: DeprecatedFile{
				Root: root, Path: filepath.Join(root, "commands", "verify-skill.md"), Target: "opencode",
			},
			want: []string{"opencode", "commands/verify-skill.md"},
		},
		{
			name: "claude skill",
			file: DeprecatedFile{
				Root: root, Path: filepath.Join(root, "skills", "verify-security", "SKILL.md"), Target: "claude",
			},
			want: []string{"claude", "skills/verify-security/SKILL.md"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDeprecatedFileForDisplay(tt.file)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("formatDeprecatedFileForDisplay() = %q, want %q", got, want)
				}
			}
			if strings.HasSuffix(got, filepath.Base(tt.file.Path)) {
				t.Errorf("display is ambiguous basename-only output: %q", got)
			}
		})
	}
}

func TestDeprecatedManifestContainsNewCommandEntries(t *testing.T) {
	tests := map[string]string{
		"opencode verify-skill command":    "commands/verify-skill.md",
		"opencode verify-security command": "commands/verify-security.md",
		"claude verify-skill skill":        "skills/verify-skill/SKILL.md",
		"claude verify-security skill":     "skills/verify-security/SKILL.md",
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			target := "opencode"
			if strings.HasPrefix(name, "claude") {
				target = "claude"
			}
			if !containsString(DeprecatedManifest[target], tt) {
				t.Errorf("DeprecatedManifest[%q] does not contain %q", target, tt)
			}
		})
	}
}

func TestDeprecatedManifestContainsGhostSkillDirectories(t *testing.T) {
	ghostSkills := []string{
		"skills/adversarial-review",
		"skills/verification-before-completion",
		"skills/nestjs-patterns",
		"skills/thermo-nuclear-code-quality-review",
		"skills/typescript-advanced-types",
	}

	for _, target := range []string{"opencode", "claude"} {
		t.Run(target, func(t *testing.T) {
			for _, skill := range ghostSkills {
				if !containsString(DeprecatedManifest[target], skill) {
					t.Errorf("DeprecatedManifest[%q] does not contain %q", target, skill)
				}
			}
		})
	}
}

func TestRemoveDeprecatedFilesRemovesSkillDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	skillDir := filepath.Join(root, "skills", "adversarial-review")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("deprecated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "extra.md"), []byte("deprecated"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: skillDir, Root: root, Target: "opencode"}})
	if err != nil {
		t.Fatalf("RemoveDeprecatedFiles failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Errorf("deprecated skill directory still exists, stat error: %v", err)
	}
}

func TestRemoveDeprecatedFilesCleansEmptyParentDirectory(t *testing.T) {
	tests := map[string]struct {
		keepSibling bool
		parentGone  bool
	}{
		"empty parent is removed":       {parentGone: true},
		"non-empty parent is preserved": {keepSibling: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := claudeDir()
			cleanupDir := filepath.Join(root, "skills", "onboard")
			deprecated := filepath.Join(cleanupDir, "SKILL.md")
			if err := os.MkdirAll(cleanupDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(deprecated, []byte("deprecated"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.keepSibling {
				if err := os.WriteFile(filepath.Join(cleanupDir, "keep.md"), []byte("keep"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: deprecated, Root: root, Target: "claude"}}); err != nil {
				t.Fatalf("RemoveDeprecatedFiles failed: %v", err)
			}
			_, err := os.Stat(cleanupDir)
			if tt.parentGone && !os.IsNotExist(err) {
				t.Errorf("empty parent directory still exists, stat error: %v", err)
			}
			if !tt.parentGone && err != nil {
				t.Errorf("parent directory should remain, stat error: %v", err)
			}
		})
	}
}

func TestRemoveDeprecatedFilesAbsentSafe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	missing := filepath.Join(root, "commands", "verify-security.md")

	removed, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: missing, Root: root, Target: "opencode"}})
	if err != nil {
		t.Fatalf("RemoveDeprecatedFiles should be absent-safe: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed for an absent path, got %d", removed)
	}
}

func TestRemoveDeprecatedFilesProtectsCustomSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	customSkill := filepath.Join(root, "skills", "my-custom-skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(customSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customSkill, []byte("user custom skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	deprecated := filepath.Join(root, "skills", "adversarial-review")
	if _, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: deprecated, Root: root, Target: "opencode"}}); err != nil {
		t.Fatalf("RemoveDeprecatedFiles failed: %v", err)
	}
	content, err := os.ReadFile(customSkill)
	if err != nil {
		t.Fatalf("custom skill was removed: %v", err)
	}
	if string(content) != "user custom skill" {
		t.Errorf("custom skill was modified: %q", content)
	}
}

func TestRemoveDeprecatedFilesProtectsCustomSkillInMixedDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	targetDir := opencodeDir()
	deprecatedDir := filepath.Join(targetDir, "skills", "adversarial-review")
	customSkill := filepath.Join(targetDir, "skills", "my-custom-skill", "SKILL.md")

	if err := os.MkdirAll(filepath.Join(deprecatedDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(customSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deprecatedDir, "SKILL.md"), []byte("deprecated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deprecatedDir, "nested", "extra.md"), []byte("deprecated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customSkill, []byte("user custom skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: deprecatedDir, Root: targetDir, Target: "opencode"}}); err != nil {
		t.Fatalf("RemoveDeprecatedFiles failed: %v", err)
	}
	if _, err := os.Stat(deprecatedDir); !os.IsNotExist(err) {
		t.Errorf("deprecated skill directory still exists, stat error: %v", err)
	}
	content, err := os.ReadFile(customSkill)
	if err != nil {
		t.Fatalf("custom skill was removed: %v", err)
	}
	if string(content) != "user custom skill" {
		t.Errorf("custom skill was modified: %q", content)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "skills")); err != nil {
		t.Fatalf("skills parent should remain for the custom skill: %v", err)
	}
}

func TestRemoveDeprecatedFilesPreservesManagedRootDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	targetDir := opencodeDir()
	commandsDir := filepath.Join(targetDir, "commands")
	deprecated := filepath.Join(commandsDir, "verify-skill.md")

	if err := os.MkdirAll(commandsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deprecated, []byte("deprecated"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := RemoveDeprecatedFiles([]DeprecatedFile{{Path: deprecated, Root: targetDir, Target: "opencode"}}); err != nil {
		t.Fatalf("RemoveDeprecatedFiles failed: %v", err)
	}
	if _, err := os.Stat(deprecated); !os.IsNotExist(err) {
		t.Errorf("deprecated file still exists, stat error: %v", err)
	}
	if _, err := os.Stat(commandsDir); err != nil {
		t.Fatalf("managed commands root should remain: %v", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestRemoveDeprecatedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()

	// Create test files
	file1 := filepath.Join(root, "commands", "onboard.md")
	file2 := filepath.Join(root, "tools", "advisor.ts")
	if err := os.MkdirAll(filepath.Dir(file1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(file2), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file1, []byte("test"), 0o644)
	os.WriteFile(file2, []byte("test"), 0o644)

	files := []DeprecatedFile{
		{Path: file1, Root: root, Target: "opencode"},
		{Path: file2, Root: root, Target: "opencode"},
	}

	removed, err := RemoveDeprecatedFiles(files)
	if err != nil {
		t.Fatalf("RemoveDeprecatedFiles failed: %v", err)
	}

	if removed != 2 {
		t.Fatalf("expected 2 removed, got %d", removed)
	}

	// Verify files are gone
	if _, err := os.Stat(file1); err == nil {
		t.Error("file1 still exists after removal")
	}
	if _, err := os.Stat(file2); err == nil {
		t.Error("file2 still exists after removal")
	}
}

func TestRemoveDeprecatedFilesNonexistent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	files := []DeprecatedFile{
		{Path: filepath.Join(root, "commands", "verify-security.md"), Root: root, Target: "opencode"},
	}

	removed, err := RemoveDeprecatedFiles(files)
	if err != nil {
		t.Fatalf("RemoveDeprecatedFiles should not error on nonexistent files: %v", err)
	}

	if removed != 0 {
		t.Fatalf("expected 0 removed for nonexistent file, got %d", removed)
	}
}

func TestRemoveDeprecatedFilesSecurity_RejectsOutsideRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	outside := t.TempDir()
	candidate := filepath.Join(outside, "deprecated.md")
	original := []byte("external content")
	if err := os.WriteFile(candidate, original, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := RemoveDeprecatedFiles([]DeprecatedFile{{
		Path:   candidate,
		Root:   root,
		Target: "opencode",
	}})
	if err == nil {
		t.Fatal("expected outside-root deprecated candidate to be rejected")
	}
	got, readErr := os.ReadFile(candidate)
	if readErr != nil {
		t.Fatalf("outside file was removed: %v", readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("outside file was modified: got %q, want %q", got, original)
	}
}

func TestRemoveDeprecatedFilesSecurity_RejectsParentTraversal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	candidate := filepath.Join(root, "commands", "verify-skill.md")
	original := []byte("authorized candidate must remain unchanged")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, original, 0o644); err != nil {
		t.Fatal(err)
	}
	rawPath := root + string(filepath.Separator) + "commands" + string(filepath.Separator) + ".." + string(filepath.Separator) + "commands" + string(filepath.Separator) + "verify-skill.md"

	removed, err := RemoveDeprecatedFiles([]DeprecatedFile{{
		Path:   rawPath,
		Root:   root,
		Target: "opencode",
	}})
	expectedErr := `reject deprecated path "` + rawPath + `": path is not clean`
	if err == nil || err.Error() != expectedErr {
		t.Fatalf("expected error %q, got %v", expectedErr, err)
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed, got %d", removed)
	}
	got, readErr := os.ReadFile(candidate)
	if readErr != nil {
		t.Fatalf("clean target candidate was removed: %v", readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("clean target candidate was modified: got %q, want %q", got, original)
	}
}

func TestRemoveDeprecatedFilesSecurity_RejectsSymlinkedAncestor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	external := t.TempDir()
	managedRoot := filepath.Join(root, "skills")
	if err := os.Symlink(external, managedRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	candidate := filepath.Join(managedRoot, "adversarial-review")
	if err := os.WriteFile(candidate, []byte("external content"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := RemoveDeprecatedFiles([]DeprecatedFile{{
		Path:   candidate,
		Root:   root,
		Target: "opencode",
	}})
	if err == nil {
		t.Fatal("expected candidate beneath a symlinked ancestor to be rejected")
	}
	if _, statErr := os.Stat(candidate); statErr != nil {
		t.Fatalf("external content was removed through symlinked ancestor: %v", statErr)
	}
}

func TestRemoveDeprecatedFilesRejectsMissingRoot(t *testing.T) {
	managedRoot := t.TempDir()
	external := t.TempDir()
	candidate := filepath.Join(external, "deprecated.md")
	original := []byte("must remain unchanged")
	if err := os.WriteFile(candidate, original, 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveDeprecatedFiles([]DeprecatedFile{{
		Path:   candidate,
		Target: "opencode",
	}})
	if err == nil {
		t.Fatal("expected missing cleanup root to be rejected")
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed, got %d", removed)
	}
	got, readErr := os.ReadFile(candidate)
	if readErr != nil {
		t.Fatalf("candidate file was removed: %v", readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("candidate file was modified: got %q, want %q", got, original)
	}
	if _, statErr := os.Stat(filepath.Join(managedRoot, "deprecated.md")); !os.IsNotExist(statErr) {
		t.Fatalf("unexpected output under managed root, stat error: %v", statErr)
	}
}

func TestRemoveDeprecatedFilesSecurity_RejectsNonCleanCandidatePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	candidate := filepath.Join(root, "commands", "onboard.md")
	sentinel := filepath.Join(root, "commands", "sentinel")
	const candidateContent = "candidate must remain unchanged"
	const sentinelContent = "sentinel must remain unchanged"

	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte(candidateContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte(sentinelContent), 0o644); err != nil {
		t.Fatal(err)
	}

	nonCleanPath := root + string(filepath.Separator) + "commands" + string(filepath.Separator) + ".." + string(filepath.Separator) + "commands" + string(filepath.Separator) + "onboard.md"
	removed, err := RemoveDeprecatedFiles([]DeprecatedFile{{
		Path:   nonCleanPath,
		Root:   root,
		Target: "opencode",
	}})
	if err == nil {
		t.Fatal("expected non-clean candidate path to be rejected")
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed, got %d", removed)
	}

	got, readErr := os.ReadFile(candidate)
	if readErr != nil {
		t.Fatalf("candidate was removed: %v", readErr)
	}
	if string(got) != candidateContent {
		t.Fatalf("candidate was modified: got %q, want %q", got, candidateContent)
	}
	got, readErr = os.ReadFile(sentinel)
	if readErr != nil {
		t.Fatalf("sentinel was removed: %v", readErr)
	}
	if string(got) != sentinelContent {
		t.Fatalf("sentinel was modified: got %q, want %q", got, sentinelContent)
	}
}

func TestValidateDeprecatedFileRejectsSymlinkedAncestorAboveRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	external := t.TempDir()
	externalRoot := filepath.Join(external, ".config", "opencode")
	candidate := filepath.Join(externalRoot, "skills", "adversarial-review")
	sentinel := filepath.Join(external, "sentinel.txt")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("external candidate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, ".config"), filepath.Join(home, ".config")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	root := filepath.Join(home, ".config", "opencode")
	_, err := RemoveDeprecatedFiles([]DeprecatedFile{{
		Path:   filepath.Join(root, "skills", "adversarial-review"),
		Root:   root,
		Target: "opencode",
	}})
	if err == nil {
		t.Fatal("expected cleanup to reject a symlinked ancestor above the cleanup root")
	}
	if got, readErr := os.ReadFile(candidate); readErr != nil || string(got) != "external candidate" {
		t.Fatalf("external candidate changed: content=%q err=%v", got, readErr)
	}
	if got, readErr := os.ReadFile(sentinel); readErr != nil || string(got) != "sentinel" {
		t.Fatalf("external sentinel changed: content=%q err=%v", got, readErr)
	}
}

func TestValidateInstallDestinationRejectsSymlinkedRoot(t *testing.T) {
	tmpDir := t.TempDir()
	external := filepath.Join(tmpDir, "external")
	destination := filepath.Join(tmpDir, "destination")
	if err := os.Mkdir(external, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, destination); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := validateInstallDestination(destination); err == nil {
		t.Fatal("expected symlinked install destination to be rejected")
	}
}

func TestValidateInstallDestinationRejectsSymlinkedAncestor(t *testing.T) {
	tmpDir := t.TempDir()
	home := filepath.Join(tmpDir, "home")
	externalConfig := filepath.Join(tmpDir, "external-config")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(externalConfig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalConfig, filepath.Join(home, ".config")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := validateInstallDestination(filepath.Join(home, ".config", "opencode")); err == nil {
		t.Fatal("expected symlinked install ancestor to be rejected")
	}
}

func TestValidateInstallDestinationRejectsSymlinkedAncestorWithMissingLeaf(t *testing.T) {
	home := t.TempDir()
	external := t.TempDir()
	config := filepath.Join(home, ".config")
	target := filepath.Join(config, "opencode")
	if err := os.Symlink(external, config); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := validateInstallDestination(target); err == nil {
		t.Fatal("expected symlinked ancestor with missing leaf to be rejected")
	}
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Fatalf("unexpected target output, stat error: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(external, "opencode")); !os.IsNotExist(statErr) {
		t.Fatalf("unexpected external output, stat error: %v", statErr)
	}
}

func TestValidateInstallDestinationRejectsNonCleanPath(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "target")
	if err := os.WriteFile(candidate, []byte("candidate must remain unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}

	rawPath := root + string(filepath.Separator) + "dir" + string(filepath.Separator) + ".." + string(filepath.Separator) + "target"
	if err := validateInstallDestination(rawPath); err == nil {
		t.Fatal("expected non-clean install destination to be rejected")
	}
}

func TestValidateInstallDestinationAllowsMissingDescendants(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config")
	if err := os.Mkdir(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := validateInstallDestination(filepath.Join(configDir, "opencode", "skills")); err != nil {
		t.Fatalf("missing non-symlink descendants should be accepted: %v", err)
	}
}

func TestFindDeprecatedFilesSurfacesScanErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".config"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := FindDeprecatedFiles()
	if err == nil {
		t.Fatal("expected FindDeprecatedFiles to surface a discovery error")
	}
	if !strings.Contains(err.Error(), "scan") && !strings.Contains(err.Error(), "lstat") {
		t.Fatalf("expected contextual scan error, got %v", err)
	}
}

func TestRemoveDeprecatedFilesSecurity_RemovesSafeInRootCandidate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	candidate := filepath.Join(root, "skills", "adversarial-review")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("deprecated"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := RemoveDeprecatedFiles([]DeprecatedFile{{
		Path:   candidate,
		Root:   root,
		Target: "opencode",
	}})
	if err != nil {
		t.Fatalf("safe in-root candidate should be removed: %v", err)
	}
	if _, statErr := os.Stat(candidate); !os.IsNotExist(statErr) {
		t.Fatalf("safe in-root candidate still exists, stat error: %v", statErr)
	}
}

func TestRemoveDeprecatedFilesRejectsUnauthorizedInRootPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	candidate := filepath.Join(root, "commands", "user-owned.md")
	const sentinel = "user-owned content"
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveDeprecatedFiles([]DeprecatedFile{{
		Path: candidate, Root: root, Target: "opencode",
	}})
	if err == nil {
		t.Fatal("expected unauthorized in-root candidate to be rejected")
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed, got %d", removed)
	}
	got, readErr := os.ReadFile(candidate)
	if readErr != nil {
		t.Fatalf("user-owned candidate was removed: %v", readErr)
	}
	if string(got) != sentinel {
		t.Fatalf("user-owned candidate was modified: got %q, want %q", got, sentinel)
	}
}

func TestRemoveDeprecatedFilesRejectsWrongCanonicalRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	canonical := opencodeDir()
	candidate := filepath.Join(canonical, "commands", "verify-skill.md")
	const sentinel = "deprecated command"
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	wrongRoot := filepath.Join(t.TempDir(), "opencode")
	if err := os.MkdirAll(wrongRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveDeprecatedFiles([]DeprecatedFile{{
		Path: candidate, Root: wrongRoot, Target: "opencode",
	}})
	if err == nil {
		t.Fatal("expected candidate with wrong canonical root to be rejected")
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed, got %d", removed)
	}
	got, readErr := os.ReadFile(candidate)
	if readErr != nil {
		t.Fatalf("manifest candidate was removed: %v", readErr)
	}
	if string(got) != sentinel {
		t.Fatalf("manifest candidate was modified: got %q, want %q", got, sentinel)
	}
}

func TestRemoveDeprecatedFilesAllowsAuthorizedManifestCandidate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	candidate := filepath.Join(root, "skills", "adversarial-review")
	if err := os.MkdirAll(candidate, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "SKILL.md"), []byte("deprecated skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveDeprecatedFiles([]DeprecatedFile{{
		Path: candidate, Root: root, Target: "opencode",
	}})
	if err != nil {
		t.Fatalf("authorized manifest candidate should be removed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("authorized manifest directory still exists, stat error: %v", err)
	}
}

func TestRemoveDeprecatedFilesRemovesFinalSymlinkWithoutFollowing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := opencodeDir()
	external := t.TempDir()
	managedRoot := filepath.Join(root, "skills")
	link := filepath.Join(managedRoot, "typescript-advanced-types")
	sentinel := filepath.Join(external, "sentinel.txt")
	const sentinelContent = "external content must remain unchanged"

	if err := os.MkdirAll(managedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte(sentinelContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	removed, err := RemoveDeprecatedFiles([]DeprecatedFile{{
		Path:   link,
		Root:   root,
		Target: "opencode",
	}})
	if err != nil {
		t.Fatalf("RemoveDeprecatedFiles failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("final symlink still exists, lstat error: %v", err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("external directory was removed: %v", err)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("external sentinel was removed: %v", err)
	}
	if string(got) != sentinelContent {
		t.Fatalf("external sentinel was modified: got %q, want %q", got, sentinelContent)
	}
	if _, err := os.Stat(managedRoot); err != nil {
		t.Fatalf("managed skills root should remain: %v", err)
	}
}

func TestCleanupConsentDecisionMatrix(t *testing.T) {
	tests := []struct {
		name          string
		interactive   bool
		cleanup       bool
		answer        string
		wantRemaining bool
	}{
		{name: "interactive affirmative", interactive: true, answer: "y\n"},
		{name: "interactive declined", interactive: true, answer: "n\n", wantRemaining: true},
		{name: "non-interactive without explicit flag", wantRemaining: true},
		{name: "non-interactive explicit flag", cleanup: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deprecated := installTestDeprecatedFile(t)
			if tt.interactive {
				setCleanupPromptInput(t, tt.answer)
			}
			err := InstallOpencode(installTestSource(t), &models.InstallRequest{
				Interactive:       tt.interactive,
				CleanupDeprecated: tt.cleanup,
			})
			if err != nil {
				t.Fatalf("InstallOpencode failed: %v", err)
			}
			_, statErr := os.Stat(deprecated)
			remaining := statErr == nil
			if remaining != tt.wantRemaining {
				t.Fatalf("deprecated path remaining = %v, want %v", remaining, tt.wantRemaining)
			}
		})
	}
}

func installTestSource(t *testing.T) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(source, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "opencode", "opencode.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	npm := filepath.Join(bin, "npm")
	if err := os.WriteFile(npm, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return source
}

func installTestDeprecatedFile(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "opencode", "commands", "onboard.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("deprecated"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func setCleanupPromptInput(t *testing.T, answer string) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := write.WriteString(answer); err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	original := os.Stdin
	os.Stdin = read
	t.Cleanup(func() {
		os.Stdin = original
		_ = read.Close()
	})
}
