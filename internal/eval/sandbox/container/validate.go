package container

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	absoluteMaxTimeout     = 2 * time.Hour
	absoluteMaxOutputBytes = 256 << 20
	absoluteMaxMemoryBytes = 64 << 30
	absoluteMaxDiskBytes   = 1 << 40
	absoluteMaxTmpfsBytes  = 4 << 30
	absoluteMaxPIDs        = 4096
	maxContainerArguments  = 4096
	maxArgumentBytes       = 1 << 20
	maxEnvironmentEntries  = 256
	maxEnvironmentBytes    = 64 << 10
	maxBundleMounts        = 256
)

var (
	digestImagePattern  = regexp.MustCompile(`^(.+)@sha256:([a-f0-9]{64})$`)
	repositoryComponent = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	runIDPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	mountNamePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	networkNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	endpointHostPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)
)

var forbiddenExecutableNames = map[string]struct{}{
	"ash": {}, "bash": {}, "busybox": {}, "cmd": {}, "cmd.exe": {},
	"dash": {}, "fish": {}, "ksh": {}, "powershell": {},
	"powershell.exe": {}, "pwsh": {}, "sh": {}, "zsh": {},
}

func normalizeConfig(config Config) (Config, error) {
	config.AllowedContainerEnv = append([]string(nil), config.AllowedContainerEnv...)
	if config.PodmanEnvironment != nil {
		copyEnvironment := make(map[string]string, len(config.PodmanEnvironment))
		for key, value := range config.PodmanEnvironment {
			copyEnvironment[key] = value
		}
		config.PodmanEnvironment = copyEnvironment
	}
	if config.PodmanPath == "" {
		return Config{}, fmt.Errorf("podman path is required")
	}
	if !filepath.IsAbs(config.PodmanPath) {
		return Config{}, fmt.Errorf("podman path must be absolute: %q", config.PodmanPath)
	}
	resolved, err := filepath.EvalSymlinks(config.PodmanPath)
	if err != nil {
		return Config{}, fmt.Errorf("resolve podman executable: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Config{}, fmt.Errorf("stat podman executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return Config{}, fmt.Errorf("podman path is not an executable regular file: %q", resolved)
	}
	config.PodmanPath = resolved
	if config.ProbeTimeout == 0 {
		config.ProbeTimeout = 10 * time.Second
	}
	if config.ProbeTimeout < time.Millisecond || config.ProbeTimeout > time.Minute {
		return Config{}, fmt.Errorf("probe timeout must be between 1ms and 1m")
	}
	allowed := make(map[string]struct{}, len(config.AllowedContainerEnv))
	if len(config.AllowedContainerEnv) > maxEnvironmentEntries {
		return Config{}, fmt.Errorf("allowed container environment count exceeds %d", maxEnvironmentEntries)
	}
	for _, key := range config.AllowedContainerEnv {
		if err := validateEnvKey(key); err != nil {
			return Config{}, fmt.Errorf("allowed container environment: %w", err)
		}
		if secretEnvironmentKey(key) {
			return Config{}, fmt.Errorf("credential-like container environment key is forbidden: %q", key)
		}
		if reservedContainerEnvironmentKey(key) {
			return Config{}, fmt.Errorf("container environment key is evaluator-owned and cannot be overridden: %q", key)
		}
		if _, duplicate := allowed[key]; duplicate {
			return Config{}, fmt.Errorf("duplicate allowed container environment key %q", key)
		}
		allowed[key] = struct{}{}
	}
	if len(config.PodmanEnvironment) > 64 {
		return Config{}, fmt.Errorf("podman environment count exceeds 64")
	}
	seenPodmanEnvironment := make(map[string]struct{}, len(config.PodmanEnvironment))
	for key, value := range config.PodmanEnvironment {
		if err := validateEnvKey(key); err != nil {
			return Config{}, fmt.Errorf("podman environment: %w", err)
		}
		if secretEnvironmentKey(key) {
			return Config{}, fmt.Errorf("credential-like podman environment key is forbidden: %q", key)
		}
		if unsafePodmanEnvironmentKey(key) {
			return Config{}, fmt.Errorf("podman environment key can change the isolation boundary and is forbidden: %q", key)
		}
		if !allowedPodmanEnvironmentKey(key) {
			return Config{}, fmt.Errorf("podman environment key is not in the reduced allowlist: %q", key)
		}
		upperKey := strings.ToUpper(key)
		if key != upperKey {
			return Config{}, fmt.Errorf("podman environment key must use canonical uppercase spelling: %q", key)
		}
		if _, duplicate := seenPodmanEnvironment[upperKey]; duplicate {
			return Config{}, fmt.Errorf("duplicate podman environment key %q", key)
		}
		seenPodmanEnvironment[upperKey] = struct{}{}
		if strings.IndexByte(value, 0) >= 0 {
			return Config{}, fmt.Errorf("podman environment value for %q contains NUL", key)
		}
		switch strings.ToUpper(key) {
		case "XDG_DATA_HOME", "XDG_RUNTIME_DIR":
			resolved, pathErr := canonicalDirectory(value, "podman environment "+key)
			if pathErr != nil {
				return Config{}, pathErr
			}
			config.PodmanEnvironment[key] = resolved
		case "PODMAN_NO_PAUSE_PROCESS":
			if value != "0" && value != "1" {
				return Config{}, fmt.Errorf("PODMAN_NO_PAUSE_PROCESS must be 0 or 1")
			}
		}
	}
	if err := verifyNoSystemDefaultMounts(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// FindPodman resolves Podman without retaining PATH as runtime authority. The
// returned absolute path can be placed in Config.PodmanPath.
func FindPodman() (string, error) {
	path, err := exec.LookPath("podman")
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

func normalizeRequest(config Config, request Request) (Request, error) {
	request.Bundles = append([]BundleMount(nil), request.Bundles...)
	request.Argv = append([]string(nil), request.Argv...)
	if request.Environment != nil {
		copyEnvironment := make(map[string]string, len(request.Environment))
		for key, value := range request.Environment {
			copyEnvironment[key] = value
		}
		request.Environment = copyEnvironment
	}
	if !runIDPattern.MatchString(request.RunID) {
		return Request{}, fmt.Errorf("invalid run id %q", request.RunID)
	}
	if len(request.Image) > 512 {
		return Request{}, fmt.Errorf("container image reference exceeds 512 bytes")
	}
	if !validPinnedImageReference(request.Image) {
		return Request{}, fmt.Errorf("%w: %q", ErrImageNotPinned, request.Image)
	}
	var err error
	request.FixtureDir, err = canonicalDirectory(request.FixtureDir, "fixture")
	if err != nil {
		return Request{}, err
	}
	if request.ConfigSource == "" {
		return Request{}, fmt.Errorf("config source is required to prevent ambient runtime configuration")
	}
	request.ConfigSource, err = canonicalDirectory(request.ConfigSource, "config source")
	if err != nil {
		return Request{}, err
	}
	if pathsOverlap(request.FixtureDir, request.ConfigSource) {
		return Request{}, fmt.Errorf("fixture and config source must not overlap")
	}
	seenBundles := make(map[string]struct{}, len(request.Bundles))
	if len(request.Bundles) > maxBundleMounts {
		return Request{}, fmt.Errorf("bundle mount count exceeds %d", maxBundleMounts)
	}
	for i := range request.Bundles {
		bundle := &request.Bundles[i]
		if !mountNamePattern.MatchString(bundle.Name) {
			return Request{}, fmt.Errorf("invalid bundle name %q", bundle.Name)
		}
		if _, duplicate := seenBundles[bundle.Name]; duplicate {
			return Request{}, fmt.Errorf("duplicate bundle name %q", bundle.Name)
		}
		seenBundles[bundle.Name] = struct{}{}
		bundle.Source, err = canonicalDirectory(bundle.Source, "bundle "+bundle.Name)
		if err != nil {
			return Request{}, err
		}
		if pathsOverlap(request.FixtureDir, bundle.Source) {
			return Request{}, fmt.Errorf("fixture and bundle %q source must not overlap", bundle.Name)
		}
	}
	sort.Slice(request.Bundles, func(i, j int) bool { return request.Bundles[i].Name < request.Bundles[j].Name })
	sources := []struct {
		label string
		path  string
	}{{label: "fixture", path: request.FixtureDir}, {label: "config source", path: request.ConfigSource}}
	for _, bundle := range request.Bundles {
		sources = append(sources, struct {
			label string
			path  string
		}{label: "bundle " + bundle.Name, path: bundle.Source})
	}
	for _, source := range sources {
		if pathWithin(source.path, config.PodmanPath) {
			return Request{}, fmt.Errorf("%s contains the host Podman executable", source.label)
		}
		for key, hostPath := range config.PodmanEnvironment {
			if (key == "XDG_DATA_HOME" || key == "XDG_RUNTIME_DIR") && pathsOverlap(source.path, hostPath) {
				return Request{}, fmt.Errorf("%s overlaps host Podman %s", source.label, key)
			}
		}
	}
	if len(request.Argv) == 0 || request.Argv[0] == "" {
		return Request{}, fmt.Errorf("container argv must not be empty")
	}
	if len(request.Argv) > maxContainerArguments {
		return Request{}, fmt.Errorf("container argv count exceeds %d", maxContainerArguments)
	}
	argumentBytes := 0
	for _, arg := range request.Argv {
		if strings.IndexByte(arg, 0) >= 0 || !utf8.ValidString(arg) {
			return Request{}, fmt.Errorf("container argv contains NUL or invalid UTF-8")
		}
		if len(arg) > maxArgumentBytes-argumentBytes {
			return Request{}, fmt.Errorf("container argv exceeds %d bytes", maxArgumentBytes)
		}
		argumentBytes += len(arg)
	}
	if _, forbidden := forbiddenExecutableNames[strings.ToLower(filepath.Base(request.Argv[0]))]; forbidden {
		return Request{}, fmt.Errorf("shell executable is forbidden: %q", request.Argv[0])
	}
	if request.WorkDir == "" {
		request.WorkDir = WorkspacePath
	}
	if !withinContainerWorkspace(request.WorkDir) {
		return Request{}, fmt.Errorf("container working directory must stay below %s: %q", WorkspacePath, request.WorkDir)
	}
	if err := validateWorkDir(request.FixtureDir, request.WorkDir); err != nil {
		return Request{}, err
	}
	allowedEnv := make(map[string]struct{}, len(config.AllowedContainerEnv))
	for _, key := range config.AllowedContainerEnv {
		allowedEnv[key] = struct{}{}
	}
	if len(request.Environment) > maxEnvironmentEntries {
		return Request{}, fmt.Errorf("container environment count exceeds %d", maxEnvironmentEntries)
	}
	environmentBytes := 0
	for key, value := range request.Environment {
		if err := validateEnvKey(key); err != nil {
			return Request{}, fmt.Errorf("container environment: %w", err)
		}
		if secretEnvironmentKey(key) {
			return Request{}, fmt.Errorf("credential-like container environment key is forbidden: %q", key)
		}
		if _, ok := allowedEnv[key]; !ok {
			return Request{}, fmt.Errorf("container environment key %q is not allowlisted", key)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return Request{}, fmt.Errorf("container environment value for %q contains NUL", key)
		}
		entryBytes := len(key) + len(value) + 1
		if entryBytes > maxEnvironmentBytes-environmentBytes {
			return Request{}, fmt.Errorf("container environment exceeds %d bytes", maxEnvironmentBytes)
		}
		environmentBytes += entryBytes
	}
	request.Network, err = normalizeNetworkPolicy(request.Network)
	if err != nil {
		return Request{}, err
	}
	request.Limits, err = normalizeLimits(request.Limits)
	if err != nil {
		return Request{}, err
	}
	return request, nil
}

func validPinnedImageReference(reference string) bool {
	if reference != strings.ToLower(reference) || strings.Contains(reference, "://") {
		return false
	}
	matches := digestImagePattern.FindStringSubmatch(reference)
	if len(matches) != 3 {
		return false
	}
	name := matches[1]
	slash := strings.IndexByte(name, '/')
	if slash <= 0 || slash == len(name)-1 {
		return false
	}
	registry, repository := name[:slash], name[slash+1:]
	if !validRegistryAuthority(registry) {
		return false
	}
	for _, component := range strings.Split(repository, "/") {
		if !repositoryComponent.MatchString(component) {
			return false
		}
	}
	return true
}

func validRegistryAuthority(authority string) bool {
	if authority == "" || strings.ContainsAny(authority, "@/ \t\r\n\\") {
		return false
	}
	host := authority
	port := ""
	if strings.HasPrefix(authority, "[") {
		closing := strings.IndexByte(authority, ']')
		if closing < 0 {
			return false
		}
		host = authority[1:closing]
		if closing+1 < len(authority) {
			if authority[closing+1] != ':' {
				return false
			}
			port = authority[closing+2:]
		}
		if net.ParseIP(host) == nil || !strings.Contains(host, ":") {
			return false
		}
	} else {
		if strings.Count(authority, ":") > 1 {
			return false
		}
		if colon := strings.LastIndexByte(authority, ':'); colon >= 0 {
			host, port = authority[:colon], authority[colon+1:]
		}
	}
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 || strconv.Itoa(value) != port {
			return false
		}
	} else if strings.HasSuffix(authority, ":") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	return host == "localhost" || strings.Contains(host, ".") && endpointHostPattern.MatchString(host) && !strings.Contains(host, "..")
}

func pathsOverlap(first, second string) bool {
	return pathWithin(first, second) || pathWithin(second, first)
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func canonicalDirectory(path string, label string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("%s must be an absolute clean path: %q", label, path)
	}
	// Podman's --mount parser uses commas as separators and has no portable
	// escaping accepted by all supported releases. Reject instead of mounting a
	// different path than the control plane validated.
	if strings.Contains(path, ",") {
		return "", fmt.Errorf("%s path contains unsupported comma: %q", label, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory: %q", label, resolved)
	}
	return resolved, nil
}

func normalizeLimits(limits Limits) (Limits, error) {
	defaults := DefaultLimits()
	if limits.CPUs == 0 {
		limits.CPUs = defaults.CPUs
	}
	if limits.MemoryBytes == 0 {
		limits.MemoryBytes = defaults.MemoryBytes
	}
	if limits.PIDs == 0 {
		limits.PIDs = defaults.PIDs
	}
	if limits.DiskBytes == 0 {
		limits.DiskBytes = defaults.DiskBytes
	}
	if limits.TmpfsBytes == 0 {
		limits.TmpfsBytes = defaults.TmpfsBytes
	}
	if limits.MaxStdoutBytes == 0 {
		limits.MaxStdoutBytes = defaults.MaxStdoutBytes
	}
	if limits.MaxStderrBytes == 0 {
		limits.MaxStderrBytes = defaults.MaxStderrBytes
	}
	if limits.Timeout == 0 {
		limits.Timeout = defaults.Timeout
	}
	if math.IsNaN(limits.CPUs) || math.IsInf(limits.CPUs, 0) || limits.CPUs < .01 || limits.CPUs > 64 {
		return Limits{}, fmt.Errorf("cpus must be between 0.01 and 64")
	}
	if limits.MemoryBytes < 16<<20 || limits.MemoryBytes > absoluteMaxMemoryBytes {
		return Limits{}, fmt.Errorf("memory_bytes must be between 16MiB and 64GiB")
	}
	if limits.PIDs < 1 || limits.PIDs > absoluteMaxPIDs {
		return Limits{}, fmt.Errorf("pids must be between 1 and %d", absoluteMaxPIDs)
	}
	if limits.DiskBytes < 1<<20 || limits.DiskBytes > absoluteMaxDiskBytes {
		return Limits{}, fmt.Errorf("disk_bytes must be between 1MiB and 1TiB")
	}
	if limits.TmpfsBytes < 1<<20 || limits.TmpfsBytes > absoluteMaxTmpfsBytes {
		return Limits{}, fmt.Errorf("tmpfs_bytes must be between 1MiB and 4GiB")
	}
	if limits.MaxStdoutBytes < 1 || limits.MaxStdoutBytes > absoluteMaxOutputBytes || limits.MaxStderrBytes < 1 || limits.MaxStderrBytes > absoluteMaxOutputBytes {
		return Limits{}, fmt.Errorf("stdout/stderr limits must be between 1 and 256MiB")
	}
	if limits.Timeout < time.Millisecond || limits.Timeout > absoluteMaxTimeout {
		return Limits{}, fmt.Errorf("timeout must be between 1ms and 2h")
	}
	return limits, nil
}

func normalizeNetworkPolicy(policy NetworkPolicy) (NetworkPolicy, error) {
	if policy.Mode == "" {
		policy.Mode = NetworkNone
	}
	switch policy.Mode {
	case NetworkNone:
		if policy.ProviderProxy != "" || len(policy.Registries) != 0 {
			return NetworkPolicy{}, fmt.Errorf("network none forbids proxy and registries")
		}
	case NetworkProviderProxyOnly:
		if policy.ProviderProxy == "" || len(policy.Registries) != 0 {
			return NetworkPolicy{}, fmt.Errorf("provider-proxy-only requires exactly one provider proxy and no registries")
		}
		endpoint, err := normalizeEndpoint(policy.ProviderProxy, false)
		if err != nil {
			return NetworkPolicy{}, fmt.Errorf("provider proxy: %w", err)
		}
		policy.ProviderProxy = endpoint
	case NetworkRegistryAllowlist:
		if policy.ProviderProxy != "" || len(policy.Registries) == 0 {
			return NetworkPolicy{}, fmt.Errorf("registry-allowlist requires registries and forbids provider proxy")
		}
		seen := make(map[string]struct{}, len(policy.Registries))
		normalized := make([]string, 0, len(policy.Registries))
		for _, endpoint := range policy.Registries {
			endpoint, err := normalizeEndpoint(endpoint, true)
			if err != nil {
				return NetworkPolicy{}, fmt.Errorf("registry endpoint: %w", err)
			}
			if _, duplicate := seen[endpoint]; duplicate {
				return NetworkPolicy{}, fmt.Errorf("duplicate registry endpoint %q", endpoint)
			}
			seen[endpoint] = struct{}{}
			normalized = append(normalized, endpoint)
		}
		sort.Strings(normalized)
		policy.Registries = normalized
	default:
		return NetworkPolicy{}, fmt.Errorf("unsupported network mode %q", policy.Mode)
	}
	return policy, nil
}

func normalizeEndpoint(raw string, requireTLS bool) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid absolute endpoint %q", raw)
	}
	if parsed.Scheme != "https" && (!(!requireTLS && parsed.Scheme == "http")) {
		return "", fmt.Errorf("unsupported endpoint scheme %q", parsed.Scheme)
	}
	if parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("endpoint must not contain credentials, path, query, or fragment")
	}
	host := parsed.Hostname()
	if host == "" || strings.ContainsAny(host, "* \\") {
		return "", fmt.Errorf("invalid endpoint host")
	}
	if parsed.Scheme == "http" {
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("plaintext provider proxy must be loopback")
		}
	}
	port := parsed.Port()
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", fmt.Errorf("invalid endpoint port")
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		host = strings.ToLower(host)
		if !endpointHostPattern.MatchString(host) || strings.Contains(host, "..") {
			return "", fmt.Errorf("endpoint host must be an ASCII DNS name or IP address")
		}
	}
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	if port != "" {
		authority += ":" + port
	}
	return parsed.Scheme + "://" + authority, nil
}

