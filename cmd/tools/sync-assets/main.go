package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/joeldevz/skynex/internal/assets"
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

	sources := []string{"opencode", "claude-code", "skills"}
	skip := []string{"node_modules", ".git", "__pycache__", ".ruff_cache"}

	// Preserve the tracked bootstrap placeholder needed to compile this tool
	// before generated assets exist in a clean checkout.
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading asset directory: %v\n", err)
		os.Exit(1)
	}
	for _, entry := range entries {
		if entry.Name() == "README.md" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dataDir, entry.Name())); err != nil {
			fmt.Fprintf(os.Stderr, "Error cleaning assets: %v\n", err)
			os.Exit(1)
		}
	}

	for _, src := range sources {
		srcPath := filepath.Join(root, src)
		dstPath := filepath.Join(dataDir, src)

		if _, err := os.Stat(srcPath); os.IsNotExist(err) {
			fmt.Printf("  skip %s (not found)\n", src)
			continue
		}

		fmt.Printf("  copying %s -> internal/assets/data/%s\n", src, src)
		if err := copyDir(srcPath, dstPath, skip); err != nil {
			fmt.Fprintf(os.Stderr, "Error copying %s: %v\n", src, err)
			os.Exit(1)
		}
	}

	fmt.Println("Assets synced to internal/assets/data/")
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

func copyDir(src, dst string, skip []string) error {
	skyFiles, err := assets.SkyAgentsShippingFiles(os.DirFS(src))
	if err != nil {
		return err
	}
	skipSet := make(map[string]bool)
	for _, s := range skip {
		skipSet[s] = true
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipSet[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, _ := filepath.Rel(src, path)
		if !assets.IncludeSkyAgentsPath(filepath.ToSlash(rel), d.IsDir(), skyFiles) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dstPath := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(dstPath, 0o755)
		}
		return copyFile(path, dstPath)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, _ := in.Stat()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
