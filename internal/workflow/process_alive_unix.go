//go:build !windows

package workflow

import "syscall"

func workflowProcessAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}
