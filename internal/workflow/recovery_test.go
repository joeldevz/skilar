package workflow

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/gitcandidate"
)

func recoveryRepo(t *testing.T) (string, gitcandidate.ContextSeal, gitcandidate.Candidate) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	runGit(t, "", "init", repo)
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "file.txt")
	runGit(t, repo, "commit", "-m", "base")
	seal, err := gitcandidate.CaptureContext(repo)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := gitcandidate.Freeze(seal, gitcandidate.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	return repo, seal, candidate
}
func blockedRecovery(t *testing.T, repo string, basis RecoveryBasis) (*SQLiteStore, Workflow) {
	t.Helper()
	store, err := OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(Workflow{ID: "wf-resume", Route: RouteSimple, MinimumRisk: RiskLow, BasisTree: basis.PreTreeOID}); err != nil {
		t.Fatal(err)
	}
	w, err := store.Transition(Transition{WorkflowID: "wf-resume", ExpectedState: StateCreated, ExpectedVersion: 0, NextState: StateDiscovering, IdempotencyKey: "discover"})
	if err != nil {
		t.Fatal(err)
	}
	w, err = store.Transition(Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: StateBlocked, ResumeTarget: StateDiscovering, IdempotencyKey: "block", ArtifactIDs: []string{"blocker-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SaveRecoveryBasis(w.ID, basis); err != nil {
		t.Fatal(err)
	}
	return store, w
}

func TestResumeReconcilesPreAndPostTreesAndReclaimsExpiredLease(t *testing.T) {
	for _, outcome := range []string{"pre", "post"} {
		t.Run(outcome, func(t *testing.T) {
			repo, seal, candidate := recoveryRepo(t)
			basis := RecoveryBasis{Seal: seal, CandidatePolicy: gitcandidate.Policy{}}
			if outcome == "pre" {
				basis.PreTreeOID = candidate.TreeOID
				basis.PostTreeOID = "other"
			} else {
				basis.PreTreeOID = "other"
				basis.PostTreeOID = candidate.TreeOID
			}
			store, _ := blockedRecovery(t, repo, basis)
			defer store.Close()
			now := time.Now()
			if _, err := store.AcquireLease("worktree:"+seal.WorktreeID, "dead-owner", "dead-token", now.Add(-2*time.Minute), now.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			resumed, err := store.Resume(context.Background(), repo, ResumeRequest{WorkflowID: "wf-resume", BlockerID: "blocker-1", IdempotencyKey: "resume", Owner: "new-owner", FencingToken: "new-token", Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if resumed.State != StateDiscovering {
				t.Fatalf("state=%s", resumed.State)
			}
			var token string
			if err = store.Database().QueryRow(`SELECT fencing_token FROM leases WHERE resource=?`, "worktree:"+seal.WorktreeID).Scan(&token); err != nil || token != "new-token" {
				t.Fatalf("token=%s err=%v", token, err)
			}
		})
	}
}

func TestResumeUnknownTreeTransitionsToIntegrationConflict(t *testing.T) {
	repo, seal, candidate := recoveryRepo(t)
	store, _ := blockedRecovery(t, repo, RecoveryBasis{Seal: seal, CandidatePolicy: gitcandidate.Policy{}, PreTreeOID: candidate.TreeOID, PostTreeOID: "other"})
	defer store.Close()
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("unknown\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Resume(context.Background(), repo, ResumeRequest{WorkflowID: "wf-resume", BlockerID: "blocker-1", IdempotencyKey: "conflict"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateIntegrationConflict {
		t.Fatalf("state=%s", got.State)
	}
}

func TestResumeFailsClosedForMissingBasisOrWrongBlocker(t *testing.T) {
	repo, seal, candidate := recoveryRepo(t)
	store, _ := blockedRecovery(t, repo, RecoveryBasis{Seal: seal, CandidatePolicy: gitcandidate.Policy{}, PreTreeOID: candidate.TreeOID})
	defer store.Close()
	if _, err := store.Resume(context.Background(), repo, ResumeRequest{WorkflowID: "wf-resume", BlockerID: "wrong", IdempotencyKey: "resume"}); err == nil {
		t.Fatal("accepted wrong blocker")
	}
	if _, err := store.Database().Exec(`DELETE FROM recovery_bases WHERE workflow_id='wf-resume'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resume(context.Background(), repo, ResumeRequest{WorkflowID: "wf-resume", BlockerID: "blocker-1", IdempotencyKey: "resume"}); !errors.Is(err, ErrRecoveryBasisMissing) {
		t.Fatalf("error=%v", err)
	}
}

func TestProcessDeathReleasesLocalLock(t *testing.T) {
	if os.Getenv("SKYNEX_LOCK_CHILD") != "" {
		lock, err := acquireLocalLock(os.Getenv("SKYNEX_LOCK_CHILD"))
		if err != nil {
			os.Exit(2)
		}
		_ = lock
		time.Sleep(time.Hour)
		return
	}
	path := filepath.Join(t.TempDir(), "lock")
	cmd := exec.Command(os.Args[0], "-test.run=TestProcessDeathReleasesLocalLock")
	cmd.Env = append(os.Environ(), "SKYNEX_LOCK_CHILD="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		lock, err := acquireLocalLock(path)
		if err != nil {
			break
		}
		lock.Close()
		if time.Now().After(deadline) {
			t.Fatal("child never acquired lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	lock, err := acquireLocalLock(path)
	if err != nil {
		t.Fatalf("lock not released after death: %v", err)
	}
	lock.Close()
}

func TestIncompleteSQLiteTransactionRollsBackAfterCrash(t *testing.T) {
	if path := os.Getenv("SKYNEX_TX_CHILD"); path != "" {
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
		if err != nil {
			os.Exit(2)
		}
		tx, err := db.Begin()
		if err != nil {
			os.Exit(3)
		}
		_, err = tx.Exec(`INSERT INTO workflows(id,state,state_version,route,minimum_risk,basis_tree,resume_target) VALUES('crash','created',0,'simple','low','tree','')`)
		if err != nil {
			os.Exit(4)
		}
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "state", "workflows.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	cmd := exec.Command(os.Args[0], "-test.run=TestIncompleteSQLiteTransactionRollsBackAfterCrash")
	cmd.Env = append(os.Environ(), "SKYNEX_TX_CHILD="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child=%v %s", err, out)
	}
	store, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Get("crash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncommitted row survived: %v", err)
	}
	if _, err = store.Create(Workflow{ID: "crash", BasisTree: "tree", Route: RouteSimple, MinimumRisk: RiskLow}); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
}
