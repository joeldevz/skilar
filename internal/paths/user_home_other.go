//go:build !windows

package paths

import "os"

func userHomeDir() (string, error) {
	return os.UserHomeDir()
}
