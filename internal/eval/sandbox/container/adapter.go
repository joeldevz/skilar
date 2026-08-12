package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Adapter struct {
	config Config
}

const isolatedContainersConfig = `[containers]
default_capabilities=[]
devices=[]
env=[]
env_host=false
http_proxy=false
mounts=[]
volumes=[]
`

const processCleanupGrace = 2 * time.Second

func New(config Config) (*Adapter, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	return &Adapter{config: normalized}, nil
}

// BuildRunArgv validates request before returning the exact Podman arguments.
// It is useful for provenance recording and policy tests; Run always rebuilds
// the vector after acquiring its verified network lease.
func (a *Adapter) BuildRunArgv(request Request, cidFile string, lease NetworkLease) ([]string, error) {
	request, err := normalizeRequest(a.config, request)
	if err != nil {
		return nil, err
	}
	return buildRunArgv(request, cidFile, lease)
}

func (a *Adapter) Probe(ctx context.Context) ProbeReport {
	report := ProbeReport{
		NetworkPolicies: map[NetworkMode]Capability{
			NetworkNone: {Available: true, Evidence: "podman --network=none"},
		},
	}
	if ctx == nil {
		report.Issues = append(report.Issues, "nil probe context")
		return report
	}
	probeCtx, cancel := context.WithTimeout(ctx, a.config.ProbeTimeout)
	defer cancel()
	versionOutput, versionErr := a.runProbeCommand(probeCtx, []string{"version", "--format=json"})
	if versionErr != nil {
		report.Podman = Capability{Reason: versionErr.Error()}
		report.Issues = append(report.Issues, "podman version probe failed: "+versionErr.Error())
	} else {
		report.Podman = Capability{Available: true, Evidence: digestValue(json.RawMessage(versionOutput))}
		report.Version = parsePodmanVersion(versionOutput)
	}
	infoOutput, infoErr := a.runProbeCommand(probeCtx, []string{"info", "--format=json"})
	if infoErr != nil {
		report.Issues = append(report.Issues, "podman info probe failed: "+infoErr.Error())
	} else {
		report.Rootless, report.ResourceLimits = parsePodmanIsolation(infoOutput)
		if !report.Rootless {
			report.Issues = append(report.Issues, "podman is not reporting rootless mode")
		}
		if !report.ResourceLimits.Available {
			reason := report.ResourceLimits.Reason
			if reason == "" {
				reason = ErrResourceLimitsUnavailable.Error()
			}
			report.Issues = append(report.Issues, reason)
		}
	}
	if a.config.DiskBoundary == nil {
		report.DiskBoundary = Capability{Reason: ErrDiskBoundaryUnavailable.Error()}
		report.Issues = append(report.Issues, ErrDiskBoundaryUnavailable.Error())
	} else {
		report.DiskBoundary = a.config.DiskBoundary.Probe(probeCtx)
		if !report.DiskBoundary.Available {
			reason := report.DiskBoundary.Reason
			if reason == "" {
				reason = ErrDiskBoundaryUnavailable.Error()
			}
			report.Issues = append(report.Issues, reason)
		}
	}
	for _, mode := range []NetworkMode{NetworkProviderProxyOnly, NetworkRegistryAllowlist} {
		policy := NetworkPolicy{Mode: mode}
		if mode == NetworkProviderProxyOnly {
			policy.ProviderProxy = "https://provider.invalid"
		} else {
			policy.Registries = []string{"https://registry.invalid"}
		}
		if a.config.NetworkController == nil {
			report.NetworkPolicies[mode] = Capability{Reason: ErrNetworkPolicyUnavailable.Error()}
		} else {
			report.NetworkPolicies[mode] = a.config.NetworkController.Probe(probeCtx, policy)
		}
	}
	report.Ready = report.Podman.Available && infoErr == nil && report.Rootless && report.ResourceLimits.Available && report.DiskBoundary.Available
	return report
}

