//go:build !windows

package workflow

import (
	"os"
	"syscall"
)

func fileHasMultipleLinks(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink != 1
}
