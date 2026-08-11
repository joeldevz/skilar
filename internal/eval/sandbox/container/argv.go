package container

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
)

// buildRunArgv returns the exact Podman argument vector for an already
// normalized request. For network:none, lease must be zero. Other modes
// require a validated lease returned by NetworkController.
func buildRunArgv(request Request, cidFile string, lease NetworkLease) ([]string, error) {
	if cidFile == "" || !filepath.IsAbs(cidFile) {
		return nil, fmt.Errorf("cidfile must be an absolute path")
	}
	if request.Network.Mode == NetworkNone {
		if lease.NetworkName != "" || lease.EvidenceDigest != "" {
			return nil, fmt.Errorf("network none forbids a network lease")
		}
	} else if err := validateNetworkLease(lease); err != nil {
		return nil, err
	}
	entrypoint, err := json.Marshal([]string{request.Argv[0]})
	if err != nil {
		return nil, fmt.Errorf("encode entrypoint: %w", err)
	}
	hooksDir := filepath.Join(filepath.Dir(cidFile), "hooks")
	name := containerName(request.RunID, cidFile)
	args := []string{
		"--hooks-dir=" + hooksDir,
		"run",
		"--rm",
		"--name=" + name,
		"--cidfile=" + cidFile,
		"--pull=never",
		"--no-healthcheck",
		"--read-only",
		"--read-only-tmpfs=false",
		"--image-volume=ignore",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--userns=keep-id",
		"--ipc=private",
		"--pid=private",
		"--uts=private",
		"--hostname=skynex-eval",
		"--log-driver=none",
		"--stop-timeout=1",
		"--unsetenv-all",
	}
	if request.Network.Mode == NetworkNone {
		args = append(args, "--network=none")
	} else {
		args = append(args, "--network="+lease.NetworkName)
	}
	args = append(args,
		"--cpus="+strconv.FormatFloat(request.Limits.CPUs, 'f', -1, 64),
		"--memory="+strconv.FormatInt(request.Limits.MemoryBytes, 10),
		"--memory-swap="+strconv.FormatInt(request.Limits.MemoryBytes, 10),
		"--pids-limit="+strconv.Itoa(request.Limits.PIDs),
		"--tmpfs=/tmp:rw,exec,nosuid,nodev,mode=1777,size="+strconv.FormatInt(request.Limits.TmpfsBytes, 10),
		"--mount=type=bind,src="+request.FixtureDir+",dst="+WorkspacePath+",readonly=false,bind-propagation=rprivate,bind-nonrecursive",
	)
	if request.ConfigSource != "" {
		args = append(args, "--mount=type=bind,src="+request.ConfigSource+",dst="+ConfigPath+",readonly=true,bind-propagation=rprivate,bind-nonrecursive")
	}
	for _, bundle := range request.Bundles {
		args = append(args, "--mount=type=bind,src="+bundle.Source+",dst="+path.Join(BundlesPath, bundle.Name)+",readonly=true,bind-propagation=rprivate,bind-nonrecursive")
	}
	args = append(args,
		"--workdir="+request.WorkDir,
		"--entrypoint="+string(entrypoint),
		"--env=HOME=/tmp/home",
		"--env=LANG=C",
		"--env=LC_ALL=C",
		"--env=PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--env=TMPDIR=/tmp",
		"--env=TZ=UTC",
		"--env=XDG_CACHE_HOME=/tmp/cache",
		"--env=XDG_CONFIG_HOME="+ConfigPath,
		"--env=XDG_DATA_HOME=/tmp/data",
		"--env=XDG_STATE_HOME=/tmp/state",
	)
	keys := make([]string, 0, len(request.Environment))
	for key := range request.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--env="+key+"="+request.Environment[key])
	}
	args = append(args, request.Image)
	args = append(args, request.Argv[1:]...)
	return args, nil
}

func containerName(runID, cidFile string) string {
	sum := sha256.Sum256([]byte(cidFile))
	return "skynex-eval-" + runID + "-" + fmt.Sprintf("%x", sum[:8])
}
