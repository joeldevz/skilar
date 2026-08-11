package sandbox

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/joeldevz/skynex/internal/safefs"
)

func validateAbsoluteDir(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("directory must be an absolute clean path: %q", path)
	}
	root, err := safefs.Open(path)
	if err != nil {
		return fmt.Errorf("open directory %q: %w", path, err)
	}
	return root.Close()
}

func relativeDir(name string) (string, error) {
	if name == "" || name == "." {
		return ".", nil
	}
	return safeRelative(name)
}

func safeRelative(name string) (string, error) {
	clean, err := safefs.Relative(name)
	if err != nil {
		return "", err
	}
	// Path strings are not URL-decoded by the filesystem. Still reject encoded
	// traversal/separators so a later adapter cannot accidentally turn a safe
	// literal into an escape by decoding it.
	decoded, err := url.PathUnescape(name)
	if err != nil {
		return "", fmt.Errorf("invalid path escape in %q: %w", name, err)
	}
	if decoded != name {
		decoded = strings.ReplaceAll(decoded, "\\", "/")
		if decoded == "." || decoded == ".." || strings.HasPrefix(decoded, "/") ||
			strings.Contains(decoded, "../") || strings.Contains(decoded, "/../") ||
			strings.Contains(decoded, "//") {
			return "", fmt.Errorf("encoded traversal is not allowed: %q", name)
		}
		for _, r := range decoded {
			if r == '/' || r == '\\' {
				return "", fmt.Errorf("encoded path separator is not allowed: %q", name)
			}
		}
	}
	return clean, nil
}

func ensureRealDirectory(root *os.Root, name string) error {
	name, err := relativeDir(name)
	if err != nil {
		return err
	}
	if name == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if current == "" {
			current = part
		} else {
			current += "/" + part
		}
		info, statErr := root.Lstat(current)
		if statErr != nil {
			return fmt.Errorf("inspect working directory %q: %w", name, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("working directory component is not a real directory: %q", current)
		}
	}
	return nil
}
