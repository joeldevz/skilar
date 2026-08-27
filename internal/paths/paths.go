package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ClaudeDir returns ~/.claude on Unix, %LOCALAPPDATA%\claude on Windows
func ClaudeDir() string {
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "claude")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func resolveOpencodeDir(home string) (string, error) {
	if home == "" {
		return "", fmt.Errorf("home directory is empty")
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("home directory %q is not absolute", home)
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

// OpencodeDir returns ~/.config/opencode on every OS
func OpencodeDir() string {
	home, err := userHomeDir()
	if err != nil {
		panic(fmt.Sprintf("resolve opencode directory: home directory: %v", err))
	}
	dir, err := resolveOpencodeDir(home)
	if err != nil {
		panic(fmt.Sprintf("resolve opencode directory: %v", err))
	}
	return dir
}

// StateDir returns ~/.config/skynex on Unix, %LOCALAPPDATA%\skynex on Windows
func StateDir() string {
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "skynex")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "skynex")
}

// NeuroxBinDir returns ~/.local/bin on Unix, %LOCALAPPDATA%\neurox on Windows
func NeuroxBinDir() string {
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "neurox")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin")
}

// NeuroxBinName returns "neurox" on Unix, "neurox.exe" on Windows
func NeuroxBinName() string {
	if runtime.GOOS == "windows" {
		return "neurox.exe"
	}
	return "neurox"
}
