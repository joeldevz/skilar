//go:build windows

package workflow

import (
	"errors"
)

type localLock struct{}

func acquireLocalLock(string) (*localLock, error) {
	return nil, errors.New("exclusive workflow lock is not implemented on Windows")
}
func (l *localLock) Close() error { return nil }
