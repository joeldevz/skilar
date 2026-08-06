package gitcandidate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type ContextSeal struct {
	RepositoryRoot string
	GitCommonDir   string
	WorktreeID     string
	SymbolicHEAD   string
	Detached       bool
	BaseRef        string
	BaseCommitOID  string
	BaseTreeOID    string
	ObjectFormat   string
}

type Policy struct {
	// IncludeIgnored contains repository-relative ignored paths explicitly selected
	// as deliverable inputs. Ignored paths are excluded otherwise.
	IncludeIgnored []string
}

func (p Policy) Hash() string {
	paths := append([]string(nil), p.IncludeIgnored...)
	sort.Strings(paths)
	sum := sha256.Sum256([]byte("gitcandidate-policy-v1\x00" + strings.Join(paths, "\x00")))
	return hex.EncodeToString(sum[:])
}

type ManifestEntry struct {
	Path string
	Mode string
	Kind string
	OID  string
}

type Candidate struct {
	Seal       ContextSeal
	TreeOID    string
	Manifest   []ManifestEntry
	PolicyHash string
}

type Drift struct {
	Worktree bool
	HEAD     bool
	BaseRef  bool
	Reasons  []string
}

var ErrAmbiguousIndex = errors.New("gitcandidate: user index contains staged-only or unmerged state")

func (d Drift) Any() bool { return d.Worktree || d.HEAD || d.BaseRef }

func CaptureContext(repo string) (ContextSeal, error) {
	root, err := gitText(repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return ContextSeal{}, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return ContextSeal{}, err
	}
	gitDir, err := gitText(root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return ContextSeal{}, err
	}
	common, err := gitText(root, "rev-parse", "--git-common-dir")
	if err != nil {
		return ContextSeal{}, err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	common, err = filepath.Abs(common)
	if err != nil {
		return ContextSeal{}, err
	}
	commit, err := gitText(root, "rev-parse", "HEAD")
	if err != nil {
		return ContextSeal{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	tree, err := gitText(root, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return ContextSeal{}, err
	}
	format, err := gitText(root, "rev-parse", "--show-object-format")
	if err != nil {
		return ContextSeal{}, err
	}
	ref, refErr := gitText(root, "symbolic-ref", "-q", "HEAD")
	detached := refErr != nil
	if detached {
		ref = ""
	}
	return ContextSeal{RepositoryRoot: root, GitCommonDir: common, WorktreeID: filepath.Clean(gitDir), SymbolicHEAD: ref, Detached: detached, BaseRef: ref, BaseCommitOID: commit, BaseTreeOID: tree, ObjectFormat: format}, nil
}

func Freeze(seal ContextSeal, policy Policy) (Candidate, error) {
	if err := validateContextIdentity(seal); err != nil {
		return Candidate{}, err
	}
	if err := rejectAmbiguousUserIndex(seal); err != nil {
		return Candidate{}, err
	}
	index, err := os.CreateTemp("", "skynex-index-*")
	if err != nil {
		return Candidate{}, err
	}
	indexPath := index.Name()
	if err = index.Close(); err != nil {
		return Candidate{}, err
	}
	_ = os.Remove(indexPath)
	defer os.Remove(indexPath)
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if _, err = gitRun(seal.RepositoryRoot, env, "read-tree", seal.BaseTreeOID); err != nil {
		return Candidate{}, err
	}
	// First overlay every tracked worktree edit/deletion, including tracked export paths.
	if _, err = gitRun(seal.RepositoryRoot, env, "add", "-u", "--", "."); err != nil {
		return Candidate{}, err
	}
	// Then add eligible untracked files. Generated exports remain out of candidate
	// scope unless they were already tracked by the base tree. Avoid naming children
	// of a wholly ignored .skynex directory: Git treats those negative pathspecs as
	// an explicit request for the ignored parent and rejects the entire add.
	addArgs := []string{"add", "--all", "--", "."}
	skIgnore, ignoreErr := pathIgnored(seal.RepositoryRoot, ".skynex")
	if ignoreErr != nil {
		return Candidate{}, ignoreErr
	}
	if !skIgnore {
		addArgs = append(addArgs, ":(exclude).skynex/exports/**", ":(exclude).skynex/receipts/**")
	}
	if _, err = gitRun(seal.RepositoryRoot, env, addArgs...); err != nil {
		return Candidate{}, err
	}
	for _, path := range policy.IncludeIgnored {
		clean, err := cleanRelative(path)
		if err != nil {
			return Candidate{}, err
		}
		if _, err = gitRun(seal.RepositoryRoot, env, "add", "-f", "--", clean); err != nil {
			return Candidate{}, fmt.Errorf("include ignored %q: %w", clean, err)
		}
	}
	tree, err := gitTextEnv(seal.RepositoryRoot, env, "write-tree")
	if err != nil {
		return Candidate{}, err
	}
	manifest, err := readManifest(seal.RepositoryRoot, tree)
	if err != nil {
		return Candidate{}, err
	}
	return Candidate{Seal: seal, TreeOID: tree, Manifest: manifest, PolicyHash: policy.Hash()}, nil
}

func rejectAmbiguousUserIndex(seal ContextSeal) error {
	unmerged, err := gitBytes(seal.RepositoryRoot, nil, "ls-files", "-u")
	if err != nil {
		return err
	}
	if len(unmerged) != 0 {
		return ErrAmbiguousIndex
	}
	staged, err := changedPaths(seal.RepositoryRoot, "diff", "--cached", "--name-only", "-z", seal.BaseCommitOID, "--")
	if err != nil {
		return err
	}
	unstaged, err := changedPaths(seal.RepositoryRoot, "diff", "--name-only", "-z", "--")
	if err != nil {
		return err
	}
	for path := range staged {
		if _, differsFromIndex := unstaged[path]; differsFromIndex {
			return fmt.Errorf("%w: %s", ErrAmbiguousIndex, path)
		}
	}
	return nil
}

func changedPaths(repo string, args ...string) (map[string]struct{}, error) {
	out, err := gitBytes(repo, nil, args...)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]struct{})
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) != 0 {
			paths[string(raw)] = struct{}{}
		}
	}
	return paths, nil
}

