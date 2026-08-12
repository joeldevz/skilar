package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joeldevz/skynex/internal/approval"
	artifactstore "github.com/joeldevz/skynex/internal/artifact"
	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/processregistry"
	"github.com/joeldevz/skynex/internal/workflow"
)

var ErrMalformedReview = errors.New("review: malformed OpenCode result")
var ErrReviewIdleTimeout = errors.New("review: OpenCode produced no output before idle timeout")

func semanticPrompt(floor RiskFloor) string {
	return fmt.Sprintf(`Assess risk for the immutable candidate. The deterministic minimum risk is %q and cannot be lowered; requested_risk MUST be %q or higher. Write exactly one JSON object and nothing else to $SKYNEX_RESULT_FILE. Schema: {"requested_risk":"low|medium|high","selected_lens":"|risk|readability|reliability|resilience","justification":"non-empty string"}. requested_risk is required. selected_lens is required and non-empty only when requested_risk is medium. No Markdown or code fences.`, floor.Risk, floor.Risk)
}

func lensPrompt(lens Lens) string {
	return fmt.Sprintf(`Review lens %s only for the immutable candidate. Write exactly one JSON object and nothing else to $SKYNEX_RESULT_FILE. Schema: {"findings":[{"id":"optional string","lens":"optional string","severity":"severe|warning|info","message":"non-empty string","reproducible":true|false,"candidate_caused":true|false,"evidence_ids":["string"]}]}. findings is required (empty array allowed). No Markdown or code fences.`, lens)
}

func validateSemanticOutput(raw []byte, floor RiskFloor, out *semanticOutput) string {
	if err := json.Unmarshal(raw, out); err != nil {
		return "invalid JSON: " + err.Error()
	}
	if out.RequestedRisk != RiskLow && out.RequestedRisk != RiskMedium && out.RequestedRisk != RiskHigh {
		return "missing or invalid requested_risk"
	}
	if out.Justification == "" {
		return "missing justification"
	}
	if rank(out.RequestedRisk) < rank(floor.Risk) {
		return fmt.Sprintf("requested_risk %q is below deterministic minimum %q", out.RequestedRisk, floor.Risk)
	}
	validLens := out.SelectedLens == LensRisk || out.SelectedLens == LensReadability || out.SelectedLens == LensReliability || out.SelectedLens == LensResilience
	if out.RequestedRisk == RiskMedium && !validLens {
		return "missing or invalid selected_lens for medium risk"
	}
	if out.RequestedRisk != RiskMedium && out.SelectedLens != "" {
		return "selected_lens must be empty unless requested_risk is medium"
	}
	return ""
}

func validateLensOutput(raw []byte, out *lensOutput) string {
	if err := json.Unmarshal(raw, out); err != nil {
		return "invalid JSON: " + err.Error()
	}
	var shape map[string]json.RawMessage
	_ = json.Unmarshal(raw, &shape)
	if _, ok := shape["findings"]; !ok {
		return "missing findings"
	}
	for i, f := range out.Findings {
		if f.Message == "" {
			return fmt.Sprintf("finding %d missing message", i)
		}
		if f.Severity != "severe" && f.Severity != "warning" && f.Severity != "info" {
			return fmt.Sprintf("finding %d invalid severity", i)
		}
	}
	return ""
}

func (r *OpenCodeReviewRunner) persistMalformed(workflowID, candidateID, lens string, raw []byte, detail string) {
	redacted := (&artifactstore.Store{}).Redact(raw)
	if len(redacted) > 4096 {
		redacted = redacted[:4096]
	}
	preview := detail + ": " + string(redacted)
	if len(preview) > 4096 {
		preview = preview[:4096]
	}
	_, _ = r.Store.Database().Exec(`UPDATE review_invocations SET status='malformed',error_preview=? WHERE id=?`, preview, reviewInvocationID(workflowID, candidateID, lens))
}

type Finding struct {
	ID              string   `json:"id"`
	SourceID        string   `json:"source_id,omitempty"`
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
	IdleTimeout       time.Duration
	// Preflight is invoked before the durable candidate_frozen -> reviewing
	// transition. It must not create review invocations or consume authority.
	Preflight func(context.Context) error
}
type OpenCodeReviewRunner struct {
	Store   *workflow.SQLiteStore
	Options OpenCodeReviewOptions
}

