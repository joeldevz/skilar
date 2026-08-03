// Package skillsync reconciles the files owned by Skynex in OpenCode's skills
// directory. A Session retains all trusted roots and the exact source bundle
// observed at construction; Inspect and Apply must use that same session.
package skillsync

import (
	"crypto/sha256"
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

	"github.com/joeldevz/skynex/internal/safefs"
)

const ManifestVersion = 1

const (
	// Skills are human-authored Markdown; four MiB is ample while bounding
	// hashing and reconciliation allocations.
	maxSkillFileBytes int64 = 4 << 20
	// The manifest may enumerate many skills, so it gets a larger but finite
	// cap before JSON decoding.
	maxManifestBytes int64 = 16 << 20
)

type Status string

const (
	Current  Status = "Current"
	Outdated Status = "Outdated"
	Modified Status = "Modified"
	Missing  Status = "Missing"
	Unknown  Status = "Unknown"
	Retired  Status = "Retired"
)

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Version       int    `json:"version"`
	Source        string `json:"source"`
	SourceKind    string `json:"sourceKind"`
	BundleVersion string `json:"bundleVersion"`
	BundleCommit  string `json:"bundleCommit"`
	TreeSHA256    string `json:"treeSHA256"`
	Files         []File `json:"files"`
	Package       string `json:"package,omitempty"`
	Target        string `json:"target,omitempty"`
	GeneratedAt   string `json:"generatedAt,omitempty"`
}

type Entry struct {
	Path             string
	Status           Status
	BundleSHA256     string
	LocalSHA256      string
	Owned            bool
	BundleTreeSHA256 string
}

type Report struct {
	Entries          []Entry
	Manifest         *Manifest
	Legacy           bool
	Adoptable        bool
	BundleTreeSHA256 string
	session          *Session
}

// Session is the descriptor-scoped reconciliation context. Bundle bytes are
// captured once so a source directory cannot change between preview and apply.
type Session struct {
	bundle         fs.FS
	bundleFiles    map[string]string
	bundleData     map[string][]byte
	bundleTree     string
	target         *safefs.Root
	manifestParent *safefs.Root
	manifestName   string
	closed         bool
}

func NewSession(bundle fs.FS, target, manifestPath string) (*Session, error) {
	if bundle == nil {
		return nil, errors.New("bundle is required")
	}
	files, data, err := bundleSnapshot(bundle)
	if err != nil {
		return nil, err
	}
	if err := validateRoot(target); err != nil {
		return nil, err
	}
	targetRoot, err := safefs.OpenOrCreate(target, 0o700)
	if err != nil {
		return nil, err
	}
	parent, err := safefs.OpenOrCreate(filepath.Dir(manifestPath), 0o700)
	if err != nil {
		_ = targetRoot.Close()
		return nil, err
	}
	return &Session{bundle: bundle, bundleFiles: files, bundleData: data, bundleTree: treeHash(filesToManifest(files)), target: targetRoot, manifestParent: parent, manifestName: filepath.Base(manifestPath)}, nil
}

func (s *Session) Close() error {
	var errs []error
	if s.target != nil {
		errs = append(errs, s.target.Close())
		s.target = nil
	}
	if s.manifestParent != nil {
		errs = append(errs, s.manifestParent.Close())
		s.manifestParent = nil
	}
	s.closed = true
	return errors.Join(errs...)
}

// Close releases the retained roots captured by Inspect. Apply must happen
// before Close; callers should always close a report when reconciliation ends.
func (r *Report) Close() error {
	if r == nil || r.session == nil {
		return nil
	}
	return r.session.Close()
}

type Decision string

const (
	Keep    Decision = "keep"
	Replace Decision = "replace"
)

var ErrChangedSinceDecision = errors.New("skills changed since decision")

const specialHash = "<unsupported-local-entry>"

// Test-only seam used to exercise final-entry replacement races.
var beforeSkillsMutation func() error

// BindDecision records the exact observation a human approved. Decisions
// without these bindings are rejected for destructive reconciliation.
func BindDecision(path string, decision Decision, localHash, bundleHash, treeHash string) Decision {
	return Decision(fmt.Sprintf("%s|path=%s|local=%s|bundle=%s|tree=%s", decision, path, localHash, bundleHash, treeHash))
}

