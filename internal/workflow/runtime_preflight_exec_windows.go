//go:build windows

package workflow

import "io/fs"

// Windows has no POSIX execute bit. LookPath already applies its executable
// extension rules, so the resolved target merely needs not to be a directory.
func runtimeExecutable(info fs.FileInfo) bool {
	return info != nil && !info.IsDir()
}
