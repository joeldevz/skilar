//go:build go1.25

// Package safefs contains the small rooted-filesystem primitive used by
// installers.  Mutation callers must keep the Root alive for the whole
// operation; validation of a pathname is never used as a substitute for it.
package safefs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type Root = os.Root

func Relative(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" || name == "." || strings.HasPrefix(name, "/") || filepath.IsAbs(name) || strings.Contains(name, "//") || name != strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(name))), "./") {
		return "", fmt.Errorf("invalid relative path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == "" {
			return "", fmt.Errorf("invalid relative path %q", name)
		}
	}
	return name, nil
}

// Open opens an already validated, non-symlink directory as a rooted handle.
func Open(path string) (*os.Root, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("invalid root %q", path)
	}
	return openAbsolute(path, false, 0o700)
}

func OpenOrCreate(path string, perm os.FileMode) (*os.Root, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("invalid root %q", path)
	}
	return openAbsolute(path, true, perm)
}

// openAbsolute acquires an absolute directory by retaining every already
// opened parent while descending.  In particular it never validates a path,
// closes the parent, and reopens the child through the ambient namespace.
func openAbsolute(path string, create bool, perm os.FileMode) (*os.Root, error) {
	volume := filepath.VolumeName(path)
	rel := strings.TrimPrefix(path, volume)
	separator := string(filepath.Separator)
	if !strings.HasPrefix(rel, separator) {
		return nil, fmt.Errorf("invalid absolute root %q", path)
	}
	anchorName := volume + separator
	current, err := os.OpenRoot(anchorName)
	if err != nil {
		return nil, err
	}
	parts := strings.FieldsFunc(strings.TrimPrefix(rel, separator), func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return current, nil
	}
	for _, part := range parts {
		if part == "." || part == ".." || part == "" {
			_ = current.Close()
			return nil, fmt.Errorf("invalid root component %q", part)
		}
		info, statErr := current.Lstat(part)
		if errors.Is(statErr, fs.ErrNotExist) && create {
			if statErr = current.Mkdir(part, perm); statErr == nil || errors.Is(statErr, fs.ErrExist) {
				info, statErr = current.Lstat(part)
			}
		}
		if statErr != nil {
			_ = current.Close()
			return nil, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			_ = current.Close()
			return nil, fmt.Errorf("root component is not a real directory: %q", part)
		}
		child, openErr := current.OpenRoot(part)
		if openErr != nil {
			_ = current.Close()
			return nil, openErr
		}
		_ = current.Close()
		current = child
	}
	return current, nil
}

func Ensure(root *os.Root, name string, perm os.FileMode) error {
	name, err := Relative(name)
	if err != nil {
		return err
	}
	return root.MkdirAll(name, perm)
}

// ChmodRoot changes the mode of the directory represented by root through a
// descriptor retained by os.Root. It deliberately does not chmod the ambient
// pathname used to obtain the root.
func ChmodRoot(root *os.Root, perm os.FileMode) error {
	if root == nil {
		return errors.New("nil filesystem root")
	}
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(perm)
}

// ReadFileAbsoluteVerified opens the parent directory once and performs the
// identity-checked read relative to that retained descriptor.
func ReadFileAbsoluteVerified(path string, maxBytes int64) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("invalid absolute file path %q", path)
	}
	root, err := Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return ReadFileVerified(root, filepath.Base(path), maxBytes)
}

// TempFile creates a uniquely named file below root without leaving the
// retained descriptor's namespace.
func TempFile(root *os.Root, prefix string) (string, *os.File, error) {
	return temp(root, ".", prefix)
}

// ReadFileVerified reads one regular file without following a symlink and
// verifies that the directory entry was not replaced while it was read. The
// before/descriptor/after identity checks are intentionally all rooted in the
// retained descriptor.
func ReadFileVerified(root *os.Root, name string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes >= int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("invalid maximum file size %d", maxBytes)
	}
	name, err := Relative(name)
	if err != nil {
		return nil, err
	}
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing non-regular file %q", name)
	}
	if !singleLink(before) {
		return nil, fmt.Errorf("refusing hard-linked file %q", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	if only, linkErr := singleLinkFile(f); linkErr != nil || !only {
		_ = f.Close()
		if linkErr != nil {
			return nil, linkErr
		}
		return nil, fmt.Errorf("refusing hard-linked file %q", name)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxBytes+1))
	stat, statErr := f.Stat()
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if statErr != nil {
		return nil, statErr
	}
	after, afterErr := root.Lstat(name)
	if afterErr != nil {
		return nil, afterErr
	}
	if !sameIdentity(before, stat) || !sameIdentity(before, after) || !singleLink(stat) || !singleLink(after) || closeErr != nil {
		return nil, fmt.Errorf("file changed while reading %q", name)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file %q exceeds maximum size of %d bytes", name, maxBytes)
	}
	return data, nil
}

func sameIdentity(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode().Type() == b.Mode().Type() && singleLink(a) == singleLink(b)
}

func sameFileIdentity(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode().Type() == b.Mode().Type()
}