func (a *Adapter) Doctor(ctx context.Context, request Request) DoctorReport {
	report := DoctorReport{Probe: a.Probe(ctx)}
	request, err := normalizeRequest(a.config, request)
	if err != nil {
		report.Issues = append(report.Issues, err.Error())
		return report
	}
	if ctx == nil {
		report.Issues = append(report.Issues, "nil doctor context")
		return report
	}
	doctorCtx, cancel := context.WithTimeout(ctx, a.config.ProbeTimeout)
	defer cancel()
	if _, err := a.runProbeCommand(doctorCtx, []string{"image", "exists", request.Image}); err != nil {
		report.Issues = append(report.Issues, "pinned image is not present locally: "+err.Error())
	} else {
		report.ImagePresent = true
	}
	if a.config.DiskBoundary == nil {
		report.Issues = append(report.Issues, ErrDiskBoundaryUnavailable.Error())
	} else {
		evidence, err := a.config.DiskBoundary.Verify(doctorCtx, request.FixtureDir, request.Limits.DiskBytes)
		if err != nil {
			report.Issues = append(report.Issues, "disk boundary verification failed: "+err.Error())
		} else if !validDiskEvidence(evidence, request.Limits.DiskBytes) {
			report.Issues = append(report.Issues, "disk boundary returned invalid or weaker evidence")
		} else {
			report.Disk = evidence
		}
	}
	if request.Network.Mode == NetworkNone {
		report.Network = Capability{Available: true, Evidence: "podman --network=none"}
	} else if a.config.NetworkController == nil {
		report.Network = Capability{Reason: ErrNetworkPolicyUnavailable.Error()}
		report.Issues = append(report.Issues, ErrNetworkPolicyUnavailable.Error())
	} else {
		report.Network = a.config.NetworkController.Probe(doctorCtx, request.Network)
		if !report.Network.Available {
			reason := report.Network.Reason
			if reason == "" {
				reason = ErrNetworkPolicyUnavailable.Error()
			}
			report.Issues = append(report.Issues, reason)
		}
	}
	report.Ready = report.Probe.Ready && report.ImagePresent && report.Disk.Enforced && report.Network.Available && len(report.Issues) == 0
	return report
}

