//go:build !windows

package review

import (
	"os"
	"os/exec"
	"syscall"
)

const evaluatorManagedDetachEnvironment = "SKYNEX_EVAL_MANAGED_DETACH"

func evaluatorManagedReviewProcess() bool {
	return os.Getenv(evaluatorManagedDetachEnvironment) == "1"
}

func configureReviewProcess(cmd *exec.Cmd, evaluatorManaged bool) {
	if !evaluatorManaged {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if evaluatorManaged {
			return cmd.Process.Kill()
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func reviewProcessAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
