//go:build linux

package adapters

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

type installCwd struct {
	file     *os.File
	path     string
	identity os.FileInfo
}

func openInstallCwd(path string, expected os.FileInfo) (*installCwd, error) {
	if expected == nil || !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("dependency install destination is not a validated directory")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open dependency install directory: %w", err)
	}
	identity, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat dependency install directory: %w", err)
	}
	if !sameInstallCwdIdentity(expected, identity) || identity.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("dependency install directory identity changed while opening")
	}
	current, err := os.Lstat(path)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("revalidate dependency install directory: %w", err)
	}
	if !sameInstallCwdIdentity(expected, current) || current.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("dependency install directory identity changed while opening")
	}
	return &installCwd{file: file, path: path, identity: expected}, nil
}

func (cwd *installCwd) configure(cmd *exec.Cmd) error {
	if cwd == nil || cwd.file == nil {
		return fmt.Errorf("dependency install directory handle is closed")
	}
	if err := cwd.verify(cwd.path); err != nil {
		return fmt.Errorf("revalidate dependency install directory before descriptor cwd: %w", err)
	}
	descriptorPath := fmt.Sprintf("/proc/self/fd/%d", cwd.file.Fd())
	if _, err := os.Stat(descriptorPath); err != nil {
		return fmt.Errorf("descriptor-backed dependency cwd unavailable (requires /proc): %w", err)
	}
	cmd.Dir = descriptorPath
	return nil
}

func (cwd *installCwd) verify(path string) error {
	if cwd == nil || cwd.file == nil {
		return fmt.Errorf("dependency install directory handle is closed")
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !sameInstallCwdIdentity(cwd.identity, current) || current.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("dependency install directory identity changed")
	}
	return nil
}

func (cwd *installCwd) keepAlive() { runtime.KeepAlive(cwd.file) }

func (cwd *installCwd) Close() error {
	if cwd == nil || cwd.file == nil {
		return nil
	}
	err := cwd.file.Close()
	cwd.file = nil
	return err
}
