package verification

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/joeldevz/skynex/internal/artifact"
	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/workflow"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Command struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
}
type Plan struct {
	Checks         []Command
	Acceptance     []Command
	Timeout        time.Duration
	MaxOutputBytes int
}
type Evidence struct {
	ID, Kind, CandidateTree, Command string
	Args                             []string
	ExitCode                         int
	OutputDigest                     string
	StartedAt, FinishedAt            time.Time
	Truncated                        bool
	OutputArtifactID                 string
	output                           []byte
}
type Result struct {
	Candidate gitcandidate.Candidate
	Record    review.CandidateRecord
	Floor     review.RiskFloor
	Evidence  []Evidence
	Passed    bool
}
type Runner struct {
	Store            *workflow.SQLiteStore
	Policy           gitcandidate.Policy
	RiskPolicy       review.RiskPolicy
	EngineVersion    string
	BeforeTransition func() error
}

func (r *Runner) Run(ctx context.Context, workflowID string, seal gitcandidate.ContextSeal, plan Plan) (Result, error) {
	w, err := r.Store.Get(workflowID)
	if err != nil {
		return Result{}, err
	}
	if w.State != workflow.StateVerifying {
		return Result{}, fmt.Errorf("verification: workflow is %s", w.State)
	}
	if existing, ok := r.load(workflowID); ok {
		drift, driftErr := gitcandidate.DetectDrift(existing.Candidate, r.Policy)
		if driftErr != nil {
			return Result{}, driftErr
		}
		if drift.Any() {
			_, _ = r.Store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReplanRequired, IdempotencyKey: "verification:drift:v1"})
			return Result{}, errors.New("verification: candidate drifted before replay")
		}
		if r.BeforeTransition != nil {
			if err = r.BeforeTransition(); err != nil {
				return Result{}, err
			}
		}
		drift, err = gitcandidate.DetectDrift(existing.Candidate, r.Policy)
		if err != nil {
			return Result{}, err
		}
		if drift.Any() {
			_, _ = r.Store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReplanRequired, IdempotencyKey: "verification:drift:v1"})
			return Result{}, errors.New("verification: candidate drifted before freeze")
		}
		_, err = r.Store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateCandidateFrozen, IdempotencyKey: "verification:frozen:v1"})
		return existing, err
	}
	candidate, err := gitcandidate.Freeze(seal, r.Policy)
	if err != nil {
		return Result{}, err
	}
	limit := plan.MaxOutputBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	timeout := plan.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	var evidence []Evidence
	passed := true
	for _, group := range []struct {
		kind     string
		commands []Command
	}{{"check", plan.Checks}, {"acceptance", plan.Acceptance}} {
		for _, command := range group.commands {
			item, runErr := runCommand(ctx, seal.RepositoryRoot, candidate.TreeOID, group.kind, command, timeout, limit)
			artifacts := artifact.Store{DB: r.Store.Database(), Root: filepath.Join(seal.GitCommonDir, "skynex", "artifacts")}
			if records, artifactErr := artifacts.PutLog(workflowID, "log", item.output); artifactErr != nil {
				return Result{}, artifactErr
			} else if len(records) > 0 {
				item.OutputArtifactID = records[0].ID
				_ = artifacts.Ref(records[0].ID, "verification_evidence", item.ID)
			}
			item.output = nil
			evidence = append(evidence, item)
			if runErr != nil {
				passed = false
				break
			}
		}
		if !passed {
			break
		}
	}
	after, err := gitcandidate.Freeze(seal, r.Policy)
	if err != nil {
		return Result{}, err
	}
	if after.TreeOID != candidate.TreeOID {
		passed = false
	}
	// The workflow basis may be an adopted dirty tree captured at start, while
	// the context seal intentionally remains anchored to the original HEAD for
	// repository identity and drift checks. Classify only mutations made after
	// that adopted basis; otherwise a zero-operation workflow would treat the
	// user's pre-existing files as its own changes.
	changes, err := candidateChanges(seal.RepositoryRoot, w.BasisTree, candidate.TreeOID)
	if err != nil {
		return Result{}, err
	}
	floor := review.DeterministicFloor(w.Route, changes, r.RiskPolicy)
	record, err := review.NewCandidateRecord(workflowID, candidate, r.RiskPolicy.Hash(), r.EngineVersion, time.Now())
	if err != nil {
		return Result{}, err
	}
	result := Result{Candidate: candidate, Record: record, Floor: floor, Evidence: evidence, Passed: passed}
	if err = r.persist(workflowID, result); err != nil {
		return Result{}, err
	}
	if !passed {
		_, err = r.Store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReplanRequired, IdempotencyKey: "verification:failed:v1"})
		return result, err
	}
	if r.BeforeTransition != nil {
		if err = r.BeforeTransition(); err != nil {
			return result, err
		}
	}
	drift, driftErr := gitcandidate.DetectDrift(candidate, r.Policy)
	if driftErr != nil {
		return result, driftErr
	}
	if drift.Any() {
		_, _ = r.Store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReplanRequired, IdempotencyKey: "verification:drift:v1"})
		return result, errors.New("verification: candidate drifted before freeze")
	}
	_, err = r.Store.Transition(workflow.Transition{WorkflowID: w.ID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateCandidateFrozen, IdempotencyKey: "verification:frozen:v1"})
	return result, err
}
func (r *Runner) persist(workflowID string, result Result) error {
	raw, _ := json.Marshal(result)
	tx, err := r.Store.Database().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO verification_runs(workflow_id,candidate_tree,result) VALUES(?,?,?)`, workflowID, result.Candidate.TreeOID, raw); err != nil {
		return err
	}
	for _, e := range result.Evidence {
		item, _ := json.Marshal(e)
		if _, err = tx.Exec(`INSERT OR IGNORE INTO verification_evidence(id,workflow_id,candidate_tree,kind,evidence) VALUES(?,?,?,?,?)`, e.ID, workflowID, e.CandidateTree, e.Kind, item); err != nil {
			return err
		}
	}
	record, _ := json.Marshal(result.Record)
	if _, err = tx.Exec(`INSERT OR IGNORE INTO review_candidates(id,workflow_id,tree_oid,policy_hash,record) VALUES(?,?,?,?,?)`, result.Record.ID, workflowID, result.Record.TreeOID, result.Record.PolicyHash, record); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *Runner) load(workflowID string) (Result, bool) {
	var raw []byte
	if r.Store.Database().QueryRow(`SELECT result FROM verification_runs WHERE workflow_id=?`, workflowID).Scan(&raw) != nil {
		return Result{}, false
	}
	var result Result
	if json.Unmarshal(raw, &result) != nil {
		return Result{}, false
	}
	return result, true
}

type limitBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return original, nil
}
func runCommand(parent context.Context, dir, tree, kind string, c Command, timeout time.Duration, limit int) (Evidence, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	started := time.Now().UTC()
	buffer := &limitBuffer{limit: limit}
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = dir
	cmd.Stdout = buffer
	cmd.Stderr = buffer
	err := cmd.Run()
	exit := 0
	if err != nil {
		exit = -1
		if value, ok := err.(*exec.ExitError); ok {
			exit = value.ExitCode()
		}
	}
	sum := sha256.Sum256(buffer.Bytes())
	basis := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", tree, kind, c.Name, exit, hex.EncodeToString(sum[:]))
	idSum := sha256.Sum256([]byte(basis))
	item := Evidence{ID: "ve_" + hex.EncodeToString(idSum[:]), Kind: kind, CandidateTree: tree, Command: c.Name, Args: append([]string(nil), c.Args...), ExitCode: exit, OutputDigest: hex.EncodeToString(sum[:]), StartedAt: started, FinishedAt: time.Now().UTC(), Truncated: buffer.truncated}
	item.output = append([]byte(nil), buffer.Bytes()...)
	return item, err
}
func candidateChanges(repo, base, candidate string) ([]review.Change, error) {
	cmd := exec.Command("git", "diff", "--name-only", "-z", base, candidate)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var result []review.Change
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		path := string(raw)
		kind := review.ChangeText
		baseName := filepath.Base(path)
		if baseName == "go.mod" || baseName == "go.sum" || baseName == "package.json" || baseName == "package-lock.json" {
			kind = review.ChangeDependency
		}
		if strings.Contains(path, "schema") || strings.Contains(path, "migration") {
			kind = review.ChangeSchema
		}
		result = append(result, review.Change{Path: path, Kind: kind})
	}
	return result, nil
}
func ToReviewEvidence(record review.CandidateRecord, evidence []Evidence) []review.Evidence {
	result := make([]review.Evidence, 0, len(evidence))
	for _, e := range evidence {
		kind := review.EvidenceCheck
		if e.Kind == "acceptance" {
			kind = review.EvidenceAcceptance
		}
		result = append(result, review.Evidence{ID: e.ID, Kind: kind, CandidateRecordID: record.ID, CandidateTreeOID: record.TreeOID, PolicyHash: record.PolicyHash, Digest: e.OutputDigest})
	}
	return result
}
func ValidateEvidenceBasis(record review.CandidateRecord, evidence []Evidence) error {
	for _, e := range evidence {
		if e.CandidateTree != record.TreeOID {
			return review.ErrEvidenceMismatch
		}
	}
	return nil
}
