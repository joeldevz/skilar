//go:build !windows

package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEvaluatorManagedOpenCodeProcessRequiresExactValue(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{{"", false}, {"0", false}, {"true", false}, {" 1", false}, {"1 ", false}, {"1", true}} {
		t.Setenv(evaluatorManagedDetachEnvironment, test.value)
		if got := evaluatorManagedOpenCodeProcess(); got != test.want {
			t.Fatalf("managed mode for %q = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestOpenCodeProcessGroupModes(t *testing.T) {
	t.Run("normal owns nested group", func(t *testing.T) {
		cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "exec sleep 600")
		configureOpenCodeProcess(cmd, false)
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
			t.Fatalf("normal SysProcAttr = %#v", cmd.SysProcAttr)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer reapOpenCodeTestProcess(cmd)
		pgid, err := syscall.Getpgid(cmd.Process.Pid)
		if err != nil || pgid != cmd.Process.Pid {
			t.Fatalf("normal pgid = %d, pid = %d, err = %v", pgid, cmd.Process.Pid, err)
		}
	})

	t.Run("evaluator managed inherits lifecycle group", func(t *testing.T) {
		cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "exec sleep 600")
		configureOpenCodeProcess(cmd, true)
		if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
			t.Fatalf("managed SysProcAttr = %#v", cmd.SysProcAttr)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer reapOpenCodeTestProcess(cmd)
		pgid, err := syscall.Getpgid(cmd.Process.Pid)
		if err != nil || pgid != syscall.Getpgrp() {
			t.Fatalf("managed pgid = %d, lifecycle pgid = %d, err = %v", pgid, syscall.Getpgrp(), err)
		}
	})
}

func TestOpenCodeCancellationScopeMatchesMode(t *testing.T) {
	for _, test := range []struct {
		name               string
		managed            bool
		wantDescendantLive bool
	}{{"normal kills nested group", false, false}, {"managed kills only nested OpenCode PID", true, true}} {
		t.Run(test.name, func(t *testing.T) {
			cmd, childPID := startOpenCodeCancellationTree(t, test.managed)
			groupID := cmd.Process.Pid
			if err := cmd.Cancel(); err != nil && err != os.ErrProcessDone {
				t.Fatalf("cancel: %v", err)
			}
			_ = cmd.Wait()
			childLive := processExists(childPID)
			if !test.managed {
				childLive = !waitForProcessGone(childPID, 2*time.Second)
			}
			if childLive != test.wantDescendantLive {
				t.Fatalf("descendant live after cancel = %v, want %v", childLive, test.wantDescendantLive)
			}
			// The managed test intentionally leaves the descendant alive to prove
			// cancellation targeted only the nested PID. Reap its isolated test
			// group explicitly; the evaluator lifecycle owns this in production.
			_ = syscall.Kill(-groupID, syscall.SIGKILL)
			if !waitForProcessGone(childPID, 2*time.Second) {
				t.Fatalf("descendant %d survived test cleanup", childPID)
			}
		})
	}
}

func startOpenCodeCancellationTree(t *testing.T, managed bool) (*exec.Cmd, int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", `sleep 600 & echo $! > "$1"; wait`, "sh", pidFile)
	configureOpenCodeProcess(cmd, managed)
	if managed {
		// Isolate this assertion from the test runner's process group. The
		// production attribute behavior is asserted separately above.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr == nil && pid > 0 {
				return cmd, pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("child PID was not published: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func reapOpenCodeTestProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	} else {
		_ = cmd.Process.Kill()
	}
	_, _ = cmd.Process.Wait()
}

func processExists(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat))
	return len(fields) < 3 || fields[2] != "Z"
}

func waitForProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !processExists(pid)
}
