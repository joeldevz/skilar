//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package lifecycle

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGTERM)
}

func killProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

func processGroupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func isProcessGone(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

func waitForProcessGroup(pid int, timeout time.Duration) bool {
	if !processGroupAlive(pid) {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !processGroupAlive(pid) {
				return true
			}
		case <-timer.C:
			return !processGroupAlive(pid)
		}
	}
}
