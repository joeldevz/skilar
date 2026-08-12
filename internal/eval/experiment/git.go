package experiment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joeldevz/skynex/internal/eval/contracts"
)

const (
	gitInspectionTimeout = 30 * time.Second
	gitHeadMaxBytes      = 1 << 10
	gitStatusMaxBytes    = 8 << 20
	gitPatchMaxBytes     = 64 << 20
	gitStderrMaxBytes    = 64 << 10
)

// GitProvenance is the exact, read-only Git observation for a frozen bundle.
// DirtyPatchDigest is intentionally empty for a clean worktree.
type GitProvenance struct {
	GitSHA           string
	DirtyPatchDigest string
	Dirty            bool
}

// InspectGitBundle resolves Git from the ambient PATH at call time and observes
// HEAD and the complete tracked/untracked status for root. This wrapper exists
// for callers that have not established an executable closure; callers making
// a reproducibility or provenance decision must resolve and pin Git themselves
// and call InspectGitBundleWithExecutable instead.
func InspectGitBundle(root string) (GitProvenance, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return GitProvenance{}, fmt.Errorf("resolve git executable: %w", err)
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		return GitProvenance{}, fmt.Errorf("resolve absolute git executable: %w", err)
	}
	// Preserve the wrapper's historical ability to use a Git selected through
	// a symlink while passing only a non-symlink path to the strict API.
	gitPath, err = filepath.EvalSymlinks(gitPath)
	if err != nil {
		return GitProvenance{}, fmt.Errorf("canonicalize git executable: %w", err)
	}
	return InspectGitBundleWithExecutable(root, gitPath)
}

// InspectGitBundleWithExecutable observes Git provenance using exactly
// gitExecutable; it never resolves the executable through PATH. gitExecutable
// must be a clean absolute path whose final component is a non-symlink regular
// executable. Callers that pin executable contents must revalidate that pin
// immediately before and after this call.
//
// The inspection does not invoke hooks, pagers, text converters, external diff
// drivers, credential prompts, or ambient system/global Git configuration. Git
// is run with optional locks disabled so this operation does not refresh the
// index.
func InspectGitBundleWithExecutable(root, gitExecutable string) (GitProvenance, error) {
	gitPath, err := validateGitExecutablePath(gitExecutable)
	if err != nil {
		return GitProvenance{}, fmt.Errorf("validate git executable: %w", err)
	}

	root, err = filepath.Abs(root)
	if err != nil {
		return GitProvenance{}, fmt.Errorf("resolve git bundle root: %w", err)
	}
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return GitProvenance{}, fmt.Errorf("stat git bundle root: %w", err)
	}
	if !info.IsDir() {
		return GitProvenance{}, fmt.Errorf("git bundle root is not a directory")
	}

	headBytes, err := runGitInspection(gitPath, root, gitHeadMaxBytes,
		"rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return GitProvenance{}, fmt.Errorf("inspect git HEAD: %w", err)
	}
	head := strings.TrimSuffix(string(headBytes), "\n")
	head = strings.TrimSuffix(head, "\r")
	if !gitSHAPattern.MatchString(head) {
		return GitProvenance{}, fmt.Errorf("inspect git HEAD: unexpected object id")
	}

	status, err := runGitInspection(gitPath, root, gitStatusMaxBytes,
		"status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=no", "--", ".")
	if err != nil {
		return GitProvenance{}, fmt.Errorf("inspect git status: %w", err)
	}
	patch, err := runGitInspection(gitPath, root, gitPatchMaxBytes,
		"diff", "--binary", "--full-index", "--no-color", "--no-ext-diff", "--no-textconv", "HEAD", "--", ".")
	if err != nil {
		return GitProvenance{}, fmt.Errorf("inspect git patch: %w", err)
	}

	result := GitProvenance{GitSHA: head, Dirty: len(status) != 0 || len(patch) != 0}
	if !result.Dirty {
		return result, nil
	}
	result.DirtyPatchDigest, err = contracts.CanonicalDigest(struct {
		StatusDigest string `json:"status_digest"`
		PatchDigest  string `json:"patch_digest"`
	}{
		StatusDigest: bytesDigest(status),
		PatchDigest:  bytesDigest(patch),
	})
	if err != nil {
		return GitProvenance{}, fmt.Errorf("digest git status and patch: %w", err)
	}
	return result, nil
}

func validateGitExecutablePath(path string) (string, error) {
	if path == "" || strings.TrimSpace(path) != path || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("path is required, must not contain NUL, and must not have surrounding whitespace")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute: %q", path)
	}
	if clean := filepath.Clean(path); clean != path {
		return "", fmt.Errorf("path must be clean: got %q, clean form is %q", path, clean)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%q is a symbolic link", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%q is not executable", path)
	}
	return path, nil
}

