package installer

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/safefs"
)

type snapshotEntry struct {
	Path          string `json:"path"`
	Existed       bool   `json:"existed"`
	Kind          string `json:"kind"`
	Mode          uint32 `json:"mode"`
	SymlinkTarget string `json:"symlinkTarget,omitempty"`
	Payload       string `json:"payload,omitempty"`
}

type snapshotManifest struct {
	Version   int             `json:"version"`
	Entries   []snapshotEntry `json:"entries"`
	CreatedAt string          `json:"createdAt,omitempty"`
	Status    string          `json:"status,omitempty"`
}

type snapshotState struct {
	dir            string
	manifest       string
	stateRoot      *safefs.Root
	snapshotRoot   *safefs.Root
	snapshotParent *safefs.Root
	payloadRoot    *safefs.Root
	entries        []snapshotEntry
	roots          []string
	rooted         map[string]*rootedPath
}

type rootedPath struct {
	parent   *safefs.Root
	name     string
	path     string
	identity os.FileInfo
}

// Snapshot limits are intentionally conservative: recovery data must never be
// able to consume an unbounded amount of state-directory space.
const (
	maxSnapshotRegularBytes  int64 = 64 << 20
	maxSnapshotRegularFiles        = 10_000
	maxSnapshotEntries             = 100_000
	maxSnapshotManifestBytes       = 16 << 20
)

type snapshotLimits struct {
	regularBytes int64
	regularFiles int
	entries      int
}

func (l *snapshotLimits) addEntry() error {
	if l.entries >= maxSnapshotEntries {
		return fmt.Errorf("snapshot limits exceeded (entries <= %d)", maxSnapshotEntries)
	}
	l.entries++
	return nil
}

func (l *snapshotLimits) add(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() < 0 || l.regularFiles == maxSnapshotRegularFiles || info.Size() > maxSnapshotRegularBytes {
		return fmt.Errorf("snapshot limits exceeded (regular files <= %d, bytes <= %d)", maxSnapshotRegularFiles, maxSnapshotRegularBytes)
	}
	l.regularFiles++
	return nil
}

func (l *snapshotLimits) addBytes(n int64) error {
	if n < 0 || l.regularBytes > maxSnapshotRegularBytes-n {
		return fmt.Errorf("snapshot limits exceeded (regular files <= %d, bytes <= %d)", maxSnapshotRegularFiles, maxSnapshotRegularBytes)
	}
	l.regularBytes += n
	return nil
}

// snapshotCap bounds recovery material retained after transactions. Snapshots
// are never pruned automatically; callers must recover or remove them.
const snapshotCap = 5

var ErrSnapshotCapacity = errors.New("snapshot capacity reached")

// Private test seam. Production callers cannot influence snapshot ordering or recovery.
var snapshotHooks struct {
	BeforeMutate       func() error
	BeforeRollback     func() error
	BeforeRestore      func(string) error
	AfterRestore       func(string) error
	AfterRestoreRename func(string) error
	BeforePayloadCopy  func() error
}

// Apply snapshots all plan roots, runs mutate, and restores the snapshot on failure.
func Apply(plan *Plan, mutate func() error) (err error) {
	if plan == nil || mutate == nil {
		return errors.New("snapshot apply: plan and mutate are required")
	}
	roots, stateDir, err := snapshotRoots(plan)
	if err != nil {
		return err
	}
	if err := validateRoots(roots, stateDir); err != nil {
		return err
	}

	s, err := prepareSnapshot(roots, stateDir, plan.destinations.OpencodeDir)
	if err != nil {
		return err
	}
	if snapshotHooks.BeforeMutate != nil {
		if err := snapshotHooks.BeforeMutate(); err != nil {
			return errors.Join(err, s.rollback())
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			rb := s.rollback()
			if rb != nil {
				panic(errors.Join(fmt.Errorf("panic during apply: %v", recovered), rb))
			}
			panic(recovered)
		}
		if err != nil {
			err = errors.Join(err, s.rollback())
			return
		}
		if cleanupErr := s.commit(); cleanupErr != nil {
			err = errors.Join(fmt.Errorf("snapshot commit failed; managed config/skills rolled back; dependencies may require rerun: %w", cleanupErr), s.rollback())
		}
	}()
	if err = mutate(); err != nil {
		return err
	}
	return nil
}