func (r *OpenCodeReviewRunner) modelIdentity() string {
	if r.Options.Model != "" {
		return r.Options.Model
	}
	// OpenCode may select a provider/model from its own configuration when the
	// workflow does not pin --model. Keep that selection mode explicit and
	// auditable without passing this sentinel back to the OpenCode CLI.
	return "opencode-provider-default"
}

func (r *OpenCodeReviewRunner) Run(ctx context.Context, workflowID string) (Receipt, error) {
	r.reconcileInterrupted(workflowID)
	w, err := r.Store.Get(workflowID)
	if err != nil {
		return Receipt{}, err
	}
	if authority, e := NewSQLiteStore(r.Store.Database()).Authority(workflowID); e == nil {
		return authority, nil
	}
	if r.Options.Preflight != nil {
		if err = r.Options.Preflight(ctx); err != nil {
			return Receipt{}, err
		}
	}
	if w.State == workflow.StateCandidateFrozen {
		w, err = r.Store.Transition(workflow.Transition{WorkflowID: workflowID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReviewing, IdempotencyKey: reviewTransitionKey("start", w.StateVersion)})
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
		_, _ = r.Store.Transition(workflow.Transition{WorkflowID: workflowID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReplanRequired, IdempotencyKey: reviewTransitionKey("drift", w.StateVersion)})
		return Receipt{}, ErrCandidateMismatch
	}
	prompt := semanticPrompt(verified.Floor)
	semanticRaw, err := r.invoke(ctx, workflowID, verified.Record, "semantic", prompt)
	if err != nil {
		return Receipt{}, err
	}
	var semantic semanticOutput
	if detail := validateSemanticOutput(semanticRaw, verified.Floor, &semantic); detail != "" {
		r.persistMalformed(workflowID, verified.Record.ID, "semantic", semanticRaw, detail)
		return Receipt{}, fmt.Errorf("%w: %s", ErrMalformedReview, detail)
	}
	if err = r.persistCheckpoint(workflowID, verified.Record, "semantic", prompt, semanticRaw); err != nil {
		return Receipt{}, err
	}
	assessment, err := AssessSemantic(verified.Record, verified.Floor, SemanticInput{RequestedRisk: semantic.RequestedRisk, SelectedLens: semantic.SelectedLens, Justification: semantic.Justification, ModelProvider: "opencode", ModelID: r.modelIdentity(), PromptTemplateID: "semantic:v2", RenderedRedactedPrompt: fmt.Sprintf("semantic review; deterministic minimum=%s", verified.Floor.Risk)}, time.Now())
	if err != nil {
		return Receipt{}, err
	}
	if assessment.EffectiveRisk == RiskHigh {
		if _, err = approval.Require(r.Store.Database(), workflowID, "review", verified.Record.TreeOID, verified.Record.PolicyHash, time.Now()); err != nil {
			return Receipt{}, err
		}
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
	var severeFindingIDs []string
	for _, lens := range lenses {
		lensRaw, invokeErr := r.invoke(ctx, workflowID, verified.Record, string(lens), lensPrompt(lens))
		if invokeErr != nil {
			return Receipt{}, invokeErr
		}
		var output lensOutput
		if detail := validateLensOutput(lensRaw, &output); detail != "" {
			r.persistMalformed(workflowID, verified.Record.ID, string(lens), lensRaw, detail)
			return Receipt{}, fmt.Errorf("%w: %s", ErrMalformedReview, detail)
		}
		if err = r.persistCheckpoint(workflowID, verified.Record, string(lens), lensPrompt(lens), lensRaw); err != nil {
			return Receipt{}, err
		}
		evidence = append(evidence, Evidence{ID: "evidence:review:" + workflowID + ":" + string(lens), Kind: EvidenceReview, CandidateRecordID: verified.Record.ID, CandidateTreeOID: verified.Record.TreeOID, PolicyHash: verified.Record.PolicyHash, Digest: hash(lensRaw), Lens: lens})
		for i := range output.Findings {
			f := &output.Findings[i]
			f.Lens = string(lens)
			// Model-provided IDs are untrusted labels, not global authority. Keep
			// them only as source metadata and assign an engine-owned identity.
			f.SourceID = f.ID
			f.ID = fmt.Sprintf("finding:%s:%s:%s:%d", workflowID, verified.Record.ID, lens, i)
			item, _ := json.Marshal(f)
			var existingWorkflow, existingTree string
			var existingItem []byte
			existingErr := r.Store.Database().QueryRow(`SELECT workflow_id,candidate_tree,finding FROM review_findings WHERE id=?`, f.ID).Scan(&existingWorkflow, &existingTree, &existingItem)
			if existingErr == nil {
				if existingWorkflow != workflowID || existingTree != verified.Record.TreeOID || !bytes.Equal(existingItem, item) {
					return Receipt{}, fmt.Errorf("review: authoritative finding identity collision for %s (historical/global ID conflict)", f.ID)
				}
			} else if _, err = r.Store.Database().Exec(`INSERT INTO review_findings(id,workflow_id,candidate_tree,lens,finding) VALUES(?,?,?,?,?)`, f.ID, workflowID, verified.Record.TreeOID, lens, item); err != nil {
				return Receipt{}, fmt.Errorf("review: authoritative finding identity collision for %s (historical/global ID conflict): %w", f.ID, err)
			}
			eid := "evidence:" + f.ID
			evidence = append(evidence, Evidence{ID: eid, Kind: EvidenceReview, CandidateRecordID: verified.Record.ID, CandidateTreeOID: verified.Record.TreeOID, PolicyHash: verified.Record.PolicyHash, Digest: hash(item), Lens: lens})
			if f.Severity == "severe" && f.Reproducible && f.CandidateCaused {
				severe = true
				severeFindingIDs = append(severeFindingIDs, f.ID)
			}
		}
	}
	if severe {
		_, _ = r.Store.Transition(workflow.Transition{WorkflowID: workflowID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReplanRequired, IdempotencyKey: reviewTransitionKey("severe", w.StateVersion), ArtifactIDs: severeFindingIDs})
		return Receipt{}, errors.New("review: severe reproducible candidate-caused finding")
	}
	for _, e := range evidence {
		assessment.EvidenceIDs = append(assessment.EvidenceIDs, e.ID)
	}
	receipt, err := NewSQLiteStore(r.Store.Database()).Issue(IssueRequest{Candidate: verified.Record, Floor: verified.Floor, Assessment: assessment, Evidence: evidence, IssuedAt: time.Now()})
	if err != nil {
		return Receipt{}, err
	}
	artifacts := artifactstore.Store{DB: r.Store.Database(), Root: filepath.Join(verified.Record.Seal.GitCommonDir, "skynex", "artifacts")}
	rows, _ := r.Store.Database().Query(`SELECT evidence FROM verification_evidence WHERE workflow_id=?`, workflowID)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			_ = rows.Scan(&raw)
			var item struct{ OutputArtifactID string }
			if json.Unmarshal(raw, &item) == nil && item.OutputArtifactID != "" {
				_ = artifacts.Ref(item.OutputArtifactID, "receipt_authority", workflowID)
			}
		}
	}
	w, _ = r.Store.Get(workflowID)
	_, err = r.Store.Transition(workflow.Transition{WorkflowID: workflowID, ExpectedState: w.State, ExpectedVersion: w.StateVersion, NextState: workflow.StateReceipted, IdempotencyKey: reviewTransitionKey("receipted", w.StateVersion), ArtifactIDs: []string{receipt.ID}})
	return receipt, err
}

