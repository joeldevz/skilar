//go:build windows

package safefs

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// os.FileInfo never carries the Windows link count, so a FileInfo on its own
// cannot decide the question; callers holding a descriptor use singleLinkFile,
// which asks the kernel through GetFileInformationByHandle.
func linkCount(os.FileInfo) (uint64, bool) { return 0, false }

func hasFileIdentity(info os.FileInfo) bool {
	_, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok
}

func singleLinkFile(f *os.File) (bool, error) {
	conn, err := f.SyscallConn()
	if err != nil {
		return false, err
	}
	var info windows.ByHandleFileInformation
	var callErr error
	if controlErr := conn.Control(func(fd uintptr) {
		callErr = windows.GetFileInformationByHandle(windows.Handle(fd), &info)
	}); controlErr != nil {
		return false, controlErr
	}
	if callErr != nil {
		return false, callErr
	}
	return info.NumberOfLinks == 1, nil
}
