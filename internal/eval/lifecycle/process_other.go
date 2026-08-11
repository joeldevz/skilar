//go:build plan9

package lifecycle

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

func configureProcessGroup(_ *exec.Cmd) {}

func terminateProcessGroup(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(os.Interrupt)
}

func killProcessGroup(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

func processGroupAlive(_ int) bool { return false }

func isProcessGone(err error) bool { return errors.Is(err, os.ErrProcessDone) }

func waitForProcessGroup(_ int, _ time.Duration) bool { return true }
