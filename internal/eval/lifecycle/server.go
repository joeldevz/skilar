// Package lifecycle owns one isolated opencode serve process per evaluation
// run. It binds the process to a run workspace, uses a private log, and tears
// down the complete process group on every exit path.
package lifecycle

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/mcpproxy"
)

const (
	defaultStartupTimeout   = 30 * time.Second
	defaultShutdownTimeout  = 5 * time.Second
	defaultHealthTimeout    = 2 * time.Second
	defaultHealthInterval   = 100 * time.Millisecond
	evaluatorServerUsername = "skynex-eval"
	maxControlledPluginSize = 4 << 20
	// EvaluatorManagedDetachEnvironment is an internal, exact-value capability.
	// The evaluator injects it only for Workflow V2 canary runtimes so detached
	// Skynex descendants remain inside the lifecycle-owned process group.
	EvaluatorManagedDetachEnvironment = "SKYNEX_EVAL_MANAGED_DETACH"
)

var safeInheritedEnvironment = []string{
	"PATH",
}

// Config holds server configuration.
type Config struct {
	// Port zero selects a currently free loopback port.
	Port     int
	Hostname string
	Timeout  time.Duration
	Binary   string

	// WorkDir is the exact cwd exposed by this OpenCode instance. RunDir owns
	// private runtime artifacts such as logs and need not equal WorkDir.
	WorkDir string
	RunDir  string
	LogPath string

	// ConfigHome is the evaluator-owned XDG configuration root containing the
	// frozen OpenCode bundle. OpenAIOAuthFile must be a dedicated credential
	// source containing exactly one OpenAI OAuth entry; it is copied into the
	// run-private data home and is never exposed to or modified by OpenCode.
	ConfigHome      string
	OpenAIOAuthFile string
	// OpenAIOAuthSession is preferred for a multi-run serialized experiment. It
	// requires a fresh credential that covers the requested horizon and restages
	// it without sharing sessions/cache. It never refreshes the source. A
	// runtime-side credential change fails closed because this local boundary
	// cannot attribute same-UID filesystem writes.
	OpenAIOAuthSession         *OpenAIOAuthSession
	OpenAIOAuthMinimumValidity time.Duration

	// Only named variables are inherited. Env is then applied as explicit
	// overrides. Provider credentials must be deliberately included in one of
	// these two fields.
	EnvAllowlist []string
	Env          map[string]string
	// MCPProxyManifest is evaluator-owned runtime authority. Unlike Env it is
	// installed only after reserved-environment validation and must be a
	// protected file contained by RunDir.
	MCPProxyManifest string

	ShutdownTimeout time.Duration
	HealthTimeout   time.Duration
	HealthInterval  time.Duration
	ExpectedVersion string
	HTTPClient      *http.Client

	// Evaluation servers use --pure unless ControlledPlugin proves the one
	// evaluator-owned file URL present in the frozen config. AllowImpure is
	// retained for compatibility but cannot bypass that identity fence. Pure is
	// an affirmative override. ExtraArgs are appended without invoking a shell.
	Pure             bool
	AllowImpure      bool
	ControlledPlugin *ControlledPluginIdentity
	ExtraArgs        []string

	// CommandArgs replaces the default arguments and is primarily useful for
	// deterministic fake-server tests. {port}, {hostname}, and {workdir} are
	// expanded without a shell.
	CommandArgs []string
}

// ControlledPluginIdentity is the exact plugin authority that permits the
// lifecycle to omit OpenCode's --pure flag.
type ControlledPluginIdentity struct {
	Path          string
	ContentDigest string
}

func verifyControlledPluginBoundary(cfg Config) error {
	if cfg.ControlledPlugin == nil {
		if cfg.AllowImpure {
			return errors.New("uncontrolled OpenCode plugin loading is forbidden")
		}
		return nil
	}
	if cfg.Pure {
		return errors.New("controlled plugin and --pure are mutually exclusive")
	}
	if err := verifyControlledPluginIdentity(cfg.ControlledPlugin); err != nil {
		return err
	}
	if cfg.ConfigHome == "" {
		return errors.New("controlled plugin requires an evaluator-owned OpenCode config home")
	}
	if filepath.Clean(cfg.ConfigHome) != cfg.ConfigHome {
		return errors.New("controlled OpenCode config home must be an exact clean path")
	}
	if err := validateControlledConfigHome(cfg.ConfigHome); err != nil {
		return err
	}
	resolvedConfigHome, err := filepath.EvalSymlinks(cfg.ConfigHome)
	if err != nil || filepath.Clean(resolvedConfigHome) != cfg.ConfigHome {
		return errors.New("controlled OpenCode config home must not contain symlink components")
	}
	rootEntries, err := os.ReadDir(cfg.ConfigHome)
	if err != nil || len(rootEntries) != 1 || rootEntries[0].Name() != "opencode" || !rootEntries[0].IsDir() {
		return errors.New("controlled OpenCode config home must contain only the opencode directory")
	}
	configDir := filepath.Join(cfg.ConfigHome, "opencode")
	dirInfo, err := os.Lstat(configDir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("controlled OpenCode config directory is unavailable or symlinked")
	}
	configEntries, err := os.ReadDir(configDir)
	if err != nil || len(configEntries) != 1 || configEntries[0].Name() != "opencode.json" || !configEntries[0].Type().IsRegular() {
		return errors.New("controlled OpenCode config directory must contain only regular opencode.json")
	}
	configPath := filepath.Join(configDir, "opencode.json")
	before, err := os.Lstat(configPath)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 8<<20 {
		return errors.New("controlled OpenCode config file is unavailable, non-regular, or too large")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read controlled OpenCode config: %w", err)
	}
	after, err := os.Lstat(configPath)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() {
		return errors.New("controlled OpenCode config changed while it was verified")
	}
	var config map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil || config == nil {
		return errors.New("controlled OpenCode config is not one JSON object")
	}
	if trailingErr := decoder.Decode(new(any)); !errors.Is(trailingErr, io.EOF) {
		return errors.New("controlled OpenCode config contains trailing JSON")
	}
	plugins, ok := config["plugin"].([]any)
	wantURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(cfg.ControlledPlugin.Path)}).String()
	if !ok || len(plugins) != 1 || plugins[0] != wantURL {
		return errors.New("controlled OpenCode config must contain exactly the attested plugin file URL")
	}
	return nil
}

