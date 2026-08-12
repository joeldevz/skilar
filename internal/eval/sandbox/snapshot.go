package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joeldevz/skynex/internal/safefs"
)

type EntryKind string

const (
	EntryFile     EntryKind = "file"
	EntryDir      EntryKind = "dir"
	EntrySymlink  EntryKind = "symlink"
	EntryHardlink EntryKind = "hardlink"
	EntrySpecial  EntryKind = "special"
)

// Entry is one canonical filesystem observation. SHA256 is populated only for
// regular, singly-linked files; unsafe entries are recorded without following
// or reading them.
type Entry struct {
	Path   string    `json:"path"`
	Kind   EntryKind `json:"kind"`
	Mode   uint32    `json:"mode"`
	Size   int64     `json:"size,omitempty"`
	SHA256 string    `json:"sha256,omitempty"`
}

type Snapshot struct {
	Digest     string  `json:"digest"`
	Entries    []Entry `json:"entries"`
	FileCount  int     `json:"file_count"`
	TotalBytes int64   `json:"total_bytes"`
}

// SnapshotTree records a tree without following unsafe entries. Unsafe entry
// kinds remain in the returned snapshot so post-run security judges can fail on
// observed violations.
func SnapshotTree(path string, limits SnapshotLimits) (Snapshot, error) {
	if err := validateAbsoluteDir(path); err != nil {
		return Snapshot{}, err
	}
	root, err := safefs.Open(path)
	if err != nil {
		return Snapshot{}, err
	}
	defer root.Close()
	return takeSnapshot(root, limits)
}

// DigestTree is the strict, read-only variant used for trusted inputs and
// provenance manifests. It rejects symlinks, hard links and special files.
func DigestTree(path string, limits SnapshotLimits) (Snapshot, error) {
	snapshot, err := SnapshotTree(path, limits)
	if err != nil {
		return Snapshot{}, err
	}
	if unsafe := snapshot.UnsafeEntries(); len(unsafe) != 0 {
		return Snapshot{}, fmt.Errorf("tree contains unsafe entry %q (%s)", unsafe[0].Path, unsafe[0].Kind)
	}
	return snapshot, nil
}

func (s Snapshot) Entry(path string) (Entry, bool) {
	i := sort.Search(len(s.Entries), func(i int) bool { return s.Entries[i].Path >= path })
	if i < len(s.Entries) && s.Entries[i].Path == path {
		return s.Entries[i], true
	}
	return Entry{}, false
}

func (s Snapshot) UnsafeEntries() []Entry {
	var unsafe []Entry
	for _, entry := range s.Entries {
		if entry.Kind == EntrySymlink || entry.Kind == EntryHardlink || entry.Kind == EntrySpecial {
			unsafe = append(unsafe, entry)
		}
	}
	return unsafe
}

type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeRemoved  ChangeKind = "removed"
	ChangeModified ChangeKind = "modified"
)

type Change struct {
	Path   string
	Kind   ChangeKind
	Before *Entry
	After  *Entry
}

