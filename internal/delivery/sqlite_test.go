package delivery

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/joeldevz/skynex/internal/workflow"
)

func TestSQLiteIntentReopenRecoversCrashBeforeRefCAS(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(t.TempDir(), "state", "workflows.db")
	db, err := workflow.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	crash := errors.New("simulated crash")
	gate := Gate{Authority: f.reviews, Intents: NewSQLiteIntentStore(db.Database()), BeforeRefUpdate: func() error { return crash }}
	if _, err = gate.Commit(context.Background(), f.request()); !errors.Is(err, crash) {
		t.Fatalf("first error=%v", err)
	}
	if got := gitOut(t, f.repo, "rev-parse", "HEAD"); got != f.record.Seal.BaseCommitOID {
		t.Fatalf("ref moved before recovery: %s", got)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = workflow.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	recovered, err := (&Gate{Authority: f.reviews, Intents: NewSQLiteIntentStore(db.Database())}).Commit(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Recovered || gitOut(t, f.repo, "rev-parse", "HEAD") != recovered.CommitOID {
		t.Fatalf("recovery=%#v", recovered)
	}
}
