package workflow

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResumeSupportedMatchesTheLockImplementation(t *testing.T) {
	if want := runtime.GOOS != "windows"; ResumeSupported() != want {
		t.Fatalf("ResumeSupported()=%v on %s", ResumeSupported(), runtime.GOOS)
	}
	if ResumeSupported() {
		return
	}
	if _, err := acquireLocalLock(filepath.Join(t.TempDir(), "worktree.lock")); !errors.Is(err, ErrResumeUnsupported) {
		t.Fatalf("acquireLocalLock err=%v", err)
	}
}

func TestResumeDeclaresTheUnsupportedPlatformBeforeTouchingState(t *testing.T) {
	if ResumeSupported() {
		t.Skip("platform supports the exclusive worktree lock")
	}
	store := openTestSQLite(t)
	createSQLiteWorkflow(t, store)
	_, err := store.Resume(context.Background(), t.TempDir(), ResumeRequest{WorkflowID: "wf-1", BlockerID: "blocker", IdempotencyKey: "resume"})
	if !errors.Is(err, ErrResumeUnsupported) {
		t.Fatalf("err=%v", err)
	}
	w, getErr := store.Get("wf-1")
	if getErr != nil || w.StateVersion != 0 {
		t.Fatalf("workflow mutated: %+v err=%v", w, getErr)
	}
}