func Remove(root *os.Root, name string) error {
	name, err := Relative(name)
	if err != nil {
		return err
	}
	if err := root.RemoveAll(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// StageVerified atomically renames one inspected, single-link regular file to
// a cryptographically random private quarantine directory. The callback is
// invoked after capture and before the rename, for race-injection and
// transaction seams.
func StageVerified(root *os.Root, name string, stageDir string, digest string, before func(string) error) (string, error) {
	name, err := Relative(name)
	if err != nil {
		return "", err
	}
	if stageDir != "." {
		stageDir, err = Relative(stageDir)
		if err != nil {
			return "", err
		}
	}
	if stageDir == "" {
		stageDir = "."
	}
	beforeInfo, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	if !beforeInfo.Mode().IsRegular() || beforeInfo.Mode()&os.ModeSymlink != 0 || !singleLink(beforeInfo) {
		return "", fmt.Errorf("refusing non-regular or hard-linked file %q", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	fdInfo, statErr := f.Stat()
	data, readErr := io.ReadAll(io.LimitReader(f, 64<<20+1))
	closeErr := f.Close()
	if statErr != nil {
		return "", statErr
	}
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if int64(len(data)) > 64<<20 {
		return "", fmt.Errorf("file %q exceeds maximum size of %d bytes", name, 64<<20)
	}
	if !sameIdentity(beforeInfo, fdInfo) || !singleLink(fdInfo) {
		return "", fmt.Errorf("file changed while staging %q", name)
	}
	if digest != "" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != digest {
			return "", fmt.Errorf("file digest mismatch %q", name)
		}
	}
	if before != nil {
		if err := before(name); err != nil {
			return "", err
		}
	}
	afterCapture, err := root.Lstat(name)
	if err != nil || !sameIdentity(beforeInfo, afterCapture) {
		return "", fmt.Errorf("file changed before staging %q", name)
	}
	quarantine, err := uninstallStageName(root, stageDir)
	if err != nil {
		return "", err
	}
	if err := root.Mkdir(quarantine, 0o700); err != nil {
		return "", err
	}
	stage := filepath.ToSlash(filepath.Join(quarantine, "item"))
	if _, err := root.Lstat(stage); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			return "", fmt.Errorf("staging target already exists")
		}
		return "", err
	}
	if err := root.Rename(name, stage); err != nil {
		return "", err
	}
	stagedInfo, err := root.Lstat(stage)
	if err != nil || !sameIdentity(beforeInfo, stagedInfo) {
		if err == nil {
			// Link is deliberately no-clobber: an occupied source preserves both
			// the replacement and this quarantined recovery artifact.
			if linkErr := root.Link(stage, name); linkErr == nil {
				if removeErr := root.Remove(stage); removeErr != nil {
					return stage, errors.Join(fmt.Errorf("staged file identity mismatch %q", name), removeErr)
				}
				return stage, fmt.Errorf("staged file identity mismatch %q", name)
			}
		}
		return stage, fmt.Errorf("staged file identity mismatch %q; recovery artifact retained", name)
	}
	return stage, nil
}

func uninstallStageName(root *os.Root, dir string) (string, error) {
	for i := 0; i < 8; i++ {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return "", err
		}
		name := filepath.ToSlash(filepath.Join(dir, ".skynex-uninstall-"+hex.EncodeToString(token[:])))
		if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("unable to allocate unused staging name")
}

// ReadDir reads a directory through the retained root descriptor. It is kept
// separate from os.ReadDir so cleanup cannot be redirected by a raced parent.
func ReadDir(root *os.Root, name string) ([]os.DirEntry, error) {
	name, err := Relative(name)
	if err != nil {
		return nil, err
	}
	dir, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.ReadDir(-1)
}

func WriteAtomic(root *os.Root, name string, data []byte, perm os.FileMode, prefix string) error {
	name, err := Relative(name)
	if err != nil {
		return err
	}
	dir := filepath.ToSlash(filepath.Dir(name))
	if dir == "." {
		dir = "."
	}
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmpName, tmp, err := temp(root, dir, prefix)
	if err != nil {
		return err
	}
	defer root.Remove(tmpName)
	if err = tmp.Chmod(perm); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return root.Rename(tmpName, name)
}

func CopyAtomic(root *os.Root, name string, r io.Reader, perm os.FileMode, prefix string) error {
	_, err := CopyAtomicInfo(root, name, r, perm, prefix, nil)
	return err
}

// CopyAtomicInfo copies r to name by atomically renaming a temporary file and
// returns the temporary file's identity captured before it is closed and
// renamed.
func CopyAtomicInfo(root *os.Root, name string, r io.Reader, perm os.FileMode, prefix string, afterRename func() error) (os.FileInfo, error) {
	name, err := Relative(name)
	if err != nil {
		return nil, err
	}
	dir := filepath.ToSlash(filepath.Dir(name))
	if err = root.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmpName, tmp, err := temp(root, dir, prefix)
	if err != nil {
		return nil, err
	}
	defer root.Remove(tmpName)
	if err = tmp.Chmod(perm); err == nil {
		_, err = io.Copy(tmp, r)
	}
	if err == nil {
		err = tmp.Sync()
	}
	var identity os.FileInfo
	if err == nil {
		identity, err = tmp.Stat()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if err := root.Rename(tmpName, name); err != nil {
		return nil, err
	}
	if afterRename != nil {
		if err := afterRename(); err != nil {
			return identity, err
		}
	}
	return identity, nil
}

func temp(root *os.Root, dir, prefix string) (string, *os.File, error) {
	var token [12]byte
	for i := 0; i < 8; i++ {
		if _, err := rand.Read(token[:]); err != nil {
			return "", nil, err
		}
		name := filepath.ToSlash(filepath.Join(dir, prefix+hex.EncodeToString(token[:])))
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return name, f, err
	}
	return "", nil, errors.New("unable to create rooted temporary file")
}
