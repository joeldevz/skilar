package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/joeldevz/skynex/internal/assets"
	"github.com/joeldevz/skynex/internal/safefs"
)

const maxSyncFileBytes = 4 << 20
const maxSyncEntries = 4096

// syncTree retains every opened directory. Leaf operations use the immediate
// parent handle, never a multi-component path that could follow a new symlink.
type syncTree struct {
	dirs map[string]*os.Root
}

func openSyncTree(dir string) (*syncTree, error) {
	root, err := safefs.Open(dir)
	if err != nil {
		return nil, err
	}
	return &syncTree{dirs: map[string]*os.Root{".": root}}, nil
}

func (t *syncTree) close() {
	for _, root := range t.dirs {
		root.Close()
	}
}

func (t *syncTree) directory(name string, create bool) (*os.Root, error) {
	if root, ok := t.dirs[name]; ok {
		return root, nil
	}
	clean, err := safefs.Relative(name)
	if err != nil || clean != name || len(name) > 4096 || len(t.dirs) >= maxSyncEntries {
		return nil, fmt.Errorf("invalid or excessive sync directories: %q", name)
	}
	parent, err := t.directory(path.Dir(name), create)
	if err != nil {
		return nil, err
	}
	leaf := path.Base(name)
	before, err := parent.Lstat(leaf)
	if os.IsNotExist(err) && create {
		if err = parent.Mkdir(leaf, 0o755); err != nil {
			return nil, err
		}
		before, err = parent.Lstat(leaf)
	}
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("sync ancestor is not a real directory: %s", name)
	}
	child, err := parent.OpenRoot(leaf)
	if err != nil {
		return nil, err
	}
	opened, statErr := child.Stat(".")
	after, afterErr := parent.Lstat(leaf)
	if statErr != nil || afterErr != nil || !os.SameFile(before, opened) ||
		!after.IsDir() || !os.SameFile(before, after) {
		child.Close()
		return nil, fmt.Errorf("sync directory changed during open: %s", name)
	}
	t.dirs[name] = child
	return child, nil
}

func (t *syncTree) inspect(name string) (os.FileInfo, error) {
	clean, err := safefs.Relative(name)
	if err != nil || clean != name {
		return nil, fmt.Errorf("invalid sync path: %q", name)
	}
	parent, err := t.directory(path.Dir(name), false)
	if err != nil {
		return nil, err
	}
	info, err := parent.Lstat(path.Base(name))
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 ||
		(!info.IsDir() && (!info.Mode().IsRegular() || !safefs.SingleLink(info))) {
		return nil, fmt.Errorf("sync entry is linked or not regular: %s", name)
	}
	return info, nil
}

// Open implements fs.FS using bounded verified snapshots, including the manifest.
// Neither manifest parsing nor fs.Stat can follow source ancestor/final links.
func (t *syncTree) Open(name string) (fs.File, error) {
	info, err := t.inspect(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("sync source is not a regular file: %s", name)
	}
	parent, err := t.directory(path.Dir(name), false)
	if err != nil {
		return nil, err
	}
	raw, err := safefs.ReadFileVerified(parent, path.Base(name), maxSyncFileBytes)
	if err != nil {
		return nil, err
	}
	return &syncSnapshot{Reader: bytes.NewReader(raw), info: info}, nil
}

type syncSnapshot struct {
	*bytes.Reader
	info os.FileInfo
}

func (f *syncSnapshot) Stat() (os.FileInfo, error) { return f.info, nil }
func (f *syncSnapshot) Close() error               { return nil }

// Open the nearest existing destination ancestor without creating anything.
// Missing directories are created through these retained handles after preflight.
func syncDestination(dst string) (*syncTree, string, error) {
	ancestor := dst
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			tree, err := openSyncTree(ancestor)
			if err != nil {
				return nil, "", err
			}
			rel, err := filepath.Rel(ancestor, dst)
			if err != nil {
				tree.close()
				return nil, "", err
			}
			return tree, filepath.ToSlash(rel), nil
		}
		if !os.IsNotExist(err) || filepath.Dir(ancestor) == ancestor {
			return nil, "", err
		}
		ancestor = filepath.Dir(ancestor)
	}
}

