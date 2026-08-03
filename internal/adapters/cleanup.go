package adapters

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DeprecatedFile is a single deprecated file to potentially remove.
type DeprecatedFile struct {
	Path           string // absolute path
	Root           string // absolute, trusted cleanup root
	Target         string // "opencode" or "claude"
	ExpectedDigest string // when set, only this known managed content may be removed
}

// DeprecatedManifest defines all deprecated skynex-managed files.
// Maps target → list of relative paths (relative to ~/.config/opencode or ~/.claude).
var DeprecatedManifest = map[string][]string{
	"opencode": {
		"agents/manager.md",
		"agents/linear-orchestrator.md",
		"commands/linear.md",
		"commands/onboard.md",
		"tools/advisor.ts",
		"commands/verify-skill.md",
		"commands/verify-security.md",
		"skills/adversarial-review",
		"skills/verification-before-completion",
		"skills/nestjs-patterns",
		"skills/thermo-nuclear-code-quality-review",
		"skills/typescript-advanced-types",
	},
	"claude": {
		"skills/onboard/SKILL.md",
		"agents/product-planner.md",
		"skills/verify-skill/SKILL.md",
		"skills/verify-security/SKILL.md",
		"skills/adversarial-review",
		"skills/verification-before-completion",
		"skills/nestjs-patterns",
		"skills/thermo-nuclear-code-quality-review",
		"skills/typescript-advanced-types",
		"skills/plan",
		"skills/execute",
		"skills/test",
		"skills/review",
		"skills/status",
		"skills/apply-feedback",
		"skills/context",
		"skills/diff",
		"skills/estimate",
		"skills/plan-rewrite",
	},
}

var legacyExactDigests = map[string]string{"agents/manager.md": "af28404635ef21a56134be959008aa302dbe48296e64c8700d25dc737d3b1893", "agents/linear-orchestrator.md": "63cd6ec3272ae96c28b06ce798b1139cf10142974dbf075350957e89ecb406ca", "commands/linear.md": "149dee85fe4a81963b3a5572e375fefa4fae54e9a09385a986390a2d4b9122f5"}

// FindDeprecatedFiles scans target directories for deprecated files.
// Returns a grouped map: target → []DeprecatedFile (only existing files).
func FindDeprecatedFiles() (map[string][]DeprecatedFile, error) {
	result := make(map[string][]DeprecatedFile)

	for target, paths := range DeprecatedManifest {
		var existing []DeprecatedFile
		var baseDir string

		switch target {
		case "opencode":
			baseDir = opencodeDir()
		case "claude":
			baseDir = claudeDir()
		default:
			continue
		}

		for _, relPath := range paths {
			absPath := filepath.Join(baseDir, relPath)
			if _, err := os.Lstat(absPath); err == nil {
				existing = append(existing, DeprecatedFile{
					Path:   absPath,
					Root:   baseDir,
					Target: target,
					ExpectedDigest: func() string {
						if target == "opencode" {
							return legacyExactDigests[relPath]
						}
						return ""
					}(),
				})
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("scan deprecated %s path %s: %w", target, absPath, err)
			}
		}

		if len(existing) > 0 {
			result[target] = existing
		}
	}

	return result, nil
}

// RemoveDeprecatedFiles removes the given deprecated files.
// Returns count removed and any errors.
func RemoveDeprecatedFiles(files []DeprecatedFile) (int, error) {
	// Validate every candidate before mutating anything. This is deliberately a
	// separate pass so a later unsafe path cannot leave an earlier path removed.
	for _, f := range files {
		if err := validateDeprecatedFile(f); err != nil {
			return 0, err
		}
	}
	filtered := files[:0]
	for _, f := range files {
		if f.ExpectedDigest != "" {
			raw, err := os.ReadFile(f.Path)
			if err != nil {
				return 0, err
			}
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) != f.ExpectedDigest {
				fmt.Printf("    Preserving modified deprecated file: %s\n", f.Path)
				continue
			}
		}
		filtered = append(filtered, f)
	}
	files = filtered

	count := 0
	for _, f := range files {
		if err := validateDeprecatedFile(f); err != nil {
			return count, fmt.Errorf("revalidate deprecated path %q: %w", f.Path, err)
		}
		// This Lstat is intentionally immediately before the destructive call.
		// Portable pathname APIs cannot eliminate every concurrent mutation
		// between these operations; they only ensure we never intentionally
		// follow the final candidate as part of cleanup.
		info, err := os.Lstat(f.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return count, fmt.Errorf("stat %s: %w", f.Path, err)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			// Remove the manifest-approved link itself, never its target.
			err = os.Remove(f.Path)
		} else if info.IsDir() {
			err = os.RemoveAll(f.Path)
		} else {
			err = os.Remove(f.Path)
		}
		if err != nil {
			return count, fmt.Errorf("remove %s: %w", f.Path, err)
		}
		count++

		if !info.IsDir() {
			parent := filepath.Dir(f.Path)
			entries, readErr := os.ReadDir(parent)
			if readErr == nil && len(entries) == 0 && !isManagedRoot(parent, f.Target) {
				if err := os.Remove(parent); err != nil && !os.IsNotExist(err) {
					return count, fmt.Errorf("remove empty parent %s: %w", parent, err)
				}
			}
		}
	}
	return count, nil
}

