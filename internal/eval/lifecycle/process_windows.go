//go:build windows

package lifecycle

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func terminateProcessGroup(pid int) error {
	return taskkill(pid, false)
}

func killProcessGroup(pid int) error {
	return taskkill(pid, true)
}

func taskkill(pid int, force bool) error {
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	err := exec.Command("taskkill", args...).Run()
	if err != nil && !processGroupAlive(pid) {
		return os.ErrProcessDone
	}
	return err
}

func processGroupAlive(pid int) bool {
	output, err := exec.Command(
		"tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH",
	).Output()
	if err != nil {
		return false
	}
	text := strings.TrimSpace(string(output))
	return text != "" && !strings.HasPrefix(strings.ToLower(text), "info:")
}

func isProcessGone(err error) bool { return errors.Is(err, os.ErrProcessDone) }

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
