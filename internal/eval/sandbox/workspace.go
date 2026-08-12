package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/joeldevz/skynex/internal/safefs"
)

// Workspace owns one private run root and its rooted fixture handle.
type Workspace struct {
	path             string
	runPath          string
	runName          string
	parent           *os.Root
	runRoot          *os.Root
	fixtureRoot      *os.Root
	runner           *commandRunner
	snapshotLimits   SnapshotLimits
	sourceDir        string
	sourceInitial    Snapshot
	Before           Snapshot
	InitialGitStatus GitStatusEvidence
	SetupResults     []CommandResult

	closeMu sync.Mutex
	closed  bool
}

func (w *Workspace) Path() string { return w.path }

func (w *Workspace) RunPath() string { return w.runPath }

func (w *Workspace) ExecutionMode() string { return ExecutionModeTrustedLocal }

func (w *Workspace) NetworkMode() string { return NetworkHostUnisolated }

// Materialize copies and verifies a trusted source, commits the clean Git
// baseline, applies dirty-worktree seeds, runs setup, and only then captures the
// immutable before snapshot.
func Materialize(ctx context.Context, config Config) (*Workspace, error) {
	if ctx == nil {
		return nil, errors.New("nil materialization context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAbsoluteDir(config.ParentDir); err != nil {
		return nil, fmt.Errorf("invalid run parent: %w", err)
	}
	if err := validateAbsoluteDir(config.SourceDir); err != nil {
		return nil, fmt.Errorf("invalid fixture source: %w", err)
	}
	limits, err := normalizeSnapshotLimits(config.Snapshot)
	if err != nil {
		return nil, err
	}
	sourceInitial, err := DigestTree(config.SourceDir, limits)
	if err != nil {
		return nil, fmt.Errorf("digest source fixture: %w", err)
	}
	if config.ExpectedSourceDigest != "" && config.ExpectedSourceDigest != sourceInitial.Digest {
		return nil, fmt.Errorf("source fixture digest mismatch: got %s, expected %s", sourceInitial.Digest, config.ExpectedSourceDigest)
	}

	parent, err := safefs.Open(config.ParentDir)
	if err != nil {
		return nil, fmt.Errorf("open run parent: %w", err)
	}
	runName, err := createPrivateRunDir(parent)
	if err != nil {
		_ = parent.Close()
		return nil, err
	}
	w := &Workspace{
		runName: runName, parent: parent, snapshotLimits: limits,
		sourceDir: config.SourceDir, sourceInitial: sourceInitial,
	}
	fail := func(cause error) (*Workspace, error) {
		if cleanupErr := w.Close(); cleanupErr != nil {
			return nil, errors.Join(cause, fmt.Errorf("cleanup failed workspace: %w", cleanupErr))
		}
		return nil, cause
	}
	w.runPath = filepath.Join(config.ParentDir, runName)
	w.path = filepath.Join(w.runPath, "fixture")
	w.runRoot, err = parent.OpenRoot(runName)
	if err != nil {
		return fail(fmt.Errorf("open private run root: %w", err))
	}
	for _, dir := range []string{"fixture", "control", "control/home", "control/tmp"} {
		if err := w.runRoot.MkdirAll(dir, 0o700); err != nil {
			return fail(fmt.Errorf("create private run directory %q: %w", dir, err))
		}
	}
	w.fixtureRoot, err = w.runRoot.OpenRoot("fixture")
	if err != nil {
		return fail(fmt.Errorf("open fixture root: %w", err))
	}
	if err := copyVerifiedTree(config.SourceDir, w.fixtureRoot, sourceInitial, limits); err != nil {
		return fail(fmt.Errorf("copy fixture: %w", err))
	}
	copied, err := takeSnapshot(w.fixtureRoot, limits)
	if err != nil {
		return fail(fmt.Errorf("verify copied fixture: %w", err))
	}
	if copied.Digest != sourceInitial.Digest {
		return fail(fmt.Errorf("copied fixture digest mismatch: got %s, expected %s", copied.Digest, sourceInitial.Digest))
	}
	sourceAfterCopy, err := DigestTree(config.SourceDir, limits)
	if err != nil || sourceAfterCopy.Digest != sourceInitial.Digest {
		if err == nil {
			err = fmt.Errorf("source digest changed from %s to %s", sourceInitial.Digest, sourceAfterCopy.Digest)
		}
		return fail(fmt.Errorf("source fixture changed while copying: %w", err))
	}

	runnerConfig := cloneRunnerConfig(config.Runner)
	if config.InitialGit && !containsString(runnerConfig.AllowedExecutables, "git") {
		runnerConfig.AllowedExecutables = append(runnerConfig.AllowedExecutables, "git")
	}
	w.runner, err = newCommandRunner(
		w.path, w.fixtureRoot,
		filepath.Join(w.runPath, "control", "home"),
		filepath.Join(w.runPath, "control", "tmp"),
		runnerConfig,
	)
	if err != nil {
		return fail(fmt.Errorf("configure command runner: %w", err))
	}
	if !config.InitialGit && !config.GitSeed.Empty() {
		return fail(errors.New("Git seed requires initial_git"))
	}
	if err := validateGitSeed(config.GitSeed); err != nil {
		return fail(err)
	}
	if config.InitialGit {
		if err := w.applySeedGroup(config.GitSeed.Tracked); err != nil {
			return fail(fmt.Errorf("apply tracked Git seed: %w", err))
		}
		if err := w.initializeGit(ctx); err != nil {
			return fail(err)
		}
		excluded := append([]string(nil), seedPaths(config.GitSeed.Staged)...)
		excluded = append(excluded, seedPaths(config.GitSeed.Untracked)...)
		excluded = append(excluded, seedPaths(config.GitSeed.Ignored)...)
		if err := w.commitGitBaseline(ctx, excluded); err != nil {
			return fail(err)
		}
		if err := w.applySeedGroup(config.GitSeed.Staged); err != nil {
			return fail(fmt.Errorf("apply staged Git seed: %w", err))
		}
		if len(config.GitSeed.Staged) != 0 {
			argv := []string{"git", "add", "--"}
			argv = append(argv, seedPaths(config.GitSeed.Staged)...)
			if result := w.Run(ctx, Command{ID: "git.seed-stage", Argv: argv}); !result.Successful() {
				return fail(fmt.Errorf("stage Git seed: exit=%d %s %s", result.ExitCode, result.Error, result.Stderr))
			}
		}
		if err := w.applySeedGroup(config.GitSeed.Untracked); err != nil {
			return fail(fmt.Errorf("apply untracked Git seed: %w", err))
		}
		if err := w.applySeedGroup(config.GitSeed.Ignored); err != nil {
			return fail(fmt.Errorf("apply ignored Git seed: %w", err))
		}
	}
	for _, command := range config.Setup {
		result := w.Run(ctx, command)
		w.SetupResults = append(w.SetupResults, result)
		if !result.Accepted(command.ExpectedExit) {
			return fail(fmt.Errorf("setup command %q failed (exit=%d): %s %s", command.ID, result.ExitCode, result.Error, result.Stderr))
		}
	}
	if config.InitialGit {
		w.InitialGitStatus, err = w.CaptureGitStatus(ctx)
		if err != nil {
			return fail(fmt.Errorf("capture initial Git status: %w", err))
		}
		if err := w.verifyGitSeed(ctx, config.GitSeed, w.InitialGitStatus); err != nil {
			return fail(fmt.Errorf("verify Git seed after setup: %w", err))
		}
	}
	w.Before, err = takeSnapshot(w.fixtureRoot, limits)
	if err != nil {
		return fail(fmt.Errorf("capture initial workspace snapshot: %w", err))
	}
	if unsafe := w.Before.UnsafeEntries(); len(unsafe) != 0 {
		return fail(fmt.Errorf("setup produced unsafe entry %q (%s)", unsafe[0].Path, unsafe[0].Kind))
	}
	sourceFinal, err := DigestTree(config.SourceDir, limits)
	if err != nil || sourceFinal.Digest != sourceInitial.Digest {
		if err == nil {
			err = fmt.Errorf("source digest changed from %s to %s", sourceInitial.Digest, sourceFinal.Digest)
		}
		return fail(fmt.Errorf("source fixture changed during setup: %w", err))
	}
	return w, nil
}

func cloneRunnerConfig(config RunnerConfig) RunnerConfig {
	config.AllowedExecutables = append([]string(nil), config.AllowedExecutables...)
	config.AllowedEnv = append([]string(nil), config.AllowedEnv...)
	if config.ExecutablePaths != nil {
		copy := make(map[string]string, len(config.ExecutablePaths))
		for declaration, path := range config.ExecutablePaths {
			copy[declaration] = path
		}
		config.ExecutablePaths = copy
	}
	if config.Environment != nil {
		copy := make(map[string]string, len(config.Environment))
		for key, value := range config.Environment {
			copy[key] = value
		}
		config.Environment = copy
	}
	return config
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func createPrivateRunDir(parent *os.Root) (string, error) {
	for attempt := 0; attempt < 32; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("generate private run name: %w", err)
		}
		name := "run-" + hex.EncodeToString(random[:])
		if err := parent.Mkdir(name, 0o700); err == nil {
			return name, nil
		} else if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("create private run root: %w", err)
		}
	}
	return "", errors.New("could not allocate a unique private run root")
}

