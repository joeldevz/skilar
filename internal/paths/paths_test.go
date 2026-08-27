package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestClaudeDir_Unix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only test")
	}
	home, _ := os.UserHomeDir()
	got := ClaudeDir()
	want := filepath.Join(home, ".claude")
	if got != want {
		t.Errorf("ClaudeDir() = %q, want %q", got, want)
	}
}

func TestOpencodeDir_Unix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only test")
	}
	home, _ := os.UserHomeDir()
	got := OpencodeDir()
	want := filepath.Join(home, ".config", "opencode")
	if got != want {
		t.Errorf("OpencodeDir() = %q, want %q", got, want)
	}
}

func TestResolveOpencodeDir(t *testing.T) {
	home := filepath.Join("home", "user")
	appdata := filepath.Join("legacy", "appdata")
	want := filepath.Join(home, ".config", "opencode")

	for _, tc := range []struct {
		name    string
		windows bool
	}{
		{name: "Windows ignores APPDATA", windows: true},
		{name: "Unix uses home", windows: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveOpencodeDir(home, appdata, tc.windows); got != want {
				t.Fatalf("resolveOpencodeDir(%q, %q, %t) = %q, want %q", home, appdata, tc.windows, got, want)
			}
		})
	}
}

func TestStateDir_Unix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only test")
	}
	home, _ := os.UserHomeDir()
	got := StateDir()
	want := filepath.Join(home, ".config", "skynex")
	if got != want {
		t.Errorf("StateDir() = %q, want %q", got, want)
	}
}