func verifyBundleGitProvenance(root string, expected FrozenBundle) error {
	ownedRepository, err := hasOwnedGitMetadata(root)
	if err != nil {
		return err
	}
	declared := expected.GitSHA != "" || expected.DirtyPatchDigest != ""
	if !ownedRepository && !declared {
		return nil
	}
	if ownedRepository && expected.GitSHA == "" {
		return fmt.Errorf("git_sha is required for a bundle rooted at a Git repository")
	}

	actual, err := InspectGitBundle(root)
	if err != nil {
		return err
	}
	if actual.GitSHA != expected.GitSHA {
		return fmt.Errorf("git_sha mismatch: got %s, expected %s", actual.GitSHA, expected.GitSHA)
	}
	if actual.Dirty {
		if expected.DirtyPatchDigest == "" {
			return fmt.Errorf("dirty_patch_digest is required for a dirty Git bundle")
		}
		if actual.DirtyPatchDigest != expected.DirtyPatchDigest {
			return fmt.Errorf("dirty_patch_digest mismatch: got %s, expected %s", actual.DirtyPatchDigest, expected.DirtyPatchDigest)
		}
		return nil
	}
	if expected.DirtyPatchDigest != "" {
		return fmt.Errorf("dirty_patch_digest is declared for a clean Git bundle")
	}
	return nil
}

func hasOwnedGitMetadata(root string) (bool, error) {
	info, err := os.Lstat(filepath.Join(root, ".git"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect bundle Git metadata: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("bundle Git metadata must not be a symbolic link")
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return false, fmt.Errorf("bundle Git metadata must be a directory or regular gitfile")
	}
	return true, nil
}

func runGitInspection(gitPath, root string, stdoutLimit int64, commandArgs ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitInspectionTimeout)
	defer cancel()

	args := []string{
		"--no-pager",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false",
		"-c", "core.attributesFile=" + os.DevNull,
		"-c", "core.excludesFile=" + os.DevNull,
		"-c", "color.ui=false",
		"-c", "maintenance.auto=false",
		"-C", root,
	}
	args = append(args, commandArgs...)
	cmd := exec.CommandContext(ctx, gitPath, args...)
	cmd.Dir = root
	cmd.Env = gitInspectionEnvironment()
	configureGitProcess(cmd)

	limitReached := make(chan struct{}, 1)
	stdout := newGitBoundedBuffer(stdoutLimit, limitReached)
	stderr := newGitBoundedBuffer(gitStderrMaxBytes, limitReached)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start git: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-limitReached:
		cancel()
		_ = terminateGitProcess(cmd.Process.Pid)
		<-waitCh
		return nil, fmt.Errorf("git output exceeds bounded limit")
	case <-ctx.Done():
		_ = terminateGitProcess(cmd.Process.Pid)
		<-waitCh
		return nil, fmt.Errorf("git inspection timed out after %s", gitInspectionTimeout)
	}
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return nil, fmt.Errorf("git exited unsuccessfully: %w", waitErr)
		}
		return nil, fmt.Errorf("git exited unsuccessfully: %w: %s", waitErr, strconv.QuoteToASCII(message))
	}
	if stdout.Truncated() || stderr.Truncated() {
		return nil, fmt.Errorf("git output exceeds bounded limit")
	}
	return stdout.Bytes(), nil
}

func gitInspectionEnvironment() []string {
	values := map[string]string{
		"GIT_ATTR_NOSYSTEM":   "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_FLUSH":           "1",
		"GIT_LFS_SKIP_SMUDGE": "1",
		"GIT_NO_LAZY_FETCH":   "1",
		"GIT_OPTIONAL_LOCKS":  "0",
		"GIT_PAGER":           "",
		"GIT_TERMINAL_PROMPT": "0",
		"GCM_INTERACTIVE":     "Never",
		"LANG":                "C",
		"LC_ALL":              "C",
		"PAGER":               "",
		"TZ":                  "UTC",
	}
	for _, key := range []string{"PATH", "SYSTEMROOT", "WINDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	if runtime.GOOS == "windows" {
		for _, key := range []string{"SystemRoot", "WinDir"} {
			if value, ok := os.LookupEnv(key); ok {
				values[key] = value
			}
		}
	}
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	return env
}

func bytesDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

type gitBoundedBuffer struct {
	data      bytes.Buffer
	maxBytes  int64
	truncated bool
	limitOnce sync.Once
	limitCh   chan<- struct{}
}

func newGitBoundedBuffer(maxBytes int64, limitCh chan<- struct{}) *gitBoundedBuffer {
	return &gitBoundedBuffer{maxBytes: maxBytes, limitCh: limitCh}
}

func (b *gitBoundedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := b.maxBytes - int64(b.data.Len())
	if remaining > 0 {
		keep := int64(len(data))
		if keep > remaining {
			keep = remaining
		}
		_, _ = b.data.Write(data[:int(keep)])
	}
	if int64(len(data)) > remaining {
		b.truncated = true
		b.limitOnce.Do(func() {
			select {
			case b.limitCh <- struct{}{}:
			default:
			}
		})
	}
	return written, nil
}

func (b *gitBoundedBuffer) Bytes() []byte {
	return append([]byte(nil), b.data.Bytes()...)
}

func (b *gitBoundedBuffer) String() string {
	return b.data.String()
}

func (b *gitBoundedBuffer) Truncated() bool {
	return b.truncated
}
