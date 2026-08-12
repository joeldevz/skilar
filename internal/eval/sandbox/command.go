package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	absoluteMaxCommandTimeout = 30 * time.Minute
	absoluteMaxOutputBytes    = 64 << 20
	absoluteMaxQuiescence     = 10 * time.Second
)

var deniedShells = map[string]struct{}{
	"ash": {}, "bash": {}, "busybox": {}, "cmd": {}, "cmd.exe": {},
	"dash": {}, "fish": {}, "ksh": {}, "powershell": {},
	"powershell.exe": {}, "pwsh": {}, "sh": {}, "zsh": {},
}

type commandRunner struct {
	rootPath   string
	root       *os.Root
	allowed    map[string]string
	allowedEnv map[string]struct{}
	baseEnv    map[string]string
	config     RunnerConfig
}

func normalizeRunnerConfig(config RunnerConfig) (RunnerConfig, error) {
	defaults := DefaultRunnerConfig()
	if config.DefaultTimeout == 0 {
		config.DefaultTimeout = defaults.DefaultTimeout
	}
	if config.MaxTimeout == 0 {
		config.MaxTimeout = defaults.MaxTimeout
	}
	if config.MaxStdoutBytes == 0 {
		config.MaxStdoutBytes = defaults.MaxStdoutBytes
	}
	if config.MaxStderrBytes == 0 {
		config.MaxStderrBytes = defaults.MaxStderrBytes
	}
	if config.Quiescence == 0 {
		config.Quiescence = defaults.Quiescence
	}
	if config.DefaultTimeout < 1 || config.MaxTimeout < config.DefaultTimeout || config.MaxTimeout > absoluteMaxCommandTimeout {
		return RunnerConfig{}, fmt.Errorf("invalid command timeouts: default=%s max=%s", config.DefaultTimeout, config.MaxTimeout)
	}
	if config.MaxStdoutBytes < 1 || config.MaxStdoutBytes > absoluteMaxOutputBytes ||
		config.MaxStderrBytes < 1 || config.MaxStderrBytes > absoluteMaxOutputBytes {
		return RunnerConfig{}, fmt.Errorf("invalid output limits: stdout=%d stderr=%d", config.MaxStdoutBytes, config.MaxStderrBytes)
	}
	if config.Quiescence < 1 || config.Quiescence > absoluteMaxQuiescence {
		return RunnerConfig{}, fmt.Errorf("invalid quiescence duration %s", config.Quiescence)
	}
	return config, nil
}

func newCommandRunner(rootPath string, root *os.Root, controlHome string, controlTemp string, config RunnerConfig) (*commandRunner, error) {
	config, err := normalizeRunnerConfig(config)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]string, len(config.AllowedExecutables))
	pathDirs := make(map[string]struct{})
	for _, declared := range config.AllowedExecutables {
		selected := declared
		if pinned, ok := config.ExecutablePaths[declared]; ok {
			if !filepath.IsAbs(pinned) {
				return nil, fmt.Errorf("pinned executable path for %q must be absolute: %q", declared, pinned)
			}
			selected = pinned
		}
		resolved, resolveErr := resolveExecutable(selected)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if previous, exists := allowed[declared]; exists && previous != resolved {
			return nil, fmt.Errorf("executable %q resolves ambiguously", declared)
		}
		allowed[declared] = resolved
		pathDirs[filepath.Dir(resolved)] = struct{}{}
	}
	for declared := range config.ExecutablePaths {
		if _, ok := allowed[declared]; !ok {
			return nil, fmt.Errorf("pinned executable %q is not allowlisted", declared)
		}
	}
	allowedEnv := make(map[string]struct{}, len(config.AllowedEnv))
	for _, key := range config.AllowedEnv {
		if err := validateEnvironmentKey(key); err != nil {
			return nil, err
		}
		if secretEnvironmentKey(key) {
			return nil, fmt.Errorf("credential-like environment key is forbidden: %q", key)
		}
		allowedEnv[key] = struct{}{}
	}
	base := map[string]string{
		"HOME":                controlHome,
		"LANG":                "C",
		"LC_ALL":              "C",
		"TMPDIR":              controlTemp,
		"TZ":                  "UTC",
		"GIT_AUTHOR_NAME":     "Skynex Eval",
		"GIT_AUTHOR_EMAIL":    "eval@invalid.local",
		"GIT_COMMITTER_NAME":  "Skynex Eval",
		"GIT_COMMITTER_EMAIL": "eval@invalid.local",
		"GIT_AUTHOR_DATE":     "2000-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE":  "2000-01-01T00:00:00Z",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   nullDevice(),
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_PAGER":           "",
		"PAGER":               "",
	}
	if runtime.GOOS == "windows" {
		base["USERPROFILE"] = controlHome
		base["TEMP"] = controlTemp
		base["TMP"] = controlTemp
	}
	dirs := make([]string, 0, len(pathDirs))
	for dir := range pathDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	base["PATH"] = strings.Join(dirs, string(os.PathListSeparator))
	for key, value := range config.Environment {
		if _, ok := allowedEnv[key]; !ok {
			return nil, fmt.Errorf("base environment key %q is not allowlisted", key)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("environment value for %q contains NUL", key)
		}
		base[key] = value
	}
	return &commandRunner{
		rootPath: rootPath, root: root, allowed: allowed, allowedEnv: allowedEnv,
		baseEnv: base, config: config,
	}, nil
}

