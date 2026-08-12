//go:build unix

package main

import (
	"errors"
	"io/fs"
	"syscall"
)

func fileInfoOwnedByRoot(info fs.FileInfo) (bool, error) {
	if info == nil {
		return false, errors.New("file identity is required")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("file ownership identity is unavailable")
	}
	return stat.Uid == 0, nil
}
