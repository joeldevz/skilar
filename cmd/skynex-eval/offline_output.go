package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/experiment"
	"github.com/joeldevz/skynex/internal/safefs"
)

// validateCompareOutputLocation resolves existing symlink aliases without
// creating the output. The returned path is absolute and anchored beneath the
// canonical nearest existing ancestor.
func validateCompareOutputLocation(output, manifest, control, candidate string, bundles []experiment.VerifiedBundle) (string, error) {
	if output == "" || strings.TrimSpace(output) != output || strings.IndexByte(output, 0) >= 0 {
		return "", fmt.Errorf("comparison output path is required, must not contain NUL, and must not have surrounding whitespace")
	}
	resolvedOutput, err := resolveOfflineFuturePath(output)
	if err != nil {
		return "", fmt.Errorf("resolve comparison output %q: %w", output, err)
	}
	for _, protected := range []struct {
		name string
		path string
	}{
		{name: "manifest", path: manifest},
		{name: "control artifact", path: control},
		{name: "candidate artifact", path: candidate},
	} {
		resolvedProtected, resolveErr := resolveOfflineExistingPath(protected.path)
		if resolveErr != nil {
			return "", fmt.Errorf("resolve %s %q: %w", protected.name, protected.path, resolveErr)
		}
		equal, compareErr := offlinePathWithinOrEqual(resolvedProtected, resolvedOutput)
		if compareErr != nil {
			return "", fmt.Errorf("compare output with %s: %w", protected.name, compareErr)
		}
		if equal {
			return "", fmt.Errorf("comparison output %q must not overwrite or descend from the %s %q", output, protected.name, protected.path)
		}
	}
	for _, bundle := range bundles {
		resolvedRoot, resolveErr := resolveOfflineExistingPath(bundle.AbsoluteRoot)
		if resolveErr != nil {
			return "", fmt.Errorf("resolve verified %s bundle %q: %w", bundle.Name, bundle.AbsoluteRoot, resolveErr)
		}
		inside, compareErr := offlinePathWithinOrEqual(resolvedRoot, resolvedOutput)
		if compareErr != nil {
			return "", fmt.Errorf("compare output with verified %s bundle: %w", bundle.Name, compareErr)
		}
		if inside {
			return "", fmt.Errorf("comparison output %q must be outside verified %s bundle %q", output, bundle.Name, bundle.AbsoluteRoot)
		}
	}
	if _, err := os.Lstat(resolvedOutput); err == nil {
		return "", fmt.Errorf("refusing to replace existing comparison output %q: %w", output, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("inspect comparison output %q: %w", output, err)
	}
	return resolvedOutput, nil
}

func resolveOfflineExistingPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(filepath.Clean(absolute))
}

// resolveOfflineFuturePath canonicalizes every existing ancestor and appends
// the missing suffix lexically. Existing dangling symlinks fail closed.
func resolveOfflineFuturePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	probe := filepath.Clean(absolute)
	missing := make([]string, 0, 4)
	for {
		if _, statErr := os.Lstat(probe); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(probe)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("no existing ancestor for %q", path)
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

func offlinePathWithinOrEqual(root, candidate string) (bool, error) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err == nil && (relative == "." || relative != ".." && !filepath.IsAbs(relative) && !offlineStartsWithParent(relative)) {
		return true, nil
	}
	// Lexical comparison is insufficient on case-insensitive filesystems. Walk
	// upward from the nearest existing candidate ancestor and compare directory
	// identities so alternate casing cannot disguise a path inside root.
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false, err
	}
	probe := filepath.Clean(candidate)
	for {
		info, statErr := os.Stat(probe)
		if statErr == nil {
			if os.SameFile(rootInfo, info) {
				return true, nil
			}
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return false, statErr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return false, nil
		}
		probe = parent
	}
}

func offlineStartsWithParent(relative string) bool {
	return relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

// compareOutputReservation retains the destination directory but deliberately
// does not create a destination placeholder. Publish hard-links a fully fsynced
// same-directory temporary into the final name. Link is atomic and fails with
// EEXIST, so a file created at any point before publication is never replaced.
type compareOutputReservation struct {
	root      *os.Root
	name      string
	published bool
}

func saveCompareOutputNoClobber(path string, value any) (err error) {
	reservation, err := reserveCompareOutput(path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, reservation.Close())
	}()
	return reservation.Publish(value)
}

func reserveCompareOutput(path string) (*compareOutputReservation, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("comparison output must be a clean absolute path")
	}
	root, err := safefs.OpenOrCreate(filepath.Dir(path), 0o700)
	if err != nil {
		return nil, fmt.Errorf("open comparison output directory: %w", err)
	}
	name := filepath.Base(path)
	if _, err := root.Lstat(name); err == nil {
		_ = root.Close()
		return nil, fmt.Errorf("refusing to replace existing comparison output %q: %w", path, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		_ = root.Close()
		return nil, fmt.Errorf("inspect comparison output %q: %w", path, err)
	}
	return &compareOutputReservation{root: root, name: name}, nil
}

func (reservation *compareOutputReservation) Publish(value any) error {
	if reservation == nil || reservation.root == nil || reservation.published {
		return fmt.Errorf("active comparison output reservation is required")
	}
	data, err := baseline.CanonicalJSON(value)
	if err != nil {
		return err
	}
	if int64(len(data)+1) > baseline.DefaultMaxJSONBytes {
		return fmt.Errorf("JSON output exceeds %d bytes", baseline.DefaultMaxJSONBytes)
	}
	temporaryName, temporary, err := safefs.TempFile(reservation.root, ".skynex-eval-compare-")
	if err != nil {
		return fmt.Errorf("create temporary comparison output: %w", err)
	}
	defer func() { _ = reservation.root.Remove(temporaryName) }()
	if _, err = temporary.Write(data); err == nil {
		_, err = temporary.Write([]byte{'\n'})
	}
	if err == nil {
		err = temporary.Sync()
	}
	temporaryInfo, statErr := temporary.Stat()
	if err == nil {
		err = statErr
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write temporary comparison output: %w", err)
	}
	// Link is an atomic no-replace publication. Unlike Rename, it cannot
	// overwrite a path raced into place after the initial location check.
	if err := reservation.root.Link(temporaryName, reservation.name); err != nil {
		return fmt.Errorf("publish comparison output without clobbering: %w", err)
	}
	reservation.published = true
	published, err := reservation.root.Lstat(reservation.name)
	if err != nil || published.Mode()&os.ModeSymlink != 0 || !published.Mode().IsRegular() ||
		temporaryInfo == nil || !os.SameFile(temporaryInfo, published) {
		if err == nil {
			err = fmt.Errorf("published output identity is unexpected")
		}
		return fmt.Errorf("verify published comparison output: %w", err)
	}
	if err := reservation.root.Remove(temporaryName); err != nil {
		return fmt.Errorf("remove linked comparison temporary: %w", err)
	}
	directory, err := reservation.root.Open(".")
	if err != nil {
		return fmt.Errorf("open comparison output directory for sync: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return fmt.Errorf("sync comparison output directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close comparison output directory: %w", closeErr)
	}
	return nil
}

func (reservation *compareOutputReservation) Close() error {
	if reservation == nil {
		return nil
	}
	var closeErr error
	if reservation.root != nil {
		if err := reservation.root.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close comparison output directory: %w", err))
		}
	}
	return closeErr
}
