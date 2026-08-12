package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RuntimePreflightError is deliberately structured: callers can render it as
// JSON and automation can decide whether retrying is safe without guessing from
// a human error string.
type RuntimePreflightError struct {
	Code            string `json:"code"`
	Phase           string `json:"phase"`
	RetrySafe       bool   `json:"retry_safe"`
	MutationOutcome string `json:"mutation_outcome"`
	NextAction      Action `json:"next_action"`
	Detail          string `json:"detail,omitempty"`
}

type Action struct {
	Operation string `json:"operation"`
	Hint      string `json:"hint"`
}

func (e *RuntimePreflightError) Error() string {
	encoded, err := json.Marshal(e)
	if err != nil {
		return "workflow runtime preflight failed"
	}
	return string(encoded)
}

// RuntimeCapabilities is a typed capability manifest supplied by an OpenCode
// integration. Empty Models/Agents means the integration cannot enumerate that
// dimension; it must not be treated as a string-parsed negative result.
type RuntimeCapabilities struct {
	DefaultAgent bool
	Models       map[string]bool
	Agents       map[string]bool
}

type RuntimePreflightRequest struct {
	Phase, Executable, Model, Agent, WorkDir string
	ModelExplicit, AgentExplicit             bool
	RequireResultFile                        bool
	ResultTransport                          string
}

// ResultTransportFileV1 is the protocol implemented by execution.OpenCodeAdapter:
// it injects SKYNEX_RESULT_FILE and reads the JSON artifact from that location.
// Workflows must declare it explicitly; a binary named "opencode" is not proof
// that it understands this Skynex-specific contract.
const ResultTransportFileV1 = "skynex-result-file-v1"

type RuntimePreflight struct {
	LookPath            func(string) (string, error)
	Stat                func(string) (fs.FileInfo, error)
	CreateTemp          func(string, string) (*os.File, error)
	Remove              func(string) error
	TempDir             func() string
	AvailableBytes      func(string) (uint64, error)
	MinimumTempBytes    uint64
	ResolveCapabilities func(context.Context, string) (RuntimeCapabilities, error)
}

const DefaultMinimumTempBytes uint64 = 256 << 20

func DefaultRuntimePreflight() RuntimePreflight {
	return RuntimePreflight{
		LookPath:         exec.LookPath,
		Stat:             os.Stat,
		CreateTemp:       os.CreateTemp,
		Remove:           os.Remove,
		TempDir:          os.TempDir,
		AvailableBytes:   runtimeAvailableBytes,
		MinimumTempBytes: DefaultMinimumTempBytes,
		ResolveCapabilities: func(_ context.Context, _ string) (RuntimeCapabilities, error) {
			// OpenCode has no stable capability manifest in this version, so model
			// and agent membership remain unknown rather than guessed from output.
			return RuntimeCapabilities{DefaultAgent: true}, nil
		},
	}
}

func (p RuntimePreflight) Check(ctx context.Context, r RuntimePreflightRequest) error {
	phase := r.Phase
	if phase != "run" && phase != "review" {
		phase = "preflight"
	}
	fail := func(code, detail, hint string) error {
		return &RuntimePreflightError{Code: code, Phase: phase, RetrySafe: true, MutationOutcome: "not_started", NextAction: Action{Operation: "configure_runtime", Hint: hint}, Detail: detail}
	}
	if r.Phase != "run" && r.Phase != "review" {
		return fail("invalid_phase", r.Phase, "use run or review")
	}
	executable := r.Executable
	if executable == "" {
		executable = "opencode"
	}
	lookPath := p.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	resolved, err := lookPath(executable)
	if err != nil {
		return fail("opencode_unavailable", err.Error(), "install OpenCode or pass --opencode PATH")
	}
	stat := p.Stat
	if stat == nil {
		stat = os.Stat
	}
	info, err := stat(resolved)
	if err != nil || !runtimeExecutable(info) {
		detail := "not executable"
		if err != nil {
			detail = err.Error()
		}
		return fail("opencode_not_executable", detail, "choose an executable OpenCode binary")
	}
	if strings.ContainsAny(r.Model, "\r\n\t") || strings.ContainsAny(r.Agent, "\r\n\t") {
		return fail("runtime_identifier_invalid", "model and agent identifiers cannot contain control whitespace", "correct --model or --agent")
	}
	resolver := p.ResolveCapabilities
	if resolver == nil {
		resolver = DefaultRuntimePreflight().ResolveCapabilities
	}
	caps, err := resolver(ctx, resolved)
	if err != nil {
		return fail("runtime_capabilities_unavailable", err.Error(), "repair the OpenCode runtime and retry")
	}
	if r.RequireResultFile && r.ResultTransport != ResultTransportFileV1 {
		return fail("result_transport_undeclared", "workflow does not declare the skynex result-file transport", "start a new workflow with an adapter declaring skynex-result-file-v1")
	}
	if r.ModelExplicit && len(caps.Models) > 0 && !caps.Models[r.Model] {
		return fail("model_unavailable", r.Model, "select an available --model")
	}
	if r.AgentExplicit && len(caps.Agents) > 0 && !caps.Agents[r.Agent] {
		return fail("agent_unavailable", r.Agent, "select an available --agent")
	}
	if !r.AgentExplicit && !caps.DefaultAgent {
		return fail("default_agent_unavailable", "runtime has no valid default agent", "configure an available --agent")
	}
	if r.WorkDir == "" {
		return fail("workdir_unavailable", "empty work directory", "run inside the workflow repository")
	}
	if info, err = stat(r.WorkDir); err != nil || !info.IsDir() {
		detail := "not a directory"
		if err != nil {
			detail = err.Error()
		}
		return fail("workdir_unavailable", detail, "restore access to the workflow repository")
	}
	tempDir := p.TempDir
	if tempDir == nil {
		tempDir = os.TempDir
	}
	createTemp := p.CreateTemp
	if createTemp == nil {
		createTemp = os.CreateTemp
	}
	remove := p.Remove
	if remove == nil {
		remove = os.Remove
	}
	tmp, err := createTemp(tempDir(), ".skynex-runtime-preflight-")
	if err != nil {
		return fail("temp_unusable", err.Error(), "set TMPDIR to a writable directory")
	}
	name := tmp.Name()
	closeErr := tmp.Close()
	removeErr := remove(name)
	if closeErr != nil || removeErr != nil {
		return fail("temp_unusable", errors.Join(closeErr, removeErr).Error(), "set TMPDIR to a writable directory")
	}
	available := p.AvailableBytes
	if available != nil {
		bytes, spaceErr := available(filepath.Clean(tempDir()))
		if spaceErr != nil {
			return fail("temp_space_unknown", spaceErr.Error(), "make the temporary filesystem available")
		}
		minimum := p.MinimumTempBytes
		if minimum == 0 {
			minimum = DefaultMinimumTempBytes
		}
		if bytes < minimum {
			return fail("temp_space_insufficient", fmt.Sprintf("%d bytes available; %d required", bytes, minimum), "free temporary disk space or set TMPDIR")
		}
	}
	return nil
}
