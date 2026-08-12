//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"time"
)

// trusted-local on Windows can terminate the direct process but does not claim
// a job-object isolation boundary. The isolated-container runtime is required
// for hostile process trees.
func configureProcessGroup(cmd *exec.Cmd) {}

func terminateProcessGroup(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

func waitProcessGroupQuiescent(int, time.Duration) bool { return true }

func processSignal(*os.ProcessState) string { return "" }