func verifyControlledPluginIdentity(identity *ControlledPluginIdentity) error {
	if identity == nil {
		return nil
	}
	if identity.Path == "" || !filepath.IsAbs(identity.Path) || filepath.Clean(identity.Path) != identity.Path {
		return errors.New("controlled plugin path must be exact, clean, and absolute")
	}
	if !strings.HasPrefix(identity.ContentDigest, "sha256:") || len(identity.ContentDigest) != len("sha256:")+sha256.Size*2 ||
		strings.ToLower(identity.ContentDigest) != identity.ContentDigest {
		return errors.New("controlled plugin digest must be a lowercase sha256 digest")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(identity.ContentDigest, "sha256:")); err != nil {
		return errors.New("controlled plugin digest must be a lowercase sha256 digest")
	}
	resolved, err := filepath.EvalSymlinks(identity.Path)
	if err != nil {
		return fmt.Errorf("resolve controlled plugin path: %w", err)
	}
	if filepath.Clean(resolved) != identity.Path {
		return errors.New("controlled plugin path must not contain symlink components")
	}
	before, err := os.Lstat(identity.Path)
	if err != nil {
		return fmt.Errorf("inspect controlled plugin: %w", err)
	}
	if !before.Mode().IsRegular() || before.Size() > maxControlledPluginSize {
		return fmt.Errorf("controlled plugin must be a regular file no larger than %d bytes", maxControlledPluginSize)
	}
	file, err := os.Open(identity.Path)
	if err != nil {
		return fmt.Errorf("open controlled plugin: %w", err)
	}
	hash := sha256.New()
	read, copyErr := io.Copy(hash, io.LimitReader(file, maxControlledPluginSize+1))
	opened, statErr := file.Stat()
	closeErr := file.Close()
	if copyErr != nil || statErr != nil || closeErr != nil {
		return errors.New("read controlled plugin")
	}
	if read > maxControlledPluginSize {
		return fmt.Errorf("controlled plugin exceeds %d bytes", maxControlledPluginSize)
	}
	after, err := os.Lstat(identity.Path)
	if err != nil || !opened.Mode().IsRegular() || !after.Mode().IsRegular() ||
		!os.SameFile(before, opened) || !os.SameFile(before, after) || before.Size() != after.Size() {
		return errors.New("controlled plugin changed while it was verified")
	}
	got := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if got != identity.ContentDigest {
		return errors.New("controlled plugin content digest mismatch")
	}
	return nil
}

type serverState uint8

const (
	stateNew serverState = iota
	stateStarting
	stateRunning
	stateStopping
	stateStopped
)

// Server manages one opencode serve process.
type Server struct {
	cfg Config

	// launchMu serializes Stop against the complete launch transaction: private
	// profile materialization, credential staging, cmd.Start, and PID
	// publication. Without it, Stop could consume stopOnce while Start was still
	// preparing a process and leave the subsequently launched server orphaned.
	launchMu       sync.Mutex
	mu             sync.RWMutex
	state          serverState
	cmd            *exec.Cmd
	pid            int
	logFile        *os.File
	logPath        string
	runDir         string
	workDir        string
	port           int
	baseURL        string
	health         client.HealthInfo
	waitDone       chan struct{}
	waitErr        error
	oauthLease     *openAIOAuthLease
	privateEnvDirs []string
	serverPassword string

	// beforeCommandStart is a deterministic test seam. It is invoked while
	// launchMu and mu are held and must not call back into Server methods.
	beforeCommandStart func()

	stopOnce sync.Once
	stopErr  error
}

