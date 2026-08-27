package paths

import (
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

func resolveOpencodeDir(home, _ string, _ bool) string {
	return filepath.Join(home, ".config", "opencode")
}

// OpencodeDir returns ~/.config/opencode on every OS
func OpencodeDir() string {
	home, _ := os.UserHomeDir()
	return resolveOpencodeDir(home, "", runtime.GOOS == "windows")
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