func DetectDrift(candidate Candidate, policy Policy) (Drift, error) {
	var drift Drift
	current, err := CaptureContext(candidate.Seal.RepositoryRoot)
	if err != nil {
		return drift, err
	}
	if current.Detached != candidate.Seal.Detached || current.SymbolicHEAD != candidate.Seal.SymbolicHEAD {
		drift.HEAD = true
		drift.Reasons = append(drift.Reasons, "HEAD identity changed")
	}
	if current.BaseCommitOID != candidate.Seal.BaseCommitOID {
		drift.BaseRef = true
		drift.Reasons = append(drift.Reasons, "base ref moved")
	}
	if current.WorktreeID != candidate.Seal.WorktreeID || current.GitCommonDir != candidate.Seal.GitCommonDir {
		drift.HEAD = true
		drift.Reasons = append(drift.Reasons, "repository or worktree identity changed")
	}
	// Freeze against the original seal only while its ref context still matches.
	if !drift.HEAD && !drift.BaseRef {
		currentCandidate, freezeErr := Freeze(candidate.Seal, policy)
		if freezeErr != nil {
			return drift, freezeErr
		}
		if currentCandidate.TreeOID != candidate.TreeOID || currentCandidate.PolicyHash != candidate.PolicyHash {
			drift.Worktree = true
			drift.Reasons = append(drift.Reasons, "candidate-scoped worktree changed")
		}
	}
	return drift, nil
}

func validateContextIdentity(seal ContextSeal) error {
	current, err := CaptureContext(seal.RepositoryRoot)
	if err != nil {
		return err
	}
	if current.WorktreeID != seal.WorktreeID || current.GitCommonDir != seal.GitCommonDir || current.Detached != seal.Detached || current.SymbolicHEAD != seal.SymbolicHEAD || current.BaseCommitOID != seal.BaseCommitOID || current.BaseTreeOID != seal.BaseTreeOID {
		return errors.New("gitcandidate: repository context seal drifted")
	}
	return nil
}

func readManifest(repo, tree string) ([]ManifestEntry, error) {
	out, err := gitBytes(repo, nil, "ls-tree", "-rz", tree)
	if err != nil {
		return nil, err
	}
	records := bytes.Split(out, []byte{0})
	entries := make([]ManifestEntry, 0, len(records))
	for _, record := range records {
		if len(record) == 0 {
			continue
		}
		tab := bytes.IndexByte(record, '\t')
		if tab < 0 {
			return nil, errors.New("gitcandidate: malformed ls-tree output")
		}
		meta := strings.Fields(string(record[:tab]))
		if len(meta) != 3 {
			return nil, errors.New("gitcandidate: malformed ls-tree metadata")
		}
		entries = append(entries, ManifestEntry{Path: string(record[tab+1:]), Mode: meta[0], Kind: meta[1], OID: meta[2]})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func cleanRelative(path string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(path) {
		return "", fmt.Errorf("gitcandidate: unsafe path %q", path)
	}
	return clean, nil
}
func gitText(repo string, args ...string) (string, error) {
	out, err := gitBytes(repo, nil, args...)
	return strings.TrimSpace(string(out)), err
}
func gitTextEnv(repo string, env []string, args ...string) (string, error) {
	out, err := gitBytes(repo, env, args...)
	return strings.TrimSpace(string(out)), err
}
func gitRun(repo string, env []string, args ...string) ([]byte, error) {
	return gitBytes(repo, env, args...)
}

func pathIgnored(repo, path string) (bool, error) {
	cmd := exec.Command("git", "check-ignore", "-q", "--no-index", "--", path)
	cmd.Dir = repo
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git check-ignore %s: %w", path, err)
}

func gitBytes(repo string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