// ProbeEvidence fingerprints the effective OpenCode runtime without invoking
// a model. Raw documents remain in memory; callers decide whether a sanitized
// copy is safe to persist.
type ProbeEvidence struct {
	CapturedAt time.Time               `json:"captured_at"`
	Health     client.HealthInfo       `json:"health"`
	Path       client.RawDocument      `json:"path"`
	Config     client.RawDocument      `json:"config"`
	Agents     client.RawDocument      `json:"agents"`
	Tools      client.RawDocument      `json:"tools"`
	MCP        client.MCPStatusCatalog `json:"mcp"`
	Providers  client.ProviderCatalog  `json:"providers"`
	OpenAPI    client.RawDocument      `json:"openapi"`
}

// VersionMismatchError is a hard compatibility fence, not a transient
// readiness failure.
type VersionMismatchError struct {
	Got      string
	Expected string
}

func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf("unsupported OpenCode version %q, expected exactly %q", e.Got, e.Expected)
}

// NewServer creates a server with safe evaluation defaults.
func NewServer() *Server {
	return NewServerWithConfig(Config{
		Timeout: defaultStartupTimeout,
		Binary:  "opencode",
	})
}

// NewServerWithConfig creates a server. Port allocation and filesystem writes
// are deferred until Start so construction is side-effect free.
func NewServerWithConfig(cfg Config) *Server {
	if cfg.Hostname == "" {
		cfg.Hostname = "127.0.0.1"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultStartupTimeout
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	if cfg.HealthTimeout <= 0 {
		cfg.HealthTimeout = defaultHealthTimeout
	}
	if cfg.HealthInterval <= 0 {
		cfg.HealthInterval = defaultHealthInterval
	}
	if cfg.Binary == "" {
		cfg.Binary = "opencode"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	return &Server{cfg: cfg, state: stateNew}
}

// Start launches opencode and waits for a versioned health response. It makes
// an immediate health attempt and then reacts to a ticker, context cancellation,
// process exit, or the startup deadline; there is no fixed readiness sleep.
func (s *Server) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.cfg.OpenAIOAuthFile != "" || s.cfg.OpenAIOAuthSession != nil {
		if err := requireCurrentCleanOAuthPlatform(); err != nil {
			return err
		}
	}
	s.launchMu.Lock()
	launchLocked := true
	unlockLaunch := func() {
		if launchLocked {
			launchLocked = false
			s.launchMu.Unlock()
		}
	}
	defer unlockLaunch()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.state != stateNew {
		s.mu.Unlock()
		return fmt.Errorf("server cannot start from state %d", s.state)
	}
	s.state = stateStarting
	s.mu.Unlock()

	if err := s.prepare(); err != nil {
		s.markStopped()
		return err
	}
	if s.cfg.OpenAIOAuthFile != "" && s.cfg.OpenAIOAuthSession != nil {
		_ = s.closeLog()
		s.markStopped()
		return errors.New("configure either OpenAIOAuthFile or OpenAIOAuthSession, not both")
	}
	if err := ctx.Err(); err != nil {
		_ = s.closeLog()
		s.markStopped()
		return err
	}

	s.mu.Lock()
	args := s.commandArgsLocked()
	cmd := exec.Command(s.cfg.Binary, args...)
	cmd.Dir = s.workDir
	env, err := buildEnvironment(s.cfg.EnvAllowlist, s.cfg.Env, s.runDir)
	if err != nil {
		s.mu.Unlock()
		_ = s.closeLog()
		s.markStopped()
		return err
	}
	s.privateEnvDirs = privateEnvironmentPaths(env)
	if s.cfg.MCPProxyManifest != "" {
		manifest := filepath.Clean(s.cfg.MCPProxyManifest)
		relative, relErr := filepath.Rel(s.runDir, manifest)
		info, statErr := os.Lstat(manifest)
		if !filepath.IsAbs(manifest) || manifest != s.cfg.MCPProxyManifest || relErr != nil || relative == "." || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			s.mu.Unlock()
			_ = s.closeLog()
			_ = s.cleanupPrivateEnvironment()
			s.markStopped()
			return errors.New("protected MCP proxy manifest is invalid")
		}
		env = replaceEnvironmentValue(env, mcpproxy.ManifestEnvironment, manifest)
	}
	// --pure disables external plugins, while this evaluator-owned switch also
	// prevents a candidate fixture from injecting project configuration before
	// the runtime contract is probed.
	env = replaceEnvironmentValue(env, "OPENCODE_DISABLE_PROJECT_CONFIG", "1")
	env = replaceEnvironmentValue(env, "OPENCODE_DISABLE_MODELS_FETCH", "1")
	env = replaceEnvironmentValue(env, "OPENCODE_SERVER_USERNAME", evaluatorServerUsername)
	env = replaceEnvironmentValue(env, "OPENCODE_SERVER_PASSWORD", s.serverPassword)
	if s.cfg.ConfigHome != "" {
		if err := validateControlledConfigHome(s.cfg.ConfigHome); err != nil {
			s.mu.Unlock()
			_ = s.closeLog()
			_ = s.cleanupPrivateEnvironment()
			s.markStopped()
			return err
		}
		env = replaceEnvironmentValue(env, "XDG_CONFIG_HOME", s.cfg.ConfigHome)
	}
	if s.cfg.OpenAIOAuthFile != "" || s.cfg.OpenAIOAuthSession != nil {
		dataHome, ok := lookupEnvironmentValue(env, "XDG_DATA_HOME")
		if !ok {
			s.mu.Unlock()
			_ = s.closeLog()
			_ = s.cleanupPrivateEnvironment()
			s.markStopped()
			return errors.New("private XDG_DATA_HOME is unavailable")
		}
		oauthSession := s.cfg.OpenAIOAuthSession
		if oauthSession == nil {
			var sessionErr error
			oauthSession, sessionErr = NewOpenAIOAuthSession(s.cfg.OpenAIOAuthFile)
			if sessionErr != nil {
				s.mu.Unlock()
				_ = s.closeLog()
				_ = s.cleanupPrivateEnvironment()
				s.markStopped()
				return fmt.Errorf("load clean OpenAI OAuth session: %w", sessionErr)
			}
		}
		lease, leaseErr := oauthSession.stage(ctx, dataHome, s.cfg.OpenAIOAuthMinimumValidity)
		if leaseErr == nil {
			s.oauthLease = lease
		}
		if leaseErr != nil {
			s.mu.Unlock()
			_ = s.closeLog()
			_ = s.cleanupPrivateEnvironment()
			s.markStopped()
			return fmt.Errorf("materialize clean OpenAI OAuth session: %w", leaseErr)
		}
	}
	cmd.Env = env
	cmd.Stdout = s.logFile
	cmd.Stderr = s.logFile
	configureProcessGroup(cmd)
	s.cmd = cmd
	s.waitDone = make(chan struct{})
	// Start while holding the lifecycle lock and publish the PID before Stop can
	// observe stateStarting. Otherwise a concurrent Stop can consume stopOnce
	// with pid=0, after which this process would start without any remaining
	// teardown path.
	if s.beforeCommandStart != nil {
		s.beforeCommandStart()
	}
	if err := verifyControlledPluginBoundary(s.cfg); err != nil {
		s.cmd = nil
		s.waitDone = nil
		s.state = stateNew
		s.mu.Unlock()
		unlockLaunch()
		return errors.Join(fmt.Errorf("revalidate controlled plugin before OpenCode launch: %w", err), s.Stop())
	}
	if err := cmd.Start(); err != nil {
		s.cmd = nil
		s.waitDone = nil
		s.state = stateNew
		s.mu.Unlock()
		unlockLaunch()
		return errors.Join(fmt.Errorf("start opencode server: %w", err), s.Stop())
	}
	s.pid = cmd.Process.Pid
	s.mu.Unlock()
	unlockLaunch()
	go s.waitForProcess(cmd)

	startupTimer := time.NewTimer(s.cfg.Timeout)
	defer startupTimer.Stop()
	ticker := time.NewTicker(s.cfg.HealthInterval)
	defer ticker.Stop()

	var lastHealthErr error
	for {
		healthCtx, cancel := context.WithTimeout(ctx, s.cfg.HealthTimeout)
		info, healthErr := s.Health(healthCtx)
		cancel()
		if healthErr == nil {
			if err := verifyControlledPluginBoundary(s.cfg); err != nil {
				return errors.Join(fmt.Errorf("revalidate controlled plugin after OpenCode startup: %w", err), s.Stop())
			}
			s.mu.Lock()
			if s.state != stateStarting {
				waitErr := s.waitErr
				s.mu.Unlock()
				if waitErr != nil {
					return fmt.Errorf("opencode server stopped during healthcheck: %w", waitErr)
				}
				return errors.New("opencode server stopped during healthcheck")
			}
			s.state = stateRunning
			s.health = info
			s.mu.Unlock()
			go s.stopWhenContextEnds(ctx)
			return nil
		}
		lastHealthErr = healthErr
		var versionMismatch *VersionMismatchError
		if errors.As(healthErr, &versionMismatch) {
			return errors.Join(versionMismatch, s.Stop())
		}

		s.mu.RLock()
		waitDone := s.waitDone
		s.mu.RUnlock()
		select {
		case <-ctx.Done():
			stopErr := s.Stop()
			return errors.Join(ctx.Err(), stopErr)
		case <-startupTimer.C:
			stopErr := s.Stop()
			return errors.Join(fmt.Errorf("healthcheck timeout after %s: %w", s.cfg.Timeout, lastHealthErr), stopErr)
		case <-waitDone:
			s.mu.RLock()
			waitErr := s.waitErr
			s.mu.RUnlock()
			stopErr := s.Stop()
			if waitErr == nil {
				waitErr = errors.New("process exited successfully before becoming healthy")
			}
			return errors.Join(fmt.Errorf("opencode server exited before healthcheck: %w", waitErr), stopErr)
		case <-ticker.C:
		}
	}
}

