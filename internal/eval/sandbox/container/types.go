// Package container provides the isolated-container execution boundary used by
// the evaluator. It deliberately supports only argv-based Podman invocation and
// fails closed when a requested boundary cannot be proven by the host.
package container

import (
	"context"
	"errors"
	"time"
)

const (
	WorkspacePath = "/workspace"
	ConfigPath    = "/eval/config"
	BundlesPath   = "/eval/bundles"
)

var (
	ErrImageNotPinned            = errors.New("container image must be pinned by sha256 digest")
	ErrNetworkPolicyUnavailable  = errors.New("requested network policy has no enforcing backend")
	ErrDiskBoundaryUnavailable   = errors.New("writable fixture has no verified disk boundary")
	ErrOutputLimit               = errors.New("container output limit exceeded")
	ErrTimeout                   = errors.New("container execution timed out")
	ErrRootlessRequired          = errors.New("podman must report rootless mode")
	ErrResourceLimitsUnavailable = errors.New("podman rootless resource limits are not enforceable")
	ErrProcessCleanup            = errors.New("podman process did not terminate within cleanup grace period")
	ErrUnsafePodmanConfig        = errors.New("host Podman configuration can inject undeclared authority")
)

type NetworkMode string

const (
	NetworkNone              NetworkMode = "none"
	NetworkProviderProxyOnly NetworkMode = "provider-proxy-only"
	NetworkRegistryAllowlist NetworkMode = "registry-allowlist"
)

// NetworkPolicy is the logical egress policy. ProviderProxy and Registries are
// policy inputs for an enforcing NetworkController; they are never converted
// directly into an unrestricted Podman network argument.
type NetworkPolicy struct {
	Mode          NetworkMode `json:"mode"`
	ProviderProxy string      `json:"provider_proxy,omitempty"`
	Registries    []string    `json:"registries,omitempty"`
}

// NetworkLease is returned only after a NetworkController has installed and
// verified its egress policy. NetworkName is the preconfigured Podman network;
// EvidenceDigest identifies the controller rules used for this run.
type NetworkLease struct {
	NetworkName    string `json:"network_name"`
	EvidenceDigest string `json:"evidence_digest"`
}

type Capability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
}

// NetworkController owns firewall/proxy setup outside this package. The
// adapter never trusts a network name supplied by a case or fixture.
type NetworkController interface {
	Probe(context.Context, NetworkPolicy) Capability
	Prepare(context.Context, string, NetworkPolicy) (NetworkLease, error)
	Release(context.Context, NetworkLease) error
}

type DiskEvidence struct {
	Enforced       bool   `json:"enforced"`
	LimitBytes     int64  `json:"limit_bytes"`
	EvidenceDigest string `json:"evidence_digest"`
}

// DiskBoundary verifies an already-enforced quota for the writable fixture.
// A directory-size monitor is intentionally not accepted as a hard boundary:
// it can observe an overflow only after host disk has already been consumed.
type DiskBoundary interface {
	Probe(context.Context) Capability
	Verify(context.Context, string, int64) (DiskEvidence, error)
}

type Limits struct {
	CPUs           float64       `json:"cpus"`
	MemoryBytes    int64         `json:"memory_bytes"`
	PIDs           int           `json:"pids"`
	DiskBytes      int64         `json:"disk_bytes"`
	TmpfsBytes     int64         `json:"tmpfs_bytes"`
	MaxStdoutBytes int64         `json:"max_stdout_bytes"`
	MaxStderrBytes int64         `json:"max_stderr_bytes"`
	Timeout        time.Duration `json:"timeout"`
}

func DefaultLimits() Limits {
	return Limits{
		CPUs:           1,
		MemoryBytes:    1 << 30,
		PIDs:           128,
		DiskBytes:      2 << 30,
		TmpfsBytes:     128 << 20,
		MaxStdoutBytes: 4 << 20,
		MaxStderrBytes: 4 << 20,
		Timeout:        10 * time.Minute,
	}
}

// Config contains control-plane settings only. PodmanEnvironment is explicit
// and reduced; the parent process environment is never inherited. Rootless
// callers normally declare XDG_DATA_HOME (local image storage) and
// XDG_RUNTIME_DIR. HOME, config paths, loader variables, connection selectors,
// and credential-bearing variables are evaluator-owned or forbidden.
type Config struct {
	PodmanPath          string
	PodmanEnvironment   map[string]string
	AllowedContainerEnv []string
	NetworkController   NetworkController
	DiskBoundary        DiskBoundary
	ProbeTimeout        time.Duration
}

type BundleMount struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// Request is one container invocation. Argv is passed after the image without
// a shell. ConfigSource is the evaluator-owned XDG_CONFIG_HOME root (for
// OpenCode it contains opencode/opencode.json); it and bundle sources are
// mounted read-only. FixtureDir is the sole writable host bind mount.
type Request struct {
	RunID        string
	Image        string
	FixtureDir   string
	ConfigSource string
	Bundles      []BundleMount
	Argv         []string
	WorkDir      string
	Environment  map[string]string
	Network      NetworkPolicy
	Limits       Limits
}

type ProbeReport struct {
	Ready           bool                       `json:"ready"`
	Podman          Capability                 `json:"podman"`
	Version         string                     `json:"version,omitempty"`
	Rootless        bool                       `json:"rootless"`
	ResourceLimits  Capability                 `json:"resource_limits"`
	DiskBoundary    Capability                 `json:"disk_boundary"`
	NetworkPolicies map[NetworkMode]Capability `json:"network_policies"`
	Issues          []string                   `json:"issues,omitempty"`
}

type DoctorReport struct {
	Ready        bool         `json:"ready"`
	Probe        ProbeReport  `json:"probe"`
	ImagePresent bool         `json:"image_present"`
	Disk         DiskEvidence `json:"disk"`
	Network      Capability   `json:"network"`
	Issues       []string     `json:"issues,omitempty"`
}

type Result struct {
	Argv                []string      `json:"argv"`
	Started             bool          `json:"started"`
	Completed           bool          `json:"completed"`
	ExitCode            int           `json:"exit_code"`
	Signal              string        `json:"signal,omitempty"`
	TimedOut            bool          `json:"timed_out"`
	Canceled            bool          `json:"canceled"`
	OutputLimitExceeded bool          `json:"output_limit_exceeded"`
	Stdout              string        `json:"stdout"`
	Stderr              string        `json:"stderr"`
	StdoutTruncated     bool          `json:"stdout_truncated"`
	StderrTruncated     bool          `json:"stderr_truncated"`
	CleanupAttempted    bool          `json:"cleanup_attempted"`
	CleanupSucceeded    bool          `json:"cleanup_succeeded"`
	ContainerName       string        `json:"container_name,omitempty"`
	ContainerID         string        `json:"container_id,omitempty"`
	Disk                DiskEvidence  `json:"disk"`
	NetworkMode         NetworkMode   `json:"network_mode"`
	Network             NetworkLease  `json:"network,omitempty"`
	NetworkReleased     bool          `json:"network_released,omitempty"`
	NetworkRetained     bool          `json:"network_retained_for_cleanup,omitempty"`
	StartedAt           time.Time     `json:"started_at,omitempty"`
	FinishedAt          time.Time     `json:"finished_at,omitempty"`
	Duration            time.Duration `json:"duration"`
	Error               string        `json:"error,omitempty"`
}

func (r Result) Successful() bool {
	return r.Started && r.Completed && r.ExitCode == 0 && r.Error == "" &&
		!r.TimedOut && !r.Canceled && !r.OutputLimitExceeded &&
		(!r.CleanupAttempted || r.CleanupSucceeded)
}