func (a *Adapter) Run(ctx context.Context, request Request) (Result, error) {
	result := Result{ExitCode: -1}
	finish := func(err error) (Result, error) {
		result.FinishedAt = time.Now().UTC()
		if !result.StartedAt.IsZero() {
			result.Duration = result.FinishedAt.Sub(result.StartedAt)
		}
		if err != nil {
			result.Error = err.Error()
		}
		return result, err
	}
	if ctx == nil {
		return finish(errors.New("nil run context"))
	}
	if err := ctx.Err(); err != nil {
		result.Canceled = true
		return finish(err)
	}
	var err error
	request, err = normalizeRequest(a.config, request)
	if err != nil {
		return finish(err)
	}
	result.NetworkMode = request.Network.Mode
	runCtx, cancel := context.WithTimeout(ctx, request.Limits.Timeout)
	defer cancel()
	finishBounded := func(err error) (Result, error) {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			result.TimedOut = true
			return finish(errors.Join(ErrTimeout, err))
		}
		if runCtx.Err() != nil {
			result.Canceled = true
			return finish(errors.Join(runCtx.Err(), err))
		}
		return finish(err)
	}
	if a.config.DiskBoundary == nil {
		return finish(ErrDiskBoundaryUnavailable)
	}
	if err := a.rejectExecutableAliases(runCtx, request); err != nil {
		return finishBounded(err)
	}
	if err := a.ensureRuntimeIsolation(runCtx); err != nil {
		return finishBounded(err)
	}
	disk, err := a.config.DiskBoundary.Verify(runCtx, request.FixtureDir, request.Limits.DiskBytes)
	if err != nil {
		return finishBounded(fmt.Errorf("verify disk boundary: %w", err))
	}
	if !validDiskEvidence(disk, request.Limits.DiskBytes) {
		return finish(fmt.Errorf("%w: invalid or weaker evidence", ErrDiskBoundaryUnavailable))
	}
	result.Disk = disk

	lease := NetworkLease{}
	leaseAcquired := false
	if request.Network.Mode != NetworkNone {
		if a.config.NetworkController == nil {
			return finish(ErrNetworkPolicyUnavailable)
		}
		lease, err = a.config.NetworkController.Prepare(runCtx, request.RunID, request.Network)
		if err != nil {
			return finishBounded(fmt.Errorf("prepare network policy: %w", err))
		}
		result.Network = lease
		if err := validateNetworkLease(lease); err != nil {
			releaseErr := a.releaseNetwork(lease)
			result.NetworkReleased = releaseErr == nil
			result.NetworkRetained = releaseErr != nil
			return finish(errors.Join(fmt.Errorf("prepare network policy: %w", err), releaseErr))
		}
		leaseAcquired = true
	}
	releaseBeforeStart := func(cause error) (Result, error) {
		if leaseAcquired {
			releaseErr := a.releaseNetwork(lease)
			result.NetworkReleased = releaseErr == nil
			result.NetworkRetained = releaseErr != nil
			leaseAcquired = false
			cause = errors.Join(cause, releaseErr)
		}
		return finishBounded(cause)
	}

	createdControlDir, err := os.MkdirTemp("", "skynex-eval-container-")
	if err != nil {
		return releaseBeforeStart(fmt.Errorf("create private container control directory: %w", err))
	}
	controlDir, err := canonicalDirectory(createdControlDir, "private container control directory")
	if err != nil {
		_ = removeControlDir(createdControlDir)
		return releaseBeforeStart(err)
	}
	if pathsOverlap(controlDir, request.FixtureDir) || pathsOverlap(controlDir, request.ConfigSource) {
		_ = removeControlDir(controlDir)
		return releaseBeforeStart(errors.New("private container control directory overlaps a mounted source"))
	}
	for _, bundle := range request.Bundles {
		if pathsOverlap(controlDir, bundle.Source) {
			_ = removeControlDir(controlDir)
			return releaseBeforeStart(fmt.Errorf("private container control directory overlaps bundle %q", bundle.Name))
		}
	}
	if err := prepareControlDir(controlDir); err != nil {
		_ = removeControlDir(controlDir)
		return releaseBeforeStart(err)
	}
	cidFile := controlDir + string(os.PathSeparator) + "container.cid"
	defer func() {
		_ = removeControlDir(controlDir)
	}()
	argv, err := buildRunArgv(request, cidFile, lease)
	if err != nil {
		return releaseBeforeStart(err)
	}
	result.Argv = append([]string(nil), argv...)
	result.ContainerName = containerName(request.RunID, cidFile)

	limitReached := make(chan struct{}, 1)
	stdout := newBoundedBuffer(request.Limits.MaxStdoutBytes, limitReached)
	stderr := newBoundedBuffer(request.Limits.MaxStderrBytes, limitReached)
	cmd := exec.Command(a.config.PodmanPath, argv...)
	cmd.Args[0] = a.config.PodmanPath
	cmd.Env = a.hostEnvironment(controlDir)
	cmd.Dir = controlDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	configureProcess(cmd)

	result.StartedAt = time.Now().UTC()
	if err := cmd.Start(); err != nil {
		return releaseBeforeStart(fmt.Errorf("start podman: %w", err))
	}
	result.Started = true
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var waitErr error
	var forcedErr error
	waitCompleted := true
	select {
	case waitErr = <-waitCh:
	case <-limitReached:
		result.OutputLimitExceeded = true
		forcedErr = ErrOutputLimit
		var stopErr error
		waitErr, waitCompleted, stopErr = stopProcessAndWait(cmd, waitCh)
		forcedErr = errors.Join(forcedErr, stopErr)
	case <-runCtx.Done():
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			result.TimedOut = true
			forcedErr = ErrTimeout
		} else {
			result.Canceled = true
			forcedErr = runCtx.Err()
		}
		var stopErr error
		waitErr, waitCompleted, stopErr = stopProcessAndWait(cmd, waitCh)
		forcedErr = errors.Join(forcedErr, stopErr)
	}
	result.Completed = waitCompleted
	result.Stdout, result.StdoutTruncated = stdout.snapshot()
	result.Stderr, result.StderrTruncated = stderr.snapshot()
	if forcedErr == nil && (result.StdoutTruncated || result.StderrTruncated) {
		result.OutputLimitExceeded = true
		forcedErr = ErrOutputLimit
	}
	if waitCompleted {
		result.Signal = signalName(cmd.ProcessState)
		if cmd.ProcessState != nil {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
	}

	result.CleanupAttempted = true
	containerID, cleanupErr := a.removeContainer(result.ContainerName, cidFile, controlDir)
	result.ContainerID = containerID
	if !waitCompleted {
		// A successful rm before the creator has terminated is not proof of
		// absence: the still-running Podman process could create the container
		// after the rm lookup. Keep any egress policy for an external reaper.
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%w: podman creator termination is unproven", ErrProcessCleanup))
	}
	result.CleanupSucceeded = cleanupErr == nil
	if leaseAcquired {
		if cleanupErr == nil {
			releaseErr := a.releaseNetwork(lease)
			result.NetworkReleased = releaseErr == nil
			if releaseErr != nil {
				cleanupErr = fmt.Errorf("release network: %w", releaseErr)
				result.CleanupSucceeded = false
				result.NetworkRetained = true
			}
		} else {
			// A failed rm means the container may still be alive. Keep the
			// enforcing network in place for a reaper instead of silently
			// widening egress for a stranded process.
			result.NetworkRetained = true
			cleanupErr = errors.Join(cleanupErr, errors.New("network policy retained because container absence was not proven"))
		}
		leaseAcquired = false
	}
	if forcedErr != nil {
		return finish(errors.Join(forcedErr, cleanupErr))
	}
	if cleanupErr != nil {
		return finish(fmt.Errorf("container cleanup: %w", cleanupErr))
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			return finish(fmt.Errorf("wait for podman: %w", waitErr))
		}
	}
	return finish(nil)
}

