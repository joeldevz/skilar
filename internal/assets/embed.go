package assets

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// dataFS contains the embedded assets (populated by go run ./cmd/tools/sync-assets/ or CI).
// When the directory is empty or only contains .gitkeep, this will be an empty FS.
//
//go:embed data
var dataFS embed.FS

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