func resolveExecutable(declared string) (string, error) {
	if declared == "" || strings.TrimSpace(declared) != declared || strings.IndexByte(declared, 0) >= 0 {
		return "", fmt.Errorf("invalid executable declaration %q", declared)
	}
	base := strings.ToLower(filepath.Base(declared))
	if _, denied := deniedShells[base]; denied {
		return "", fmt.Errorf("shell executable is forbidden: %q", declared)
	}
	if strings.ContainsAny(declared, `/\\`) && !filepath.IsAbs(declared) {
		return "", fmt.Errorf("executable paths must be absolute: %q", declared)
	}
	resolved, err := exec.LookPath(declared)
	if err != nil {
		return "", fmt.Errorf("resolve allowlisted executable %q: %w", declared, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("make executable path absolute %q: %w", declared, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat executable %q: %w", resolved, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("allowlisted executable is not an executable regular file: %q", resolved)
	}
	return filepath.Clean(resolved), nil
}

func validateEnvironmentKey(key string) error {
	if key == "" || strings.IndexAny(key, "=\x00") >= 0 {
		return fmt.Errorf("invalid environment key %q", key)
	}
	return nil
}

func secretEnvironmentKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, fragment := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "PRIVATE_KEY", "AUTHORIZATION"} {
		if strings.Contains(upper, fragment) {
			return true
		}
	}
	return false
}