func (a *Adapter) rejectExecutableAliases(ctx context.Context, request Request) error {
	executableInfo, err := os.Stat(a.config.PodmanPath)
	if err != nil {
		return fmt.Errorf("stat Podman executable before run: %w", err)
	}
	sources := []string{request.FixtureDir, request.ConfigSource}
	for _, bundle := range request.Bundles {
		sources = append(sources, bundle.Source)
	}
	for _, source := range sources {
		err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("mounted source contains a symlink: %s", path)
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("mounted source contains a special file: %s", path)
			}
			if fileLinkCount(info) != 1 {
				return fmt.Errorf("mounted source contains a hard-linked file: %s", path)
			}
			if os.SameFile(executableInfo, info) {
				return fmt.Errorf("mounted source contains a hardlink to the host Podman executable: %s", path)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("inspect mounted source for Podman executable aliases: %w", err)
		}
	}
	return nil
}

func (a *Adapter) removeContainer(name string, cidFile string, controlDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), a.config.ProbeTimeout)
	defer cancel()
	containerID, idErr := readContainerID(cidFile)
	argv := []string{"rm", "--force", "--ignore", "--volumes", name}
	output, err := a.runControlCommand(ctx, controlDir, argv, 4096)
	if err != nil {
		return containerID, errors.Join(idErr, fmt.Errorf("podman rm: %w: %s", err, boundedText(output, 4096)))
	}
	return containerID, idErr
}

func (a *Adapter) runProbeCommand(ctx context.Context, argv []string) ([]byte, error) {
	controlDir, err := os.MkdirTemp("", "skynex-eval-container-")
	if err != nil {
		return nil, fmt.Errorf("create private probe control directory: %w", err)
	}
	if err := prepareControlDir(controlDir); err != nil {
		_ = removeControlDir(controlDir)
		return nil, err
	}
	defer func() { _ = removeControlDir(controlDir) }()
	output, err := a.runControlCommand(ctx, controlDir, argv, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("podman %s: %w: %s", strings.Join(argv, " "), err, boundedText(output, 4096))
	}
	return output, nil
}

func (a *Adapter) ensureRuntimeIsolation(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, a.config.ProbeTimeout)
	defer cancel()
	output, err := a.runProbeCommand(probeCtx, []string{"info", "--format=json"})
	if err != nil {
		return fmt.Errorf("verify rootless podman: %w", err)
	}
	rootless, limits := parsePodmanIsolation(output)
	if !rootless {
		return ErrRootlessRequired
	}
	if !limits.Available {
		return fmt.Errorf("%w: %s", ErrResourceLimitsUnavailable, limits.Reason)
	}
	return nil
}

func readContainerID(cidFile string) (string, error) {
	raw, err := os.ReadFile(cidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read podman cidfile: %w", err)
	}
	containerID := strings.TrimSpace(string(raw))
	if len(containerID) != 64 {
		return "", fmt.Errorf("podman cidfile contains an invalid container ID")
	}
	for _, char := range containerID {
		if char < '0' || char > '9' {
			if char < 'a' || char > 'f' {
				return "", fmt.Errorf("podman cidfile contains an invalid container ID")
			}
		}
	}
	return containerID, nil
}

func (a *Adapter) runControlCommand(ctx context.Context, controlDir string, argv []string, maxOutput int64) ([]byte, error) {
	limitReached := make(chan struct{}, 1)
	output := newBoundedBuffer(maxOutput, limitReached)
	cmd := exec.Command(a.config.PodmanPath, argv...)
	cmd.Args[0] = a.config.PodmanPath
	cmd.Env = a.hostEnvironment(controlDir)
	cmd.Dir = controlDir
	cmd.Stdout = output
	cmd.Stderr = output
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var waitErr error
	var stopErr error
	select {
	case waitErr = <-waitCh:
	case <-limitReached:
		waitErr, _, stopErr = stopProcessAndWait(cmd, waitCh)
	case <-ctx.Done():
		waitErr, _, stopErr = stopProcessAndWait(cmd, waitCh)
	}
	raw, truncated := output.snapshot()
	if ctx.Err() != nil {
		return []byte(raw), errors.Join(ctx.Err(), stopErr)
	}
	if truncated {
		return []byte(raw), errors.Join(ErrOutputLimit, stopErr)
	}
	return []byte(raw), errors.Join(waitErr, stopErr)
}

