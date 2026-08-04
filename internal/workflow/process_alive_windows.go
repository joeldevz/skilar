//go:build windows

package workflow

import "syscall"

const processQueryLimitedInformation = 0x1000

func workflowProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	return syscall.CloseHandle(handle) == nil
}
