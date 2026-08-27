//go:build windows

package paths

import "golang.org/x/sys/windows"

func windowsUserHomeDir() (string, error) {
	return windows.KnownFolderPath(windows.FOLDERID_Profile, 0)
}

func userHomeDir() (string, error) {
	return windowsUserHomeDir()
}