func snapshotRoots(plan *Plan) ([]string, string, error) {
	d := plan.destinations
	state := d.StateDir
	if !isCleanAbsolute(state) {
		return nil, "", fmt.Errorf("snapshot: invalid StateDir %q", state)
	}
	canonicalState := []string{
		filepath.Join(state, "skills.ownership.json"),
		filepath.Join(state, "skills.config.json"),
		filepath.Join(state, "skills.lock.json"),
	}
	var roots []string
	add := func(path string) {
		if path != "" {
			roots = append(roots, path)
		}
	}
	hasTargetOperation := false
	for _, op := range plan.Operations {
		switch op.Kind {
		case InstallTarget:
			hasTargetOperation = true
			canonical, ok := canonicalTargetDestination(op.Target, d)
			if !ok {
				return nil, "", fmt.Errorf("snapshot: unknown install target %q", op.Target)
			}
			if !isCleanAbsolute(op.Destination) || op.Destination != canonical {
				return nil, "", fmt.Errorf("snapshot: install target %q destination %q does not match canonical destination %q", op.Target, op.Destination, canonical)
			}
			add(canonical)
		case WriteState:
			if !isCanonicalStateFile(op.Destination, canonicalState) {
				return nil, "", fmt.Errorf("snapshot: state operation destination %q is not a canonical state file", op.Destination)
			}
		case CleanupDeprecated:
			if !isCleanAbsolute(op.Destination) || op.Destination != state {
				return nil, "", fmt.Errorf("snapshot: cleanup destination %q is not StateDir", op.Destination)
			}
		}
	}
	if !hasTargetOperation {
		if len(plan.Operations) != 0 {
			return nil, "", errors.New("snapshot: plan has operations but no install target")
		}
		// Hand-built plans without operations retain the historical target
		// selection behavior. Build always emits InstallTarget operations.
		if isCleanAbsolute(d.ClaudeDir) {
			add(d.ClaudeDir)
		}
		if isCleanAbsolute(d.ClaudeConfigFile) && (d.ClaudeConfigFile == d.ClaudeDir || isWithin(d.ClaudeConfigFile, d.ClaudeDir)) {
			return nil, "", fmt.Errorf("snapshot: overlapping roots %q and %q", d.ClaudeDir, d.ClaudeConfigFile)
		}
		if isCleanAbsolute(d.OpencodeDir) {
			add(d.OpencodeDir)
		}
	}
	for _, path := range canonicalState {
		add(path)
	}
	if len(roots) == 0 {
		return nil, "", errors.New("snapshot apply: no snapshot roots selected")
	}
	return uniquePaths(roots), state, nil
}

func canonicalTargetDestination(target string, d Destinations) (string, bool) {
	switch target {
	case "claude":
		return d.ClaudeDir, isCleanAbsolute(d.ClaudeDir)
	case "opencode":
		return d.OpencodeDir, isCleanAbsolute(d.OpencodeDir)
	default:
		return "", false
	}
}

func isCleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && path == filepath.Clean(path)
}

func isCanonicalStateFile(path string, canonical []string) bool {
	if !isCleanAbsolute(path) {
		return false
	}
	for _, candidate := range canonical {
		if path == candidate {
			return true
		}
	}
	return false
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func validateRoots(roots []string, state string) error {
	if !filepath.IsAbs(state) || state != filepath.Clean(state) {
		return fmt.Errorf("snapshot: invalid StateDir %q", state)
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) || root != filepath.Clean(root) || root == string(filepath.Separator) {
			return fmt.Errorf("snapshot: invalid root %q", root)
		}
		if root == state || isWithin(state, root) {
			return fmt.Errorf("snapshot: StateDir is inside target %q", root)
		}
	}
	for i, a := range roots {
		for j, b := range roots {
			if i != j && (isWithin(a, b) || isWithin(b, a)) {
				return fmt.Errorf("snapshot: overlapping roots %q and %q", a, b)
			}
		}
	}
	return nil
}