func reviewTransitionKey(kind string, stateVersion uint64) string {
	return fmt.Sprintf("review:%s:v1:sv%d", kind, stateVersion)
}

func reviewInvocationID(workflowID, candidateID, lens string) string {
	return "review:" + workflowID + ":" + candidateID + ":" + lens
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
	invocationID := reviewInvocationID(workflowID, c.ID, lens)
	promptHash := hash([]byte(prompt))
	modelIdentity := r.modelIdentity()
	checkpointID := hash([]byte(workflowID + "\x00" + c.TreeOID + "\x00" + c.PolicyHash + "\x00" + lens + "\x00" + modelIdentity + "\x00" + promptHash))
	var cached []byte
	if err := r.Store.Database().QueryRow(`SELECT result_json FROM review_checkpoints WHERE id=? AND workflow_id=? AND candidate_tree=? AND policy_hash=? AND lens=? AND model=? AND prompt_hash=?`, checkpointID, workflowID, c.TreeOID, c.PolicyHash, lens, modelIdentity, promptHash).Scan(&cached); err == nil {
		return cached, nil
	}
	// Recover a result durably written after process completion but before the
	// caller validated/checkpointed it. Validation still happens in Run.
	legacyInvocationID := "review:" + workflowID + ":" + lens
	if err := r.Store.Database().QueryRow(`SELECT result_json FROM review_invocations WHERE id IN (?,?) AND workflow_id=? AND candidate_tree=? AND lens=? AND model=? AND prompt_hash=? AND policy_hash=? AND status='completed' AND length(result_json)>0 ORDER BY CASE id WHEN ? THEN 0 ELSE 1 END LIMIT 1`, invocationID, legacyInvocationID, workflowID, c.TreeOID, lens, modelIdentity, promptHash, c.PolicyHash, invocationID).Scan(&cached); err == nil {
		// review_invocations stores the raw process result; the fresh path
		// returns it redacted, so a resumed run must produce the same bytes or
		// the receipt digest would not be reproducible across an interruption.
		return (&artifactstore.Store{}).Redact(cached), nil
	}
	timeout := r.Options.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	exe := r.Options.Executable
	var executableIdentity os.FileInfo
	if exe == "" {
		exe = "opencode"
	}
	hermetic := hermeticReviewOpenCodeMode()
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
	if err = makeReviewWorktreeReadOnly(wt); err != nil {
		return nil, err
	}
	if hermetic {
		exe, err = resolveHermeticReviewExecutable(r.Options.Executable, wt)
		if err != nil {
			return nil, err
		}
		executableIdentity, err = snapshotHermeticReviewExecutable(exe)
		if err != nil {
			return nil, err
		}
	}
	immutableBefore, err := snapshotReviewWorktree(wt)
	if err != nil {
		return nil, fmt.Errorf("review immutable snapshot: %w", err)
	}
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
	args := reviewOpenCodeArgs(wt, prompt, r.Options.Model, hermetic)
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	unregister := processregistry.Register(workflowID, "review:"+workflowID+":"+lens, cancel)
	defer unregister()
	idleTimeout := r.Options.IdleTimeout
	if idleTimeout <= 0 {
		// Stdout silence is not proof of inactivity: OpenCode can spend several
		// minutes waiting on an active provider request while producing no stream
		// bytes. Until a runtime-activity signal is available, never let the idle
		// watchdog expire earlier than the command's authoritative timeout.
		idleTimeout = timeout + time.Second
	}
	activity := make(chan struct{}, 1)
	output := &reviewRollingBuffer{limit: 4096, onWrite: func(snapshot []byte) {
		select {
		case activity <- struct{}{}:
		default:
		}
		preview := (&artifactstore.Store{}).Redact(snapshot)
		for attempt := 0; attempt < 3; attempt++ {
			stamp := time.Now().UTC().Format(time.RFC3339Nano)
			if _, updateErr := r.Store.Database().Exec(`UPDATE review_invocations SET last_activity_at=?,heartbeat_at=?,error_preview=? WHERE id=? AND status='running'`, stamp, stamp, string(preview), invocationID); updateErr == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = wt
	cmd.Env, err = reviewOpenCodeRuntimeEnv(parentDir, result, hermetic)
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	configureReviewProcess(cmd, evaluatorManagedReviewProcess())
	if hermetic {
		if err = revalidateHermeticReviewExecutable(r.Options.Executable, wt, exe, executableIdentity); err != nil {
			return nil, err
		}
	}
	start := time.Now().UTC()
	id := invocationID
	_, err = r.Store.Database().Exec(`INSERT OR REPLACE INTO review_invocations(id,workflow_id,candidate_tree,lens,model,status,output_digest,started_at,finished_at,error_preview,pid,heartbeat_at,last_activity_at,result_json,prompt_hash,policy_hash) VALUES(?,?,?,?,?,'running','',?,'','',0,?,?,'',?,?)`, id, workflowID, c.TreeOID, lens, modelIdentity, start.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano), promptHash, c.PolicyHash)
	if err != nil {
		return nil, err
	}
	runErr := cmd.Start()
	if runErr == nil {
		_, _ = r.Store.Database().Exec(`UPDATE review_invocations SET pid=? WHERE id=?`, cmd.Process.Pid, id)
	}
	var outputReaders sync.WaitGroup
	if runErr == nil {
		outputReaders.Add(2)
		go func() { defer outputReaders.Done(); _, _ = io.Copy(output, stdoutPipe) }()
		go func() { defer outputReaders.Done(); _, _ = io.Copy(output, stderrPipe) }()
	}
	idleDone := make(chan struct{})
	var idleFired atomic.Bool
	var watchdog sync.WaitGroup
	if runErr == nil {
		watchdog.Add(1)
		go func() {
			defer watchdog.Done()
			timer := time.NewTimer(idleTimeout)
			defer timer.Stop()
			heartbeat := time.NewTicker(100 * time.Millisecond)
			defer heartbeat.Stop()
			for {
				select {
				case <-idleDone:
					return
				case <-activity:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(idleTimeout)
				case now := <-heartbeat.C:
					preview := (&artifactstore.Store{}).Redact(output.Bytes())
					_, _ = r.Store.Database().Exec(`UPDATE review_invocations SET heartbeat_at=?,error_preview=? WHERE id=? AND status='running'`, now.UTC().Format(time.RFC3339Nano), string(preview), id)
				case <-timer.C:
					idleFired.Store(true)
					cancel()
					return
				}
			}
		}()
	}
	// Drain both pipes to EOF before Wait closes their descriptors. Waiting
	// first can race the final stderr bytes and turn a useful redacted failure
	// preview into an opaque "exit status 1" under race instrumentation.
	outputReaders.Wait()
	if runErr == nil {
		runErr = cmd.Wait()
	}
	close(idleDone)
	watchdog.Wait()
	end := time.Now().UTC()
	status := "completed"
	if runErr != nil {
		status = "failed"
	}
	if idleFired.Load() {
		status = "idle_timeout"
	} else if errors.Is(ctx.Err(), context.Canceled) {
		status = "cancelled"
	} else if ctx.Err() != nil {
		status = "timeout"
	}
	raw, readErr := os.ReadFile(result)
	if raw == nil {
		raw = []byte{}
	}
	redactor := artifactstore.Store{}
	preview := redactor.Redact(output.Bytes())
	limit := r.Options.MaxOutputBytes
	if limit <= 0 || limit > 4096 {
		limit = 4096
	}
	if len(preview) > limit {
		preview = preview[:limit]
	}
	digest := hash(output.Bytes())
	if readErr == nil {
		digest = hash(raw)
	}
	if _, persistErr := r.Store.Database().Exec(`UPDATE review_invocations SET status=?,output_digest=?,finished_at=?,heartbeat_at=?,error_preview=?,result_json=? WHERE id=?`, status, digest, end.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), string(preview), raw, id); persistErr != nil {
		return nil, fmt.Errorf("review %s terminal persistence: %w", lens, persistErr)
	}
	if immutableErr := verifyReviewWorktreeImmutable(wt, c.TreeOID, immutableBefore); immutableErr != nil {
		_, _ = r.Store.Database().Exec(`UPDATE review_invocations SET status='failed',error_preview=? WHERE id=?`, immutableErr.Error(), id)
		return nil, immutableErr
	}
	if idleFired.Load() {
		return nil, fmt.Errorf("review %s invocation: %w", lens, ErrReviewIdleTimeout)
	}
	if ctx.Err() != nil {
		if len(preview) == 0 {
			return nil, fmt.Errorf("review %s invocation: %w", lens, ctx.Err())
		}
		return nil, fmt.Errorf("review %s invocation: %w: %s", lens, ctx.Err(), preview)
	}
	if runErr != nil {
		if len(preview) == 0 {
			return nil, fmt.Errorf("review %s invocation: %w", lens, runErr)
		}
		return nil, fmt.Errorf("review %s invocation: %w: %s", lens, runErr, preview)
	}
	if readErr != nil {
		return nil, ErrMalformedReview
	}
	return redactor.Redact(raw), nil
}

func reviewOpenCodeArgs(worktree, prompt, model string, hermetic bool) []string {
	args := []string{"run"}
	if hermetic {
		args = append(args, "--pure")
	}
	args = append(args, "--format", "json", "--auto", "--dir", worktree, "--agent", "workflow-reviewer")
	if model != "" {
		args = append(args, "--model", model)
	}
	return append(args, prompt+" Write JSON to $SKYNEX_RESULT_FILE")
}

func makeReviewWorktreeReadOnly(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o400)
		if info.IsDir() {
			mode = 0o500
		} else if info.Mode().Perm()&0o111 != 0 {
			mode = 0o500
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("review worktree read-only %s: %w", path, err)
		}
		return nil
	})
}

