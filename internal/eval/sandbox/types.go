// Package sandbox provides the trusted-local execution primitive used by the
// evaluator. It reduces ambient authority and produces deterministic evidence,
// but it is deliberately not presented as an OS or network security boundary.
package sandbox

import (
	"context"
	"errors"
	"os"
	"time"
)

const (
	ExecutionModeTrustedLocal = "trusted-local"
	NetworkHostUnisolated     = "host-unisolated"
)

var (
	ErrOutputLimit = errors.New("command output limit exceeded")
	ErrProcessTree = errors.New("command left descendant processes running")
	ErrClosed      = errors.New("sandbox workspace is closed")
)

// SnapshotLimits bounds evidence collection. A zero value is replaced by the
// conservative defaults returned by DefaultSnapshotLimits.
type SnapshotLimits struct {
	MaxFiles      int
	MaxTotalBytes int64
	MaxFileBytes  int64
}

func DefaultSnapshotLimits() SnapshotLimits {
	return SnapshotLimits{
		MaxFiles:      10_000,
		MaxTotalBytes: 256 << 20,
		MaxFileBytes:  32 << 20,
	}
}

// RunnerConfig declares all executable and environment authority available to
// setup and oracle commands. Executables are resolved once by the control
// plane; a command never performs a PATH lookup inside the fixture.
type RunnerConfig struct {
	AllowedExecutables []string
	// ExecutablePaths pins a declaration to the exact absolute executable
	// selected by the evaluator control plane. Entries without a pin retain the
	// legacy one-time PATH resolution used by standalone sandbox callers.
	ExecutablePaths map[string]string
	AllowedEnv      []string
	Environment     map[string]string
	DefaultTimeout  time.Duration
	MaxTimeout      time.Duration
	MaxStdoutBytes  int64
	MaxStderrBytes  int64
	Quiescence      time.Duration
}

func DefaultRunnerConfig() RunnerConfig {
	return RunnerConfig{
		DefaultTimeout: 30 * time.Second,
		MaxTimeout:     5 * time.Minute,
		MaxStdoutBytes: 1 << 20,
		MaxStderrBytes: 1 << 20,
		Quiescence:     100 * time.Millisecond,
	}
}

// Command is always an argv vector. No field is interpreted by a shell.
type Command struct {
	ID             string
	Argv           []string
	Dir            string
	Env            map[string]string
	Timeout        time.Duration
	ExpectedExit   []int
	MaxOutputBytes int64
}

// CommandResult is evaluator evidence, including failures which happen before
// an exit status exists.
type CommandResult struct {
	ID                  string
	Argv                []string
	Dir                 string
	Started             bool
	Completed           bool
	ExitCode            int
	Signal              string
	TimedOut            bool
	Canceled            bool
	Stdout              string
	Stderr              string
	StdoutTruncated     bool
	StderrTruncated     bool
	OutputLimitExceeded bool
	CleanProcessTree    bool
	StartedAt           time.Time
	FinishedAt          time.Time
	Duration            time.Duration
	Error               string
}

func (r CommandResult) Successful() bool {
	return r.Started && r.Completed && r.ExitCode == 0 && r.Error == "" &&
		!r.TimedOut && !r.Canceled && !r.OutputLimitExceeded && r.CleanProcessTree
}

// Accepted reports infrastructure success plus an exit code declared by the
// trusted case. A nil ExpectedExit retains the ordinary zero-exit default.
func (r CommandResult) Accepted(expected []int) bool {
	if !r.Started || !r.Completed || r.Error != "" || r.TimedOut || r.Canceled || r.OutputLimitExceeded || !r.CleanProcessTree {
		return false
	}
	if len(expected) == 0 {
		expected = []int{0}
	}
	for _, code := range expected {
		if r.ExitCode == code {
			return true
		}
	}
	return false
}

// Config defines one fresh workspace. ParentDir and SourceDir must be absolute,
// clean paths. The source is never modified.
type Config struct {
	ParentDir            string
	SourceDir            string
	ExpectedSourceDigest string
	InitialGit           bool
	GitSeed              GitSeed
	Setup                []Command
	Runner               RunnerConfig
	Snapshot             SnapshotLimits
}

// GitSeed describes an exact pre-run worktree state. Content must be non-nil
// (including for an empty file) or Digest must identify bytes already present
// in the copied fixture. Mode zero preserves an existing mode or defaults new
// files to 0644.
type GitSeed struct {
	Tracked   []SeedFile
	Staged    []SeedFile
	Untracked []SeedFile
	Ignored   []SeedFile
}

type SeedFile struct {
	Path    string
	Content []byte
	Digest  string
	Mode    os.FileMode
}

func (s GitSeed) Empty() bool {
	return len(s.Tracked) == 0 && len(s.Staged) == 0 && len(s.Untracked) == 0 && len(s.Ignored) == 0
}

type GitStatusEntry struct {
	Path           string
	OriginalPath   string
	Kind           string
	IndexStatus    byte
	WorktreeStatus byte
}

// GitStatusEvidence retains the exact NUL-delimited porcelain-v2 bytes plus a
// parsed, path-safe view. Digest is over Raw without textual normalization.
type GitStatusEvidence struct {
	// Digest commits to Raw, the exact porcelain-v2 -z response.
	Digest string
	Raw    []byte
	// Head is the exact commit object named by HEAD. IndexRaw is the exact
	// NUL-delimited `git ls-files --stage` response and IndexDigest commits to
	// those bytes. StateDigest commits to all three observations so a persisted
	// evidence item cannot silently omit HEAD or index state.
	Head        string
	IndexDigest string
	IndexRaw    []byte
	StateDigest string
	Entries     []GitStatusEntry
}

// GitStateComparison reports whether the immutable repository basis and every
// pre-existing dirty entry survived a run. Additional after-only worktree
// entries are intentionally left to the ordinary filesystem scope oracle.
type GitStateComparison struct {
	Complete                bool
	HeadPreserved           bool
	IndexPreserved          bool
	InitialEntriesPreserved bool
}

func (c GitStateComparison) Preserved() bool {
	return c.Complete && c.HeadPreserved && c.IndexPreserved && c.InitialEntriesPreserved
}

// Executor is the narrow command surface exposed by a workspace.
type Executor interface {
	Run(context.Context, Command) CommandResult
}
