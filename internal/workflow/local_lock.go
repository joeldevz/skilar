package workflow

import "errors"

// ErrResumeUnsupported is the explicit platform contract for recovery: resume
// reconciles a blocked workflow against the live worktree and must hold the
// exclusive worktree lock while it does so. Where that lock has no
// implementation the capability is declared unsupported up front instead of
// failing deep inside reconciliation.
var ErrResumeUnsupported = errors.New("workflow resume is not supported on Windows because exclusive worktree locking is unavailable; abort the workflow or resume it from a Unix host")

// ResumeSupported reports whether this platform can acquire the exclusive
// worktree lock that Resume requires.
func ResumeSupported() bool { return localLockSupported }