func stopProcessAndWait(cmd *exec.Cmd, waitCh <-chan error) (error, bool, error) {
	killErr := killProcessTree(cmd.Process)
	timer := time.NewTimer(processCleanupGrace)
	defer timer.Stop()
	select {
	case waitErr := <-waitCh:
		return waitErr, true, killErr
	case <-timer.C:
		return nil, false, errors.Join(killErr, ErrProcessCleanup)
	}
}

func (a *Adapter) releaseNetwork(lease NetworkLease) error {
	ctx, cancel := context.WithTimeout(context.Background(), a.config.ProbeTimeout)
	defer cancel()
	return a.config.NetworkController.Release(ctx, lease)
}

func (a *Adapter) hostEnvironment(controlDir string) []string {
	values := map[string]string{
		"CONTAINERS_CONF": isolatedConfigPath(controlDir),
		"HOME":            controlDir + string(os.PathSeparator) + "home",
		"LANG":            "C",
		"LC_ALL":          "C",
		"PATH":            "/usr/sbin:/usr/bin:/sbin:/bin",
		"TMPDIR":          controlDir + string(os.PathSeparator) + "tmp",
		"TZ":              "UTC",
		"XDG_CONFIG_HOME": controlDir + string(os.PathSeparator) + "config",
	}
	for key, value := range a.config.PodmanEnvironment {
		values[key] = value
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
	return env
}

func isolatedConfigPath(controlDir string) string {
	return controlDir + string(os.PathSeparator) + "containers.conf"
}

func prepareControlDir(controlDir string) error {
	if err := os.Chmod(controlDir, 0o700); err != nil {
		return fmt.Errorf("restrict container control directory: %w", err)
	}
	for _, name := range []string{"config", "home", "hooks", "tmp"} {
		if err := os.Mkdir(controlDir+string(os.PathSeparator)+name, 0o700); err != nil {
			return fmt.Errorf("create private Podman %s directory: %w", name, err)
		}
	}
	if err := os.WriteFile(isolatedConfigPath(controlDir), []byte(isolatedContainersConfig), 0o600); err != nil {
		return fmt.Errorf("write isolated Podman config: %w", err)
	}
	return nil
}

func removeControlDir(controlDir string) error {
	if !filepath.IsAbs(controlDir) || !strings.HasPrefix(filepath.Base(controlDir), "skynex-eval-container-") {
		return fmt.Errorf("refuse to remove unresolved control directory %q", controlDir)
	}
	info, err := os.Lstat(controlDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to remove non-directory control path %q", controlDir)
	}
	return os.RemoveAll(controlDir)
}

func parsePodmanVersion(raw []byte) string {
	var payload struct {
		Client struct {
			Version string `json:"Version"`
		} `json:"Client"`
		Version struct {
			Version string `json:"Version"`
		} `json:"version"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	if payload.Client.Version != "" {
		return payload.Client.Version
	}
	return payload.Version.Version
}

func parsePodmanIsolation(raw []byte) (bool, Capability) {
	var payload struct {
		Host struct {
			CgroupVersion     string   `json:"cgroupVersion"`
			CgroupControllers []string `json:"cgroupControllers"`
			Security          struct {
				Rootless bool `json:"rootless"`
			} `json:"security"`
		} `json:"host"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, Capability{Reason: "decode podman cgroup capability: " + err.Error()}
	}
	if payload.Host.CgroupVersion != "v2" {
		return payload.Host.Security.Rootless, Capability{Reason: ErrResourceLimitsUnavailable.Error() + ": cgroup v2 is required"}
	}
	controllers := make(map[string]struct{}, len(payload.Host.CgroupControllers))
	for _, controller := range payload.Host.CgroupControllers {
		controllers[strings.ToLower(controller)] = struct{}{}
	}
	for _, required := range []string{"cpu", "memory", "pids"} {
		if _, available := controllers[required]; !available {
			return payload.Host.Security.Rootless, Capability{Reason: ErrResourceLimitsUnavailable.Error() + ": missing delegated " + required + " controller"}
		}
	}
	evidence := digestValue(map[string]any{"version": payload.Host.CgroupVersion, "controllers": []string{"cpu", "memory", "pids"}})
	return payload.Host.Security.Rootless, Capability{Available: true, Evidence: evidence}
}

func boundedText(value []byte, limit int) string {
	if len(value) > limit {
		return string(value[:limit]) + "...[truncated]"
	}
	return string(value)
}
