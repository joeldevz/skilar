//go:build !windows

package workflow

import "io/fs"

func runtimeExecutable(info fs.FileInfo) bool {
	return info != nil && !info.IsDir() && info.Mode()&0o111 != 0
}