func validateNetworkLease(lease NetworkLease) error {
	if !networkNamePattern.MatchString(lease.NetworkName) {
		return fmt.Errorf("network controller returned invalid network name %q", lease.NetworkName)
	}
	switch strings.ToLower(lease.NetworkName) {
	case "bridge", "default", "host", "none", "pasta", "podman", "private", "slirp4netns":
		return fmt.Errorf("network controller returned reserved or broad network name %q", lease.NetworkName)
	}
	if !validDigest(lease.EvidenceDigest) {
		return fmt.Errorf("network controller returned invalid evidence digest")
	}
	return nil
}

func validDiskEvidence(evidence DiskEvidence, expected int64) bool {
	return evidence.Enforced && evidence.LimitBytes > 0 && evidence.LimitBytes <= expected && validDigest(evidence.EvidenceDigest)
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	hexValue := strings.TrimPrefix(value, "sha256:")
	if hexValue != strings.ToLower(hexValue) {
		return false
	}
	_, err := hex.DecodeString(hexValue)
	return err == nil
}

func digestValue(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateEnvKey(key string) error {
	if key == "" || strings.IndexAny(key, "=\x00") >= 0 {
		return fmt.Errorf("invalid environment key %q", key)
	}
	return nil
}

func secretEnvironmentKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, exact := range []string{
		"SSH_AUTH_SOCK", "SSH_AGENT_PID", "DOCKER_CONFIG", "REGISTRY_AUTH_FILE",
		"KUBECONFIG", "NETRC", "NPM_CONFIG_USERCONFIG", "PIP_CONFIG_FILE",
		"GNUPGHOME", "AZURE_CONFIG_DIR", "GOOGLE_APPLICATION_CREDENTIALS",
	} {
		if upper == exact {
			return true
		}
	}
	for _, prefix := range []string{"AWS_", "AZURE_", "GCP_", "GOOGLE_", "OCI_", "GH_", "GITHUB_", "GITLAB_"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	for _, fragment := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "PRIVATE_KEY", "AUTHORIZATION", "COOKIE"} {
		if strings.Contains(upper, fragment) {
			return true
		}
	}
	return false
}

