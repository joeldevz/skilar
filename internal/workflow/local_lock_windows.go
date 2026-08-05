//go:build windows

package workflow

const localLockSupported = false

type localLock struct{}

func acquireLocalLock(string) (*localLock, error) {
	return nil, ErrResumeUnsupported
}
func (l *localLock) Close() error { return nil }
