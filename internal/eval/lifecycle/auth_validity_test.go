package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/eval/baseline"
)

func TestOpenAIOAuthSessionRequiresFreshDedicatedCredentialWithoutRefreshing(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	source := filepath.Join(t.TempDir(), "auth.json")
	original := openAIOAuthCredential{
		Type: "oauth", Access: "access", Refresh: "refresh",
		Expires: now.Add(5 * time.Minute).UnixMilli(), AccountID: "account-a",
	}
	if err := baseline.SaveJSON(source, map[string]openAIOAuthCredential{"openai": original}, baseline.IOOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewOpenAIOAuthSession(source)
	if err != nil {
		t.Fatal(err)
	}
	session.now = func() time.Time { return now }
	if _, err := session.stage(context.Background(), t.TempDir(), 10*time.Minute); err == nil || !strings.Contains(err.Error(), "renew the clean profile") {
		t.Fatalf("near-expiry stage error = %v", err)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("validity preflight modified the dedicated OAuth source")
	}
}

func TestOpenAIOAuthSessionStagesCredentialCoveringRunHorizon(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	source := filepath.Join(t.TempDir(), "auth.json")
	want := openAIOAuthCredential{
		Type: "oauth", Access: "access", Refresh: "refresh",
		Expires: now.Add(time.Hour).UnixMilli(), AccountID: "account-a",
	}
	if err := baseline.SaveJSON(source, map[string]openAIOAuthCredential{"openai": want}, baseline.IOOptions{}); err != nil {
		t.Fatal(err)
	}
	session, err := NewOpenAIOAuthSession(source)
	if err != nil {
		t.Fatal(err)
	}
	session.now = func() time.Time { return now }
	lease, err := session.stage(context.Background(), t.TempDir(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loadExactOpenAIOAuth(lease.authPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("staged credential = %#v, want %#v", got, want)
	}
	if err := lease.release(true); err != nil {
		t.Fatal(err)
	}
}
