//go:build windows

package adapters

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

type installCwd struct {
	file     *os.File
	files    []*os.File
	path     string
	identity os.FileInfo
}

const installCwdShare = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE

// fileAttributeTagInfo is the FILE_ATTRIBUTE_TAG_INFO structure.
type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

func openInstallCwd(path string, expected os.FileInfo) (*installCwd, error) {
	if expected == nil || !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("dependency install destination is not a validated directory")
	}
	if beforeInstallCwdOpen != nil {
		if err := beforeInstallCwdOpen(); err != nil {
			return nil, fmt.Errorf("prepare dependency install directory: %w", err)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve dependency install directory: %w", err)
	}
	var files []*os.File
	for component := abs; ; component = filepath.Dir(component) {
		access := uint32(windows.GENERIC_READ)
		if component == abs {
			access |= windows.GENERIC_WRITE
		}
		handle, openErr := windows.CreateFile(windows.StringToUTF16Ptr(component), access,
			installCwdShare, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if openErr != nil {
			closeInstallCwdFiles(files)
			return nil, fmt.Errorf("open dependency install directory path: %w", openErr)
		}
		var info fileAttributeTagInfo
		if attrErr := windows.GetFileInformationByHandleEx(handle, windows.FileAttributeTagInfo,
			(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); attrErr != nil {
			_ = windows.CloseHandle(handle)
			closeInstallCwdFiles(files)
			return nil, fmt.Errorf("inspect dependency install directory attributes: %w", attrErr)
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			_ = windows.CloseHandle(handle)
			closeInstallCwdFiles(files)
			return nil, fmt.Errorf("dependency install directory contains a reparse-point component")
		}
		files = append(files, os.NewFile(uintptr(handle), component))
		parent := filepath.Dir(component)
		if parent == component {
			break
		}
	}
	file := files[0]
	identity, err := file.Stat()
	if err != nil {
		closeInstallCwdFiles(files)
		return nil, fmt.Errorf("stat dependency install directory: %w", err)
	}
	if !sameInstallCwdIdentity(expected, identity) || identity.Mode()&os.ModeSymlink != 0 {
		closeInstallCwdFiles(files)
		return nil, fmt.Errorf("dependency install directory identity changed while opening")
	}
	cwd := &installCwd{file: file, files: files, path: path, identity: identity}
	if err := cwd.verify(path); err != nil {
		_ = cwd.Close()
		return nil, fmt.Errorf("revalidate dependency install directory: %w", err)
	}
	return cwd, nil
}

func (cwd *installCwd) configure(cmd *exec.Cmd) error {
	if cwd == nil || cwd.file == nil {
		return fmt.Errorf("dependency install directory handle is closed")
	}
	if err := cwd.verify(cwd.path); err != nil {
		return fmt.Errorf("revalidate dependency install directory before subprocess: %w", err)
	}
	cmd.Dir = cwd.path
	return nil
}

func (cwd *installCwd) verify(path string) error {
	if cwd == nil || cwd.file == nil {
		return fmt.Errorf("dependency install directory handle is closed")
	}
	current, err := cwd.file.Stat()
	if err != nil {
		return err
	}
	if !sameInstallCwdIdentity(cwd.identity, current) || current.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("dependency install directory identity changed")
	}
	return nil
}

func (cwd *installCwd) keepAlive() { runtime.KeepAlive(cwd) }

func (cwd *installCwd) Close() error {
	if cwd == nil || len(cwd.files) == 0 {
		return nil
	}
	var errs []error
	for _, file := range cwd.files {
		if err := file.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	cwd.files = nil
	cwd.file = nil
	return errors.Join(errs...)
}

func closeInstallCwdFiles(files []*os.File) {
	for _, file := range files {
		_ = file.Close()
	}
}
