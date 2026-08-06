//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func requireDetachedWorkflowSupport() error  { return nil }
func configureDetachedProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func signalDetachedProcess(p *os.Process, pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