func decisionMatches(raw Decision, e Entry) bool {
	parts := strings.Split(string(raw), "|")
	if len(parts) != 5 || (parts[0] != string(Keep) && parts[0] != string(Replace)) {
		return false
	}
	want := map[string]string{}
	for _, part := range parts[1:] {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return false
		}
		want[kv[0]] = kv[1]
	}
	return want["path"] == e.Path && want["local"] == e.LocalSHA256 && want["bundle"] == e.BundleSHA256 && want["tree"] == e.BundleTreeSHA256
}

func decisionValue(raw Decision) Decision { return Decision(strings.SplitN(string(raw), "|", 2)[0]) }

// Inspect compares bundle (the exact fs.FS that will be installed) with target.
func Inspect(bundle fs.FS, target, manifestPath string) (Report, error) {
	s, err := NewSession(bundle, target, manifestPath)
	if err != nil {
		return Report{}, err
	}
	r, err := s.Inspect()
	if err != nil {
		_ = s.Close()
		return Report{}, err
	}
	r.session = s
	return r, nil
}

func (s *Session) Inspect() (Report, error) {
	if s == nil || s.closed {
		return Report{}, errors.New("skills session is closed")
	}
	manifest, legacy, err := s.readManifest()
	if err != nil {
		return Report{}, err
	}
	local, err := localFilesRoot(s.target)
	if err != nil {
		return Report{}, err
	}
	files := s.bundleFiles
	owned := map[string]File{}
	if manifest != nil {
		for _, f := range manifest.Files {
			owned[f.Path] = f
		}
	}
	paths := map[string]bool{}
	for p := range files {
		paths[p] = true
	}
	for p := range local {
		paths[p] = true
	}
	for p := range owned {
		paths[p] = true
	}
	ordered := make([]string, 0, len(paths))
	for p := range paths {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	r := Report{Manifest: manifest, Legacy: legacy, session: s}
	r.BundleTreeSHA256 = s.bundleTree
	allExact := true
	for _, p := range ordered {
		b, bok := files[p]
		l, lok := local[p]
		old, ownedHere := owned[p]
		e := Entry{Path: p, BundleSHA256: b, LocalSHA256: l, Owned: ownedHere}
		switch {
		case lok && l == specialHash:
			e.Status = Unknown
			allExact = false
		case !lok && bok:
			e.Status = Missing
			allExact = false
		case !bok && ownedHere:
			if lok && l == old.SHA256 {
				e.Status = Retired
			} else if lok {
				e.Status = Modified
			} else {
				e.Status = Missing
			}
			allExact = false
		case !bok && lok:
			e.Status = Unknown
			allExact = false
		case legacy:
			if l == b {
				e.Status = Current
			} else {
				e.Status = Unknown
				allExact = false
			}
		default:
			switch {
			case l == old.SHA256 && b == old.SHA256:
				e.Status = Current
			case l == old.SHA256:
				e.Status = Outdated
			case l == b && !ownedHere:
				e.Status = Unknown
			default:
				e.Status = Modified
			}
			if e.Status != Current {
				allExact = false
			}
		}
		e.BundleTreeSHA256 = r.BundleTreeSHA256
		r.Entries = append(r.Entries, e)
	}
	r.Adoptable = legacy && allExact
	return r, nil
}

// Apply reconciles only owned files, preserving Unknown and requiring decisions
// to be made against the hashes observed by Inspect.
func Apply(bundle fs.FS, target, manifestPath string, report Report, decisions map[string]Decision, metadata Manifest) error {
	_ = bundle
	_ = target
	_ = manifestPath
	if report.session == nil {
		return errors.New("apply requires the retained session returned by Inspect")
	}
	return report.session.Apply(report, decisions, metadata)
}

func (s *Session) Apply(report Report, decisions map[string]Decision, metadata Manifest) error {
	if s == nil || s.closed || report.session != s {
		return errors.New("apply requires the retained session returned by Inspect")
	}
	currentBundle, _, err := bundleSnapshot(s.bundle)
	if err != nil {
		return err
	}
	if treeHash(filesToManifest(currentBundle)) != s.bundleTree {
		return fmt.Errorf("%w: bundle source", ErrChangedSinceDecision)
	}
	current, err := s.Inspect()
	if err != nil {
		return err
	}
	if current.BundleTreeSHA256 != report.BundleTreeSHA256 {
		return fmt.Errorf("%w: bundle tree", ErrChangedSinceDecision)
	}
	if current.Manifest != nil && (metadata.Source == "" || metadata.SourceKind == "" || metadata.BundleVersion == "" || metadata.Package == "" || metadata.Target == "") {
		return errors.New("ownership manifest update requires complete source metadata")
	}
	if current.Manifest != nil && (current.Manifest.Source != metadata.Source || current.Manifest.SourceKind != metadata.SourceKind || current.Manifest.BundleVersion != metadata.BundleVersion || current.Manifest.BundleCommit != metadata.BundleCommit || current.Manifest.Package != metadata.Package || current.Manifest.Target != metadata.Target) {
		return errors.New("ownership manifest provenance does not match current source")
	}
	for _, e := range report.Entries {
		if got := findEntry(current, e.Path); got.LocalSHA256 != e.LocalSHA256 || got.BundleSHA256 != e.BundleSHA256 || got.BundleTreeSHA256 != e.BundleTreeSHA256 || got.Status != e.Status {
			return fmt.Errorf("%w: %s", ErrChangedSinceDecision, e.Path)
		}
	}
	targetRoot := s.target
	for _, e := range report.Entries {
		if e.Status != Missing && e.Status != Outdated && e.Status != Modified && e.Status != Retired {
			continue
		}
		boundDecision := decisionMatches(decisions[e.Path], e)
		choice := decisionValue(decisions[e.Path])
		if (e.Status == Modified || e.Status == Retired) && !boundDecision {
			continue
		}
		if e.Status == Retired && choice != Replace {
			continue
		}
		if e.Status == Modified && choice != Replace {
			continue
		}
		if e.BundleSHA256 == "" {
			if e.Status == Modified && choice != Replace {
				continue
			}
			if err := removeContained(targetRoot, e.Path); err != nil {
				return err
			}
			continue
		}
		if e.Status == Missing && e.BundleSHA256 == "" && !e.Owned {
			continue
		}
		if e.Status == Missing && e.BundleSHA256 == "" {
			if e.Owned {
				if err := removeContained(targetRoot, e.Path); err != nil {
					return err
				}
			}
			continue
		}
		if err := copyBundleData(s.bundleData, targetRoot, e.Path, e.BundleSHA256); err != nil {
			return err
		}
	}
	metadata.Version = ManifestVersion
	metadata.Files = filesToManifest(s.bundleFiles)
	metadata.TreeSHA256 = manifestTreeHash(metadata)
	if metadata.SourceKind == "" {
		metadata.SourceKind = "embedded"
	}
	return s.writeManifest(metadata)
}

func findEntry(r Report, path string) Entry {
	for _, e := range r.Entries {
		if e.Path == path {
			return e
		}
	}
	return Entry{Path: path}
}

func bundleFiles(f fs.FS) (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(f, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return fmt.Errorf("bundle contains unsupported file %q", p)
		}
		b, e := readFSFileVerified(f, p)
		if e != nil {
			return e
		}
		clean, e := cleanPath(p)
		if e != nil {
			return e
		}
		out[clean] = hashBytes(b)
		return nil
	})
	return out, err
}

