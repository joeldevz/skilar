//go:build windows

package workflow

import "syscall"

const processQueryLimitedInformation = 0x1000

// stillActive is STILL_ACTIVE. A handle can still be opened for a process that
// has already exited but not been fully released, so liveness needs the exit
// code as well, matching internal/review's Windows helper.
const stillActive = 259

func workflowProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var exitCode uint32
	return syscall.GetExitCodeProcess(handle, &exitCode) == nil && exitCode == stillActive
}
