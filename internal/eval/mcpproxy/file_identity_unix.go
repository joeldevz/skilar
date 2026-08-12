//go:build unix

package mcpproxy

import (
	"fmt"
	"os"
	"syscall"
)

func verifyProtectedFileIdentity(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("file identity unavailable")
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("file has multiple hard links")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("file owner differs from evaluator")
	}
	return nil
}
