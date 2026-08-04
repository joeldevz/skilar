//go:build !windows

package review

import (
	"os"
	"os/exec"
	"syscall"
)

func configureReviewProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func reviewProcessAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