// Bounded refresh preflights all input/output/removal files, then uses only
// retained parents and atomic replacement. It never recursively deletes a tree.
func syncSkyAgents(src, dst string) error {
	source, err := openSyncTree(src)
	if err != nil {
		return err
	}
	defer source.close()
	files, err := assets.SkyAgentsShippingFiles(source)
	if err != nil {
		return err
	}
	if len(files) == 0 || len(files) > maxSyncEntries {
		return fmt.Errorf("bounded Sky Agents shipping manifest is required")
	}
	for _, name := range []string{"package.json", "package-lock.json", "bun.lock", "pnpm-lock.yaml", "plugins/sky-agents/tui.ts", "plugins/sky-agents-config/index.ts"} {
		files[name] = true
	}
	payload := make(map[string][]byte, len(files))
	paths := make([]string, 0, len(files))
	total := 0
	for name := range files {
		raw, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		total += len(raw)
		if total > 64<<20 {
			return fmt.Errorf("Sky Agents shipping payload exceeds 64 MiB")
		}
		payload[name] = raw
		paths = append(paths, name)
	}
	sort.Strings(paths)
	dest, prefix, err := syncDestination(dst)
	if err != nil {
		return err
	}
	defer dest.close()
	observed := map[string]os.FileInfo{}
	var stale []string
	var preflight func(string) error
	preflight = func(name string) error {
		if _, seen := observed[name]; seen {
			return nil
		}
		if len(observed) >= maxSyncEntries {
			return fmt.Errorf("too many bounded sync entries")
		}
		info, err := dest.inspect(path.Join(prefix, name))
		if os.IsNotExist(err) {
			observed[name] = nil
			return nil
		}
		if err != nil {
			return err
		}
		observed[name] = info
		if info.IsDir() {
			if files[name] || name == "plugins/sky-agents.tsx" {
				return fmt.Errorf("sync output is a directory: %s", name)
			}
			dir, err := dest.directory(path.Join(prefix, name), false)
			if err != nil {
				return err
			}
			f, err := dir.Open(".")
			if err != nil {
				return err
			}
			entries, readErr := f.ReadDir(maxSyncEntries + 1)
			f.Close()
			if readErr != nil && readErr != io.EOF {
				return readErr
			}
			if len(entries) > maxSyncEntries {
				return fmt.Errorf("too many bounded sync entries")
			}
			for _, entry := range entries {
				if err := preflight(path.Join(name, entry.Name())); err != nil {
					return err
				}
			}
		} else if !files[name] {
			stale = append(stale, name)
		}
		return nil
	}
	for _, name := range append(paths, assets.SkyAgentsDirectory, "plugins/sky-agents", "plugins/sky-agents-config", "plugins/sky-agents.tsx") {
		if err := preflight(name); err != nil {
			return err
		}
	}
	// Recheck the entire plan before the first mutation, through retained parents.
	for name, before := range observed {
		after, err := dest.inspect(path.Join(prefix, name))
		if before == nil && os.IsNotExist(err) {
			continue
		}
		if err != nil || before == nil || !os.SameFile(before, after) || before.Mode().Type() != after.Mode().Type() {
			return fmt.Errorf("sync output changed during preflight: %s", name)
		}
	}
	for _, name := range paths {
		full := path.Join(prefix, name)
		before := observed[name]
		after, inspectErr := dest.inspect(full)
		if !(before == nil && os.IsNotExist(inspectErr)) &&
			(inspectErr != nil || before == nil || !os.SameFile(before, after)) {
			return fmt.Errorf("sync output changed before replacement: %s", name)
		}
		parent, err := dest.directory(path.Dir(full), true)
		if err != nil {
			return err
		}
		if err := safefs.WriteAtomic(parent, path.Base(full), payload[name], 0o644, ".sky-sync-"); err != nil {
			return err
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		full := path.Join(prefix, name)
		info, err := dest.inspect(full)
		if err != nil || !info.Mode().IsRegular() || !os.SameFile(observed[name], info) {
			return fmt.Errorf("stale sync file changed before removal: %s", name)
		}
		parent, err := dest.directory(path.Dir(full), false)
		if err != nil {
			return err
		}
		if err := parent.Remove(path.Base(full)); err != nil {
			return err
		}
	}
	return nil
}
