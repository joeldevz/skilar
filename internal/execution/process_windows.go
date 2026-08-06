//go:build windows

package execution

import (
	"os"
	"os/exec"
)

// Windows has no Unix process groups. Keep foreground execution portable and
// safely terminate the OpenCode process itself when its context is cancelled.
func configureOpenCodeProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Kill()
	}
}
