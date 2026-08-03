package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/workflow"
)

var ErrMalformedReview = errors.New("review: malformed OpenCode result")

type Finding struct {
	ID              string   `json:"id"`
	Lens            string   `json:"lens"`
	Severity        string   `json:"severity"`
	Message         string   `json:"message"`
	Reproducible    bool     `json:"reproducible"`
	CandidateCaused bool     `json:"candidate_caused"`
	EvidenceIDs     []string `json:"evidence_ids"`
}
type semanticOutput struct {
	RequestedRisk Risk   `json:"requested_risk"`
	SelectedLens  Lens   `json:"selected_lens"`
	Justification string `json:"justification"`
}
type lensOutput struct {
	Findings []Finding `json:"findings"`
}
type OpenCodeReviewOptions struct {
	Executable, Model string
	Timeout           time.Duration
	MaxOutputBytes    int
}
type OpenCodeReviewRunner struct {
	Store   *workflow.SQLiteStore
	Options OpenCodeReviewOptions
}

func (r *OpenCodeReviewRunner) Run(ctx context.Context, workflowID string) (Receipt, error) {
	w, err := r.Store.Get(workflowID)
	if err != nil {
		return Receipt{}, err
	}
	if authority, e := NewSQLiteStore(r.Store.Database()).Authority(workflowID); e == nil {
		return authority, nil
	}
	if w.State == workflow.StateCandidateFrozen {
		w, err = r.Store.Transition(workflow.Transition{WorkflowID: workflowID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReviewing, IdempotencyKey: "review:start:v1"})
		if err != nil {
			return Receipt{}, err
		}
	}
	if w.State != workflow.StateReviewing {
		return Receipt{}, fmt.Errorf("review: workflow is %s", w.State)
	}
	var raw []byte
	if err = r.Store.Database().QueryRow(`SELECT result FROM verification_runs WHERE workflow_id=?`, workflowID).Scan(&raw); err != nil {
		return Receipt{}, err
	}
	var verified struct {
		Candidate gitcandidate.Candidate
		Record    CandidateRecord
		Floor     RiskFloor
	}
	if err = json.Unmarshal(raw, &verified); err != nil {
		return Receipt{}, err
	}
	if err = ValidateCandidateRecord(verified.Record); err != nil {
		return Receipt{}, err
	}
	if verified.Record.TreeOID != verified.Candidate.TreeOID || verified.Record.PolicyHash != verified.Floor.PolicyHash {
		return Receipt{}, ErrCandidateMismatch
	}
	if drift, e := gitcandidate.DetectDrift(verified.Candidate, gitcandidate.Policy{}); e != nil {
		return Receipt{}, e
	} else if drift.Any() {
		_, _ = r.Store.Transition(workflow.Transition{WorkflowID: workflowID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReplanRequired, IdempotencyKey: "review:drift:v1"})
		return Receipt{}, ErrCandidateMismatch
	}
	semanticRaw, err := r.invoke(ctx, workflowID, verified.Record, "semantic", "Assess risk. Output requested_risk, selected_lens when medium, and justification.")
	if err != nil {
		return Receipt{}, err
	}
	var semantic semanticOutput
	if json.Unmarshal(semanticRaw, &semantic) != nil || semantic.Justification == "" {
		return Receipt{}, ErrMalformedReview
	}
	assessment, err := AssessSemantic(verified.Record, verified.Floor, SemanticInput{RequestedRisk: semantic.RequestedRisk, SelectedLens: semantic.SelectedLens, Justification: semantic.Justification, ModelProvider: "opencode", ModelID: r.Options.Model, PromptTemplateID: "semantic:v1", RenderedRedactedPrompt: "semantic review"}, time.Now())
	if err != nil {
		return Receipt{}, err
	}
	evidence, err := r.verificationEvidence(verified.Record)
	if err != nil {
		return Receipt{}, err
	}
	var lenses []Lens
	if assessment.SelectedDepth == DepthOneLens {
		lenses = []Lens{assessment.SelectedLens}
	}
	if assessment.SelectedDepth == DepthFourLenses {
		lenses = []Lens{LensRisk, LensReadability, LensReliability, LensResilience}
	}
	severe := false
	for _, lens := range lenses {
		lensRaw, invokeErr := r.invoke(ctx, workflowID, verified.Record, string(lens), "Review lens "+string(lens)+". Output structured findings only; do not modify files.")
		if invokeErr != nil {
			return Receipt{}, invokeErr
		}
		var output lensOutput
		if json.Unmarshal(lensRaw, &output) != nil {
			return Receipt{}, ErrMalformedReview
		}
		evidence = append(evidence, Evidence{ID: "evidence:review:" + workflowID + ":" + string(lens), Kind: EvidenceReview, CandidateRecordID: verified.Record.ID, CandidateTreeOID: verified.Record.TreeOID, PolicyHash: verified.Record.PolicyHash, Digest: hash(lensRaw), Lens: lens})
		for i := range output.Findings {
			f := &output.Findings[i]
			f.Lens = string(lens)
			if f.ID == "" {
				f.ID = fmt.Sprintf("finding:%s:%s:%d", workflowID, lens, i)
			}
			item, _ := json.Marshal(f)
			if _, err = r.Store.Database().Exec(`INSERT OR IGNORE INTO review_findings(id,workflow_id,candidate_tree,lens,finding) VALUES(?,?,?,?,?)`, f.ID, workflowID, verified.Record.TreeOID, lens, item); err != nil {
				return Receipt{}, err
			}
			eid := "evidence:" + f.ID
			evidence = append(evidence, Evidence{ID: eid, Kind: EvidenceReview, CandidateRecordID: verified.Record.ID, CandidateTreeOID: verified.Record.TreeOID, PolicyHash: verified.Record.PolicyHash, Digest: hash(item), Lens: lens})
			if f.Severity == "severe" && f.Reproducible && f.CandidateCaused {
				severe = true
			}
		}
	}
	if severe {
		_, _ = r.Store.Transition(workflow.Transition{WorkflowID: workflowID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReplanRequired, IdempotencyKey: "review:severe:v1"})
		return Receipt{}, errors.New("review: severe reproducible candidate-caused finding")
	}
	for _, e := range evidence {
		assessment.EvidenceIDs = append(assessment.EvidenceIDs, e.ID)
	}
	receipt, err := NewSQLiteStore(r.Store.Database()).Issue(IssueRequest{Candidate: verified.Record, Floor: verified.Floor, Assessment: assessment, Evidence: evidence, IssuedAt: time.Now()})
	if err != nil {
		return Receipt{}, err
	}
	w, _ = r.Store.Get(workflowID)
	_, err = r.Store.Transition(workflow.Transition{WorkflowID: workflowID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReceipted, IdempotencyKey: "review:receipted:v1", ArtifactIDs: []string{receipt.ID}})
	return receipt, err
}

