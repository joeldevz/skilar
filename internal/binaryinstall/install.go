package binaryinstall

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/joeldevz/skynex/internal/safefs"
)

// Install copies a verified executable into destination through retained
// directory roots. Both existing source and destination entries must be
// regular, single-link files; replacement is an atomic rooted rename.
func Install(source, destination string) error {
	source, destination, err := cleanAbsolutePair(source, destination)
	if err != nil {
		return err
	}
	sourceRoot, err := safefs.Open(filepath.Dir(source))
	if err != nil {
		return fmt.Errorf("open source root: %w", err)
	}
	defer sourceRoot.Close()
	data, err := safefs.ReadFileVerified(sourceRoot, filepath.Base(source), 64<<20)
	if err != nil {
		return fmt.Errorf("read verified source: %w", err)
	}

	destRoot, err := safefs.OpenOrCreate(filepath.Dir(destination), 0o755)
	if err != nil {
		return fmt.Errorf("open destination root: %w", err)
	}
	defer destRoot.Close()
	name := filepath.Base(destination)
	if info, statErr := destRoot.Lstat(name); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refusing unsafe destination %q", destination)
		}
		if !safefs.SingleLink(info) {
			return fmt.Errorf("refusing hard-linked destination %q", destination)
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if err := safefs.WriteAtomic(destRoot, name, data, 0o755, ".skynex-install-"); err != nil {
		return fmt.Errorf("atomic install: %w", err)
	}
	return nil
}

func cleanAbsolutePair(source, destination string) (string, string, error) {
	for _, path := range []string{source, destination} {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return "", "", fmt.Errorf("path must be an absolute clean path")
		}
	}
	if filepath.Base(source) == "." || filepath.Base(destination) == "." {
		return "", "", fmt.Errorf("file path required")
	}
	return source, destination, nil
}
