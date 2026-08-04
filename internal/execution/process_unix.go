//go:build !windows

package execution

import (
	"os"
	"os/exec"
	"syscall"
)

// configureOpenCodeProcess gives the worker its own process group so a timeout
// or cancellation cannot leave child tools running after OpenCode exits.
func configureOpenCodeProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
