package execution

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
	"github.com/joeldevz/skynex/internal/processregistry"
	"github.com/joeldevz/skynex/internal/workflow"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

var (
	ErrMalformedWorkerResult   = errors.New("execution: malformed OpenCode worker result")
	ErrManagedWorktreeMutation = errors.New("execution: OpenCode mutated managed worktree")
	ErrAgentFallback           = errors.New("execution: OpenCode rejected configured primary agent and fell back")
	ErrIdleProgressTimeout     = errors.New("execution: OpenCode produced no output before idle timeout")
)

type OpenCodeOptions struct {
	Executable, Model, Agent string
	Timeout                  time.Duration
	IdleTimeout              time.Duration
	MaxOutputBytes           int
}
type OpenCodeRequest struct {
	InvocationID string
	Attempt      Attempt
	Seal         gitcandidate.ContextSeal
	Policy       gitcandidate.Policy
	ArtifactIDs  []string
	Artifacts    map[string][]byte
	Checks       []string
	Prompt       string
}
type invocationOutput struct {
	Envelope workflow.ResultEnvelope `json:"envelope"`
	Patch    PatchArtifact           `json:"patch"`
}
type OpenCodeAdapter struct {
	Store   *workflow.SQLiteStore
	Options OpenCodeOptions
}

func (a *OpenCodeAdapter) Run(ctx context.Context, request OpenCodeRequest) (WorkerResult, error) {
	if a.Store == nil {
		return WorkerResult{}, errors.New("execution: workflow store required")
	}
	executable := a.Options.Executable
	if executable == "" {
		executable = "opencode"
	}
	timeout := a.Options.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	idleTimeout := a.Options.IdleTimeout
	if idleTimeout <= 0 {
		// OpenCode can remain silent while an active provider request is in
		// flight. Until the adapter has a provider-level activity signal, the
		// command timeout is the authoritative default watchdog.
		idleTimeout = timeout + time.Second
	}
	limit := a.Options.MaxOutputBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	before, err := gitcandidate.Freeze(request.Seal, request.Policy)
	if err != nil {
		return WorkerResult{}, err
	}
	if before.TreeOID != request.Attempt.BasisTree {
		return WorkerResult{}, workflow.ErrStaleResult
	}
	parent, err := os.MkdirTemp("", "skynex-opencode-*")
	if err != nil {
		return WorkerResult{}, err
	}
	defer os.RemoveAll(parent)
	worktree := filepath.Join(parent, "worktree")
	if out, gitErr := gitCommand(request.Seal.RepositoryRoot, "worktree", "add", "--detach", worktree, request.Seal.BaseCommitOID); gitErr != nil {
		return WorkerResult{}, fmt.Errorf("execution: disposable worktree: %w: %s", gitErr, out)
	}
	defer gitCommand(request.Seal.RepositoryRoot, "worktree", "remove", "--force", worktree)
	// The attempt basis can include accepted slices which have not been committed.
	// Populate the linked worktree from the exact immutable tree, not merely HEAD.
	if out, gitErr := gitCommand(worktree, "read-tree", "--reset", "-u", request.Attempt.BasisTree); gitErr != nil {
		return WorkerResult{}, fmt.Errorf("execution: materialize attempt basis: %w: %s", gitErr, out)
	}
	inputs := filepath.Join(worktree, ".skynex-inputs")
	if err = os.Mkdir(inputs, 0o700); err != nil {
		return WorkerResult{}, err
	}
	var materialized []string
	for _, id := range request.ArtifactIDs {
		if !safeArtifactID(id) {
			return WorkerResult{}, fmt.Errorf("execution: invalid artifact ID %q", id)
		}
		data, ok := request.Artifacts[id]
		if !ok {
			return WorkerResult{}, fmt.Errorf("execution: artifact %s missing", id)
		}
		path := filepath.Join(inputs, id+".artifact")
		if err = os.WriteFile(path, data, 0o400); err != nil {
			return WorkerResult{}, err
		}
		materialized = append(materialized, path)
	}
	if err = os.Chmod(inputs, 0o500); err != nil {
		return WorkerResult{}, err
	}
	resultFile := filepath.Join(parent, "result.json")
	prompt := buildWorkerPrompt(request, materialized)
	args := []string{"run", "--format", "json", "--auto", "--dir", worktree}
	if a.Options.Model != "" {
		args = append(args, "--model", a.Options.Model)
	}
	if a.Options.Agent != "" {
		args = append(args, "--agent", a.Options.Agent)
	}
	args = append(args, prompt)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	unregister := processregistry.Register(request.Attempt.WorkflowID, request.InvocationID, cancel)
	defer unregister()
	stdout := &boundedBuffer{limit: limit}
	stderr := &boundedBuffer{limit: limit}
	cmd := exec.CommandContext(runCtx, executable, args...)
	configureOpenCodeProcess(cmd)
	cmd.Dir = worktree
	runtimeEnv, err := openCodeRuntimeEnv(parent, resultFile)
	if err != nil {
		return WorkerResult{}, err
	}
	cmd.Env = runtimeEnv
	started := time.Now().UTC()
	if err = a.persistInvocationRunning(request, args, started); err != nil {
		return WorkerResult{}, err
	}
	activity := make(chan struct{}, 1)
	noteActivity := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
	cmd.Stdout = &progressWriter{buffer: stdout, update: func(value []byte) { a.updateInvocationPreview(request.InvocationID, "stdout_preview", value) }, activity: noteActivity}
	cmd.Stderr = &progressWriter{buffer: stderr, update: func(value []byte) { a.updateInvocationPreview(request.InvocationID, "stderr_preview", value) }, activity: noteActivity}
	runErr := cmd.Start()
	if runErr != nil {
		finished := time.Now().UTC()
		_ = a.persistInvocation(request, args, -1, stdout.Bytes(), stderr.Bytes(), nil, "start_failed", started, finished)
		return WorkerResult{}, runErr
	}
	_, _ = a.Store.Database().Exec(`UPDATE invocation_runtime SET pid=?,heartbeat_at=? WHERE invocation_id=?`, cmd.Process.Pid, time.Now().UTC().Format(time.RFC3339Nano), request.InvocationID)
	idleDone := make(chan struct{})
	var idleFired atomic.Bool
	go func() {
		timer := time.NewTimer(idleTimeout)
		defer timer.Stop()
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
			case <-timer.C:
				idleFired.Store(true)
				cancel()
				return
			}
		}
	}()
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case now := <-ticker.C:
				_, _ = a.Store.Database().Exec(`UPDATE invocation_runtime SET heartbeat_at=? WHERE invocation_id=? AND status='running'`, now.UTC().Format(time.RFC3339Nano), request.InvocationID)
			}
		}
	}()
	runErr = cmd.Wait()
	close(idleDone)
	close(heartbeatDone)
	finished := time.Now().UTC()
	exitCode := 0
	if runErr != nil {
		exitCode = -1
		if value, ok := runErr.(*exec.ExitError); ok {
			exitCode = value.ExitCode()
		}
	}
	after, freezeErr := gitcandidate.Freeze(request.Seal, request.Policy)
	if freezeErr != nil {
		_ = a.persistInvocation(request, args, exitCode, stdout.Bytes(), stderr.Bytes(), nil, "failed", started, finished)
		return WorkerResult{}, freezeErr
	}
	if after.TreeOID != before.TreeOID {
		_ = a.persistInvocation(request, args, exitCode, stdout.Bytes(), stderr.Bytes(), nil, "managed_worktree_mutation", started, finished)
		return WorkerResult{}, ErrManagedWorktreeMutation
	}
	if agentFallbackPattern.Match(stderr.Bytes()) {
		_ = a.persistInvocation(request, args, exitCode, stdout.Bytes(), stderr.Bytes(), nil, "agent_rejected", started, finished)
		return WorkerResult{}, fmt.Errorf("%w: configured agent %q", ErrAgentFallback, a.Options.Agent)
	}
	if idleFired.Load() {
		_ = a.persistInvocation(request, args, exitCode, stdout.Bytes(), stderr.Bytes(), nil, "idle_timeout", started, finished)
		return WorkerResult{}, ErrIdleProgressTimeout
	}
	if runErr != nil {
		status := "failed"
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			status = "timeout"
		}
		if errors.Is(runCtx.Err(), context.Canceled) {
			status = "cancelled"
		}
		_ = a.persistInvocation(request, args, exitCode, stdout.Bytes(), stderr.Bytes(), nil, status, started, finished)
		if runCtx.Err() != nil {
			return WorkerResult{}, runCtx.Err()
		}
		return WorkerResult{}, runErr
	}
	raw, err := os.ReadFile(resultFile)
	if err != nil {
		_ = a.persistInvocation(request, args, exitCode, stdout.Bytes(), stderr.Bytes(), nil, "malformed", started, finished)
		return WorkerResult{}, ErrMalformedWorkerResult
	}
	var parsed invocationOutput
	if json.Unmarshal(raw, &parsed) != nil || parsed.Envelope.Status == "" {
		_ = a.persistInvocation(request, args, exitCode, stdout.Bytes(), stderr.Bytes(), nil, "malformed", started, finished)
		return WorkerResult{}, ErrMalformedWorkerResult
	}
	if parsed.Envelope.WorkflowID != request.Attempt.WorkflowID || parsed.Envelope.AttemptID != request.Attempt.ID || parsed.Envelope.NodeID != request.Attempt.SliceID || parsed.Envelope.BaseCandidateOID != request.Attempt.BasisTree {
		_ = a.persistInvocation(request, args, exitCode, stdout.Bytes(), stderr.Bytes(), parsed.Envelope.EvidenceIDs, "stale", started, finished)
		return WorkerResult{}, workflow.ErrStaleResult
	}
	if err = a.persistInvocation(request, args, exitCode, stdout.Bytes(), stderr.Bytes(), parsed.Envelope.EvidenceIDs, "completed", started, finished); err != nil {
		return WorkerResult{}, err
	}
	return WorkerResult{Envelope: parsed.Envelope, Patch: parsed.Patch, Owner: request.Attempt.Owner, FencingToken: request.Attempt.FencingToken}, nil
}
func (a *OpenCodeAdapter) persistInvocation(r OpenCodeRequest, args []string, exit int, stdout, stderr []byte, evidence []string, status string, start, end time.Time) error {
	command, _ := json.Marshal(append([]string{"opencode"}, args...))
	evidenceJSON, _ := json.Marshal(evidence)
	outDigest := digestRedacted(stdout)
	errDigest := digestRedacted(stderr)
	artifacts := artifact.Store{DB: a.Store.Database(), Root: filepath.Join(r.Seal.GitCommonDir, "skynex", "artifacts")}
	command = artifacts.Redact(command)
	stdoutID, stderrID := "", ""
	if records, e := artifacts.PutLog(r.Attempt.WorkflowID, "log", stdout); e == nil && len(records) > 0 {
		stdoutID = records[0].ID
		_ = artifacts.Ref(stdoutID, "opencode_invocation", r.InvocationID)
	}
	if records, e := artifacts.PutLog(r.Attempt.WorkflowID, "log", stderr); e == nil && len(records) > 0 {
		stderrID = records[0].ID
		_ = artifacts.Ref(stderrID, "opencode_invocation", r.InvocationID)
	}
	_, err := a.Store.Database().Exec(`INSERT OR REPLACE INTO opencode_invocations(invocation_id,workflow_id,attempt_id,model,command,exit_code,stdout_digest,stderr_digest,evidence_ids,status,started_at,finished_at,stdout_artifact_id,stderr_artifact_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.InvocationID, r.Attempt.WorkflowID, r.Attempt.ID, a.Options.Model, command, exit, outDigest, errDigest, evidenceJSON, status, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), stdoutID, stderrID)
	_, _ = a.Store.Database().Exec(`UPDATE invocation_runtime SET status=?,heartbeat_at=?,finished_at=?,stdout_preview=?,stderr_preview=? WHERE invocation_id=?`, status, end.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), redactedPreview(stdout), redactedPreview(stderr), r.InvocationID)
	return err
}

func buildWorkerPrompt(request OpenCodeRequest, materialized []string) string {
	contract := map[string]any{"envelope": map[string]any{"WorkflowID": request.Attempt.WorkflowID, "NodeID": request.Attempt.SliceID, "AttemptID": request.Attempt.ID, "BaseCandidateOID": request.Attempt.BasisTree, "Status": "completed", "ArtifactIDs": []string{}, "EvidenceIDs": []string{}}, "patch": map[string]any{"Operations": []map[string]any{{"Path": "relative/path", "Data": "YmFzZTY0", "Mode": 384}}}}
	raw, _ := json.Marshal(contract)
	return request.Prompt + "\nAllowed paths: " + strings.Join(request.Attempt.AllowedPaths, ", ") + "\nChecks: " + strings.Join(request.Checks, "; ") + "\nImmutable artifact files: " + strings.Join(materialized, ", ") + "\nWrite exactly one JSON object to $SKYNEX_RESULT_FILE and do not write the managed worktree. Data is base64-encoded file content; Mode is an integer permission mode. Required result contract with exact authority values:\n" + string(raw)
}

func (a *OpenCodeAdapter) persistInvocationRunning(r OpenCodeRequest, args []string, started time.Time) error {
	command, _ := json.Marshal(append([]string{"opencode"}, args...))
	emptyDigest := digestRedacted(nil)
	command = (&artifact.Store{DB: a.Store.Database()}).Redact(command)
	if _, err := a.Store.Database().Exec(`INSERT OR REPLACE INTO opencode_invocations(invocation_id,workflow_id,attempt_id,model,command,exit_code,stdout_digest,stderr_digest,evidence_ids,status,started_at,finished_at,stdout_artifact_id,stderr_artifact_id) VALUES(?,?,?,?,?,-1,?,?,?,'running',?,'','','')`, r.InvocationID, r.Attempt.WorkflowID, r.Attempt.ID, a.Options.Model, command, emptyDigest, emptyDigest, []byte("[]"), started.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	_, err := a.Store.Database().Exec(`INSERT OR REPLACE INTO invocation_runtime(invocation_id,workflow_id,attempt_id,status,pid,started_at,heartbeat_at) VALUES(?,?,?,'running',0,?,?)`, r.InvocationID, r.Attempt.WorkflowID, r.Attempt.ID, started.Format(time.RFC3339Nano), started.Format(time.RFC3339Nano))
	return err
}

func redactedPreview(value []byte) string {
	redacted := secretPattern.ReplaceAll(value, []byte("$1=[REDACTED]"))
	if len(redacted) > 4096 {
		redacted = redacted[len(redacted)-4096:]
	}
	return string(redacted)
}
func (a *OpenCodeAdapter) updateInvocationPreview(id, column string, value []byte) {
	if column != "stdout_preview" && column != "stderr_preview" {
		return
	}
	_, _ = a.Store.Database().Exec(`UPDATE invocation_runtime SET `+column+`=?,heartbeat_at=? WHERE invocation_id=? AND status='running'`, redactedPreview(value), time.Now().UTC().Format(time.RFC3339Nano), id)
}

type progressWriter struct {
	buffer   *boundedBuffer
	update   func([]byte)
	activity func()
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.buffer.Write(p)
	if n > 0 && w.activity != nil {
		w.activity()
	}
	w.update(w.buffer.Bytes())
	return n, err
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

var secretPattern = regexp.MustCompile(`(?i)(token|password|secret|api[_-]?key)[=: ]+[^\s]+`)
var agentFallbackPattern = regexp.MustCompile(`(?i)(falling back|fallback)\s+to\s+(the\s+)?default\s+agent`)

func digestRedacted(value []byte) string {
	redacted := secretPattern.ReplaceAll(value, []byte("$1=[REDACTED]"))
	sum := sha256.Sum256(redacted)
	return hex.EncodeToString(sum[:])
}
func safeArtifactID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func openCodeRuntimeEnv(parent, result string) ([]string, error) {
	runtimeData := filepath.Join(parent, "xdg-data")
	runtimeCache := filepath.Join(parent, "xdg-cache")
	runtimeState := filepath.Join(parent, "xdg-state")
	for _, path := range []string{runtimeData, runtimeCache, runtimeState} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("execution: OpenCode runtime directory: %w", err)
		}
	}
	targetData := filepath.Join(runtimeData, "opencode")
	if err := os.MkdirAll(targetData, 0o700); err != nil {
		return nil, fmt.Errorf("execution: OpenCode data directory: %w", err)
	}
	sourceData := filepath.Join(os.Getenv("HOME"), ".local", "share", "opencode")
	for _, name := range []string{"account.json", "auth.json", "mcp-auth.json"} {
		raw, err := os.ReadFile(filepath.Join(sourceData, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("execution: OpenCode identity %s: %w", name, err)
		}
		if err = os.WriteFile(filepath.Join(targetData, name), raw, 0o600); err != nil {
			return nil, fmt.Errorf("execution: OpenCode identity %s: %w", name, err)
		}
	}
	allowed := []string{"PATH", "HOME", "TMPDIR", "XDG_CONFIG_HOME", "OPENCODE_CONFIG_CONTENT"}
	var env []string
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	env = append(env,
		"XDG_DATA_HOME="+runtimeData,
		"XDG_CACHE_HOME="+runtimeCache,
		"XDG_STATE_HOME="+runtimeState,
		"SKYNEX_RESULT_FILE="+result,
	)
	return env, nil
}
func gitCommand(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
