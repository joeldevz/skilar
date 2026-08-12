package container

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const fakePodmanMarker = "SKYNEX_EVAL_FAKE_PODMAN"

type fakeInvocation struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
}

func TestMain(m *testing.M) {
	if os.Getenv(fakePodmanMarker) == "1" {
		fakePodmanMain()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakePodmanMain() {
	args := os.Args[1:]
	if logPath := os.Getenv("SKYNEX_EVAL_FAKE_LOG"); logPath != "" {
		file, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			os.Exit(91)
		}
		_ = json.NewEncoder(file).Encode(fakeInvocation{Argv: args, Env: os.Environ()})
		_ = file.Close()
	}
	if len(args) == 0 {
		os.Exit(92)
	}
	commandArgs := args
	if strings.HasPrefix(commandArgs[0], "--hooks-dir=") {
		commandArgs = commandArgs[1:]
	}
	if len(commandArgs) == 0 {
		os.Exit(92)
	}
	switch commandArgs[0] {
	case "version":
		_, _ = fmt.Fprintln(os.Stdout, `{"Client":{"Version":"5.4.2"}}`)
	case "info":
		rootless := os.Getenv("SKYNEX_EVAL_FAKE_MODE") != "rootful"
		cgroupVersion := "v2"
		controllers := []string{"cpu", "memory", "pids"}
		if os.Getenv("SKYNEX_EVAL_FAKE_MODE") == "cgroup-v1" {
			cgroupVersion = "v1"
		}
		if os.Getenv("SKYNEX_EVAL_FAKE_MODE") == "missing-memory-controller" {
			controllers = []string{"cpu", "pids"}
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"host": map[string]any{
			"security": map[string]bool{"rootless": rootless}, "cgroupVersion": cgroupVersion, "cgroupControllers": controllers,
		}})
	case "image":
		if len(commandArgs) != 3 || commandArgs[1] != "exists" || strings.Contains(commandArgs[2], "missing") {
			os.Exit(1)
		}
	case "run":
		for _, arg := range commandArgs {
			if strings.HasPrefix(arg, "--cidfile=") {
				_ = os.WriteFile(strings.TrimPrefix(arg, "--cidfile="), []byte(strings.Repeat("c", 64)+"\n"), 0o600)
			}
		}
		switch os.Getenv("SKYNEX_EVAL_FAKE_MODE") {
		case "sleep":
			time.Sleep(10 * time.Second)
		case "spam":
			_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", 64<<10))
		default:
			_, _ = fmt.Fprintln(os.Stdout, "fake container output")
		}
	case "rm":
		if os.Getenv("SKYNEX_EVAL_FAKE_MODE") == "cleanup-fail" {
			os.Exit(7)
		}
	default:
		os.Exit(93)
	}
}

type testDiskBoundary struct {
	available bool
	enforced  bool
}

type blockingDiskBoundary struct{}

func (blockingDiskBoundary) Probe(context.Context) Capability {
	return Capability{Available: true}
}

func (blockingDiskBoundary) Verify(ctx context.Context, _ string, _ int64) (DiskEvidence, error) {
	<-ctx.Done()
	return DiskEvidence{}, ctx.Err()
}

type testNetworkController struct {
	prepared []NetworkPolicy
	released int
}

func (c *testNetworkController) Probe(context.Context, NetworkPolicy) Capability {
	return Capability{Available: true, Evidence: "test firewall backend"}
}

func (c *testNetworkController) Prepare(_ context.Context, _ string, policy NetworkPolicy) (NetworkLease, error) {
	c.prepared = append(c.prepared, policy)
	return NetworkLease{NetworkName: "eval-egress-001", EvidenceDigest: digestValue(policy)}, nil
}

func (c *testNetworkController) Release(context.Context, NetworkLease) error {
	c.released++
	return nil
}

func (d testDiskBoundary) Probe(context.Context) Capability {
	return Capability{Available: d.available, Evidence: "test quota backend"}
}

func (d testDiskBoundary) Verify(_ context.Context, _ string, limit int64) (DiskEvidence, error) {
	return DiskEvidence{Enforced: d.enforced, LimitBytes: limit, EvidenceDigest: digestValue("test-disk-quota")}, nil
}

