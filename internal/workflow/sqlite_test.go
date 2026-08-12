package workflow

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func openTestSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "state", "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func createSQLiteWorkflow(t *testing.T, s *SQLiteStore) {
	t.Helper()
	if _, err := s.Create(Workflow{ID: "wf-1", Route: RouteSimple, MinimumRisk: RiskLow, BasisTree: "tree-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteTransitionPersistenceAndIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "workflows.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	createSQLiteWorkflow(t, s)
	req := Transition{WorkflowID: "wf-1", ExpectedState: StateCreated, ExpectedVersion: 0, NextState: StateDiscovering, IdempotencyKey: "discover", ArtifactIDs: []string{"graph-1"}}
	first, err := s.Transition(req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Transition(req)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || second.StateVersion != 1 {
		t.Fatalf("idempotent results differ: %#v %#v", first, second)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.Get("wf-1")
	if err != nil || w.State != StateDiscovering || w.StateVersion != 1 {
		t.Fatalf("persisted workflow=%#v err=%v", w, err)
	}
	events, err := s.Events("wf-1")
	if err != nil || len(events) != 1 || events[0].ArtifactIDs[0] != "graph-1" {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	changed := req
	changed.NextState = StateReady
	if _, err = s.Transition(changed); !errors.Is(err, ErrIdempotencyReuse) {
		t.Fatalf("reuse error=%v", err)
	}
}

func TestSQLiteConcurrentCASHasSingleWinner(t *testing.T) {
	s := openTestSQLite(t)
	createSQLiteWorkflow(t, s)
	const workers = 12
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Transition(Transition{WorkflowID: "wf-1", ExpectedState: StateCreated, ExpectedVersion: 0, NextState: StateDiscovering, IdempotencyKey: "candidate-" + string(rune('a'+i))})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
			continue
		}
		if !errors.Is(err, ErrCASConflict) {
			t.Fatalf("unexpected contender error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("winners=%d", success)
	}
	events, _ := s.Events("wf-1")
	if len(events) != 1 {
		t.Fatalf("events=%d", len(events))
	}
}

func TestSQLiteAttemptsAndLeasesPersist(t *testing.T) {
	s := openTestSQLite(t)
	createSQLiteWorkflow(t, s)
	if err := s.RegisterAttempt(Attempt{ID: "attempt-1", WorkflowID: "wf-1", NodeID: "node-1", BasisTree: "tree-1"}); err != nil {
		t.Fatal(err)
	}
	result := ResultEnvelope{WorkflowID: "wf-1", NodeID: "node-1", AttemptID: "attempt-1", BaseCandidateOID: "tree-1", Status: AttemptCompleted}
	if err := s.AcceptResult(result); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptResult(result); !errors.Is(err, ErrStaleResult) {
		t.Fatalf("duplicate result=%v", err)
	}
	now := time.Now().UTC()
	if _, err := s.AcquireLease("worktree-1", "owner-1", "token-1", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLease("worktree-1", "owner-2", "token-2", now, now.Add(time.Minute)); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("live reclaim=%v", err)
	}
	if _, err := s.HeartbeatLease("worktree-1", "owner-1", "wrong", now, now.Add(2*time.Minute)); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("wrong fence=%v", err)
	}
	reclaimAt := now.Add(2 * time.Minute)
	lease, err := s.AcquireLease("worktree-1", "owner-2", "token-2", reclaimAt, reclaimAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if lease.FencingToken != "token-2" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestRepositorySQLiteSharedAcrossWorktrees(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	runGit(t, "", "init", repo)
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "init")
	second := filepath.Join(filepath.Dir(repo), "second")
	runGit(t, repo, "worktree", "add", "-b", "second", second)
	one, err := OpenRepositorySQLite(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := OpenRepositorySQLite(second)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	if one.Path() != two.Path() {
		t.Fatalf("paths differ: %s %s", one.Path(), two.Path())
	}
	createSQLiteWorkflow(t, one)
	w, err := two.Get("wf-1")
	if err != nil || w.ID != "wf-1" {
		t.Fatalf("shared workflow=%#v err=%v", w, err)
	}
	if info, err := os.Stat(filepath.Dir(one.Path())); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode=%v err=%v", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(one.Path()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("db mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestOpenSQLiteRejectsSymlinkAndHardlink(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skip(err)
	}
	if _, err := OpenSQLite(filepath.Join(link, "db")); err == nil {
		t.Fatal("accepted symlink component")
	}
	s, err := OpenSQLite(filepath.Join(realDir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Link(filepath.Join(realDir, "db"), filepath.Join(realDir, "alias")); err != nil {
		t.Skip(err)
	}
	if _, err := OpenSQLite(filepath.Join(realDir, "db")); err == nil {
		t.Fatal("accepted hard-linked database")
	}
}

func TestSQLiteMigratesExistingV1SchemaToCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "workflows.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err = store.Database().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 19 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	for _, expected := range []string{"receipt_authority", "execution_contract_revisions", "replan_revisions"} {
		var table string
		if err = store.Database().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, expected).Scan(&table); err != nil || table != expected {
			t.Fatalf("table=%q expected=%q err=%v", table, expected, err)
		}
	}
}

func TestLiveReadOnlyCanObserveActiveWALWithoutMutatingSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflows.db")
	writer, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err = writer.Create(Workflow{ID: "wf", Route: RouteSimple, MinimumRisk: RiskLow}); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenSQLiteLiveReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	w, err := reader.Get("wf")
	if err != nil || w.ID != "wf" {
		t.Fatalf("workflow=%+v err=%v", w, err)
	}
	if _, err = reader.Database().Exec(`DELETE FROM workflows`); err == nil {
		t.Fatal("live read-only connection allowed mutation")
	}
}

func downgradeToSchema17(t *testing.T, store *SQLiteStore) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE IF EXISTS verification_run_history`,
		`DROP TABLE IF EXISTS verification_contract_revisions`,
		`PRAGMA user_version=17`,
	} {
		if _, err := store.Database().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCurrentSchemaRejectsMissingRequiredTableBeforeWorkflowMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflows.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(Workflow{ID: "preserved", Route: RouteSimple, MinimumRisk: RiskLow}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Database().Exec(`DROP TABLE verification_run_history`); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = OpenSQLite(path)
	var compatibility *CompatibilityError
	if !errors.As(err, &compatibility) {
		t.Fatalf("expected compatibility error, got %v", err)
	}
	if compatibility.DatabaseSchema != CurrentSchemaVersion || !slices.Contains(compatibility.MissingObjects, "verification_run_history") {
		t.Fatalf("compatibility=%+v", compatibility)
	}

	raw, openErr := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&immutable=1")
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer raw.Close()
	var count int
	if err = raw.QueryRow(`SELECT COUNT(*) FROM workflows WHERE id='preserved'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("workflow changed during rejected open: count=%d err=%v", count, err)
	}
}

func TestReadOnlySchemaMismatchReturnsStructuredCompatibilityDiagnostic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflows.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Database().Exec(`PRAGMA user_version=99`); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = OpenSQLiteReadOnly(path)
	encoded, ok := CompatibilityJSON(err)
	if !ok || !bytes.Contains(encoded, []byte(`"database_schema": 99`)) || !bytes.Contains(encoded, []byte(`"database_path"`)) || !bytes.Contains(encoded, []byte(`"hint"`)) {
		t.Fatalf("diagnostic=%s err=%v", encoded, err)
	}
}

