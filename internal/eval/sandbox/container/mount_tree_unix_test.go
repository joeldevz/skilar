//go:build unix

package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestRunRejectsUnsafeEntriesInMountedTrees(t *testing.T) {
	for _, test := range []struct {
		name   string
		unsafe func(*testing.T, string)
	}{
		{
			name: "symlink",
			unsafe: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Symlink("project", filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hardlink",
			unsafe: func(t *testing.T, root string) {
				t.Helper()
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("host-owned"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(outside, filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fifo",
			unsafe: func(t *testing.T, root string) {
				t.Helper()
				if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
					t.Skipf("fifo unavailable: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter, _ := newTestAdapter(t, "")
			request := testRequest(t)
			test.unsafe(t, request.FixtureDir)
			result, err := adapter.Run(context.Background(), request)
			if err == nil || result.Started || !strings.Contains(err.Error(), "mounted source") {
				t.Fatalf("unsafe mount result = %#v, error = %v", result, err)
			}
		})
	}
}

func TestRunRejectsControlDirectoryResolvedInsideMount(t *testing.T) {
	adapter, _ := newTestAdapter(t, "")
	request := testRequest(t)
	tmpLink := filepath.Join(t.TempDir(), "tmp-link")
	if err := os.Symlink(request.FixtureDir, tmpLink); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmpLink)
	if _, err := adapter.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "control directory overlaps") {
		t.Fatalf("symlinked TMPDIR overlap error = %v", err)
	}
}