type reviewWorktreeEntry struct {
	Path   string
	Mode   os.FileMode
	Digest string
}

func snapshotReviewWorktree(root string) ([]reviewWorktreeEntry, error) {
	var entries []reviewWorktreeEntry
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := reviewWorktreeEntry{Path: filepath.ToSlash(rel), Mode: info.Mode()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entry.Digest = hash([]byte(target))
		case info.Mode().IsRegular():
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry.Digest = hash(raw)
		}
		entries = append(entries, entry)
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, err
}

func verifyReviewWorktreeImmutable(root, candidateTree string, before []reviewWorktreeEntry) error {
	after, err := snapshotReviewWorktree(root)
	if err != nil {
		return fmt.Errorf("%w: snapshot after review: %v", ErrCandidateMismatch, err)
	}
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("%w: review process modified candidate worktree content or mode", ErrCandidateMismatch)
	}
	status, err := gitAt(root, "diff", "--no-ext-diff", "--name-status")
	if err != nil {
		return fmt.Errorf("%w: inspect review worktree: %v", ErrCandidateMismatch, err)
	}
	untracked, err := gitAt(root, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("%w: inspect untracked review files: %v", ErrCandidateMismatch, err)
	}
	status += untracked
	if status != "" {
		return fmt.Errorf("%w: review process dirtied candidate worktree: %s", ErrCandidateMismatch, strings.TrimSpace(status))
	}
	tree, err := gitAt(root, "write-tree")
	if err != nil {
		return fmt.Errorf("%w: inspect review index: %v", ErrCandidateMismatch, err)
	}
	if strings.TrimSpace(tree) != candidateTree {
		return fmt.Errorf("%w: review process modified candidate index", ErrCandidateMismatch)
	}
	return nil
}
func gitAt(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
func hash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

type reviewRollingBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	limit   int
	onWrite func([]byte)
}

