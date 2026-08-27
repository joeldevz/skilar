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

func TestOpencodeDir_IgnoresAPPDATA(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error = %v", err)
	}
	if !filepath.IsAbs(home) {
		t.Fatalf("os.UserHomeDir() = %q, want an absolute path", home)
	}
	t.Setenv("APPDATA", filepath.Join(t.TempDir(), "legacy-appdata"))

	want := filepath.Join(home, ".config", "opencode")
	if got := OpencodeDir(); got != want {
		t.Fatalf("OpencodeDir() with APPDATA set = %q, want %q", got, want)
	}
}

func TestResolveOpencodeDir(t *testing.T) {
	absHome := t.TempDir()
	want := filepath.Join(absHome, ".config", "opencode")

	if got, err := resolveOpencodeDir(absHome); err != nil {
		t.Fatalf("resolveOpencodeDir(%q) error = %v", absHome, err)
	} else if got != want {
		t.Fatalf("resolveOpencodeDir(%q) = %q, want %q", absHome, got, want)
	}

	for _, home := range []string{"", filepath.Join("home", "user")} {
		t.Run("reject "+home, func(t *testing.T) {
			if got, err := resolveOpencodeDir(home); err == nil {
				t.Fatalf("resolveOpencodeDir(%q) = %q, want an error", home, got)
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
