//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package lifecycle

import (
	"fmt"
	"os"
	"syscall"
)

func validateCredentialFileOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect OpenAI OAuth source ownership")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("OpenAI OAuth source must be owned by the evaluator user")
	}
	return nil
}
