package adapters

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/joeldevz/skynex/internal/safefs"
)

// DeprecatedFile is a single deprecated file to potentially remove.
type DeprecatedFile struct {
	Path   string // absolute path
	Root   string // absolute, trusted cleanup root
	Target string // "opencode" or "claude"
}

// DeprecatedManifest defines all deprecated skynex-managed files.
// Maps target → list of relative paths (relative to ~/.config/opencode or ~/.claude).
var DeprecatedManifest = map[string][]string{
	"opencode": {
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

// RemoveDeprecatedFiles explicitly retires safe regular files by renaming them
// beside the managed root. Directories, symlinks, and special files are never
// recursively deleted.
func RemoveDeprecatedFiles(files []DeprecatedFile) (int, error) {
	// Validate every candidate before mutating anything. This is deliberately a
	// separate pass so a later unsafe path cannot leave an earlier path removed.
	for _, f := range files {
		if err := validateDeprecatedFile(f); err != nil {
			return 0, err
		}
	}

	count := 0
	roots := make(map[string]*safefs.Root)
	defer func() {
		for _, root := range roots {
			_ = root.Close()
		}
	}()
	for _, f := range files {
		if err := validateDeprecatedFile(f); err != nil {
			return count, fmt.Errorf("revalidate deprecated path %q: %w", f.Path, err)
		}
		if _, err := os.Lstat(f.Root); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return count, fmt.Errorf("stat cleanup root %s: %w", f.Root, err)
		}
		root := roots[f.Root]
		if root == nil {
			var openErr error
			root, openErr = safefs.Open(f.Root)
			if openErr != nil {
				return count, fmt.Errorf("open cleanup root %s: %w", f.Root, openErr)
			}
			roots[f.Root] = root
		}
		rel, _ := filepath.Rel(f.Root, f.Path)
		rel = filepath.ToSlash(rel)
		info, err := root.Lstat(rel)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return count, fmt.Errorf("stat %s: %w", f.Path, err)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			if err := root.Remove(rel); err != nil {
				return count, fmt.Errorf("remove deprecated symlink %s: %w", f.Path, err)
			}
			count++
			continue
		}
		if !info.Mode().IsRegular() || !safefs.SingleLink(info) {
			return count, fmt.Errorf("refusing deprecated non-single regular file %s; preserved", f.Path)
		}
		backup := filepath.ToSlash(filepath.Join(filepath.Dir(rel), fmt.Sprintf(".skynex-deprecated-backup-%d", time.Now().UnixNano())))
		if err := root.Rename(rel, backup); err != nil {
			return count, fmt.Errorf("backup deprecated file %s: %w", f.Path, err)
		}
		count++
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
// filesystem anchor through path. Missing components are skipped, not treated
// as the end of the walk: a later existing component must still be checked.
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
			continue
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
	_, err := validateInstallDestinationTreeIdentity(root)
	return err
}

func validateInstallDestinationTreeIdentity(root string) (os.FileInfo, error) {
	if err := validateInstallDestination(root); err != nil {
		return nil, err
	}
	info, err := os.Lstat(filepath.Clean(root))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lstat install destination tree root %s: %w", root, err)
	}
	if !info.IsDir() {
		return info, nil
	}
	if err := validateExistingNodeModules(root); err != nil {
		return nil, err
	}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return walkErr
			}
			return fmt.Errorf("walk install destination tree %s: %w", path, walkErr)
		}
		rel, relErr := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
		if relErr == nil && (rel == "node_modules" || strings.HasPrefix(rel, "node_modules"+string(filepath.Separator))) {
			if rel == "node_modules" {
				return filepath.SkipDir
			}
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if isTopLevelNodeModulesBinLink(root, path) {
				return validateNpmBinLink(root, path)
			}
			return fmt.Errorf("install destination tree contains symlink %s", path)
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("install destination tree contains special entry %s", path)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return info, nil
}

// validateExistingNodeModules walks the dependency tree through a retained
// descriptor. The only symlinks npm-style installs legitimately create are
// direct children of node_modules/.bin; every other entry must be a real
// directory or a single-link regular file.
func validateExistingNodeModules(root string) error {
	nodeModules := filepath.Join(root, "node_modules")
	info, err := os.Lstat(nodeModules)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lstat install destination node_modules %s: %w", nodeModules, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("install destination node_modules must be a real directory: %s", nodeModules)
	}
	rootHandle, err := safefs.Open(root)
	if err != nil {
		return fmt.Errorf("open install destination root: %w", err)
	}
	defer rootHandle.Close()
	nodeRoot, err := rootHandle.OpenRoot("node_modules")
	if err != nil {
		return fmt.Errorf("open install destination node_modules: %w", err)
	}
	defer nodeRoot.Close()
	return fs.WalkDir(nodeRoot.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk install destination node_modules %s: %w", path, walkErr)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if !isNodeModulesBinChild(path) {
				return fmt.Errorf("install destination node_modules contains symlink %s", filepath.Join(nodeModules, filepath.FromSlash(path)))
			}
			return validateNpmBinLinkRoot(nodeRoot, path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("install destination node_modules contains special entry %s", filepath.Join(nodeModules, filepath.FromSlash(path)))
		}
		info, err := nodeRoot.Lstat(path)
		if err != nil {
			return err
		}
		if !safefs.SingleLink(info) {
			return fmt.Errorf("install destination node_modules contains hard-linked file %s", filepath.Join(nodeModules, filepath.FromSlash(path)))
		}
		return nil
	})
}