func validateDeprecatedFile(f DeprecatedFile) error {
	var canonicalRoot string
	switch f.Target {
	case "opencode":
		canonicalRoot = opencodeDir()
	case "claude":
		canonicalRoot = claudeDir()
	default:
		return fmt.Errorf("reject deprecated path %q: unknown cleanup target %q", f.Path, f.Target)
	}
	manifestPaths, ok := DeprecatedManifest[f.Target]
	if !ok {
		return fmt.Errorf("reject deprecated path %q: target %q is not in the deprecated manifest", f.Path, f.Target)
	}
	if f.Root == "" {
		return fmt.Errorf("reject deprecated path %q: cleanup root is required", f.Path)
	}
	if f.Root != canonicalRoot {
		return fmt.Errorf("reject deprecated path %q: cleanup root %q is not canonical root %q", f.Path, f.Root, canonicalRoot)
	}
	if filepath.Clean(f.Path) != f.Path {
		return fmt.Errorf("reject deprecated path %q: path is not clean", f.Path)
	}
	if filepath.Clean(f.Root) != f.Root {
		return fmt.Errorf("reject deprecated path %q: cleanup root is not clean", f.Path)
	}

	path := f.Path
	if !filepath.IsAbs(path) {
		return fmt.Errorf("reject deprecated path %q: path is not absolute", f.Path)
	}

	root := canonicalRoot
	if !filepath.IsAbs(root) {
		return fmt.Errorf("reject deprecated path %q: root is not absolute", f.Path)
	}
	if err := validateInstallDestination(root); err != nil {
		return fmt.Errorf("validate cleanup root %q: %w", root, err)
	}

	rel, err := filepath.Rel(canonicalRoot, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("reject deprecated path %q: outside cleanup root %q", f.Path, root)
	}
	rel = filepath.ToSlash(rel)
	manifestMatch := false
	for _, manifestPath := range manifestPaths {
		if rel == manifestPath {
			manifestMatch = true
			break
		}
	}
	if !manifestMatch {
		return fmt.Errorf("reject deprecated path %q: path %q is not authorized by the deprecated manifest", f.Path, rel)
	}

	// The manifest explicitly consents to this exact final component, so a
	// symlink there is removable. Its root, ancestors, and intermediate
	// components remain subject to the normal symlink-free destination check.
	parent := filepath.Dir(path)
	if err := validateInstallDestination(parent); err != nil {
		return fmt.Errorf("validate cleanup path %q: %w", f.Path, err)
	}
	return nil
}

// validateInstallDestination checks every existing component from the
// filesystem anchor through path. It stops at the first missing component so
// first installs may create missing descendants, but rejects symlinks anywhere
// in the existing prefix.
func validateInstallDestination(path string) error {
	cleaned := filepath.Clean(path)
	if cleaned != path {
		return fmt.Errorf("install destination %q is not a clean path", path)
	}
	if !filepath.IsAbs(cleaned) {
		return fmt.Errorf("install destination %q is not an absolute cleaned path", path)
	}

	components := []string{}
	current := cleaned
	for {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}

	for i := len(components) - 1; i >= 0; i-- {
		component := components[i]
		info, err := os.Lstat(component)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lstat install destination component %s: %w", component, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("install destination contains symlink component %s", component)
		}
	}
	return nil
}

// validateInstallDestinationTree validates the existing destination tree
// without following any symlink entries below its root.
func validateInstallDestinationTree(root string) error {
	if err := validateInstallDestination(root); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Clean(root))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lstat install destination tree root %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil
	}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return walkErr
			}
			return fmt.Errorf("walk install destination tree %s: %w", path, walkErr)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("install destination tree contains symlink %s", path)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func isManagedRoot(path, target string) bool {
	managedRoots := map[string]map[string]struct{}{
		"opencode": {
			"commands": {},
			"skills":   {},
			"tools":    {},
		},
		"claude": {
			"agents":   {},
			"skills":   {},
			"commands": {},
		},
	}
	_, ok := managedRoots[target][filepath.Base(path)]
	return ok
}

// formatDeprecatedFileForDisplay returns an unambiguous target-relative path.
// Invalid roots and outside paths are rendered explicitly rather than reduced
// to a basename that could refer to a different managed target.
func formatDeprecatedFileForDisplay(file DeprecatedFile) string {
	if file.Root == "" {
		return fmt.Sprintf("[%s] <invalid path: %s>", file.Target, normalizeDisplayPath(file.Path))
	}
	rel, err := filepath.Rel(file.Root, file.Path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Sprintf("[%s] <invalid path: %s>", file.Target, normalizeDisplayPath(file.Path))
	}
	return fmt.Sprintf("[%s] %s (deprecated)", file.Target, normalizeDisplayPath(rel))
}

func normalizeDisplayPath(path string) string {
	return strings.NewReplacer("\\", "/", string(filepath.Separator), "/").Replace(path)
}

// NotifyDeprecatedFiles lists the managed paths that will be removed.
func NotifyDeprecatedFiles(target string, files []DeprecatedFile) {
	if len(files) == 0 {
		return
	}

	fmt.Println("    Removing deprecated skynex-managed files:")
	for _, f := range files {
		displayFile := f
		if displayFile.Target == "" {
			displayFile.Target = target
		}
		fmt.Printf("      • %s\n", formatDeprecatedFileForDisplay(displayFile))
	}
}

// PromptCleanupDeprecated asks the user interactively if they want to remove deprecated files.
// Returns true if user confirms, false otherwise.
func PromptCleanupDeprecated(grouped map[string][]DeprecatedFile) bool {
	if len(grouped) == 0 {
		return false
	}

	fmt.Println("\n  Deprecated skynex-managed files detected:")
	for target, files := range grouped {
		fmt.Printf("\n    [%s]\n", target)
		for _, f := range files {
			displayFile := f
			if displayFile.Target == "" {
				displayFile.Target = target
			}
			fmt.Printf("      • %s\n", formatDeprecatedFileForDisplay(displayFile))
		}
	}

	fmt.Print("\n  Remove these deprecated files? [y/N] ")
	var input string
	fmt.Scanln(&input)
	return strings.ToLower(strings.TrimSpace(input)) == "y"
}