func (s *Server) prepare() error {
	if s.cfg.Port < 0 || s.cfg.Port > 65535 {
		return fmt.Errorf("invalid port %d", s.cfg.Port)
	}
	if err := validateConfiguredBinary(s.cfg.Binary); err != nil {
		return err
	}
	if !isLoopbackHost(s.cfg.Hostname) {
		return fmt.Errorf("server hostname %q is not loopback", s.cfg.Hostname)
	}
	if len(s.cfg.CommandArgs) == 0 {
		if err := validateExtraArgs(s.cfg.ExtraArgs); err != nil {
			return err
		}
	}
	if err := verifyControlledPluginBoundary(s.cfg); err != nil {
		return err
	}
	if err := rejectManagedOpenCodeConfig(); err != nil {
		return err
	}
	serverPassword, err := randomServerPassword()
	if err != nil {
		return err
	}
	workDir := s.cfg.WorkDir
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve working directory: %w", err)
		}
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("resolve working directory %q: %w", workDir, err)
	}
	stat, err := os.Stat(absWorkDir)
	if err != nil {
		return fmt.Errorf("inspect working directory %q: %w", absWorkDir, err)
	}
	if !stat.IsDir() {
		return fmt.Errorf("working directory %q is not a directory", absWorkDir)
	}

	port := s.cfg.Port
	if port == 0 {
		port, err = freeLoopbackPort(s.cfg.Hostname)
	} else {
		err = verifyPortAvailable(s.cfg.Hostname, port)
	}
	if err != nil {
		return fmt.Errorf("select server port: %w", err)
	}

	runDir := s.cfg.RunDir
	if runDir == "" {
		runDir, err = os.MkdirTemp("", "skynex-eval-run-*")
		if err != nil {
			return fmt.Errorf("create run directory: %w", err)
		}
		if err := os.Chmod(runDir, 0o700); err != nil {
			return fmt.Errorf("secure run directory: %w", err)
		}
	} else {
		runDir, err = filepath.Abs(runDir)
		if err != nil {
			return fmt.Errorf("resolve run directory: %w", err)
		}
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			return fmt.Errorf("create run directory: %w", err)
		}
	}

	logPath := s.cfg.LogPath
	var logFile *os.File
	if logPath == "" {
		logFile, err = os.CreateTemp(runDir, "opencode-*.log")
		if err != nil {
			return fmt.Errorf("create private server log: %w", err)
		}
		logPath = logFile.Name()
	} else {
		if !filepath.IsAbs(logPath) {
			logPath = filepath.Join(runDir, logPath)
		}
		logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("create private server log: %w", err)
		}
	}
	if err := logFile.Chmod(0o600); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("secure server log: %w", err)
	}

	s.mu.Lock()
	s.workDir = absWorkDir
	s.runDir = runDir
	s.logPath = logPath
	s.logFile = logFile
	s.port = port
	s.baseURL = "http://" + net.JoinHostPort(s.cfg.Hostname, fmt.Sprintf("%d", port))
	s.serverPassword = serverPassword
	s.mu.Unlock()
	return nil
}

