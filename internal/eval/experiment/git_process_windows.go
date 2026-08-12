//go:build windows

package experiment

import (
	"os"
	"os/exec"
)

func configureGitProcess(*exec.Cmd) {}

func terminateGitProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}
