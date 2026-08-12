//go:build unix

package sandbox

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDigestTreeRejectsSpecialFile(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Skipf("fifo unavailable: %v", err)
	}
	if _, err := DigestTree(root, SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "special") {
		t.Fatalf("DigestTree() special-file error = %v", err)
	}
}