func bundleSnapshot(f fs.FS) (map[string]string, map[string][]byte, error) {
	files, data := map[string]string{}, map[string][]byte{}
	err := fs.WalkDir(f, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return fmt.Errorf("bundle contains unsupported file %q", p)
		}
		clean, err := cleanPath(p)
		if err != nil {
			return err
		}
		b, err := readFSFileVerified(f, clean)
		if err != nil {
			return err
		}
		files[clean], data[clean] = hashBytes(b), append([]byte(nil), b...)
		return nil
	})
	return files, data, err
}

func filesToManifest(m map[string]string) []File {
	out := make([]File, 0, len(m))
	for p, h := range m {
		out = append(out, File{Path: p, SHA256: h})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
func bundleManifestFiles(f fs.FS) ([]File, error) {
	m, err := bundleFiles(f)
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, len(m))
	for p, h := range m {
		out = append(out, File{p, h})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func localFiles(root string) (map[string]string, error) {
	out := map[string]string{}
	if err := validateRoot(root); err != nil {
		return nil, err
	}
	targetRoot, err := safefs.Open(root)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer targetRoot.Close()
	return localFilesRoot(targetRoot)
}

func localFilesRoot(targetRoot *safefs.Root) (map[string]string, error) {
	out := map[string]string{}
	info, err := targetRoot.Open(".")
	if err != nil {
		return nil, err
	}
	stat, err := info.Stat()
	_ = info.Close()
	if err != nil || !stat.IsDir() {
		return nil, fmt.Errorf("skills target is not a directory")
	}
	err = fs.WalkDir(targetRoot.FS(), ".", func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == "." {
			return nil
		}
		clean, e := cleanPath(filepath.ToSlash(p))
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			out[clean] = specialHash
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			out[clean] = specialHash
			return nil
		}
		b, e := safefs.ReadFileVerified(targetRoot, clean, maxSkillFileBytes)
		if e != nil {
			return fmt.Errorf("local entry changed while reading %q: %w", clean, e)
		}
		out[clean] = hashBytes(b)
		return nil
	})
	return out, err
}

func readFSFileVerified(f fs.FS, name string) ([]byte, error) {
	before, err := fs.Stat(f, name)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("bundle contains unsupported file %q", name)
	}
	if !safefs.SingleLink(before) {
		return nil, fmt.Errorf("refusing hard-linked source entry %q", name)
	}
	opened, err := f.Open(name)
	if err != nil {
		return nil, err
	}
	b, readErr := io.ReadAll(io.LimitReader(opened, maxSkillFileBytes+1))
	stat, statErr := opened.Stat()
	closeErr := opened.Close()
	if readErr != nil || statErr != nil || closeErr != nil || !os.SameFile(before, stat) || stat.Mode().Type() != before.Mode().Type() {
		return nil, fmt.Errorf("bundle file changed while reading %q", name)
	}
	if !safefs.SingleLink(stat) {
		return nil, fmt.Errorf("refusing hard-linked source entry %q", name)
	}
	if int64(len(b)) > maxSkillFileBytes {
		return nil, fmt.Errorf("bundle file %q exceeds maximum size of %d bytes", name, maxSkillFileBytes)
	}
	return b, nil
}

