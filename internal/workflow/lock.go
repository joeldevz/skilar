package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
)

func AcquireWorktreeLock(commonDir, worktreeID string) (io.Closer, error) {
	if commonDir == "" || worktreeID == "" {
		return nil, fmt.Errorf("workflow: lock identity missing")
	}
	return acquireLocalLock(filepath.Join(commonDir, "skynex", "worktree-"+shortHash(worktreeID)+".lock"))
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