func newTestAdapter(t *testing.T, mode string) (*Adapter, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "podman.jsonl")
	environment := map[string]string{fakePodmanMarker: "1", "SKYNEX_EVAL_FAKE_LOG": logPath}
	if mode != "" {
		environment["SKYNEX_EVAL_FAKE_MODE"] = mode
	}
	adapter, err := New(Config{
		PodmanPath:          executable,
		PodmanEnvironment:   environment,
		AllowedContainerEnv: []string{"CASE_ID"},
		DiskBoundary:        testDiskBoundary{available: true, enforced: true},
		ProbeTimeout:        10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter, logPath
}

func testRequest(t *testing.T) Request {
	t.Helper()
	fixture := t.TempDir()
	if err := os.Mkdir(filepath.Join(fixture, "project"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := t.TempDir()
	bundleA := t.TempDir()
	bundleZ := t.TempDir()
	return Request{
		RunID:        "case-001",
		Image:        "registry.invalid/skynex/eval@sha256:" + strings.Repeat("a", 64),
		FixtureDir:   fixture,
		ConfigSource: config,
		Bundles: []BundleMount{
			{Name: "zeta", Source: bundleZ},
			{Name: "alpha", Source: bundleA},
		},
		Argv:        []string{"go", "test", "./...", "literal;not-a-shell"},
		WorkDir:     "/workspace/project",
		Environment: map[string]string{"CASE_ID": "case-001"},
		Network:     NetworkPolicy{Mode: NetworkNone},
		Limits: Limits{
			CPUs: .5, MemoryBytes: 64 << 20, PIDs: 32, DiskBytes: 128 << 20,
			TmpfsBytes: 16 << 20, MaxStdoutBytes: 4096, MaxStderrBytes: 2048,
			Timeout: 15 * time.Second,
		},
	}
}

func TestBuildRunArgvExactAndFailClosed(t *testing.T) {
	adapter, _ := newTestAdapter(t, "")
	request := testRequest(t)
	cidFile := filepath.Join(t.TempDir(), "container.cid")
	got, err := adapter.BuildRunArgv(request, cidFile, NetworkLease{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--hooks-dir=" + filepath.Join(filepath.Dir(cidFile), "hooks"),
		"run", "--rm", "--name=" + containerName("case-001", cidFile), "--cidfile=" + cidFile,
		"--pull=never", "--no-healthcheck", "--read-only", "--read-only-tmpfs=false", "--image-volume=ignore", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--userns=keep-id", "--ipc=private", "--pid=private", "--uts=private",
		"--hostname=skynex-eval", "--log-driver=none", "--stop-timeout=1", "--unsetenv-all",
		"--network=none", "--cpus=0.5", "--memory=67108864", "--memory-swap=67108864",
		"--pids-limit=32",
		"--tmpfs=/tmp:rw,exec,nosuid,nodev,mode=1777,size=16777216",
		"--mount=type=bind,src=" + request.FixtureDir + ",dst=/workspace,readonly=false,bind-propagation=rprivate,bind-nonrecursive",
		"--mount=type=bind,src=" + request.ConfigSource + ",dst=/eval/config,readonly=true,bind-propagation=rprivate,bind-nonrecursive",
		"--mount=type=bind,src=" + request.Bundles[1].Source + ",dst=/eval/bundles/alpha,readonly=true,bind-propagation=rprivate,bind-nonrecursive",
		"--mount=type=bind,src=" + request.Bundles[0].Source + ",dst=/eval/bundles/zeta,readonly=true,bind-propagation=rprivate,bind-nonrecursive",
		"--workdir=/workspace/project", `--entrypoint=["go"]`, "--env=HOME=/tmp/home", "--env=LANG=C", "--env=LC_ALL=C",
		"--env=PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--env=TMPDIR=/tmp", "--env=TZ=UTC", "--env=XDG_CACHE_HOME=/tmp/cache", "--env=XDG_CONFIG_HOME=/eval/config",
		"--env=XDG_DATA_HOME=/tmp/data", "--env=XDG_STATE_HOME=/tmp/state", "--env=CASE_ID=case-001",
		request.Image, "test", "./...", "literal;not-a-shell",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv mismatch\n got: %#v\nwant: %#v", got, want)
	}

	for _, image := range []string{
		"registry.invalid/eval:latest",
		"registry.invalid/eval@sha256:" + strings.Repeat("A", 64),
		"sha256:" + strings.Repeat("a", 64),
		"dir:/tmp/image@sha256:" + strings.Repeat("a", 64),
		"docker-daemon:eval@sha256:" + strings.Repeat("a", 64),
		"oci:/tmp/image@sha256:" + strings.Repeat("a", 64),
	} {
		invalid := request
		invalid.Image = image
		if _, err := adapter.BuildRunArgv(invalid, cidFile, NetworkLease{}); !errors.Is(err, ErrImageNotPinned) {
			t.Errorf("image %q error = %v, want ErrImageNotPinned", image, err)
		}
	}
	jsonEntrypoint := request
	jsonEntrypoint.Argv = []string{`["/bin/sh","-c"]`, "echo should-not-be-shell-code"}
	argv, err := adapter.BuildRunArgv(jsonEntrypoint, cidFile, NetworkLease{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, `--entrypoint=["[\"/bin/sh\",\"-c\"]"]`) {
		t.Fatalf("JSON-shaped executable was not encoded as one literal entrypoint: %#v", argv)
	}
	provider := request
	provider.Network = NetworkPolicy{Mode: NetworkProviderProxyOnly, ProviderProxy: "https://provider.invalid"}
	if _, err := adapter.BuildRunArgv(provider, cidFile, NetworkLease{}); err == nil {
		t.Fatal("provider network accepted without a verified lease")
	}
	provider.Network.ProviderProxy = "http://provider.invalid"
	if _, err := adapter.BuildRunArgv(provider, cidFile, NetworkLease{}); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("plaintext remote provider proxy error = %v", err)
	}
	provider.Network.ProviderProxy = "http://127.0.0.1:8080"
	if _, err := adapter.BuildRunArgv(provider, cidFile, NetworkLease{NetworkName: "skynex-provider", EvidenceDigest: "sha256:" + strings.Repeat("1", 64)}); err != nil {
		t.Fatalf("loopback provider proxy was rejected: %v", err)
	}
}

func TestRunUsesReducedEnvironmentAndCleansContainer(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "must-not-reach-podman")
	adapter, logPath := newTestAdapter(t, "")
	result, err := adapter.Run(context.Background(), testRequest(t))
	if err != nil {
		t.Fatalf("Run() error = %v; result = %#v", err, result)
	}
	if !result.Successful() || result.NetworkMode != NetworkNone || !result.Disk.Enforced {
		t.Fatalf("unexpected result: %#v", result)
	}
	if strings.TrimSpace(result.Stdout) != "fake container output" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	invocations := readInvocations(t, logPath)
	if len(invocations) != 3 || invocations[0].Argv[0] != "info" || len(invocations[1].Argv) < 2 || invocations[1].Argv[1] != "run" || !slices.Equal(invocations[2].Argv, []string{"rm", "--force", "--ignore", "--volumes", result.ContainerName}) {
		t.Fatalf("invocations = %#v", invocations)
	}
	for _, invocation := range invocations {
		for _, entry := range invocation.Env {
			if strings.Contains(entry, "GITHUB_TOKEN") || strings.Contains(entry, "must-not-reach-podman") {
				t.Fatalf("ambient credential leaked into podman environment: %q", entry)
			}
		}
	}
}

func TestRunLimitsAndPolicyFailures(t *testing.T) {
	t.Run("timeout cleans container", func(t *testing.T) {
		adapter, logPath := newTestAdapter(t, "sleep")
		request := testRequest(t)
		request.Limits.Timeout = 5 * time.Second
		result, err := adapter.Run(context.Background(), request)
		if !errors.Is(err, ErrTimeout) || !result.TimedOut || !result.CleanupSucceeded {
			t.Fatalf("result = %#v, err = %v", result, err)
		}
		invocations := readInvocations(t, logPath)
		if len(invocations) != 3 || invocations[2].Argv[0] != "rm" {
			t.Fatalf("cleanup invocations = %#v", invocations)
		}
	})

	t.Run("output cap cleans container", func(t *testing.T) {
		adapter, _ := newTestAdapter(t, "spam")
		request := testRequest(t)
		request.Limits.MaxStdoutBytes = 32
		result, err := adapter.Run(context.Background(), request)
		if !errors.Is(err, ErrOutputLimit) || !result.OutputLimitExceeded || !result.StdoutTruncated || len(result.Stdout) != 32 || !result.CleanupSucceeded {
			t.Fatalf("result = %#v, err = %v", result, err)
		}
	})

	t.Run("network policies fail closed", func(t *testing.T) {
		adapter, _ := newTestAdapter(t, "")
		request := testRequest(t)
		request.Network = NetworkPolicy{Mode: NetworkProviderProxyOnly, ProviderProxy: "https://provider.invalid"}
		_, err := adapter.Run(context.Background(), request)
		if !errors.Is(err, ErrNetworkPolicyUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("disk boundary is mandatory", func(t *testing.T) {
		executable, _ := os.Executable()
		adapter, err := New(Config{PodmanPath: executable, AllowedContainerEnv: []string{"CASE_ID"}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = adapter.Run(context.Background(), testRequest(t))
		if !errors.Is(err, ErrDiskBoundaryUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("rootless podman is mandatory", func(t *testing.T) {
		adapter, logPath := newTestAdapter(t, "rootful")
		result, err := adapter.Run(context.Background(), testRequest(t))
		if !errors.Is(err, ErrRootlessRequired) || result.Started {
			t.Fatalf("result = %#v, error = %v", result, err)
		}
		invocations := readInvocations(t, logPath)
		if len(invocations) != 1 || invocations[0].Argv[0] != "info" {
			t.Fatalf("rootful adapter invoked container: %#v", invocations)
		}
	})

	for _, mode := range []string{"cgroup-v1", "missing-memory-controller"} {
		t.Run("resource limits fail closed "+mode, func(t *testing.T) {
			adapter, logPath := newTestAdapter(t, mode)
			result, err := adapter.Run(context.Background(), testRequest(t))
			if !errors.Is(err, ErrResourceLimitsUnavailable) || result.Started {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
			invocations := readInvocations(t, logPath)
			if len(invocations) != 1 || invocations[0].Argv[0] != "info" {
				t.Fatalf("unlimited adapter invoked container: %#v", invocations)
			}
		})
	}

	t.Run("timeout bounds preflight", func(t *testing.T) {
		executable, _ := os.Executable()
		adapter, err := New(Config{
			PodmanPath: executable,
			PodmanEnvironment: map[string]string{
				fakePodmanMarker: "1", "SKYNEX_EVAL_FAKE_LOG": filepath.Join(t.TempDir(), "podman.jsonl"),
			},
			AllowedContainerEnv: []string{"CASE_ID"}, DiskBoundary: blockingDiskBoundary{}, ProbeTimeout: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		request := testRequest(t)
		request.Limits.Timeout = 20 * time.Millisecond
		result, err := adapter.Run(context.Background(), request)
		if !errors.Is(err, ErrTimeout) || !result.TimedOut || result.Started {
			t.Fatalf("preflight timeout result = %#v, error = %v", result, err)
		}
	})
}

func TestEnforcedProviderNetworkUsesOnlyControllerLease(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "podman.jsonl")
	controller := &testNetworkController{}
	adapter, err := New(Config{
		PodmanPath: executable,
		PodmanEnvironment: map[string]string{
			fakePodmanMarker: "1", "SKYNEX_EVAL_FAKE_LOG": logPath,
		},
		AllowedContainerEnv: []string{"CASE_ID"},
		DiskBoundary:        testDiskBoundary{available: true, enforced: true},
		NetworkController:   controller,
		ProbeTimeout:        10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(t)
	request.Network = NetworkPolicy{Mode: NetworkProviderProxyOnly, ProviderProxy: "https://proxy.eval.invalid:8443"}
	result, err := adapter.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.NetworkMode != NetworkProviderProxyOnly || result.Network.NetworkName != "eval-egress-001" || controller.released != 1 || len(controller.prepared) != 1 {
		t.Fatalf("network evidence/result = %#v controller = %#v", result, controller)
	}
	if !slices.Contains(result.Argv, "--network=eval-egress-001") || slices.Contains(result.Argv, "--network=none") {
		t.Fatalf("network argv = %#v", result.Argv)
	}

	request = testRequest(t)
	request.Network = NetworkPolicy{Mode: NetworkRegistryAllowlist, Registries: []string{"https://Z.registry.invalid", "https://a.registry.invalid"}}
	if _, err := adapter.Run(context.Background(), request); err != nil {
		t.Fatalf("registry-allowlist Run() error = %v", err)
	}
	if got := controller.prepared[1].Registries; !slices.Equal(got, []string{"https://a.registry.invalid", "https://z.registry.invalid"}) {
		t.Fatalf("normalized registry policy = %#v", got)
	}
}

func TestFailedContainerRemovalRetainsNetworkPolicy(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	controller := &testNetworkController{}
	adapter, err := New(Config{
		PodmanPath: executable,
		PodmanEnvironment: map[string]string{
			fakePodmanMarker: "1", "SKYNEX_EVAL_FAKE_LOG": filepath.Join(t.TempDir(), "podman.jsonl"),
			"SKYNEX_EVAL_FAKE_MODE": "cleanup-fail",
		},
		AllowedContainerEnv: []string{"CASE_ID"},
		DiskBoundary:        testDiskBoundary{available: true, enforced: true},
		NetworkController:   controller,
		ProbeTimeout:        10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(t)
	request.Network = NetworkPolicy{Mode: NetworkProviderProxyOnly, ProviderProxy: "https://proxy.eval.invalid"}
	result, err := adapter.Run(context.Background(), request)
	if err == nil || result.CleanupSucceeded || !result.NetworkRetained || result.NetworkReleased || controller.released != 0 {
		t.Fatalf("unsafe cleanup result = %#v, controller = %#v, error = %v", result, controller, err)
	}
}

func TestSecretsAndUnsafeInputsAreRejected(t *testing.T) {
	executable, _ := os.Executable()
	for _, key := range []string{"GITHUB_TOKEN", "SSH_AUTH_SOCK", "AWS_PROFILE", "KUBECONFIG"} {
		if _, err := New(Config{PodmanPath: executable, PodmanEnvironment: map[string]string{key: "secret-or-capability"}}); err == nil {
			t.Errorf("credential-like Podman environment %q was accepted", key)
		}
	}
	if _, err := New(Config{PodmanPath: executable, PodmanEnvironment: map[string]string{"LD_PRELOAD": "/tmp/evil.so"}}); err == nil {
		t.Error("non-allowlisted Podman environment was accepted")
	}
	if _, err := New(Config{PodmanPath: executable, AllowedContainerEnv: []string{"PATH"}}); err == nil {
		t.Error("evaluator-owned container PATH was made overridable")
	}
	for _, key := range []string{"OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "XDG_CONFIG_HOME"} {
		if _, err := New(Config{PodmanPath: executable, AllowedContainerEnv: []string{key}}); err == nil {
			t.Errorf("evaluator-owned runtime config variable %q was made overridable", key)
		}
	}
	adapter, _ := newTestAdapter(t, "")
	request := testRequest(t)
	request.Environment = map[string]string{"API_KEY": "secret"}
	if _, err := adapter.Run(context.Background(), request); err == nil {
		t.Fatal("credential-like container environment was accepted")
	}
	request = testRequest(t)
	request.ConfigSource = ""
	if _, err := adapter.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "config source is required") {
		t.Fatalf("missing config error = %v", err)
	}
	request = testRequest(t)
	request.Argv = []string{"sh", "-c", "echo unsafe"}
	if _, err := adapter.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "shell executable") {
		t.Fatalf("shell error = %v", err)
	}
	request = testRequest(t)
	request.ConfigSource = request.FixtureDir
	if _, err := adapter.BuildRunArgv(request, filepath.Join(t.TempDir(), "container.cid"), NetworkLease{}); err == nil || !strings.Contains(err.Error(), "must not overlap") {
		t.Fatalf("overlapping config/fixture error = %v", err)
	}
	request = testRequest(t)
	request.Bundles[0].Source = request.FixtureDir
	if _, err := adapter.BuildRunArgv(request, filepath.Join(t.TempDir(), "container.cid"), NetworkLease{}); err == nil || !strings.Contains(err.Error(), "must not overlap") {
		t.Fatalf("overlapping bundle/fixture error = %v", err)
	}
}

func TestProbeAndDoctorDoNotDownloadImages(t *testing.T) {
	adapter, logPath := newTestAdapter(t, "")
	probe := adapter.Probe(context.Background())
	if !probe.Ready || probe.Version != "5.4.2" || !probe.Rootless || !probe.ResourceLimits.Available {
		t.Fatalf("probe = %#v", probe)
	}
	doctor := adapter.Doctor(context.Background(), testRequest(t))
	if !doctor.Ready || !doctor.ImagePresent || !doctor.Disk.Enforced || !doctor.Network.Available {
		t.Fatalf("doctor = %#v", doctor)
	}
	for _, invocation := range readInvocations(t, logPath) {
		joined := strings.Join(invocation.Argv, " ")
		if strings.Contains(joined, "pull") || strings.Contains(joined, "build") {
			t.Fatalf("doctor attempted network/image mutation: %q", joined)
		}
	}
}

func readInvocations(t *testing.T, path string) []fakeInvocation {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var result []fakeInvocation
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var invocation fakeInvocation
		if err := json.Unmarshal(scanner.Bytes(), &invocation); err != nil {
			t.Fatal(err)
		}
		result = append(result, invocation)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