func (s *Server) commandArgsLocked() []string {
	if len(s.cfg.CommandArgs) != 0 {
		args := append([]string(nil), s.cfg.CommandArgs...)
		for i, arg := range args {
			arg = strings.ReplaceAll(arg, "{port}", fmt.Sprintf("%d", s.port))
			arg = strings.ReplaceAll(arg, "{hostname}", s.cfg.Hostname)
			arg = strings.ReplaceAll(arg, "{workdir}", s.workDir)
			args[i] = arg
		}
		return args
	}
	args := []string{"serve"}
	if s.cfg.Pure || s.cfg.ControlledPlugin == nil {
		args = append(args, "--pure")
	}
	args = append(args, "--port", fmt.Sprintf("%d", s.port), "--hostname", s.cfg.Hostname)
	args = append(args, s.cfg.ExtraArgs...)
	return args
}

func (s *Server) waitForProcess(cmd *exec.Cmd) {
	err := cmd.Wait()
	s.mu.Lock()
	s.waitErr = err
	waitDone := s.waitDone
	s.mu.Unlock()
	close(waitDone)
	// Natural parent exit must still clean up descendants in its process group.
	go func() { _ = s.Stop() }()
}

func (s *Server) stopWhenContextEnds(ctx context.Context) {
	s.mu.RLock()
	waitDone := s.waitDone
	s.mu.RUnlock()
	select {
	case <-ctx.Done():
		_ = s.Stop()
	case <-waitDone:
	}
}