func unsafePodmanEnvironmentKey(key string) bool {
	upper := strings.ToUpper(key)
	switch upper {
	case "CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE",
		"PODMAN_CONNECTIONS_CONF", "PODMAN_USERNS", "STORAGE_DRIVER", "STORAGE_OPTS",
		"HOME", "XDG_CONFIG_HOME", "TMPDIR":
		return true
	default:
		return false
	}
}

func allowedPodmanEnvironmentKey(key string) bool {
	upper := strings.ToUpper(key)
	if upper == "XDG_DATA_HOME" || upper == "XDG_RUNTIME_DIR" || upper == "PODMAN_NO_PAUSE_PROCESS" {
		return true
	}
	// Reserved for the package's executable-based tests. Real Podman ignores
	// this namespace, so it grants no runtime or container authority.
	return strings.HasPrefix(upper, "SKYNEX_EVAL_FAKE_")
}

func reservedContainerEnvironmentKey(key string) bool {
	upper := strings.ToUpper(key)
	if strings.HasPrefix(upper, "OPENCODE_") || strings.HasPrefix(upper, "XDG_") {
		return true
	}
	switch upper {
	case "HOME", "LANG", "LC_ALL", "PATH", "TMPDIR", "TZ":
		return true
	default:
		return false
	}
}

