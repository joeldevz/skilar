package review

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/workflow"
)

func TestSQLiteStoreReopenAuthorityInvalidationAndHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "workflows.db")
	db, err := workflow.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewSQLiteStore(db.Database())
	c, p := testCandidate(t, "wf-sql", "tree-sql")
	floor := DeterministicFloor(workflow.RoutePlanned, nil, p)
	a, err := AssessSemantic(c, floor, semanticInput(RiskMedium, LensReliability), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := store.Issue(IssueRequest{Candidate: c, Floor: floor, Assessment: a, Evidence: testEvidence(c, LensReliability), IssuedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = workflow.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store = NewSQLiteStore(db.Database())
	got, err := store.Authority("wf-sql")
	if err != nil || got.ID != receipt.ID {
		t.Fatalf("authority=%#v err=%v", got, err)
	}
	c2, _ := testCandidate(t, "wf-sql", "tree-sql-2")
	a2, err := AssessSemantic(c2, floor, semanticInput(RiskMedium, LensReliability), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e2 := testEvidence(c2, LensReliability)
	for n := range e2 {
		e2[n].ID += "-2"
	}
	a2.EvidenceIDs = []string{"accept-2", "check-2", "review-2"}
	r2, err := store.Issue(IssueRequest{Candidate: c2, Floor: floor, Assessment: a2, Evidence: e2, IssuedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	got, err = store.Authority("wf-sql")
	if err != nil || got.ID != r2.ID {
		t.Fatalf("replacement=%#v err=%v", got, err)
	}
	if err = store.Invalidate(Invalidation{WorkflowID: "wf-sql", CandidateRecordID: c2.ID, Reason: "drift", OccurredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Authority("wf-sql"); !errors.Is(err, ErrNoAuthority) {
		t.Fatalf("authority=%v", err)
	}
	if _, err = store.Issue(IssueRequest{Candidate: c2, Floor: floor, Assessment: a2, Evidence: e2, IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Authority("wf-sql"); !errors.Is(err, ErrNoAuthority) {
		t.Fatalf("idempotent replay restored invalid authority: %v", err)
	}
	if got, err = store.Receipt(receipt.ID); err != nil || got.ID != receipt.ID {
		t.Fatalf("history=%#v err=%v", got, err)
	}
}

func TestSQLiteStoreConcurrentIssueHasOneImmutableReceipt(t *testing.T) {
	db, err := workflow.OpenSQLite(filepath.Join(t.TempDir(), "state", "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewSQLiteStore(db.Database())
	c, p := testCandidate(t, "wf-concurrent", "tree-concurrent")
	floor := DeterministicFloor(workflow.RoutePlanned, nil, p)
	a, err := AssessSemantic(c, floor, semanticInput(RiskMedium, LensReliability), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	req := IssueRequest{Candidate: c, Floor: floor, Assessment: a, Evidence: testEvidence(c, LensReliability), IssuedAt: time.Now()}
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan Receipt, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := store.Issue(req); results <- r; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatalf("issue=%v", e)
		}
	}
	var id string
	for r := range results {
		if id == "" {
			id = r.ID
		}
		if r.ID != id {
			t.Fatalf("receipt IDs differ: %s %s", id, r.ID)
		}
	}
	var count int
	if err = db.Database().QueryRow(`SELECT COUNT(*) FROM receipts WHERE candidate_record_id=?`, c.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}
