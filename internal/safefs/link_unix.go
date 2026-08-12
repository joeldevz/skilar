//go:build !windows

package safefs

import (
	"errors"
	"os"
	"syscall"
)

func linkCount(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Nlink), true
}

func hasFileIdentity(info os.FileInfo) bool {
	_, ok := info.Sys().(*syscall.Stat_t)
	return ok
}

func singleLinkFile(f *os.File) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	count, known := linkCount(info)
	if !known {
		return false, errors.New("link count is unavailable for the open descriptor")
	}
	return count == 1, nil
}
