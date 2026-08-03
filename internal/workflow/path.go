package workflow

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func CanonicalDatabasePath(repoDir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	common := string(out)
	for len(common) > 0 && (common[len(common)-1] == '\n' || common[len(common)-1] == '\r') {
		common = common[:len(common)-1]
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(repoDir, common)
	}
	common, err = filepath.Abs(common)
	if err != nil {
		return "", err
	}
	return filepath.Join(common, "skynex", "workflows.db"), nil
}

func prepareDatabasePath(path string) error {
	dir := filepath.Dir(path)
	if err := rejectSymlinkComponents(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || fileHasMultipleLinks(info) {
			return fmt.Errorf("unsafe workflow database %q", path)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("workflow database permissions are too broad: %o", info.Mode().Perm())
		}
	} else if os.IsNotExist(err) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			return createErr
		}
		if closeErr := file.Close(); closeErr != nil {
			return closeErr
		}
	} else {
		return err
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current := string(filepath.Separator)
	for _, part := range splitPath(abs) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("workflow path contains symlink %q", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("workflow path component is not a directory %q", current)
		}
	}
	return nil
}

func splitPath(path string) []string {
	var parts []string
	for {
		dir, base := filepath.Split(path)
		if base != "" {
			parts = append([]string{base}, parts...)
		}
		next := filepath.Clean(dir)
		if next == path || next == string(filepath.Separator) || next == "." {
			break
		}
		path = next
	}
	return parts
}
