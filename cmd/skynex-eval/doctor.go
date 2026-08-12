package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/lifecycle"
	"github.com/joeldevz/skynex/internal/eval/runner"
)

const defaultOpenCodeVersion = "1.18.16"

type doctorOptions struct {
	Binary             string
	ResolvedBinary     *resolvedOpenCodeBinary
	ExpectedVersion    string
	Timeout            time.Duration
	EnvAllowlist       []string
	OpenAIOAuthFile    string
	OpenAIOAuthSession *lifecycle.OpenAIOAuthSession
	Models             []string
}

type doctorEndpoint struct {
	Name        string `json:"name"`
	Digest      string `json:"digest"`
	ContentType string `json:"content_type,omitempty"`
}

type doctorResult struct {
	Healthy               bool             `json:"healthy"`
	Version               string           `json:"version"`
	ExpectedVersion       string           `json:"expected_version"`
	EvaluatorBinaryDigest string           `json:"evaluator_binary_digest"`
	OpenCodeBinaryDigest  string           `json:"opencode_binary_digest"`
	ToolchainsDigest      string           `json:"toolchains_digest"`
	EffectiveConfigDigest string           `json:"effective_config_digest"`
	EffectiveAgentsDigest string           `json:"effective_agents_digest"`
	CapturedAt            string           `json:"captured_at"`
	Endpoints             []doctorEndpoint `json:"endpoints"`
	RequiredRoutes        []string         `json:"required_routes"`
	Models                []doctorModel    `json:"models,omitempty"`
	ConnectedProviders    []string         `json:"connected_providers,omitempty"`
	ProviderAuth          string           `json:"provider_auth,omitempty"`
	BillingMode           string           `json:"billing_mode,omitempty"`
	CredentialBoundary    string           `json:"credential_boundary,omitempty"`
	AuthIsolation         string           `json:"auth_isolation,omitempty"`
	ModelCalls            int              `json:"model_calls"`
}

type doctorModel struct {
	Selection string `json:"selection"`
	Connected bool   `json:"connected"`
}

type openCodeCompatibilityError struct{ cause error }

func (e *openCodeCompatibilityError) Error() string { return e.cause.Error() }
func (e *openCodeCompatibilityError) Unwrap() error { return e.cause }

func commandDoctor(ctx context.Context, args []string, deps dependencies) (doctorResult, error) {
	set := newFlagSet("doctor")
	binary := set.String("binary", "opencode", "OpenCode binary")
	expectedVersion := set.String("expected-version", defaultOpenCodeVersion, "exact supported OpenCode version")
	timeoutText := set.String("timeout", "30s", "probe startup deadline")
	modelsText := set.String("models", "", "comma-separated provider/model selections to verify")
	providerEnv := set.String("provider-env", "", "comma-separated provider environment names")
	openAIOAuth := set.String("openai-oauth", "", "OpenCode auth.json containing an OpenAI OAuth login")
	if err := parseFlagSet(set, args); err != nil {
		return doctorResult{}, err
	}
	if *binary == "" || *expectedVersion == "" {
		return doctorResult{}, invalidf("invalid_arguments", "--binary and --expected-version must not be empty")
	}
	timeout, err := time.ParseDuration(*timeoutText)
	if err != nil || timeout <= 0 || timeout > 5*time.Minute {
		return doctorResult{}, invalidf("invalid_arguments", "--timeout must be greater than zero and at most 5m")
	}
	envNames, err := parseEnvNames(*providerEnv)
	if err != nil {
		return doctorResult{}, invalidf("invalid_arguments", "%v", err)
	}
	openAIOAuthFile, err := resolveOpenAIOAuthFile(*openAIOAuth, envNames)
	if err != nil {
		return doctorResult{}, err
	}
	models, err := parseModelSelections(*modelsText)
	if err != nil {
		return doctorResult{}, invalidf("invalid_arguments", "%v", err)
	}
	if deps.probeRuntime == nil {
		return doctorResult{}, infraf("doctor_unavailable", fmt.Errorf("runtime probe is not configured"))
	}
	result, err := deps.probeRuntime(ctx, doctorOptions{
		Binary: *binary, ExpectedVersion: *expectedVersion, Timeout: timeout,
		EnvAllowlist: envNames, OpenAIOAuthFile: openAIOAuthFile, Models: models,
	})
	if err != nil {
		var mismatch *lifecycle.VersionMismatchError
		if errors.As(err, &mismatch) {
			return doctorResult{}, invalidf("opencode_version_mismatch", "%v", err)
		}
		var incompatible *openCodeCompatibilityError
		if errors.As(err, &incompatible) || errors.Is(err, client.ErrIncompatibleAPI) || errors.Is(err, client.ErrInvalidProviderCatalog) || errors.Is(err, client.ErrInvalidMCPStatusCatalog) {
			return doctorResult{}, invalidf("opencode_api_incompatible", "%v", err)
		}
		return doctorResult{}, infraf("opencode_unavailable", err)
	}
	return result, nil
}

