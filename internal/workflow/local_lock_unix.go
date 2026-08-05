//go:build !windows

package workflow

import (
	"os"
	"path/filepath"
	"syscall"
)

const localLockSupported = true

type localLock struct{ file *os.File }

func acquireLocalLock(path string) (*localLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return &localLock{file: file}, nil
}
func (l *localLock) Close() error {
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}
