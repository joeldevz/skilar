package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/baseline"
)

func TestOpenAIOAuthSessionFailsClosedOnCredentialMutation(t *testing.T) {
	source := filepath.Join(t.TempDir(), "auth.json")
	original := []byte(fmt.Sprintf(`{"openai":{"type":"oauth","access":"access-1","refresh":"refresh-1","expires":%d,"accountId":"account"}}`, time.Now().Add(24*time.Hour).UnixMilli()))
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := NewOpenAIOAuthSession(source)
	if err != nil {
		t.Fatal(err)
	}
	firstHome := t.TempDir()
	first, err := session.stage(context.Background(), firstHome, 0)
	if err != nil {
		t.Fatal(err)
	}
	rotated := openAIOAuthCredential{
		Type: "oauth", Access: "access-2", Refresh: "refresh-2", Expires: 2, AccountID: "account",
	}
	if _, err := saveOpenAIOAuthCredential(rotated, firstHome); err != nil {
		t.Fatal(err)
	}
	if err := first.release(true); !errors.Is(err, ErrOpenAIOAuthSessionCredentialChanged) {
		t.Fatalf("release error = %v, want credential-changed fence", err)
	}
	secondHome := t.TempDir()
	if _, err := session.stage(context.Background(), secondHome, 0); !errors.Is(err, ErrOpenAIOAuthSessionCredentialChanged) {
		t.Fatalf("reuse error = %v, want credential-changed fence", err)
	}
	if _, err := os.Stat(filepath.Join(secondHome, "opencode", "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tainted credential was staged into a second run: %v", err)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("OAuth source changed after runtime credential mutation")
	}
}

func TestOpenAIOAuthSessionReusesOnlyUnchangedCredentialAndSerializesLeases(t *testing.T) {
	source := filepath.Join(t.TempDir(), "auth.json")
	original := openAIOAuthCredential{
		Type: "oauth", Access: "access", Refresh: "refresh", Expires: time.Now().Add(24 * time.Hour).UnixMilli(), AccountID: "account-a",
	}
	if err := baseline.SaveJSON(source, map[string]openAIOAuthCredential{
		"openai": original,
	}, baseline.IOOptions{}); err != nil {
		t.Fatal(err)
	}
	session, err := NewOpenAIOAuthSession(source)
	if err != nil {
		t.Fatal(err)
	}
	firstHome := t.TempDir()
	first, err := session.stage(context.Background(), firstHome, 0)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan *openAIOAuthLease, 1)
	failed := make(chan error, 1)
	secondHome := t.TempDir()
	go func() {
		lease, stageErr := session.stage(context.Background(), secondHome, 0)
		if stageErr != nil {
			failed <- stageErr
			return
		}
		acquired <- lease
	}()
	select {
	case <-acquired:
		t.Fatal("second OAuth lease bypassed experiment serialization")
	case err := <-failed:
		t.Fatalf("second OAuth lease failed unexpectedly: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	if err := first.release(true); err != nil {
		t.Fatalf("unchanged OAuth credential failed inspection: %v", err)
	}
	select {
	case lease := <-acquired:
		got, err := loadExactOpenAIOAuth(filepath.Join(secondHome, "opencode", "auth.json"))
		if err != nil {
			t.Fatal(err)
		}
		if got != original {
			t.Fatalf("second run credential = %#v, want immutable source credential", got)
		}
		if err := lease.release(true); err != nil {
			t.Fatal(err)
		}
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("OAuth session lock was not released after unchanged credential inspection")
	}
}

func TestOpenAIOAuthSessionMalformedRuntimeProfilePermanentlyTaintsSession(t *testing.T) {
	source := filepath.Join(t.TempDir(), "auth.json")
	if err := baseline.SaveJSON(source, map[string]openAIOAuthCredential{
		"openai": {Type: "oauth", Access: "access", Refresh: "refresh", Expires: time.Now().Add(24 * time.Hour).UnixMilli()},
	}, baseline.IOOptions{}); err != nil {
		t.Fatal(err)
	}
	session, err := NewOpenAIOAuthSession(source)
	if err != nil {
		t.Fatal(err)
	}
	firstHome := t.TempDir()
	lease, err := session.stage(context.Background(), firstHome, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lease.authPath, []byte(`{"openai":{"type":"oauth","access":"attacker"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lease.release(true); !errors.Is(err, ErrOpenAIOAuthSessionCredentialChanged) {
		t.Fatalf("release error = %v, want credential-changed fence", err)
	}
	if _, err := session.stage(context.Background(), t.TempDir(), 0); !errors.Is(err, ErrOpenAIOAuthSessionCredentialChanged) {
		t.Fatalf("reuse error = %v, want persistent credential-changed fence", err)
	}
}

func TestOpenAIOAuthSessionUnstartedLeaseDoesNotImportRuntimeFile(t *testing.T) {
	source := filepath.Join(t.TempDir(), "auth.json")
	original := openAIOAuthCredential{
		Type: "oauth", Access: "access", Refresh: "refresh", Expires: time.Now().Add(24 * time.Hour).UnixMilli(),
	}
	if err := baseline.SaveJSON(source, map[string]openAIOAuthCredential{"openai": original}, baseline.IOOptions{}); err != nil {
		t.Fatal(err)
	}
	session, err := NewOpenAIOAuthSession(source)
	if err != nil {
		t.Fatal(err)
	}
	firstHome := t.TempDir()
	lease, err := session.stage(context.Background(), firstHome, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := saveOpenAIOAuthCredential(openAIOAuthCredential{
		Type: "oauth", Access: "attacker", Refresh: "attacker", Expires: 2,
	}, firstHome); err != nil {
		t.Fatal(err)
	}
	// release(false) is used only when no runtime process ever started. It must
	// discard the staged directory state rather than importing it.
	if err := lease.release(false); err != nil {
		t.Fatal(err)
	}
	secondHome := t.TempDir()
	second, err := session.stage(context.Background(), secondHome, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loadExactOpenAIOAuth(second.authPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != original {
		t.Fatalf("unstarted lease contaminated next run: %#v", got)
	}
	if err := second.release(true); err != nil {
		t.Fatal(err)
	}
}
