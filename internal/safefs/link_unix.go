//go:build !windows

package safefs

import (
	"os"
	"syscall"
)

func singleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func SingleLink(info os.FileInfo) bool { return singleLink(info) }
