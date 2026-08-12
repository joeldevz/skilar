//go:build !windows

package main

import (
	"os/exec"
	"testing"
)

func TestDetachedProcessUsesNewSessionOutsideEvaluator(t *testing.T) {
	cmd := exec.Command("true")
	configureDetachedProcess(cmd, false)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("normal detached process attributes = %#v, want Setsid", cmd.SysProcAttr)
	}
}

func TestEvaluatorManagedDetachStaysInParentProcessGroup(t *testing.T) {
	cmd := exec.Command("true")
	configureDetachedProcess(cmd, true)
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setsid {
		t.Fatalf("managed detached process attributes = %#v, must not Setsid", cmd.SysProcAttr)
	}
}