func (b *reviewRollingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	_, _ = b.buf.Write(p)
	if b.limit > 0 && b.buf.Len() > b.limit {
		all := append([]byte(nil), b.buf.Bytes()...)
		b.buf.Reset()
		_, _ = b.buf.Write(all[len(all)-b.limit:])
	}
	snapshot := append([]byte(nil), b.buf.Bytes()...)
	b.mu.Unlock()
	if len(p) > 0 && b.onWrite != nil {
		b.onWrite(snapshot)
	}
	return len(p), nil
}

func (b *reviewRollingBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (r *OpenCodeReviewRunner) persistCheckpoint(workflowID string, c CandidateRecord, lens, prompt string, raw []byte) error {
	promptHash := hash([]byte(prompt))
	modelIdentity := r.modelIdentity()
	id := hash([]byte(workflowID + "\x00" + c.TreeOID + "\x00" + c.PolicyHash + "\x00" + lens + "\x00" + modelIdentity + "\x00" + promptHash))
	_, err := r.Store.Database().Exec(`INSERT OR REPLACE INTO review_checkpoints(id,workflow_id,candidate_tree,policy_hash,lens,model,prompt_hash,result_json,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, workflowID, c.TreeOID, c.PolicyHash, lens, modelIdentity, promptHash, raw, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (r *OpenCodeReviewRunner) reconcileInterrupted(workflowID string) {
	rows, err := r.Store.Database().Query(`SELECT id,pid FROM review_invocations WHERE workflow_id=? AND status='running'`, workflowID)
	if err != nil {
		return
	}
	defer rows.Close()
	type item struct {
		id  string
		pid int
	}
	var items []item
	for rows.Next() {
		var v item
		if rows.Scan(&v.id, &v.pid) == nil {
			items = append(items, v)
		}
	}
	for _, v := range items {
		alive := false
		if v.pid > 0 {
			alive = reviewProcessAlive(v.pid)
		}
		if !alive {
			_, _ = r.Store.Database().Exec(`UPDATE review_invocations SET status='interrupted',finished_at=?,error_preview='review process disappeared before terminal persistence' WHERE id=? AND status='running'`, time.Now().UTC().Format(time.RFC3339Nano), v.id)
		}
	}
}
