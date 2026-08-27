//go:build windows

package paths

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsUserHomeDirIgnoresPoisonedUserProfile(t *testing.T) {
	poisoned := t.TempDir()
	t.Setenv("USERPROFILE", poisoned)

	want, err := windows.KnownFolderPath(windows.FOLDERID_Profile, 0)
	if err != nil {
		t.Fatalf("windows.KnownFolderPath(FOLDERID_Profile, 0) error = %v", err)
	}
	if !filepath.IsAbs(want) {
		t.Fatalf("windows.KnownFolderPath(FOLDERID_Profile, 0) = %q, want an absolute path", want)
	}

	got, err := windowsUserHomeDir()
	if err != nil {
		t.Fatalf("windowsUserHomeDir() error = %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("windowsUserHomeDir() = %q, want an absolute path", got)
	}
	if got == poisoned {
		t.Fatalf("windowsUserHomeDir() trusted poisoned USERPROFILE %q", poisoned)
	}
	if got != want {
		t.Fatalf("windowsUserHomeDir() = %q, want KnownFolderPath(FOLDERID_Profile) %q", got, want)
	}
}