func verifyNoSystemDefaultMounts() error {
	for _, path := range []string{"/usr/share/containers/mounts.conf", "/etc/containers/mounts.conf"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("%w: inspect %s: %v", ErrUnsafePodmanConfig, path, err)
		}
		if len(raw) > 1<<20 {
			return fmt.Errorf("%w: %s exceeds 1MiB", ErrUnsafePodmanConfig, path)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				return fmt.Errorf("%w: %s declares default mounts", ErrUnsafePodmanConfig, path)
			}
		}
	}
	return nil
}

func withinContainerWorkspace(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.IndexByte(path, 0) >= 0 {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return clean == WorkspacePath || strings.HasPrefix(clean, WorkspacePath+"/")
}

func validateWorkDir(fixtureDir, workDir string) error {
	containerClean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(workDir)))
	relative := strings.TrimPrefix(containerClean, WorkspacePath)
	relative = strings.TrimPrefix(relative, "/")
	hostPath := fixtureDir
	if relative != "" {
		hostPath = filepath.Join(fixtureDir, filepath.FromSlash(relative))
	}
	resolved, err := filepath.EvalSymlinks(hostPath)
	if err != nil {
		return fmt.Errorf("resolve container working directory %q: %w", workDir, err)
	}
	rel, err := filepath.Rel(fixtureDir, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("container working directory escapes fixture: %q", workDir)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat container working directory %q: %w", workDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("container working directory is not a directory: %q", workDir)
	}
	return nil
}