func (r *OpenCodeReviewRunner) verificationEvidence(c CandidateRecord) ([]Evidence, error) {
	rows, err := r.Store.Database().Query(`SELECT id,kind,evidence FROM verification_evidence WHERE workflow_id=? ORDER BY id`, c.WorkflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Evidence
	for rows.Next() {
		var id, kind string
		var raw []byte
		if err = rows.Scan(&id, &kind, &raw); err != nil {
			return nil, err
		}
		ek := EvidenceCheck
		if kind == "acceptance" {
			ek = EvidenceAcceptance
		}
		out = append(out, Evidence{ID: id, Kind: ek, CandidateRecordID: c.ID, CandidateTreeOID: c.TreeOID, PolicyHash: c.PolicyHash, Digest: hash(raw)})
	}
	return out, rows.Err()
}

func (r *OpenCodeReviewRunner) invoke(parent context.Context, workflowID string, c CandidateRecord, lens, prompt string) ([]byte, error) {
	timeout := r.Options.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	exe := r.Options.Executable
	if exe == "" {
		exe = "opencode"
	}
	parentDir, err := os.MkdirTemp("", "skynex-review-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(parentDir)
	wt := filepath.Join(parentDir, "candidate")
	if out, e := gitAt(c.Seal.RepositoryRoot, "worktree", "add", "--detach", wt, c.Seal.BaseCommitOID); e != nil {
		return nil, fmt.Errorf("review worktree: %w %s", e, out)
	}
	defer gitAt(c.Seal.RepositoryRoot, "worktree", "remove", "--force", wt)
	if out, e := gitAt(wt, "read-tree", "--reset", "-u", c.TreeOID); e != nil {
		return nil, fmt.Errorf("review tree: %w %s", e, out)
	}
	_ = filepath.Walk(wt, func(path string, info os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		mode := os.FileMode(0o400)
		if info.IsDir() {
			mode = 0o500
		}
		return os.Chmod(path, mode)
	})
	defer filepath.Walk(wt, func(path string, info os.FileInfo, e error) error {
		if e != nil {
			return nil
		}
		mode := os.FileMode(0o600)
		if info.IsDir() {
			mode = 0o700
		}
		_ = os.Chmod(path, mode)
		return nil
	})
	result := filepath.Join(parentDir, "result.json")
	args := []string{"run", "--format", "json", "--auto", "--dir", wt, "--agent", "plan"}
	if r.Options.Model != "" {
		args = append(args, "--model", r.Options.Model)
	}
	args = append(args, prompt+" Write JSON to $SKYNEX_RESULT_FILE")
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	var output bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = wt
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "SKYNEX_RESULT_FILE=" + result}
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	start := time.Now().UTC()
	runErr := cmd.Run()
	end := time.Now().UTC()
	status := "completed"
	if runErr != nil {
		status = "failed"
	}
	if ctx.Err() != nil {
		status = "timeout"
	}
	raw, readErr := os.ReadFile(result)
	digest := hash(output.Bytes())
	if readErr == nil {
		digest = hash(raw)
	}
	id := "review:" + workflowID + ":" + lens
	_, _ = r.Store.Database().Exec(`INSERT OR REPLACE INTO review_invocations(id,workflow_id,candidate_tree,lens,model,status,output_digest,started_at,finished_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, workflowID, c.TreeOID, lens, r.Options.Model, status, digest, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if runErr != nil {
		return nil, runErr
	}
	if readErr != nil {
		return nil, ErrMalformedReview
	}
	return raw, nil
}
func gitAt(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
func hash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
