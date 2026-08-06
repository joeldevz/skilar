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
func configureDetachedProcess(cmd *exec.Cmd)             {}
func signalDetachedProcess(p *os.Process, pid int) error { return p.Kill() }