func probeOpenCode(ctx context.Context, options doctorOptions) (result doctorResult, returnErr error) {
	evaluatorDigest, err := executableDigest()
	if err != nil {
		return result, fmt.Errorf("fingerprint evaluator binary: %w", err)
	}
	var resolvedBinary resolvedOpenCodeBinary
	if options.ResolvedBinary != nil {
		resolvedBinary = *options.ResolvedBinary
		if options.Binary != "" && options.Binary != resolvedBinary.Path {
			return result, fmt.Errorf("pre-resolved OpenCode path %q does not match requested path %q", resolvedBinary.Path, options.Binary)
		}
		if err := resolvedBinary.Revalidate(); err != nil {
			return result, fmt.Errorf("revalidate pre-resolved OpenCode executable: %w", err)
		}
	} else {
		resolvedBinary, err = resolveOpenCodeBinary(options.Binary)
		if err != nil {
			return result, fmt.Errorf("fingerprint OpenCode executable closure: %w", err)
		}
	}
	options.Binary = resolvedBinary.Path
	binaryDigestBefore := resolvedBinary.Digest
	toolchains, err := runner.ResolveExecutableClosure(nil)
	if err != nil {
		return result, fmt.Errorf("fingerprint effective toolchains: %w", err)
	}
	parent, err := os.MkdirTemp("", "skynex-eval-doctor-")
	if err != nil {
		return result, fmt.Errorf("create private doctor directory: %w", err)
	}
	defer os.RemoveAll(parent)
	workDir := filepath.Join(parent, "workspace")
	runDir := filepath.Join(parent, "runtime")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		return result, fmt.Errorf("create doctor workspace: %w", err)
	}
	if err := os.Mkdir(runDir, 0o700); err != nil {
		return result, fmt.Errorf("create doctor runtime directory: %w", err)
	}
	oauthSession := options.OpenAIOAuthSession
	if oauthSession == nil && options.OpenAIOAuthFile != "" {
		oauthSession, err = lifecycle.NewOpenAIOAuthSession(options.OpenAIOAuthFile)
		if err != nil {
			return result, fmt.Errorf("load dedicated OpenAI OAuth session: %w", err)
		}
	}

	server := lifecycle.NewServerWithConfig(lifecycle.Config{
		Port: 0, Hostname: "127.0.0.1", Timeout: options.Timeout,
		Binary: options.Binary, WorkDir: workDir, RunDir: runDir,
		ExpectedVersion: options.ExpectedVersion, EnvAllowlist: options.EnvAllowlist, Env: map[string]string{},
		OpenAIOAuthSession:         oauthSession,
		OpenAIOAuthMinimumValidity: options.Timeout,
	})
	if err := server.Start(ctx); err != nil {
		return result, fmt.Errorf("start OpenCode: %w", err)
	}
	defer func() {
		if err := server.Stop(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("stop OpenCode: %w", err)
		}
	}()
	probeCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	evidence, err := server.Probe(probeCtx)
	if err != nil {
		return result, fmt.Errorf("probe OpenCode: %w", err)
	}
	requiredRoutes, err := verifyRequiredOpenCodeAPI(evidence.OpenAPI.Body)
	if err != nil {
		return result, &openCodeCompatibilityError{cause: fmt.Errorf("verify OpenCode API contract: %w", err)}
	}
	models, err := verifyProviderModels(evidence.Providers, options.Models)
	if err != nil {
		if errors.Is(err, runner.ErrRuntimeModelUnavailable) {
			return result, &openCodeCompatibilityError{cause: err}
		}
		return result, err
	}
	cleanOAuth := options.OpenAIOAuthFile != "" || options.OpenAIOAuthSession != nil
	if cleanOAuth {
		if err := runner.RequireCleanOpenAIOAuthProviders(evidence.Providers.Connected); err != nil {
			return result, &openCodeCompatibilityError{cause: fmt.Errorf("%w; observed connected providers %v", err, evidence.Providers.Connected)}
		}
	}
	if err := resolvedBinary.Revalidate(); err != nil {
		return result, fmt.Errorf("OpenCode executable closure drifted during probe: %w", err)
	}
	if err := toolchains.Revalidate(); err != nil {
		return result, fmt.Errorf("effective toolchains drifted during probe: %w", err)
	}
	result = doctorResult{
		Healthy: evidence.Health.Healthy, Version: evidence.Health.Version,
		ExpectedVersion: options.ExpectedVersion, EvaluatorBinaryDigest: evaluatorDigest,
		OpenCodeBinaryDigest: binaryDigestBefore, ToolchainsDigest: toolchains.Digest(),
		EffectiveConfigDigest: evidence.Config.SHA256, EffectiveAgentsDigest: evidence.Agents.SHA256,
		CapturedAt: evidence.CapturedAt.UTC().Format(time.RFC3339Nano),
		ModelCalls: 0, RequiredRoutes: requiredRoutes, Models: models,
		ConnectedProviders: append([]string(nil), evidence.Providers.Connected...),
		Endpoints: []doctorEndpoint{
			{Name: "/path", Digest: evidence.Path.SHA256, ContentType: evidence.Path.ContentType},
			{Name: "/config", Digest: evidence.Config.SHA256, ContentType: evidence.Config.ContentType},
			{Name: "/agent", Digest: evidence.Agents.SHA256, ContentType: evidence.Agents.ContentType},
			{Name: "/experimental/tool/ids", Digest: evidence.Tools.SHA256, ContentType: evidence.Tools.ContentType},
			{Name: "/mcp", Digest: evidence.MCP.SHA256, ContentType: "application/json"},
			{Name: "/provider", Digest: evidence.Providers.SHA256, ContentType: "application/json"},
			{Name: "/doc", Digest: evidence.OpenAPI.SHA256, ContentType: evidence.OpenAPI.ContentType},
		},
	}
	if cleanOAuth {
		result.ProviderAuth = contracts.ProviderAuthModeOpenAIOAuthCleanProfileV1
		result.BillingMode = contracts.BillingModeChatGPTSubscription
		result.CredentialBoundary = contracts.CredentialBoundaryRuntimeReadable
		result.AuthIsolation = contracts.AuthIsolationDedicatedFreshTokenFailStopV1
	}
	if !result.Healthy {
		return doctorResult{}, fmt.Errorf("OpenCode health probe returned healthy=false")
	}
	return result, nil
}

func verifyRequiredOpenCodeAPI(raw json.RawMessage) ([]string, error) {
	return client.VerifyRequiredAPI(raw)
}

func parseModelSelections(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, raw := range strings.Split(value, ",") {
		selection := strings.TrimSpace(raw)
		if _, _, err := contracts.ParseModelSelection(selection); err != nil {
			return nil, fmt.Errorf("model selection %q: %w", selection, err)
		}
		if seen[selection] {
			return nil, fmt.Errorf("duplicate model selection %q", selection)
		}
		seen[selection] = true
		result = append(result, selection)
	}
	sort.Strings(result)
	return result, nil
}

func verifyProviderModels(catalog client.ProviderCatalog, selections []string) ([]doctorModel, error) {
	result := make([]doctorModel, 0, len(selections))
	for _, selection := range selections {
		if err := runner.RequireRuntimeModelForProbe(catalog, selection); err != nil {
			return nil, fmt.Errorf("verify model %s: %w", selection, err)
		}
		result = append(result, doctorModel{Selection: selection, Connected: true})
	}
	return result, nil
}
