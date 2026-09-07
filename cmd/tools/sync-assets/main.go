package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/safefs"
)

// sync-assets copies opencode/, claude-code/ and skills/ into internal/assets/data/
// so that go:embed can include them in the binary.
// Run: go run ./cmd/tools/sync-assets/
func main() {
	skyOnly := flag.Bool("sky-agents", false, "refresh only Sky Agents and OpenCode dependency metadata")
	flag.Parse()
	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	dataDir := filepath.Join(root, "internal", "assets", "data")
	if *skyOnly {
		if err := syncSkyAgents(filepath.Join(root, "opencode"), filepath.Join(dataDir, "opencode")); err != nil {
			fmt.Fprintf(os.Stderr, "Error syncing Sky Agents: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Sky Agents and OpenCode dependency assets synced")
		return
	}

	if err := syncAllAssets(root); err != nil {
		fmt.Fprintf(os.Stderr, "Error syncing assets: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Assets synced to internal/assets/data/")
}

func syncAllAssets(root string) error {
	// Acquire and verify all destination ancestors before any cleanup. Keep
	// this handle for enumeration, removal and copying, even if names change.
	dest, err := openSyncTree(filepath.Join(root, "internal", "assets", "data"))
	if err != nil {
		return err
	}
	defer dest.close()
	dataRoot := dest.dirs["."]
	sources := []string{"opencode", "claude-code", "skills"}
	skip := []string{"node_modules", ".git", "__pycache__", ".ruff_cache"}

	// Preserve the tracked bootstrap placeholder needed to compile this tool
	// before generated assets exist in a clean checkout.
	dir, err := dataRoot.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	// Validate the preserved leaf too; never retain a linked placeholder.
	if _, err := safefs.ReadFileVerified(dataRoot, "README.md", maxSyncFileBytes); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "README.md" {
			continue
		}
		if err := dataRoot.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}

	for _, src := range sources {
		srcPath := filepath.Join(root, src)

		if _, err := os.Stat(srcPath); os.IsNotExist(err) {
			fmt.Printf("  skip %s (not found)\n", src)
			continue
		}

		fmt.Printf("  copying %s -> internal/assets/data/%s\n", src, src)
		if err := copyDir(srcPath, dest, src, skip); err != nil {
			return fmt.Errorf("copy %s: %w", src, err)
		}
	}

	return nil
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}

func copyDir(src string, dst *syncTree, prefix string, skip []string) error {
	skyFiles, err := assets.SkyAgentsShippingFiles(os.DirFS(src))
	if err != nil {
		return err
	}
	skipSet := make(map[string]bool)
	for _, s := range skip {
		skipSet[s] = true
	}

	return filepath.WalkDir(src, func(sourcePath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipSet[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, _ := filepath.Rel(src, sourcePath)
		if !assets.IncludeSkyAgentsPath(filepath.ToSlash(rel), d.IsDir(), skyFiles) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dstPath := path.Join(prefix, filepath.ToSlash(rel))

		if d.IsDir() {
			_, err := dst.directory(dstPath, true)
			return err
		}
		return copyFile(sourcePath, dst, dstPath)
	})
}

func copyFile(src string, dst *syncTree, name string) error {
	parent, err := dst.directory(path.Dir(name), true)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	return safefs.CopyAtomic(parent, path.Base(name), in, info.Mode(), ".asset-sync-")
}