// Stop terminates the complete process group. It is idempotent and safe for
// concurrent callers.
func (s *Server) Stop() error {
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	s.stopOnce.Do(func() {
		s.mu.Lock()
		if s.state == stateNew || s.state == stateStopped {
			s.state = stateStopped
			s.mu.Unlock()
			s.stopErr = errors.Join(s.closeLog(), s.releaseOAuthLease(false), s.cleanupPrivateEnvironment())
			return
		}
		s.state = stateStopping
		pid := s.pid
		waitDone := s.waitDone
		s.mu.Unlock()

		var errs []error
		reaped := waitDone == nil
		waitForReap := func(timeout time.Duration) bool {
			if reaped {
				return true
			}
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case <-waitDone:
				reaped = true
				return true
			case <-timer.C:
				return false
			}
		}
		if pid > 0 {
			if err := terminateProcessGroup(pid); err != nil && !isProcessGone(err) {
				errs = append(errs, fmt.Errorf("terminate process group %d: %w", pid, err))
			}
			// Reap the leader first when TERM is sufficient. A dead but unreaped
			// leader makes kill(-pgid, 0) look live and would otherwise trigger a
			// needless escalation on every graceful shutdown.
			_ = waitForReap(s.cfg.ShutdownTimeout)
			if processGroupAlive(pid) {
				if err := killProcessGroup(pid); err != nil && !isProcessGone(err) {
					errs = append(errs, fmt.Errorf("kill process group %d: %w", pid, err))
				}
			}
			if !waitForReap(time.Second) {
				errs = append(errs, errors.New("timed out waiting for server process reap"))
			}
			// Always attest group disappearance after the bounded parent reap;
			// successful cmd.Wait alone says nothing about descendants.
			if !waitForProcessGroup(pid, time.Second) {
				errs = append(errs, fmt.Errorf("process group %d remained live after shutdown and parent reap", pid))
			}
		} else if !waitForReap(time.Second) {
			errs = append(errs, errors.New("timed out waiting for server process reap"))
		}
		if err := s.closeLog(); err != nil {
			errs = append(errs, err)
		}
		if err := s.releaseOAuthLease(pid > 0); err != nil {
			errs = append(errs, err)
		}
		if err := s.cleanupPrivateEnvironment(); err != nil {
			errs = append(errs, err)
		}
		if err := verifyControlledPluginBoundary(s.cfg); err != nil {
			errs = append(errs, fmt.Errorf("controlled plugin identity drifted during OpenCode execution: %w", err))
		}
		s.mu.Lock()
		s.state = stateStopped
		s.mu.Unlock()
		s.stopErr = errors.Join(errs...)
	})
	return s.stopErr
}

func (s *Server) releaseOAuthLease(inspect bool) error {
	s.mu.Lock()
	lease := s.oauthLease
	s.oauthLease = nil
	s.mu.Unlock()
	if lease == nil {
		return nil
	}
	return lease.release(inspect)
}