func copyVerifiedTree(sourcePath string, destination *os.Root, source Snapshot, limits SnapshotLimits) error {
	sourceRoot, err := safefs.Open(sourcePath)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	for _, entry := range source.Entries {
		if entry.Kind != EntryDir {
			continue
		}
		if err := destination.MkdirAll(entry.Path, 0o700); err != nil {
			return fmt.Errorf("create destination directory %q: %w", entry.Path, err)
		}
	}
	for _, entry := range source.Entries {
		if entry.Kind != EntryFile {
			continue
		}
		data, err := safefs.ReadFileVerified(sourceRoot, entry.Path, limits.MaxFileBytes)
		if err != nil {
			return fmt.Errorf("read source file %q: %w", entry.Path, err)
		}
		if err := safefs.WriteAtomic(destination, entry.Path, data, os.FileMode(entry.Mode).Perm(), ".eval-copy-"); err != nil {
			return fmt.Errorf("write destination file %q: %w", entry.Path, err)
		}
	}
	directories := make([]Entry, 0)
	for _, entry := range source.Entries {
		if entry.Kind == EntryDir {
			directories = append(directories, entry)
		}
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.Count(directories[i].Path, "/") > strings.Count(directories[j].Path, "/")
	})
	for _, entry := range directories {
		if err := destination.Chmod(entry.Path, os.FileMode(entry.Mode).Perm()); err != nil {
			return fmt.Errorf("set destination directory mode %q: %w", entry.Path, err)
		}
	}
	return nil
}