func Diff(before, after Snapshot) []Change {
	left := make(map[string]Entry, len(before.Entries))
	right := make(map[string]Entry, len(after.Entries))
	paths := make(map[string]struct{}, len(before.Entries)+len(after.Entries))
	for _, entry := range before.Entries {
		left[entry.Path] = entry
		paths[entry.Path] = struct{}{}
	}
	for _, entry := range after.Entries {
		right[entry.Path] = entry
		paths[entry.Path] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	changes := make([]Change, 0)
	for _, path := range ordered {
		b, bok := left[path]
		a, aok := right[path]
		switch {
		case !bok:
			copy := a
			changes = append(changes, Change{Path: path, Kind: ChangeAdded, After: &copy})
		case !aok:
			copy := b
			changes = append(changes, Change{Path: path, Kind: ChangeRemoved, Before: &copy})
		case b != a:
			beforeCopy, afterCopy := b, a
			changes = append(changes, Change{Path: path, Kind: ChangeModified, Before: &beforeCopy, After: &afterCopy})
		}
	}
	return changes
}

func normalizeSnapshotLimits(limits SnapshotLimits) (SnapshotLimits, error) {
	defaults := DefaultSnapshotLimits()
	if limits.MaxFiles == 0 {
		limits.MaxFiles = defaults.MaxFiles
	}
	if limits.MaxTotalBytes == 0 {
		limits.MaxTotalBytes = defaults.MaxTotalBytes
	}
	if limits.MaxFileBytes == 0 {
		limits.MaxFileBytes = defaults.MaxFileBytes
	}
	if limits.MaxFiles < 1 || limits.MaxTotalBytes < 1 || limits.MaxFileBytes < 1 || limits.MaxFileBytes > limits.MaxTotalBytes {
		return SnapshotLimits{}, fmt.Errorf("invalid snapshot limits: %+v", limits)
	}
	return limits, nil
}

func takeSnapshot(root *os.Root, limits SnapshotLimits) (Snapshot, error) {
	limits, err := normalizeSnapshotLimits(limits)
	if err != nil {
		return Snapshot{}, err
	}
	var result Snapshot
	if err := walkRoot(root, ".", func(path string, info fs.FileInfo) error {
		if path == ".git" || strings.HasPrefix(path, ".git/") {
			return fs.SkipDir
		}
		if len(result.Entries) >= limits.MaxFiles {
			return fmt.Errorf("snapshot exceeds file-count limit of %d entries", limits.MaxFiles)
		}
		entry := Entry{Path: path, Mode: uint32(info.Mode().Perm())}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			entry.Kind = EntrySymlink
		case info.IsDir():
			entry.Kind = EntryDir
		case info.Mode().IsRegular():
			single, linkErr := descriptorSingleLink(root, path)
			if linkErr != nil {
				return linkErr
			}
			if !single {
				entry.Kind = EntryHardlink
				break
			}
			if info.Size() > limits.MaxFileBytes {
				return fmt.Errorf("file %q exceeds per-file limit of %d bytes", path, limits.MaxFileBytes)
			}
			if info.Size() > limits.MaxTotalBytes-result.TotalBytes {
				return fmt.Errorf("snapshot exceeds total-byte limit of %d bytes", limits.MaxTotalBytes)
			}
			data, readErr := safefs.ReadFileVerified(root, path, limits.MaxFileBytes)
			if readErr != nil {
				return fmt.Errorf("read snapshot file %q: %w", path, readErr)
			}
			digest := sha256.Sum256(data)
			entry.Kind = EntryFile
			entry.Size = int64(len(data))
			entry.SHA256 = "sha256:" + hex.EncodeToString(digest[:])
			result.FileCount++
			result.TotalBytes += int64(len(data))
		default:
			entry.Kind = EntrySpecial
		}
		result.Entries = append(result.Entries, entry)
		return nil
	}); err != nil {
		return Snapshot{}, err
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	canonical, err := json.Marshal(result.Entries)
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode canonical snapshot: %w", err)
	}
	digest := sha256.Sum256(canonical)
	result.Digest = "sha256:" + hex.EncodeToString(digest[:])
	return result, nil
}

func descriptorSingleLink(root *os.Root, path string) (bool, error) {
	f, err := root.Open(path)
	if err != nil {
		return false, fmt.Errorf("open %q for link verification: %w", path, err)
	}
	single, linkErr := safefs.SingleLinkFile(f)
	closeErr := f.Close()
	if linkErr != nil {
		return false, fmt.Errorf("verify links for %q: %w", path, linkErr)
	}
	if closeErr != nil {
		return false, fmt.Errorf("close %q after link verification: %w", path, closeErr)
	}
	return single, nil
}

func walkRoot(root *os.Root, dir string, visit func(string, fs.FileInfo) error) error {
	f, err := root.Open(dir)
	if err != nil {
		return err
	}
	entries, readErr := f.ReadDir(-1)
	closeErr := f.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, dirEntry := range entries {
		path := dirEntry.Name()
		if dir != "." {
			path = filepath.ToSlash(filepath.Join(dir, dirEntry.Name()))
		}
		path, err = safeRelative(path)
		if err != nil {
			return err
		}
		info, statErr := root.Lstat(path)
		if statErr != nil {
			return statErr
		}
		visitErr := visit(path, info)
		if visitErr == fs.SkipDir {
			continue
		}
		if visitErr != nil {
			return visitErr
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			if err := walkRoot(root, path, visit); err != nil {
				return err
			}
		}
	}
	return nil
}