func cleanPath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" || p == "." || strings.HasPrefix(p, "/") || filepath.IsAbs(p) || p != pathCleanSlash(p) || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "//") {
		return "", fmt.Errorf("invalid relative path %q", p)
	}
	return p, nil
}
func pathCleanSlash(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(p))), "./")
}
func validateRoot(root string) error {
	if root == "" || !filepath.IsAbs(root) || root != filepath.Clean(root) {
		return fmt.Errorf("invalid target %q", root)
	}
	r, err := safefs.Open(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("invalid target root: %w", err)
	}
	if r != nil {
		_ = r.Close()
	}
	return nil
}
func removeContained(root *safefs.Root, rel string) error {
	clean, e := safefs.Relative(rel)
	if e != nil {
		return e
	}
	opened, e := root.Open(clean)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		// os.Root refuses to follow symlinks; remove the link itself after the
		// failed open rather than inspecting it through the ambient namespace.
		return root.Remove(clean)
	}
	i, e := opened.Stat()
	_ = opened.Close()
	if e != nil {
		return e
	}
	if i.Mode().IsRegular() {
		if !safefs.SingleLink(i) {
			return fmt.Errorf("refusing hard-linked destination %q", clean)
		}
	}
	if i.IsDir() {
		return root.RemoveAll(clean)
	}
	return root.Remove(clean)
}
func copyBundleFile(bundle fs.FS, target *safefs.Root, path, expectedHash string) error {
	clean, e := safefs.Relative(path)
	if e != nil {
		return e
	}
	data, e := readFSFileVerified(bundle, clean)
	if e != nil {
		return e
	}
	if expectedHash == "" || hashBytes(data) != expectedHash {
		return fmt.Errorf("source hash changed before mutation")
	}
	latest, err := readFSFileVerified(bundle, clean)
	if err != nil || hashBytes(latest) != expectedHash {
		return fmt.Errorf("source changed during reconciliation")
	}
	if info, err := target.Lstat(clean); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink destination %q", clean)
		}
		if info.Mode().IsRegular() {
			if !safefs.SingleLink(info) {
				return fmt.Errorf("refusing hard-linked destination %q", clean)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return safefs.WriteAtomic(target, clean, data, 0o600, ".skillsync-")
}

func rejectDestination(path string) error {
	i, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if i.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink destination %q", path)
	}
	if i.Mode().IsRegular() {
		if !safefs.SingleLink(i) {
			return fmt.Errorf("refusing hard-linked destination %q", path)
		}
	}
	return nil
}

