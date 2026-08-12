//go:build !windows

package execution

import (
	"os"
	"os/exec"
	"syscall"
)

const evaluatorManagedDetachEnvironment = "SKYNEX_EVAL_MANAGED_DETACH"

func evaluatorManagedOpenCodeProcess() bool {
	return os.Getenv(evaluatorManagedDetachEnvironment) == "1"
}

// configureOpenCodeProcess gives the worker its own process group so a timeout
// or cancellation cannot leave child tools running after OpenCode exits.
// In evaluator-managed detach mode, the lifecycle-owned OpenCode process group
// is the cleanup boundary, so nested OpenCode must remain in that group.
func configureOpenCodeProcess(cmd *exec.Cmd, evaluatorManaged bool) {
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