func (s *Server) cleanupPrivateEnvironment() error {
	s.mu.Lock()
	paths := append([]string(nil), s.privateEnvDirs...)
	s.privateEnvDirs = nil
	runDir := s.runDir
	s.mu.Unlock()
	var errs []error
	for _, path := range paths {
		relative, err := filepath.Rel(runDir, path)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			errs = append(errs, fmt.Errorf("refuse cleanup of private environment path %q", path))
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove private environment path: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (s *Server) closeLog() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logFile == nil {
		return nil
	}
	err := s.logFile.Close()
	s.logFile = nil
	if err != nil {
		return fmt.Errorf("close server log: %w", err)
	}
	return nil
}

func (s *Server) markStopped() {
	s.mu.Lock()
	s.state = stateStopped
	s.mu.Unlock()
}

// Health checks the versioned /global/health response and applies the exact
// configured version fence.
func (s *Server) Health(ctx context.Context) (client.HealthInfo, error) {
	s.mu.RLock()
	baseURL := s.baseURL
	workDir := s.workDir
	state := s.state
	httpClient := s.cfg.HTTPClient
	expectedVersion := s.cfg.ExpectedVersion
	password := s.serverPassword
	s.mu.RUnlock()
	if state == stateNew || state == stateStopped || baseURL == "" {
		return client.HealthInfo{}, errors.New("server is not running")
	}
	c := client.New(client.Config{
		BaseURL: baseURL, Directory: workDir, HTTPClient: httpClient,
		Username: evaluatorServerUsername, Password: password,
	})
	info, err := c.HealthInfoContext(ctx)
	if err != nil {
		return info, err
	}
	if expectedVersion != "" && info.Version != expectedVersion {
		return info, &VersionMismatchError{Got: info.Version, Expected: expectedVersion}
	}
	return info, nil
}

// Probe captures compatibility evidence from read-only endpoints. It performs
// no session creation and no paid model call.
func (s *Server) Probe(ctx context.Context) (ProbeEvidence, error) {
	s.mu.RLock()
	baseURL := s.baseURL
	workDir := s.workDir
	httpClient := s.cfg.HTTPClient
	password := s.serverPassword
	s.mu.RUnlock()
	if baseURL == "" {
		return ProbeEvidence{}, errors.New("server has no bound URL")
	}
	evidence := ProbeEvidence{CapturedAt: time.Now().UTC()}
	var errs []error
	c := client.New(client.Config{
		BaseURL: baseURL, Directory: workDir, HTTPClient: httpClient,
		Username: evaluatorServerUsername, Password: password,
	})
	if health, err := s.Health(ctx); err != nil {
		errs = append(errs, err)
	} else {
		evidence.Health = health
	}
	if doc, err := c.GetPathContext(ctx); err != nil {
		errs = append(errs, fmt.Errorf("probe /path: %w", err))
	} else {
		evidence.Path = doc
		if err := verifyServerDirectory(doc.Body, workDir); err != nil {
			errs = append(errs, err)
		}
	}
	if doc, err := c.GetConfigContext(ctx); err != nil {
		errs = append(errs, fmt.Errorf("probe /config: %w", err))
	} else {
		evidence.Config = doc
	}
	if doc, err := c.GetAgentsContext(ctx); err != nil {
		errs = append(errs, fmt.Errorf("probe /agent: %w", err))
	} else {
		evidence.Agents = doc
	}
	if doc, err := c.GetToolIDsDocumentContext(ctx); err != nil {
		errs = append(errs, fmt.Errorf("probe /experimental/tool/ids: %w", err))
	} else {
		evidence.Tools = doc
	}
	if catalog, err := c.GetMCPStatusCatalogContext(ctx); err != nil {
		errs = append(errs, fmt.Errorf("probe /mcp: %w", err))
	} else {
		evidence.MCP = catalog
	}
	if providers, err := c.GetProviderCatalogContext(ctx); err != nil {
		errs = append(errs, fmt.Errorf("probe /provider: %w", err))
	} else {
		evidence.Providers = providers
	}
	if doc, err := c.GetOpenAPIDocumentContext(ctx); err != nil {
		errs = append(errs, fmt.Errorf("probe /doc: %w", err))
	} else {
		evidence.OpenAPI = doc
	}
	return evidence, errors.Join(errs...)
}

func verifyServerDirectory(raw json.RawMessage, expected string) error {
	var path struct {
		Directory string `json:"directory"`
		Worktree  string `json:"worktree"`
		CWD       string `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &path); err != nil {
		return fmt.Errorf("decode /path evidence: %w", err)
	}
	actual := path.Directory
	if actual == "" {
		actual = path.Worktree
	}
	if actual == "" {
		actual = path.CWD
	}
	if actual == "" {
		return errors.New("/path evidence does not expose a working directory")
	}
	actualAbs, err := filepath.Abs(actual)
	if err != nil {
		return fmt.Errorf("resolve /path working directory: %w", err)
	}
	if filepath.Clean(actualAbs) != filepath.Clean(expected) {
		return fmt.Errorf("OpenCode cwd mismatch: got %q, expected %q", actualAbs, expected)
	}
	return nil
}

// IsHealthy preserves the original lifecycle API.
func (s *Server) IsHealthy() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.HealthTimeout)
	defer cancel()
	info, err := s.Health(ctx)
	return info.Healthy, err
}

// Port returns the selected port. With Port:0 it becomes non-zero during Start.
func (s *Server) Port() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.port != 0 {
		return s.port
	}
	return s.cfg.Port
}

// BaseURL returns the bound URL; it is empty before a Port:0 server starts.
func (s *Server) BaseURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.baseURL
}

// Client returns an authenticated client for this private server without
// exposing the ephemeral Basic Auth secret to callers.
func (s *Server) Client(directory string) *client.Client {
	s.mu.RLock()
	baseURL := s.baseURL
	password := s.serverPassword
	httpClient := s.cfg.HTTPClient
	s.mu.RUnlock()
	return client.New(client.Config{
		BaseURL: baseURL, Directory: directory, HTTPClient: httpClient,
		Username: evaluatorServerUsername, Password: password,
	})
}

// LogPath returns the per-run private log path after Start begins.
func (s *Server) LogPath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.logPath
}

// RunDir returns the directory containing lifecycle artifacts.
func (s *Server) RunDir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runDir
}

// Version returns the last successful healthcheck version.
func (s *Server) Version() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.health.Version
}

// IsRunning reports whether readiness completed successfully.
func (s *Server) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state == stateRunning
}

func buildEnvironment(allowlist []string, overrides map[string]string, runDir string) ([]string, error) {
	// Caller-provided names extend the safe process baseline. Locale/timezone
	// are evaluator-owned constants, and TLS uses the platform trust store;
	// ambient certificate overrides are deliberately excluded.
	allowlist = append(append([]string(nil), safeInheritedEnvironment...), allowlist...)
	privateDirs, err := privateEnvironmentDirectories(runDir)
	if err != nil {
		return nil, err
	}
	complete := false
	defer func() {
		if !complete {
			cleanupEnvironmentDirectories(privateDirs)
		}
	}()
	values := privateDirs
	values["LANG"] = "C"
	values["LC_ALL"] = "C"
	values["TZ"] = "UTC"
	for _, key := range allowlist {
		if err := validateEnvKey(key); err != nil {
			return nil, err
		}
		if isReservedEvaluationEnvironment(key) {
			return nil, fmt.Errorf("environment key %q is reserved for evaluation isolation", key)
		}
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		if err := validateEnvKey(key); err != nil {
			return nil, err
		}
		if isReservedEvaluationEnvironment(key) {
			if key != EvaluatorManagedDetachEnvironment || value != "1" {
				return nil, fmt.Errorf("environment key %q is reserved for evaluation isolation", key)
			}
			values[key] = value
			continue
		}
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("environment value for %q contains NUL", key)
		}
		values[key] = value
	}
	pathValue, ok := values["PATH"]
	if !ok {
		return nil, fmt.Errorf("PATH is required for the evaluation runtime")
	}
	if err := validateExecutableSearchPath(pathValue); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	complete = true
	return env, nil
}

func validateConfiguredBinary(binary string) error {
	if binary == "" || strings.TrimSpace(binary) != binary || strings.IndexByte(binary, 0) >= 0 {
		return fmt.Errorf("invalid OpenCode binary path %q", binary)
	}
	if !filepath.IsAbs(binary) || filepath.Clean(binary) != binary {
		return fmt.Errorf("OpenCode binary must be a clean absolute path: %q", binary)
	}
	info, err := os.Lstat(binary)
	if err != nil {
		return fmt.Errorf("inspect OpenCode binary %q: %w", binary, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("OpenCode binary must be a non-symlink regular file: %q", binary)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("OpenCode binary is not executable: %q", binary)
	}
	return nil
}

func validateExecutableSearchPath(value string) error {
	if value == "" {
		return fmt.Errorf("PATH must not be empty")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("PATH contains NUL")
	}
	for index, segment := range strings.Split(value, string(os.PathListSeparator)) {
		if segment == "" {
			return fmt.Errorf("PATH segment %d is empty", index)
		}
		if !filepath.IsAbs(segment) {
			return fmt.Errorf("PATH segment %d is relative: %q", index, segment)
		}
	}
	return nil
}

func privateEnvironmentDirectories(runDir string) (map[string]string, error) {
	prefixes := map[string]string{
		"HOME":            "home-",
		"TMPDIR":          "tmp-",
		"XDG_CONFIG_HOME": "xdg-config-",
		"XDG_DATA_HOME":   "xdg-data-",
		"XDG_STATE_HOME":  "xdg-state-",
		"XDG_CACHE_HOME":  "xdg-cache-",
		"XDG_RUNTIME_DIR": "xdg-runtime-",
	}
	result := make(map[string]string, len(prefixes))
	for key, prefix := range prefixes {
		directory, err := os.MkdirTemp(runDir, prefix+"*")
		if err != nil {
			return nil, fmt.Errorf("create private %s directory: %w", key, err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("secure private %s directory: %w", key, err)
		}
		result[key] = directory
	}
	return result, nil
}

func cleanupEnvironmentDirectories(values map[string]string) {
	for _, path := range values {
		_ = os.RemoveAll(path)
	}
}

func privateEnvironmentPaths(env []string) []string {
	keys := map[string]bool{
		"HOME": true, "TMPDIR": true, "XDG_CONFIG_HOME": true,
		"XDG_DATA_HOME": true, "XDG_STATE_HOME": true,
		"XDG_CACHE_HOME": true, "XDG_RUNTIME_DIR": true,
	}
	paths := make([]string, 0, len(keys))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok && keys[key] {
			paths = append(paths, value)
		}
	}
	return paths
}

func isReservedEvaluationEnvironment(key string) bool {
	upper := strings.ToUpper(key)
	if upper == mcpproxy.ManifestEnvironment {
		return true
	}
	if upper == EvaluatorManagedDetachEnvironment {
		return true
	}
	if upper == "HOME" || upper == "TMPDIR" || upper == "TEMP" || upper == "TMP" ||
		upper == "APPDATA" || upper == "LOCALAPPDATA" || upper == "USERPROFILE" ||
		upper == "LANG" || upper == "LC_ALL" || upper == "TZ" ||
		upper == "SSL_CERT_FILE" || upper == "SSL_CERT_DIR" ||
		upper == "BASH_ENV" || upper == "ENV" || upper == "SHELLOPTS" || upper == "BASHOPTS" ||
		upper == "ZDOTDIR" || upper == "RUBYOPT" || upper == "RUBYLIB" ||
		upper == "PERL5OPT" || upper == "PERL5LIB" ||
		upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY" ||
		strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_") ||
		strings.HasPrefix(upper, "PYTHON") || strings.HasPrefix(upper, "XDG_") ||
		strings.HasPrefix(upper, "OPENCODE_") {
		return true
	}
	return upper == "NODE_OPTIONS" || upper == "NODE_PATH" || upper == "BUN_OPTIONS" ||
		upper == "DENO_DIR" || upper == "DENO_INSTALL_ROOT"
}

func lookupEnvironmentValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix), true
		}
	}
	return "", false
}

func replaceEnvironmentValue(env []string, key, value string) []string {
	prefix := key + "="
	replacement := prefix + value
	for index, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[index] = replacement
			return env
		}
	}
	return append(env, replacement)
}

func validateEnvKey(key string) error {
	if key == "" || strings.ContainsAny(key, "=\x00") {
		return fmt.Errorf("invalid environment key %q", key)
	}
	return nil
}

func randomServerPassword() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate private OpenCode server password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func validateExtraArgs(args []string) error {
	reserved := []string{"--port", "--hostname", "--mdns", "--mdns-domain", "--pure"}
	for _, arg := range args {
		for _, option := range reserved {
			if arg == option || strings.HasPrefix(arg, option+"=") {
				return fmt.Errorf("extra argument %q may override lifecycle isolation", arg)
			}
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func freeLoopbackPort(host string) (int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address %T", listener.Addr())
	}
	return address.Port, nil
}

func verifyPortAvailable(host string, port int) error {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
	if err != nil {
		return err
	}
	return listener.Close()
}