func (r *commandRunner) run(ctx context.Context, spec Command) CommandResult {
	result := CommandResult{
		ID: spec.ID, Argv: append([]string(nil), spec.Argv...), Dir: spec.Dir,
		ExitCode: -1, CleanProcessTree: true,
	}
	finish := func(err error) CommandResult {
		result.FinishedAt = time.Now().UTC()
		if !result.StartedAt.IsZero() {
			result.Duration = result.FinishedAt.Sub(result.StartedAt)
		}
		if err != nil {
			result.Error = err.Error()
		}
		return result
	}
	if ctx == nil {
		return finish(errors.New("nil command context"))
	}
	if err := ctx.Err(); err != nil {
		result.Canceled = true
		return finish(err)
	}
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return finish(errors.New("command argv must not be empty"))
	}
	if _, denied := deniedShells[strings.ToLower(filepath.Base(spec.Argv[0]))]; denied {
		return finish(fmt.Errorf("shell executable is forbidden: %q", spec.Argv[0]))
	}
	executable, ok := r.allowed[spec.Argv[0]]
	if !ok {
		return finish(fmt.Errorf("executable is not allowlisted: %q", spec.Argv[0]))
	}
	dir, err := relativeDir(spec.Dir)
	if err != nil {
		return finish(fmt.Errorf("invalid command cwd: %w", err))
	}
	if err := ensureRealDirectory(r.root, dir); err != nil {
		return finish(err)
	}
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = r.config.DefaultTimeout
	}
	if timeout < 1 || timeout > r.config.MaxTimeout {
		return finish(fmt.Errorf("command timeout %s exceeds allowed range (0, %s]", timeout, r.config.MaxTimeout))
	}
	environment := make(map[string]string, len(r.baseEnv)+len(spec.Env)+1)
	for key, value := range r.baseEnv {
		environment[key] = value
	}
	for key, value := range spec.Env {
		if _, ok := r.allowedEnv[key]; !ok {
			return finish(fmt.Errorf("command environment key %q is not allowlisted", key))
		}
		if strings.IndexByte(value, 0) >= 0 {
			return finish(fmt.Errorf("environment value for %q contains NUL", key))
		}
		environment[key] = value
	}
	absDir := r.rootPath
	if dir != "." {
		absDir = filepath.Join(absDir, filepath.FromSlash(dir))
	}
	environment["PWD"] = absDir
	env := make([]string, 0, len(environment))
	for key, value := range environment {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)

	stdoutLimit, stderrLimit := r.config.MaxStdoutBytes, r.config.MaxStderrBytes
	if spec.MaxOutputBytes > 0 {
		if spec.MaxOutputBytes > absoluteMaxOutputBytes {
			return finish(fmt.Errorf("command output limit %d exceeds maximum %d", spec.MaxOutputBytes, absoluteMaxOutputBytes))
		}
		if spec.MaxOutputBytes < stdoutLimit {
			stdoutLimit = spec.MaxOutputBytes
		}
		if spec.MaxOutputBytes < stderrLimit {
			stderrLimit = spec.MaxOutputBytes
		}
	}
	limitReached := make(chan struct{}, 1)
	stdout := newBoundedBuffer(stdoutLimit, limitReached)
	stderr := newBoundedBuffer(stderrLimit, limitReached)
	cmd := exec.Command(executable, spec.Argv[1:]...)
	cmd.Args[0] = spec.Argv[0]
	cmd.Dir = absDir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	configureProcessGroup(cmd)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result.StartedAt = time.Now().UTC()
	if err := cmd.Start(); err != nil {
		return finish(fmt.Errorf("start command: %w", err))
	}
	result.Started = true
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var waitErr error
	var forcedErr error
	select {
	case waitErr = <-waitCh:
	case <-limitReached:
		result.OutputLimitExceeded = true
		forcedErr = ErrOutputLimit
		_ = terminateProcessGroup(cmd.Process.Pid)
		waitErr = <-waitCh
	case <-runCtx.Done():
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			result.TimedOut = true
		} else {
			result.Canceled = true
		}
		forcedErr = runCtx.Err()
		_ = terminateProcessGroup(cmd.Process.Pid)
		waitErr = <-waitCh
	}
	result.Completed = true
	result.Stdout, result.StdoutTruncated = stdout.StringAndTruncated()
	result.Stderr, result.StderrTruncated = stderr.StringAndTruncated()
	if result.StdoutTruncated || result.StderrTruncated {
		result.OutputLimitExceeded = true
		if forcedErr == nil {
			forcedErr = ErrOutputLimit
		}
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
		result.Signal = processSignal(cmd.ProcessState)
	}
	if clean := waitProcessGroupQuiescent(cmd.Process.Pid, r.config.Quiescence); !clean {
		result.CleanProcessTree = false
		_ = terminateProcessGroup(cmd.Process.Pid)
		_ = waitProcessGroupQuiescent(cmd.Process.Pid, r.config.Quiescence)
		if forcedErr == nil {
			forcedErr = ErrProcessTree
		}
	}
	if forcedErr != nil {
		return finish(forcedErr)
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return finish(fmt.Errorf("wait for command: %w", waitErr))
	}
	return finish(nil)
}

type boundedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	max       int64
	truncated bool
	notify    chan<- struct{}
	once      sync.Once
}

func newBoundedBuffer(max int64, notify chan<- struct{}) *boundedBuffer {
	return &boundedBuffer{max: max, notify: notify}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	remaining := b.max - int64(b.buffer.Len())
	if remaining > 0 {
		keep := int64(len(p))
		if keep > remaining {
			keep = remaining
		}
		_, _ = b.buffer.Write(p[:keep])
	}
	if int64(len(p)) > remaining {
		b.truncated = true
		b.once.Do(func() {
			select {
			case b.notify <- struct{}{}:
			default:
			}
		})
	}
	b.mu.Unlock()
	return len(p), nil
}

func (b *boundedBuffer) StringAndTruncated() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String(), b.truncated
}

func nullDevice() string {
	if runtime.GOOS == "windows" {
		return "NUL"
	}
	return "/dev/null"
}
