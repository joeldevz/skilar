package assets

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// dataFS contains the embedded assets (populated by go run ./cmd/tools/sync-assets/ or CI).
// When the directory is empty or only contains .gitkeep, this will be an empty FS.
//
//go:embed data
var dataFS embed.FS

const SkyAgentsDirectory = "v2/sky-agents"

// SkyAgentsShippingFiles is shared by checkout installs and embedded-asset sync.
// Only explicit production files are shipped; development dependencies and tests
// must not leak out of a developer checkout into an installation.
func SkyAgentsShippingFiles(source fs.FS) (map[string]bool, error) {
	manifest := SkyAgentsDirectory + "/shipping.json"
	raw, err := fs.ReadFile(source, manifest)
	if err != nil {
		if os.IsNotExist(err) {
			if _, dirErr := fs.Stat(source, SkyAgentsDirectory); os.IsNotExist(dirErr) {
				return nil, nil // Older bundles and non-OpenCode trees.
			}
		}
		return nil, fmt.Errorf("read Sky Agents shipping manifest: %w", err)
	}
	var value struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("invalid Sky Agents shipping manifest: %w", err)
	}
	files := map[string]bool{manifest: true}
	for _, path := range value.Files {
		if !fs.ValidPath(path) || strings.Contains(path, "\\") ||
			(path != "package.json" && path != "README.md" &&
				!(strings.HasPrefix(path, "src/") && (strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx")))) ||
			strings.Contains(path, ".test.") || strings.Contains(path, ".spec.") {
			return nil, fmt.Errorf("invalid Sky Agents shipping path %q", path)
		}
		full := SkyAgentsDirectory + "/" + path
		if files[full] {
			return nil, fmt.Errorf("duplicate Sky Agents shipping path %q", path)
		}
		info, err := fs.Stat(source, full)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Sky Agents shipping file is missing or not regular: %s", full)
		}
		files[full] = true
	}
	for _, required := range []string{"package.json", "src/index.ts", "src/tui.tsx"} {
		if !files[SkyAgentsDirectory+"/"+required] {
			return nil, fmt.Errorf("Sky Agents shipping manifest is missing %s", required)
		}
	}
	return files, nil
}

// IncludeSkyAgentsPath filters only the Sky Agents package, leaving other assets
// unchanged. Paths use forward slashes, like io/fs.
func IncludeSkyAgentsPath(path string, directory bool, files map[string]bool) bool {
	if path == SkyAgentsDirectory || !strings.HasPrefix(path, SkyAgentsDirectory+"/") {
		return true
	}
	if directory {
		for file := range files {
			if strings.HasPrefix(file, path+"/") {
				return true
			}
		}
		return false
	}
	return files[path]
}

// Available reports whether embedded assets are present (data/ was populated with real content).
func Available() bool {
	entries, err := dataFS.ReadDir("data")
	if err != nil {
		return false
	}
	// Only count if we have more than just placeholder files (README.md, .gitkeep)
	for _, e := range entries {
		name := e.Name()
		if name != "README.md" && name != ".gitkeep" {
			return true
		}
	}
	return false
}

// OpencodeFS returns a sub-filesystem rooted at the embedded opencode/ directory.
func OpencodeFS() (fs.FS, error) {
	return fs.Sub(dataFS, "data/opencode")
}

// ClaudeCodeFS returns a sub-filesystem rooted at the embedded claude-code/ directory.
func ClaudeCodeFS() (fs.FS, error) {
	return fs.Sub(dataFS, "data/claude-code")
}

// SkillsFS returns a sub-filesystem rooted at the embedded skills/ directory.
func SkillsFS() (fs.FS, error) {
	return fs.Sub(dataFS, "data/skills")
}

// OpencodeSkillsFS is the exact skills bundle shipped with the OpenCode
// adapter. Keeping this separate prevents a stale/generated generic tree from
// becoming the ownership source.
func OpencodeSkillsFS() (fs.FS, error) {
	return fs.Sub(dataFS, "data/opencode/skills")
}

// ExtractTo extracts a sub-FS to a destination directory on disk.
// Used to materialize embedded assets before running install logic.
func ExtractTo(sub fs.FS, destDir string) error {
	return fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(destDir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(dest, 0o700)
		}

		data, err := fs.ReadFile(sub, path)
		if err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		mode := fs.FileMode(0o600)
		if runtime.GOOS != "windows" {
			info, _ := d.Info()
			if info != nil {
				mode |= info.Mode().Perm() & 0o111
			}
		}
		return os.WriteFile(dest, data, mode)
	})
}
