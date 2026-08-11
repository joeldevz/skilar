package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/contracts"
)

const cliSchemaVersion = 1

type commandError struct {
	exitCode int
	kind     string
	err      error
	data     any
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }

type errorBody struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type envelope struct {
	SchemaVersion int        `json:"schema_version"`
	Command       string     `json:"command"`
	Status        string     `json:"status"`
	Data          any        `json:"data,omitempty"`
	Error         *errorBody `json:"error,omitempty"`
}

type dependencies struct {
	probeRuntime func(context.Context, doctorOptions) (doctorResult, error)
	runModel     func(context.Context, modelRunSpec) (modelRunResult, error)
}

func defaultDependencies() dependencies {
	return dependencies{
		probeRuntime: probeOpenCode,
		runModel:     executeModelRuns,
	}
}

func runCLI(ctx context.Context, args []string, deps dependencies, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	command := "help"
	if len(args) > 0 {
		command = args[0]
	}
	var (
		data any
		err  error
	)
	switch command {
	case "help", "-h", "--help":
		data = usageDocument()
	case "validate":
		data, err = commandValidate(args[1:])
	case "list":
		data, err = commandList(args[1:])
	case "doctor":
		data, err = commandDoctor(ctx, args[1:], deps)
	case "freeze":
		data, err = commandFreeze(ctx, args[1:])
	case "run":
		data, err = commandRun(ctx, args[1:], deps)
	case "baseline":
		data, err = commandBaseline(ctx, args[1:], deps)
	case "ab":
		data, err = commandAB(ctx, args[1:], deps)
	case "compare":
		data, err = commandCompare(args[1:])
	case "report":
		data, err = commandReport(args[1:])
	default:
		err = invalidf("unknown_command", "unknown command %q", command)
	}

	if err == nil && ctx.Err() != nil {
		code, kind := classifyCommandError(ctx.Err())
		// The command may already have produced a valid partial result (notably
		// an A/B partial_artifact). Preserve it while making the terminal context
		// state explicit in the envelope and process exit code.
		err = &commandError{exitCode: code, kind: kind, err: ctx.Err(), data: data}
	}
	if err != nil {
		code, kind := classifyCommandError(err)
		message := safeDiagnostic(err)
		_, _ = fmt.Fprintf(stderr, "skynex-eval %s: %s\n", safeDiagnosticString(command), message)
		var errorData any
		var commandErr *commandError
		if errors.As(err, &commandErr) {
			errorData = commandErr.data
		}
		_ = writeEnvelope(stdout, envelope{
			SchemaVersion: cliSchemaVersion,
			Command:       command,
			Status:        statusForExit(code),
			Data:          errorData,
			Error:         &errorBody{Kind: kind, Message: message},
		})
		return code
	}

	status, code := statusAndExit(data)
	if encodeErr := writeEnvelope(stdout, envelope{SchemaVersion: cliSchemaVersion, Command: command, Status: status, Data: data}); encodeErr != nil {
		_, _ = fmt.Fprintf(stderr, "skynex-eval %s: encode output: %s\n", safeDiagnosticString(command), safeDiagnostic(encodeErr))
		return contracts.ExitInfrastructure
	}
	return code
}

func writeEnvelope(writer io.Writer, value envelope) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func classifyCommandError(err error) (int, string) {
	// A command may add an infrastructure wrapper while unwinding. Preserve
	// explicit cancellation semantics before considering that wrapper.
	if errors.Is(err, context.DeadlineExceeded) {
		return contracts.ExitBudgetExhausted, "budget_exhausted"
	}
	if errors.Is(err, context.Canceled) {
		return contracts.ExitAborted, "aborted"
	}
	var commandErr *commandError
	if errors.As(err, &commandErr) {
		return commandErr.exitCode, commandErr.kind
	}
	return contracts.ExitInfrastructure, "infrastructure"
}

func invalidf(kind, format string, args ...any) error {
	return &commandError{exitCode: contracts.ExitInvalid, kind: kind, err: fmt.Errorf(format, args...)}
}

func infraf(kind string, err error) error {
	return &commandError{exitCode: contracts.ExitInfrastructure, kind: kind, err: err}
}

func safeDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	return safeDiagnosticString(err.Error())
}

func safeDiagnosticString(value string) string {
	var output strings.Builder
	for _, r := range value {
		switch {
		case r == '\n' || r == '\t':
			output.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			_, _ = fmt.Fprintf(&output, "\\x%02x", r)
		default:
			output.WriteRune(r)
		}
		if output.Len() >= 16<<10 {
			output.WriteString("…[truncated]")
			break
		}
	}
	return output.String()
}

func newFlagSet(command string) *flag.FlagSet {
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return set
}

func parseFlagSet(set *flag.FlagSet, args []string) error {
	if err := set.Parse(args); err != nil {
		return invalidf("invalid_arguments", "%s", err)
	}
	if set.NArg() != 0 {
		return invalidf("invalid_arguments", "unexpected positional arguments: %s", strings.Join(set.Args(), " "))
	}
	return nil
}

