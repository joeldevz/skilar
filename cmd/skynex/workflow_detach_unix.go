//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func requireDetachedWorkflowSupport() error { return nil }
func configureDetachedProcess(cmd *exec.Cmd, managed bool) {
	if !managed {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
}
func signalDetachedProcess(p *os.Process, pid int, managed bool) error {
	if managed {
		if err := p.Signal(syscall.SIGTERM); err != nil && err != os.ErrProcessDone {
			return err
		}
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func managedDetachSignals() []os.Signal { return []os.Signal{syscall.SIGTERM, syscall.SIGINT} }
