//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
)

func requireDetachedWorkflowSupport() error {
	return errors.New("detached workflow execution is not supported on Windows; run without --detach")
}
func configureDetachedProcess(cmd *exec.Cmd, managed bool)             {}
func signalDetachedProcess(p *os.Process, pid int, managed bool) error { return p.Kill() }
func managedDetachSignals() []os.Signal                                { return []os.Signal{os.Interrupt} }