func isNodeModulesBinChild(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	return len(parts) == 2 && parts[0] == ".bin"
}

func validateNpmBinLinkRoot(nodeRoot *safefs.Root, path string) error {
	target, err := nodeRoot.Readlink(path)
	if err != nil {
		return fmt.Errorf("read npm .bin link %s: %w", path, err)
	}
	if target == "" || filepath.IsAbs(target) || strings.Contains(target, "\\") || filepath.Clean(target) != target {
		return fmt.Errorf("npm .bin link has non-relative clean target %s: %s", path, target)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
	rel, err := safefs.Relative(filepath.ToSlash(resolved))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("npm .bin link escapes node_modules: %s", path)
	}
	if strings.HasPrefix(rel, ".bin/") {
		return fmt.Errorf("npm .bin link resolves into .bin: %s", path)
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		_ = part
		component := strings.Join(parts[:i+1], "/")
		info, err := nodeRoot.Lstat(component)
		if err != nil {
			return fmt.Errorf("npm .bin link target is not an existing file: %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("npm .bin link target contains symlink: %s", component)
		}
		if i < len(parts)-1 {
			if !info.IsDir() {
				return fmt.Errorf("npm .bin link target ancestor is not a directory: %s", component)
			}
		} else if !info.Mode().IsRegular() || !safefs.SingleLink(info) {
			return fmt.Errorf("npm .bin link target is not a single regular file: %s", rel)
		}
	}
	return nil
}

// validateNpmBinLink validates the target without ever resolving a symlink.
// npm links are the sole permitted symlinks in an install tree, and their
// target must be an existing, unlinked regular file in the same node_modules.
func validateNpmBinLink(root, path string) error {
	target, err := os.Readlink(path)
	if err != nil {
		return fmt.Errorf("read npm .bin link %s: %w", path, err)
	}
	if filepath.IsAbs(target) || filepath.Clean(target) != target {
		return fmt.Errorf("npm .bin link has non-relative clean target %s: %s", path, target)
	}

	nodeModules := filepath.Join(filepath.Clean(root), "node_modules")
	resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
	rel, err := filepath.Rel(nodeModules, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("npm .bin link escapes node_modules: %s", path)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > 0 && parts[0] == ".bin" {
		return fmt.Errorf("npm .bin link resolves into .bin: %s", path)
	}

	components := []string{}
	current := resolved
	for {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current || parent == nodeModules {
			break
		}
		current = parent
	}
	for i := len(components) - 1; i >= 0; i-- {
		info, err := os.Lstat(components[i])
		if err != nil {
			return fmt.Errorf("npm .bin link target is not an existing file: %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("npm .bin link target contains symlink: %s", components[i])
		}
		if i == 0 && (!info.Mode().IsRegular() || !safefs.SingleLink(info)) {
			return fmt.Errorf("npm .bin link target is not a single regular file: %s", resolved)
		}
	}
	return nil
}

// isTopLevelNodeModulesBinLink permits direct executable links created by
// npm/bun. WalkDir does not follow the link, so its target is never used for
// validation or subsequent writes.
func isTopLevelNodeModulesBinLink(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 3 && parts[0] == "node_modules" && parts[1] == ".bin"
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
	notifyDeprecatedFiles(discardReporter(), target, files)
}

func notifyDeprecatedFiles(reporter Reporter, target string, files []DeprecatedFile) {
	if len(files) == 0 {
		return
	}

	reporter.Detail("Removing deprecated skynex-managed files:")
	for _, f := range files {
		displayFile := f
		if displayFile.Target == "" {
			displayFile.Target = target
		}
		reporter.Detail("  • %s", formatDeprecatedFileForDisplay(displayFile))
	}
}

// PromptCleanupDeprecated asks the user interactively if they want to remove deprecated files.
// Returns true if user confirms, false otherwise.
func PromptCleanupDeprecated(grouped map[string][]DeprecatedFile, reporters ...Reporter) bool {
	return PromptCleanupDeprecatedWithIO(grouped, os.Stdin, io.Discard, reporters...)
}

func PromptCleanupDeprecatedWithIO(grouped map[string][]DeprecatedFile, input io.Reader, output io.Writer, reporters ...Reporter) bool {
	if len(grouped) == 0 {
		return false
	}
	var reporter Reporter = discardReporter()
	if len(reporters) > 0 && reporters[0] != nil {
		reporter = reporters[0]
	}
	reporter.Detail("Deprecated skynex-managed files detected:")
	for target, files := range grouped {
		reporter.Detail("[%s]", target)
		for _, f := range files {
			displayFile := f
			if displayFile.Target == "" {
				displayFile.Target = target
			}
			reporter.Detail("  • %s", formatDeprecatedFileForDisplay(displayFile))
		}
	}
	confirmed := false
	if input == nil || output == nil {
		return false
	}
	form := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Remove deprecated skynex-managed files?").Affirmative("Remove").Negative("Keep").Value(&confirmed))).WithAccessible(true).WithInput(input).WithOutput(output)
	if err := form.Run(); err != nil {
		return false
	}
	return confirmed
}