func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func treeHash(files []File) string {
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s\x00%s\n", f.Path, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TreeHash returns the stable SHA-256 for a sorted manifest file list.
func TreeHash(files []File) string { return treeHash(files) }

func readManifest(path string) (*Manifest, bool, error) {
	parent, err := safefs.Open(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer parent.Close()
	name := filepath.Base(path)
	if info, err := parent.Lstat(name); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("ownership manifest is a symlink")
		}
		if info.Mode().IsRegular() {
			if !safefs.SingleLink(info) {
				return nil, false, fmt.Errorf("ownership manifest is hard-linked")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	b, e := safefs.ReadFileVerified(parent, name, maxManifestBytes)
	if errors.Is(e, os.ErrNotExist) {
		return nil, true, nil
	}
	if e != nil {
		return nil, false, e
	}
	var m Manifest
	if e = json.Unmarshal(b, &m); e != nil {
		return nil, false, fmt.Errorf("malformed ownership manifest: %w", e)
	}
	if e = validateManifest(m); e != nil {
		return nil, false, e
	}
	return &m, false, nil
}

func validateManifest(m Manifest) error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("unsupported ownership manifest version %d", m.Version)
	}
	if m.Source == "" || m.SourceKind == "" || m.BundleVersion == "" || m.Package == "" || m.Target == "" {
		return errors.New("ownership manifest missing source metadata")
	}
	if !isHash(m.TreeSHA256) {
		return errors.New("invalid ownership manifest tree hash")
	}
	seen := make(map[string]struct{}, len(m.Files))
	for i := range m.Files {
		clean, err := cleanPath(m.Files[i].Path)
		if err != nil || clean != m.Files[i].Path {
			return fmt.Errorf("invalid manifest path %q", m.Files[i].Path)
		}
		if _, ok := seen[clean]; ok {
			return fmt.Errorf("duplicate manifest path %q", clean)
		}
		seen[clean] = struct{}{}
		if !isHash(m.Files[i].SHA256) {
			return fmt.Errorf("invalid hash for %q", clean)
		}
	}
	if manifestTreeHash(m) != m.TreeSHA256 {
		return errors.New("ownership manifest tree hash mismatch")
	}
	return nil
}

func manifestTreeHash(m Manifest) string {
	h := sha256.New()
	fmt.Fprintf(h, "source\x00%s\nsourceKind\x00%s\nbundleVersion\x00%s\nbundleCommit\x00%s\npackage\x00%s\ntarget\x00%s\n", m.Source, m.SourceKind, m.BundleVersion, m.BundleCommit, m.Package, m.Target)
	files := append([]File(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%s\n", f.Path, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func isHash(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func writeManifest(path string, m Manifest) error {
	if path == "" {
		return errors.New("manifest path is required")
	}
	parent := filepath.Dir(path)
	root, err := safefs.OpenOrCreate(parent, 0o700)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := validateManifest(m); err != nil {
		return err
	}
	name := filepath.Base(path)
	if info, err := root.Lstat(name); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("ownership manifest is a symlink")
		}
		if info.Mode().IsRegular() {
			if !safefs.SingleLink(info) {
				return errors.New("ownership manifest is hard-linked")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, e := json.MarshalIndent(m, "", "  ")
	if e != nil {
		return e
	}
	b = append(b, '\n')
	return safefs.WriteAtomic(root, name, b, 0o600, ".skills-ownership-")
}

func (s *Session) readManifest() (*Manifest, bool, error) {
	b, err := safefs.ReadFileVerified(s.manifestParent, s.manifestName, maxManifestBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false, fmt.Errorf("malformed ownership manifest: %w", err)
	}
	if err := validateManifest(m); err != nil {
		return nil, false, err
	}
	return &m, false, nil
}

func (s *Session) writeManifest(m Manifest) error {
	if err := validateManifest(m); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return safefs.WriteAtomic(s.manifestParent, s.manifestName, append(b, '\n'), 0o600, ".skills-ownership-")
}

func copyBundleData(bundle map[string][]byte, target *safefs.Root, path, expectedHash string) error {
	clean, err := safefs.Relative(path)
	if err != nil {
		return err
	}
	data, ok := bundle[clean]
	if !ok || expectedHash == "" || hashBytes(data) != expectedHash {
		return errors.New("source hash changed before mutation")
	}
	if beforeSkillsMutation != nil {
		if err := beforeSkillsMutation(); err != nil {
			return err
		}
	}
	return safefs.WriteAtomic(target, clean, data, 0o600, ".skillsync-")
}