func requireModelOptIn(allowed bool) error {
	if !allowed {
		return invalidf("model_calls_not_allowed", "refusing model calls that may consume quota or incur charges without --allow-model-calls")
	}
	return nil
}

func statusForExit(code int) string {
	switch code {
	case contracts.ExitSuccess:
		return string(contracts.RunStatusPass)
	case contracts.ExitFailed:
		return string(contracts.RunStatusFail)
	case contracts.ExitInvalid:
		return string(contracts.RunStatusInvalid)
	case contracts.ExitInconclusive:
		return string(contracts.RunStatusInconclusive)
	case contracts.ExitAborted:
		return string(contracts.RunStatusAborted)
	case contracts.ExitInfrastructure:
		return string(contracts.RunStatusInfraError)
	case contracts.ExitBudgetExhausted:
		return string(contracts.RunStatusBudgetExhausted)
	default:
		return string(contracts.RunStatusInvalid)
	}
}

type exitCoder interface{ CLIExitCode() int }

func statusAndExit(value any) (string, int) {
	if coded, ok := value.(exitCoder); ok {
		code := coded.CLIExitCode()
		return statusForExit(code), code
	}
	return string(contracts.RunStatusPass), contracts.ExitSuccess
}

func usageDocument() map[string]any {
	return map[string]any{
		"name": "skynex-eval",
		"commands": map[string]string{
			"validate": "validate [--suite NAME] [--cases-dir DIR] [--fixtures-dir DIR] [--schemas-dir DIR]",
			"list":     "list [--suite NAME] [--cases-dir DIR]",
			"doctor":   "doctor [--binary PATH] [--expected-version VERSION] [--models PROVIDER/MODEL[,PROVIDER/MODEL]] [--openai-oauth PATH | --provider-env NAME[,NAME]] [--timeout DURATION]",
			"freeze":   "freeze --output-dir DIR --harness DIR --control DIR --candidate DIR --id ID --suite NAME (--opencode-openapi-digest SHA256 | --doctor-result PATH) [--holdout DIR] [--runs N] [--seed N] [--binary PATH] [--control-model PROVIDER/MODEL --candidate-model PROVIDER/MODEL]",
			"run":      "run --allow-model-calls --case ID [--n N] [--openai-oauth PATH] [runtime options]",
			"baseline": "baseline --allow-model-calls --suite NAME [--n N] [--openai-oauth PATH] [--output PATH] [runtime options] (exploratory; use ab for frozen evidence)",
			"ab":       "ab --allow-model-calls --manifest PATH --openai-oauth PATH [--require-holdout] [--output-prefix PATH] [--resume-partial PATH] [runtime options]",
			"compare":  "compare --control PATH --candidate PATH --manifest PATH [--output PATH]",
			"report":   "report --input PATH [--control PATH --candidate PATH --manifest PATH (required for comparisons)]",
		},
		"safety": "run, baseline and ab require --allow-model-calls because they may consume subscription quota or incur provider charges; freeze, compare and report are offline",
		"runtime_options": []string{
			"--binary PATH", "--openai-oauth PATH (mutually exclusive with --provider-env)",
			"--provider-env NAME[,NAME]", "--cost-cap USD (provider/API billing only; unsupported with --openai-oauth)", "--trace-dir DIR",
			"--retain-trace", "--allow-impure (trusted-local only)",
		},
		"limitations": []string{
			"compare is fail-closed and cannot pass without a frozen manifest",
			"standalone baseline artifacts are exploratory; authoritative paired evidence is produced by ab",
			"manifest intent is required: development is explicitly non-release; release requires an external holdout and at least 10 pairs per case",
			"public baseline artifacts retain only ordinal, content-redacted holdout samples sufficient for deterministic gates",
			"ab currently requires manifest execution.concurrency=1",
			"ab always uses OpenCode --pure and rejects ambient configuration",
			"ab requires a dedicated OpenAI OAuth credential source and accepts only OpenAI effective models",
			"ChatGPT subscription has no authoritative per-request USD: frozen samples and timeouts bound scheduled work, not provider calls/tokens/quota under trusted-local; tree_cost_usd is not applicable",
			"A/B resume is explicit: --resume-partial requires the same frozen manifest/plan and a fresh valid OAuth credential; completed sample coordinates are not called again",
			"the local OAuth backend is development-only; release evidence requires a provider-proxy backend",
			"an external holdout bundle uses fixed cases/ and fixtures/ subdirectories and never persists traces",
			"qualitative LLM judge is not enabled by this CLI",
		},
		"exit_codes": map[string]int{
			"pass": 0, "fail": 1, "invalid": 2, "inconclusive": 3,
			"aborted": 4, "infra_error": 5, "budget_exhausted": 6,
		},
	}
}