func TestSchema18BackfillsResultTransportForInFlightWorkflows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "workflows.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{
		"wf-legacy":    `{"Request":"do it","Checks":["go test ./..."],"Timeout":600000000000,"Seal":{"RepositoryRoot":"/repo"}}`,
		"wf-empty":     `{"Request":"do it","ResultTransport":"","AllowedPaths":["internal"]}`,
		"wf-declared":  `{"Request":"do it","ResultTransport":"some-other-transport-v9"}`,
		"wf-corrupt":   `not json at all`,
		"wf-preserved": `{"Request":"keep","ModelExplicit":true,"SliceConfigs":{"a":{"Nested":[1,2,3]}}}`,
	}
	for id, input := range inputs {
		if _, err = store.Create(Workflow{ID: id, Route: RouteSimple, MinimumRisk: RiskLow, BasisTree: "tree-" + id}); err != nil {
			t.Fatal(err)
		}
		if _, err = store.Database().Exec(`INSERT INTO workflow_run_inputs(workflow_id,input) VALUES(?,?)`, id, []byte(input)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.Database().Exec(`UPDATE workflows SET state=?,state_version=4 WHERE id=?`, StateVerifying, "wf-legacy"); err != nil {
		t.Fatal(err)
	}
	downgradeToSchema17(t, store)
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	read := func(id string) map[string]json.RawMessage {
		t.Helper()
		var raw []byte
		if err := store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("%s: %v (%s)", id, err, raw)
		}
		return fields
	}
	transport := func(id string) string {
		t.Helper()
		var value string
		if err := json.Unmarshal(read(id)["ResultTransport"], &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, id := range []string{"wf-legacy", "wf-empty", "wf-preserved"} {
		if got := transport(id); got != ResultTransportFileV1 {
			t.Fatalf("%s transport=%q", id, got)
		}
	}
	if got := transport("wf-declared"); got != "some-other-transport-v9" {
		t.Fatalf("overwrote a declared transport: %q", got)
	}
	preserved := read("wf-preserved")
	if string(preserved["Request"]) != `"keep"` || string(preserved["ModelExplicit"]) != "true" || string(preserved["SliceConfigs"]) != `{"a":{"Nested":[1,2,3]}}` {
		t.Fatalf("unrelated fields changed: %v", preserved)
	}
	legacy := read("wf-legacy")
	if string(legacy["Checks"]) != `["go test ./..."]` || string(legacy["Timeout"]) != "600000000000" {
		t.Fatalf("unrelated fields changed: %v", legacy)
	}
	var corrupt []byte
	if err = store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, "wf-corrupt").Scan(&corrupt); err != nil {
		t.Fatal(err)
	}
	if string(corrupt) != "not json at all" {
		t.Fatalf("corrupt input rewritten: %q", corrupt)
	}
	w, err := store.Get("wf-legacy")
	if err != nil || w.State != StateVerifying || w.StateVersion != 4 {
		t.Fatalf("in-flight workflow disturbed: %+v err=%v", w, err)
	}
	var version int
	if err = store.Database().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 19 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestCurrentSchemaRepairsResultTransportWrittenByOlderProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "workflows.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(Workflow{ID: "wf-mixed-version", Route: RouteSimple, MinimumRisk: RiskLow, BasisTree: "tree"}); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"Request":"preserve candidate","ResultTransport":"","AllowedPaths":["src/a.ts"]}`)
	if _, err = store.Database().Exec(`INSERT INTO workflow_run_inputs(workflow_id,input) VALUES(?,?)`, "wf-mixed-version", legacy); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var raw []byte
	if err = store.Database().QueryRow(`SELECT input FROM workflow_run_inputs WHERE workflow_id=?`, "wf-mixed-version").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var input map[string]json.RawMessage
	if err = json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	var transport string
	if err = json.Unmarshal(input["ResultTransport"], &transport); err != nil {
		t.Fatal(err)
	}
	if transport != ResultTransportFileV1 {
		t.Fatalf("transport=%q", transport)
	}
	if string(input["Request"]) != `"preserve candidate"` || string(input["AllowedPaths"]) != `["src/a.ts"]` {
		t.Fatalf("unrelated input changed: %s", raw)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
