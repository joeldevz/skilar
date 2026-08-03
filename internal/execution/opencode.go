package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/processregistry"
	"github.com/joeldevz/skynex/internal/workflow"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var (
	ErrMalformedWorkerResult   = errors.New("execution: malformed OpenCode worker result")
	ErrManagedWorktreeMutation = errors.New("execution: OpenCode mutated managed worktree")
)

type OpenCodeOptions struct {
	Executable, Model, Agent string
	Timeout                  time.Duration
	MaxOutputBytes           int
}
type OpenCodeRequest struct {
	InvocationID string
	Attempt      Attempt
	Seal         gitcandidate.ContextSeal
	Policy       gitcandidate.Policy
	ArtifactIDs  []string
	Artifacts    map[string][]byte
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
	prompt := request.Prompt + "\nImmutable artifact files: " + strings.Join(materialized, ", ") + "\nWrite one JSON object with envelope and patch to $SKYNEX_RESULT_FILE. Do not write the managed worktree."
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.Dir = worktree
	cmd.Env = sanitizedEnv(resultFile)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	started := time.Now().UTC()
	runErr := cmd.Run()
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
		return WorkerResult{}, freezeErr
	}
	if after.TreeOID != before.TreeOID {
		_ = a.persistInvocation(request, args, exitCode, stdout.Bytes(), stderr.Bytes(), nil, "managed_worktree_mutation", started, finished)
		return WorkerResult{}, ErrManagedWorktreeMutation
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
	_, err := a.Store.Database().Exec(`INSERT OR REPLACE INTO opencode_invocations(invocation_id,workflow_id,attempt_id,model,command,exit_code,stdout_digest,stderr_digest,evidence_ids,status,started_at,finished_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, r.InvocationID, r.Attempt.WorkflowID, r.Attempt.ID, a.Options.Model, command, exit, outDigest, errDigest, evidenceJSON, status, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
	return err
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
func sanitizedEnv(result string) []string {
	allowed := []string{"PATH", "HOME", "TMPDIR", "XDG_CONFIG_HOME", "OPENCODE_CONFIG_CONTENT"}
	var env []string
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return append(env, "SKYNEX_RESULT_FILE="+result)
}
func gitCommand(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