func isWithin(path, ancestor string) bool {
	rel, err := filepath.Rel(ancestor, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func prepareSnapshot(roots []string, stateDir, opencodeDir string) (*snapshotState, error) {
	stateRoot, err := safefs.OpenOrCreate(stateDir, 0o700)
	if err != nil {
		return nil, fmt.Errorf("snapshot: open state directory: %w", err)
	}
	if err := stateRoot.Mkdir("snapshots", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		_ = stateRoot.Close()
		return nil, fmt.Errorf("snapshot: create directory: %w", err)
	}
	baseRoot, err := stateRoot.OpenRoot("snapshots")
	if err != nil {
		_ = stateRoot.Close()
		return nil, err
	}
	closeBase := func() {
		_ = baseRoot.Close()
		_ = stateRoot.Close()
	}
	if entries, readErr := fs.ReadDir(baseRoot.FS(), "."); readErr == nil && len(entries) >= snapshotCap {
		closeBase()
		return nil, fmt.Errorf("%w: %d recovery snapshots are retained; recover or remove one from %s before retrying", ErrSnapshotCapacity, snapshotCap, filepath.Join(stateDir, "snapshots"))
	} else if readErr != nil {
		closeBase()
		return nil, readErr
	}
	var id [16]byte
	if _, err := io.ReadFull(rand.Reader, id[:]); err != nil {
		closeBase()
		return nil, fmt.Errorf("snapshot: random id: %w", err)
	}
	dirName := hex.EncodeToString(id[:])
	if err := baseRoot.Mkdir(dirName, 0o700); err != nil {
		closeBase()
		return nil, fmt.Errorf("snapshot: create snapshot: %w", err)
	}
	snapshotRoot, err := baseRoot.OpenRoot(dirName)
	if err != nil {
		closeBase()
		return nil, err
	}
	s := &snapshotState{dir: filepath.Join(stateDir, "snapshots", dirName), manifest: filepath.Join(stateDir, "snapshots", dirName, "manifest.json"), stateRoot: stateRoot, snapshotRoot: snapshotRoot, snapshotParent: baseRoot, roots: roots, rooted: make(map[string]*rootedPath)}
	cleanupPartial := true
	defer func() {
		if cleanupPartial {
			discardPartialSnapshot(s)
		}
	}()
	if err := snapshotRoot.Mkdir("payload", 0o700); err != nil {
		return nil, err
	}
	for _, root := range roots {
		parent, openErr := safefs.OpenOrCreate(filepath.Dir(root), 0o700)
		if openErr != nil {
			return nil, fmt.Errorf("snapshot: retain root %q: %w", root, openErr)
		}
		identity, statErr := parent.Lstat(filepath.Base(root))
		if errors.Is(statErr, os.ErrNotExist) {
			identity = nil
		} else if statErr != nil {
			return nil, fmt.Errorf("snapshot: retain root %q: %w", root, statErr)
		}
		s.rooted[root] = &rootedPath{parent: parent, name: filepath.Base(root), path: root, identity: identity}
	}
	s.payloadRoot, err = snapshotRoot.OpenRoot("payload")
	if err != nil {
		return nil, err
	}
	limits := snapshotLimits{}
	for _, root := range roots {
		entries, err := capture(s.rooted[root], root, root == filepath.Join(stateDir, "snapshots"), root == opencodeDir, s.payloadRoot, &limits)
		if err != nil {
			return nil, fmt.Errorf("snapshot: capture %q: %w", root, err)
		}
		s.entries = append(s.entries, entries...)
	}
	manifest, err := json.MarshalIndent(snapshotManifest{Version: 1, Entries: s.entries, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: snapshotStatusRecovery}, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(manifest) > maxSnapshotManifestBytes {
		return nil, fmt.Errorf("snapshot limits exceeded (manifest bytes <= %d)", maxSnapshotManifestBytes)
	}
	if err := safefs.WriteAtomic(snapshotRoot, "manifest.json", append(manifest, '\n'), 0o600, ".manifest-"); err != nil {
		return nil, fmt.Errorf("snapshot: write manifest: %w", err)
	}
	cleanupPartial = false
	return s, nil
}

func capture(handle *rootedPath, root string, skipSnapshots, skipOpenCodeCodeCache bool, payloadRoot *safefs.Root, limits *snapshotLimits) ([]snapshotEntry, error) {
	parent, name := handle.parent, handle.name
	f, err := parent.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		if err := limits.addEntry(); err != nil {
			return nil, err
		}
		return []snapshotEntry{{Path: root, Kind: "missing"}}, nil
	}
	if err != nil {
		// Root.Readlink is intentionally used only for a symlink, which cannot be
		// opened as a regular descriptor by os.Root.
		if target, linkErr := parent.Readlink(name); linkErr == nil {
			if err := limits.addEntry(); err != nil {
				return nil, err
			}
			return []snapshotEntry{{Path: root, Existed: true, Kind: "symlink", SymlinkTarget: target}}, nil
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := limits.addEntry(); err != nil {
		_ = f.Close()
		return nil, err
	}
	entries := []snapshotEntry{{Path: root, Existed: true, Kind: fileKind(info), Mode: uint32(info.Mode().Perm())}}
	if !info.IsDir() {
		if info.Mode().IsRegular() {
			p, e := savePayloadFile(f, parent, name, info, payloadRoot, name, limits)
			entries[0].Payload = p
			_ = f.Close()
			return entries, e
		}
		_ = f.Close()
		return entries, nil
	}
	dirRoot, err := parent.OpenRoot(name)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	result, walkErr := captureDir(root, dirRoot, payloadRoot, entries, skipSnapshots, skipOpenCodeCodeCache, limits)
	_ = dirRoot.Close()
	_ = f.Close()
	return result, walkErr
}

func captureDir(root string, dirRoot, payloadRoot *safefs.Root, entries []snapshotEntry, skipSnapshots, skipOpenCodeCodeCache bool, limits *snapshotLimits) ([]snapshotEntry, error) {
	err := fs.WalkDir(dirRoot.FS(), ".", func(path string, de fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		rel := filepath.FromSlash(path)
		if skipSnapshots && (rel == "snapshots" || strings.HasPrefix(rel, "snapshots"+string(filepath.Separator))) {
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if skipOpenCodeCodeCache && skipOpenCodeCodeCachePath(rel) {
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if de.Type()&os.ModeSymlink != 0 {
			if err := limits.addEntry(); err != nil {
				return err
			}
			entry := snapshotEntry{Path: filepath.Join(root, rel), Existed: true, Kind: "symlink"}
			var e error
			entry.SymlinkTarget, e = dirRoot.Readlink(path)
			if e != nil {
				return e
			}
			entries = append(entries, entry)
			return nil
		}
		opened, e := dirRoot.Open(path)
		if e != nil {
			return e
		}
		info, e := opened.Stat()
		if e != nil {
			_ = opened.Close()
			return e
		}
		entry := snapshotEntry{Path: filepath.Join(root, rel), Existed: true, Kind: fileKind(info), Mode: uint32(info.Mode().Perm())}
		if err := limits.addEntry(); err != nil {
			_ = opened.Close()
			return err
		}
		if info.Mode().IsRegular() {
			readErr := error(nil)
			entry.Payload, readErr = savePayloadFile(opened, dirRoot, path, info, payloadRoot, path, limits)
			_ = opened.Close()
			if readErr != nil {
				return readErr
			}
		} else {
			_ = opened.Close()
		}
		entries = append(entries, entry)
		return nil
	})
	return entries, err
}

func skipOpenCodeCodeCachePath(rel string) bool {
	codeCache := filepath.Join("EBWebView", "Default", "Code Cache")
	return rel == codeCache || strings.HasPrefix(rel, codeCache+string(filepath.Separator))
}

func fileKind(info os.FileInfo) string {
	if info.Mode()&os.ModeSymlink != 0 {
		return "symlink"
	}
	if info.IsDir() {
		return "dir"
	}
	if info.Mode().IsRegular() {
		return "file"
	}
	return "other"
}

func closeSnapshotState(s *snapshotState) {
	if s == nil {
		return
	}
	for _, handle := range s.rooted {
		_ = handle.parent.Close()
	}
	if s.payloadRoot != nil {
		_ = s.payloadRoot.Close()
		s.payloadRoot = nil
	}
	if s.snapshotRoot != nil {
		_ = s.snapshotRoot.Close()
		s.snapshotRoot = nil
	}
	if s.snapshotParent != nil {
		_ = s.snapshotParent.Close()
		s.snapshotParent = nil
	}
	if s.stateRoot != nil {
		_ = s.stateRoot.Close()
		s.stateRoot = nil
	}
}

func discardPartialSnapshot(s *snapshotState) {
	if s == nil {
		return
	}
	parent := s.snapshotParent
	name := filepath.Base(s.dir)
	for _, handle := range s.rooted {
		_ = handle.parent.Close()
	}
	if s.payloadRoot != nil {
		_ = s.payloadRoot.Close()
	}
	if s.snapshotRoot != nil {
		_ = s.snapshotRoot.Close()
	}
	if parent != nil {
		_ = parent.RemoveAll(name)
		_ = parent.Close()
	}
	if s.stateRoot != nil {
		_ = s.stateRoot.Close()
	}
	s.payloadRoot, s.snapshotRoot, s.snapshotParent, s.stateRoot = nil, nil, nil, nil
}

func savePayload(sourceRoot *safefs.Root, source string, payloadRoot *safefs.Root) (string, error) {
	data, err := safefs.ReadFileVerified(sourceRoot, source, maxSnapshotRegularBytes)
	if err != nil {
		return "", err
	}
	return savePayloadData(data, payloadRoot, source)
}

func savePayloadData(data []byte, payloadRoot *safefs.Root, seed string) (string, error) {
	return savePayloadReader(bytes.NewReader(data), payloadRoot, seed)
}

func savePayloadFile(source *os.File, sourceRoot *safefs.Root, sourceName string, before os.FileInfo, payloadRoot *safefs.Root, seed string, limits *snapshotLimits) (string, error) {
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return "", fmt.Errorf("refusing non-regular file %q", sourceName)
	}
	if err := limits.add(before); err != nil {
		return "", err
	}
	reader := io.Reader(io.LimitReader(source, maxSnapshotRegularBytes+1))
	name, err := savePayloadReaderLimited(reader, payloadRoot, seed, limits)
	if err != nil {
		return "", err
	}
	after, statErr := source.Stat()
	if statErr != nil {
		return "", statErr
	}
	current, lstatErr := sourceRoot.Lstat(sourceName)
	if lstatErr != nil {
		return "", lstatErr
	}
	if !os.SameFile(before, after) || !os.SameFile(before, current) || after.Size() != before.Size() {
		return "", fmt.Errorf("file changed while reading %q", sourceName)
	}
	return name, nil
}

func savePayloadReader(f io.Reader, payloadRoot *safefs.Root, seed string) (string, error) {
	return savePayloadReaderLimited(f, payloadRoot, seed, nil)
}

func savePayloadReaderLimited(f io.Reader, payloadRoot *safefs.Root, seed string, limits *snapshotLimits) (string, error) {
	name := fmt.Sprintf("%08x", len(seed))
	for i := 0; ; i++ {
		candidate := fmt.Sprintf("%s-%d", name, i)
		out, e := payloadRoot.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if e != nil {
			if errors.Is(e, os.ErrExist) {
				continue
			}
			return "", e
		}
		if snapshotHooks.BeforePayloadCopy != nil {
			if e = snapshotHooks.BeforePayloadCopy(); e != nil {
				_ = out.Close()
				_ = payloadRoot.Remove(candidate)
				return "", e
			}
		}
		counting := f
		if limits != nil {
			counting = &budgetReader{reader: f, limits: limits}
		}
		if _, e = io.Copy(out, counting); e == nil {
			e = out.Sync()
		}
		closeErr := out.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil {
			_ = payloadRoot.Remove(candidate)
			return "", e
		}
		return candidate, nil
	}
}

type budgetReader struct {
	reader io.Reader
	limits *snapshotLimits
}

func (r *budgetReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		if budgetErr := r.limits.addBytes(int64(n)); budgetErr != nil {
			return n, budgetErr
		}
	}
	return n, err
}

func (s *snapshotState) rollback() error {
	defer closeSnapshotState(s)
	if snapshotHooks.BeforeRollback != nil {
		if err := snapshotHooks.BeforeRollback(); err != nil {
			return err
		}
	}
	var errs []error
	for _, root := range s.roots {
		if err := removeRooted(s, s.rooted[root]); err != nil {
			errs = append(errs, err)
		}
	}
	// A retained root that changed identity is no longer safe to reconcile.
	// Do not continue with restoreEntry: even a rooted relative pathname could
	// now address a replacement directory. The recovery snapshot is retained.
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	entries := append([]snapshotEntry(nil), s.entries...)
	sort.Slice(entries, func(i, j int) bool { return pathDepth(entries[i].Path) < pathDepth(entries[j].Path) })
	for _, e := range entries {
		if !e.Existed {
			continue
		}
		if snapshotHooks.BeforeRestore != nil {
			if err := snapshotHooks.BeforeRestore(e.Path); err != nil {
				errs = append(errs, err)
				break
			}
		}
		if err := validateRetainedRoots(s); err != nil {
			errs = append(errs, err)
			break
		}
		restoredIdentity, err := restoreEntry(s, e)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if snapshotHooks.AfterRestore != nil {
			if err := snapshotHooks.AfterRestore(e.Path); err != nil {
				errs = append(errs, err)
				break
			}
		}
		if err := refreshRestoredRootIdentity(s, e, restoredIdentity); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func refreshRestoredRootIdentity(s *snapshotState, e snapshotEntry, expected os.FileInfo) error {
	if e.Kind != "file" {
		return nil
	}
	path, ok := s.rooted[e.Path]
	if !ok {
		return nil
	}
	identity, err := path.parent.Lstat(path.name)
	if err != nil {
		return err
	}
	if expected == nil || !identity.Mode().IsRegular() || !os.SameFile(expected, identity) {
		return fmt.Errorf("snapshot: refusing rollback of replaced root %q", path.name)
	}
	path.identity = identity
	return nil
}

func validateRetainedRoots(s *snapshotState) error {
	for _, root := range s.roots {
		path := s.rooted[root]
		if path == nil {
			return errors.New("snapshot: missing retained root")
		}
		current, err := path.parent.Lstat(path.name)
		if errors.Is(err, os.ErrNotExist) {
			if path.identity != nil {
				return fmt.Errorf("snapshot: refusing rollback of replaced root %q", path.name)
			}
			continue
		}
		if err != nil {
			return err
		}
		if path.identity == nil || !os.SameFile(path.identity, current) || current.Mode().Type() != path.identity.Mode().Type() {
			return fmt.Errorf("snapshot: refusing rollback of replaced root %q", path.name)
		}
	}
	return nil
}

func pathDepth(p string) int { return strings.Count(filepath.Clean(p), string(filepath.Separator)) }
func removeRooted(s *snapshotState, path *rootedPath) error {
	if path == nil {
		return errors.New("snapshot: missing retained root")
	}
	current, err := path.parent.Lstat(path.name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if path.identity == nil {
		return path.parent.Remove(path.name)
	}
	if !os.SameFile(path.identity, current) || current.Mode().Type() != path.identity.Mode().Type() {
		return fmt.Errorf("snapshot: refusing rollback of replaced root %q", path.name)
	}
	if !current.IsDir() {
		return nil
	}
	return removeCreated(s, path)
}

func removeCreated(s *snapshotState, path *rootedPath) error {
	root, err := path.parent.OpenRoot(path.name)
	if err != nil {
		return err
	}
	defer root.Close()
	preserved := make(map[string]string, len(s.entries))
	for _, entry := range s.entries {
		preserved[filepath.Clean(entry.Path)] = entry.Kind
	}
	return removeCreatedDir(root, ".", path.path, preserved)
}

func removeCreatedDir(root *safefs.Root, dir, absolute string, preserved map[string]string) error {
	entries, err := fs.ReadDir(root.FS(), dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := filepath.ToSlash(filepath.Join(dir, entry.Name()))
		absoluteName := filepath.Clean(filepath.Join(absolute, filepath.FromSlash(name)))
		if dir == "." && entry.Name() == "node_modules" {
			continue
		}
		if entry.IsDir() {
			// Existing directories are deliberately not traversed: their pathname
			// may now designate a replacement directory. Rollback fails closed
			// rather than recursively deleting it.
			if preserved[absoluteName] != "dir" {
				if err := removeCreatedDir(root, name, absoluteName, preserved); err != nil {
					return err
				}
			}
		}
		if _, exists := preserved[absoluteName]; exists {
			continue
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func restoreEntry(s *snapshotState, e snapshotEntry) (os.FileInfo, error) {
	basePath, base, rel, err := retainedLocation(s, e.Path)
	if err != nil {
		return nil, err
	}
	_ = basePath
	root := base.parent
	name := base.name
	if rel != "." {
		opened, openErr := root.OpenRoot(name)
		if openErr != nil {
			return nil, openErr
		}
		defer opened.Close()
		root = opened
		name = filepath.ToSlash(rel)
	}
	if e.Kind == "dir" {
		if err := root.MkdirAll(name, 0o700); err != nil {
			return nil, err
		}
		opened, err := root.Open(name)
		if err != nil {
			return nil, err
		}
		info, err := opened.Stat()
		if err == nil && !info.IsDir() {
			err = fmt.Errorf("snapshot: restored directory changed type")
		}
		if err == nil {
			err = opened.Chmod(os.FileMode(e.Mode))
		}
		_ = opened.Close()
		return nil, err
	}
	if e.Kind == "symlink" {
		return nil, root.Symlink(e.SymlinkTarget, name)
	}
	if e.Kind != "file" {
		return nil, nil
	}
	in, err := s.payloadRoot.Open(e.Payload)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	if existing, statErr := root.Open(name); statErr == nil {
		info, infoErr := existing.Stat()
		_ = existing.Close()
		if infoErr != nil {
			return nil, infoErr
		}
		if info.Mode().IsRegular() {
			if !safefs.SingleLink(info) {
				return nil, fmt.Errorf("snapshot: refusing hard-linked restore destination %q", e.Path)
			}
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	var afterRename func() error
	if snapshotHooks.AfterRestoreRename != nil {
		afterRename = func() error {
			return snapshotHooks.AfterRestoreRename(e.Path)
		}
	}
	restoredIdentity, err := safefs.CopyAtomicInfo(root, name, in, os.FileMode(e.Mode), ".skynex-restore-", afterRename)
	if err != nil {
		return nil, err
	}
	return restoredIdentity, nil
}

func retainedLocation(s *snapshotState, path string) (string, *rootedPath, string, error) {
	var best string
	for root := range s.rooted {
		if path == root || isWithin(path, root) {
			if len(root) > len(best) {
				best = root
			}
		}
	}
	if best == "" {
		return "", nil, "", fmt.Errorf("snapshot: path %q is outside retained roots", path)
	}
	rel, err := filepath.Rel(best, path)
	if err != nil {
		return "", nil, "", err
	}
	return best, s.rooted[best], rel, nil
}

func (s *snapshotState) commit() error {
	defer closeSnapshotState(s)
	data, err := safefs.ReadFileVerified(s.snapshotRoot, "manifest.json", maxSnapshotManifestBytes)
	if err != nil {
		return err
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	manifest.Status = snapshotStatusCommitted
	updated, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return safefs.WriteAtomic(s.snapshotRoot, "manifest.json", append(updated, '\n'), 0o600, ".manifest-")
}
