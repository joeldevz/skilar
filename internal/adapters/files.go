package adapters

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/joeldevz/skynex/internal/safefs"
)

// Test-only race seam. The actual replacement remains a rooted atomic rename.
var beforeFileMutation func() error

// copyDir recursively copies src → dst, skipping nothing.
func copyDir(src, dst string) error {
	return copyDirExcluding(src, dst, nil)
}

// copyDirExcluding copies src → dst, skipping entries in exclude list.
func copyDirExcluding(src, dst string, exclude []string) error {
	sourceRoot, err := safefs.Open(src)
	if err != nil {
		return fmt.Errorf("open copy source: %w", err)
	}
	defer sourceRoot.Close()
	destRoot, err := safefs.OpenOrCreate(dst, 0o700)
	if err != nil {
		return fmt.Errorf("open copy destination: %w", err)
	}
	defer destRoot.Close()
	excludeSet := make(map[string]bool)
	for _, e := range exclude {
		excludeSet[e] = true
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Check exclusions
		name := d.Name()
		if excludeSet[name] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		// Skip hidden system dirs except .github
		if strings.HasPrefix(name, ".") && name != ".github" && d.IsDir() {
			return filepath.SkipDir
		}

		if d.IsDir() {
			return destRoot.MkdirAll(filepath.ToSlash(rel), 0o700)
		}
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return fmt.Errorf("unsupported source entry %q", path)
		}
		relPath := filepath.ToSlash(rel)
		before, err := sourceRoot.Lstat(relPath)
		if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
			return fmt.Errorf("source changed to non-regular entry %q", path)
		}
		if !safefs.SingleLink(before) {
			return fmt.Errorf("source is hard-linked: %s", path)
		}
		r, err := sourceRoot.Open(relPath)
		if err != nil {
			return err
		}
		opened, err := r.Stat()
		if err != nil || !os.SameFile(before, opened) || opened.Mode().Type() != before.Mode().Type() {
			_ = r.Close()
			return fmt.Errorf("source changed to non-regular entry %q", path)
		}
		after, err := sourceRoot.Lstat(relPath)
		if err != nil || !os.SameFile(before, after) || after.Mode().Type() != before.Mode().Type() {
			_ = r.Close()
			return fmt.Errorf("source changed while reading %q", path)
		}
		if info, err := destRoot.Lstat(relPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
			_ = r.Close()
			return fmt.Errorf("destination is a symlink: %s", rel)
		} else if err != nil && !os.IsNotExist(err) {
			_ = r.Close()
			return err
		}
		copyErr := safefs.CopyAtomic(destRoot, relPath, r, 0o600, ".skynex-copy-")
		closeErr := r.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		after, err = sourceRoot.Lstat(relPath)
		if err != nil || !os.SameFile(before, after) || after.Mode().Type() != before.Mode().Type() {
			return fmt.Errorf("source changed while copying %q", path)
		}
		return nil
	})
}

// copyFile copies a single file from src to dst.
// If dst already exists with restrictive permissions, chmod it first.
func copyFile(src, dst string) error {
	if err := validateInstallDestination(dst); err != nil {
		return fmt.Errorf("validate copy destination %q: %w", dst, err)
	}
	sourceRoot, err := safefs.Open(filepath.Dir(src))
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	rel := filepath.Base(src)
	info, err := sourceRoot.Lstat(rel)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file: %s", src)
	}
	if !safefs.SingleLink(info) {
		return fmt.Errorf("source is hard-linked: %s", src)
	}
	in, err := sourceRoot.Open(rel)
	if err != nil {
		return err
	}
	opened, err := in.Stat()
	if err != nil || !os.SameFile(info, opened) || !safefs.SingleLink(opened) {
		_ = in.Close()
		return fmt.Errorf("source changed while opening: %s", src)
	}
	destRoot, err := safefs.OpenOrCreate(filepath.Dir(dst), 0o700)
	if err != nil {
		_ = in.Close()
		return err
	}
	defer destRoot.Close()
	if info, err := destRoot.Lstat(filepath.Base(dst)); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			_ = in.Close()
			return fmt.Errorf("destination is a symlink: %s", dst)
		}
		if !safefs.SingleLink(info) {
			_ = in.Close()
			return fmt.Errorf("destination is hard-linked: %s", dst)
		}
	} else if !os.IsNotExist(err) {
		_ = in.Close()
		return err
	}
	if beforeFileMutation != nil {
		if err := beforeFileMutation(); err != nil {
			_ = in.Close()
			return err
		}
	}
	err = safefs.CopyAtomic(destRoot, filepath.Base(dst), in, 0o600, ".skynex-copy-")
	closeErr := in.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	after, err := sourceRoot.Lstat(rel)
	if err != nil || !os.SameFile(info, after) || !safefs.SingleLink(after) {
		return fmt.Errorf("source changed while copying: %s", src)
	}
	return nil
}

// writeFile writes content to path, creating parent dirs.
func writeFile(path, content string) error {
	if err := validateInstallDestination(path); err != nil {
		return fmt.Errorf("validate write destination %q: %w", path, err)
	}
	root, err := safefs.OpenOrCreate(filepath.Dir(path), 0o700)
	if err != nil {
		return err
	}
	defer root.Close()
	return safefs.WriteAtomic(root, filepath.Base(path), []byte(content), 0o600, ".skynex-write-")
}

func rejectUnsafeDestination(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("destination is a symlink: %s", path)
	}
	if !safefs.SingleLink(info) {
		return fmt.Errorf("destination is hard-linked: %s", path)
	}
	return nil
}