func (w *Workspace) initializeGit(ctx context.Context) error {
	commands := []Command{
		{ID: "git.init", Argv: []string{"git", "init", "-q", "--initial-branch=eval"}},
		{ID: "git.disable-hooks", Argv: []string{"git", "config", "core.hooksPath", ".git/disabled-hooks"}},
		{ID: "git.disable-signing", Argv: []string{"git", "config", "commit.gpgsign", "false"}},
		{ID: "git.disable-autocrlf", Argv: []string{"git", "config", "core.autocrlf", "false"}},
		{ID: "git.filemode", Argv: []string{"git", "config", "core.filemode", "true"}},
	}
	for _, command := range commands {
		if result := w.Run(ctx, command); !result.Successful() {
			return fmt.Errorf("initialize deterministic Git (%s): exit=%d %s %s", command.ID, result.ExitCode, result.Error, result.Stderr)
		}
	}
	return nil
}

func (w *Workspace) commitGitBaseline(ctx context.Context, excluded []string) error {
	commands := []Command{{ID: "git.add", Argv: []string{"git", "add", "--all"}}}
	if len(excluded) != 0 {
		sort.Strings(excluded)
		argv := []string{"git", "rm", "--cached", "-q", "-f", "--ignore-unmatch", "--"}
		commands = append(commands, Command{ID: "git.exclude-seed", Argv: append(argv, excluded...)})
	}
	commands = append(commands, Command{ID: "git.commit", Argv: []string{"git", "commit", "-q", "--allow-empty", "--no-gpg-sign", "-m", "deterministic fixture"}})
	for _, command := range commands {
		if result := w.Run(ctx, command); !result.Successful() {
			return fmt.Errorf("commit deterministic Git baseline (%s): exit=%d %s %s", command.ID, result.ExitCode, result.Error, result.Stderr)
		}
	}
	return nil
}

func (w *Workspace) Run(ctx context.Context, command Command) CommandResult {
	w.closeMu.Lock()
	closed := w.closed
	runner := w.runner
	w.closeMu.Unlock()
	if closed || runner == nil {
		return CommandResult{ID: command.ID, Argv: append([]string(nil), command.Argv...), Dir: command.Dir, ExitCode: -1, Error: ErrClosed.Error()}
	}
	return runner.run(ctx, command)
}

func (w *Workspace) Snapshot() (Snapshot, error) {
	w.closeMu.Lock()
	closed := w.closed
	root := w.fixtureRoot
	w.closeMu.Unlock()
	if closed || root == nil {
		return Snapshot{}, ErrClosed
	}
	return takeSnapshot(root, w.snapshotLimits)
}

// SourceUnchanged rechecks the original source through the strict digest path.
func (w *Workspace) SourceUnchanged() (bool, error) {
	current, err := DigestTree(w.sourceDir, w.snapshotLimits)
	if err != nil {
		return false, err
	}
	return current.Digest == w.sourceInitial.Digest, nil
}

// Close removes only the unpredictable child held beneath the retained parent
// root. It is safe to call more than once.
func (w *Workspace) Close() error {
	w.closeMu.Lock()
	defer w.closeMu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	var errs []error
	if w.fixtureRoot != nil {
		errs = append(errs, w.fixtureRoot.Close())
		w.fixtureRoot = nil
	}
	if w.runRoot != nil {
		errs = append(errs, w.runRoot.Close())
		w.runRoot = nil
	}
	if w.parent != nil {
		if w.runName != "" {
			errs = append(errs, safefs.Remove(w.parent, w.runName))
		}
		errs = append(errs, w.parent.Close())
		w.parent = nil
	}
	return errors.Join(errs...)
}
