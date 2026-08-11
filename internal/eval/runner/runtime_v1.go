package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/eval/client"
	"github.com/joeldevz/skynex/internal/eval/contracts"
	"github.com/joeldevz/skynex/internal/eval/lifecycle"
	"github.com/joeldevz/skynex/internal/eval/toolpolicy"
)

var (
	ErrRuntimeProviderDisconnected = errors.New("runtime provider is not connected")
	ErrRuntimeModelUnavailable     = errors.New("runtime model is unavailable")
	ErrRuntimeContractIncompatible = errors.New("runtime contract is incompatible")
)

func (f OpenCodeFactory) Start(ctx context.Context, request RuntimeRequest) (Runtime, error) {
	if request.Case.Security.ExecutionMode != contracts.ExecutionTrustedLocal {
		return nil, fmt.Errorf("local OpenCode factory cannot satisfy execution mode %q", request.Case.Security.ExecutionMode)
	}
	if request.Case.Security.Network != contracts.NetworkHostUnisolated {
		return nil, fmt.Errorf("trusted-local runtime must report host-unisolated network")
	}
	if request.WorkspacePath == "" || !filepath.IsAbs(request.WorkspacePath) {
		return nil, fmt.Errorf("runtime workspace must be absolute")
	}
	if request.RunPath == "" || !filepath.IsAbs(request.RunPath) {
		return nil, fmt.Errorf("runtime run path must be absolute")
	}
	startupTimeout := durationOr(f.StartupTimeout, 30*time.Second)
	if f.OpenAIOAuthFile != "" && f.OpenAIOAuthSession != nil {
		return nil, fmt.Errorf("configure either an OpenAI OAuth file or session, not both")
	}
	oauthSession := f.OpenAIOAuthSession
	if oauthSession == nil && f.OpenAIOAuthFile != "" {
		var err error
		oauthSession, err = lifecycle.NewOpenAIOAuthSession(f.OpenAIOAuthFile)
		if err != nil {
			return nil, fmt.Errorf("load dedicated OpenAI OAuth session: %w", err)
		}
	}
	oauthMinimumValidity := time.Duration(0)
	if oauthSession != nil {
		completionTimeout, err := time.ParseDuration(request.Case.Completion.Timeout)
		if err != nil || completionTimeout <= 0 {
			return nil, fmt.Errorf("runtime case has invalid completion timeout %q", request.Case.Completion.Timeout)
		}
		oauthMinimumValidity = completionTimeout + startupTimeout
	}
	env := make(map[string]string, len(f.Env))
	for key, value := range f.Env {
		env[key] = value
	}
	if request.ConfigRoot != "" {
		if !filepath.IsAbs(request.ConfigRoot) {
			return nil, fmt.Errorf("runtime config root must be absolute")
		}
	}
	server := lifecycle.NewServerWithConfig(lifecycle.Config{
		Port:                       0,
		Binary:                     f.Binary,
		Timeout:                    startupTimeout,
		WorkDir:                    request.WorkspacePath,
		RunDir:                     request.RunPath,
		EnvAllowlist:               append([]string(nil), f.EnvAllowlist...),
		Env:                        env,
		ConfigHome:                 request.ConfigRoot,
		OpenAIOAuthSession:         oauthSession,
		OpenAIOAuthMinimumValidity: oauthMinimumValidity,
		ExpectedVersion:            f.ExpectedVersion,
		HTTPClient:                 f.HTTPClient,
		AllowImpure:                f.AllowImpure,
	})
	if err := server.Start(ctx); err != nil {
		return nil, err
	}
	fail := func(err error) (Runtime, error) {
		return nil, errors.Join(err, server.Stop())
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	probe, err := server.Probe(probeCtx)
	cancel()
	if err != nil {
		if errors.Is(err, client.ErrInvalidProviderCatalog) || errors.Is(err, client.ErrIncompatibleAPI) {
			return fail(fmt.Errorf("%w: probe OpenCode runtime: %w", ErrRuntimeContractIncompatible, err))
		}
		return fail(fmt.Errorf("probe OpenCode runtime: %w", err))
	}
	if _, err := client.VerifyRequiredAPI(probe.OpenAPI.Body); err != nil {
		return fail(fmt.Errorf("%w: %w", ErrRuntimeContractIncompatible, err))
	}
	if err := requireRuntimeAgent(probe.Agents.Body, request.Case.Agent.Name); err != nil {
		return fail(fmt.Errorf("%w: %w", ErrRuntimeContractIncompatible, err))
	}
	if err := RequireRuntimeModelForProbe(probe.Providers, request.Case.Agent.Model); err != nil {
		if errors.Is(err, ErrRuntimeModelUnavailable) {
			err = fmt.Errorf("%w: %w", ErrRuntimeContractIncompatible, err)
		}
		return fail(err)
	}
	if f.OpenAIOAuthFile != "" || f.OpenAIOAuthSession != nil {
		if err := RequireCleanOpenAIOAuthProviders(probe.Providers.Connected); err != nil {
			return fail(fmt.Errorf("%w: %w", ErrRuntimeContractIncompatible, err))
		}
	}
	api := server.Client(request.WorkspacePath)
	verification := toolpolicy.VerifyRuntimeConfig(probe.Config.Body, request.ToolPolicy)
	if !verification.Valid {
		return fail(fmt.Errorf("%w: resolved OpenCode config violates tool policy: %s", ErrRuntimeContractIncompatible, strings.Join(verification.Violations, "; ")))
	}
	var toolIDs []string
	if err := json.Unmarshal(probe.Tools.Body, &toolIDs); err != nil {
		return fail(fmt.Errorf("%w: decode probed OpenCode tool catalog: %w", ErrRuntimeContractIncompatible, err))
	}
	promptTools, err := toolpolicy.BindPromptTools(request.ToolPolicy, toolIDs)
	if err != nil {
		return fail(fmt.Errorf("%w: bind fail-closed prompt tools: %w", ErrRuntimeContractIncompatible, err))
	}
	toolsetDigest, err := contracts.CanonicalDigest(map[string]string{
		"policy":  request.ToolPolicy.Digest,
		"catalog": probe.Tools.SHA256,
	})
	if err != nil {
		return fail(fmt.Errorf("digest effective OpenCode toolset: %w", err))
	}
	return &openCodeRuntime{
		server:      server,
		client:      api,
		promptTools: promptTools,
		info: RuntimeInfo{
			OpenCodeVersion:       probe.Health.Version,
			OpenCodeAPI:           probe.OpenAPI.SHA256,
			ConfigDigest:          probe.Config.SHA256,
			AgentsDigest:          probe.Agents.SHA256,
			ToolPolicyDigest:      request.ToolPolicy.Digest,
			ToolCatalogDigest:     probe.Tools.SHA256,
			ToolsetDigest:         toolsetDigest,
			ProviderCatalogDigest: probe.Providers.SHA256,
			ProviderAuthMode:      authModeForFactory(f),
			BillingMode:           billingModeForFactory(f),
			CredentialBoundary:    credentialBoundaryForFactory(f),
			AuthIsolation:         authIsolationForFactory(f),
			ExecutionMode:         contracts.ExecutionTrustedLocal,
			Network:               contracts.NetworkHostUnisolated,
		},
	}, nil
}

func authModeForFactory(factory OpenCodeFactory) string {
	if factory.OpenAIOAuthFile != "" || factory.OpenAIOAuthSession != nil {
		return contracts.ProviderAuthModeOpenAIOAuthCleanProfileV1
	}
	return ""
}

func billingModeForFactory(factory OpenCodeFactory) string {
	if factory.OpenAIOAuthFile != "" || factory.OpenAIOAuthSession != nil {
		return contracts.BillingModeChatGPTSubscription
	}
	return ""
}

func credentialBoundaryForFactory(factory OpenCodeFactory) string {
	if factory.OpenAIOAuthFile != "" || factory.OpenAIOAuthSession != nil {
		return contracts.CredentialBoundaryRuntimeReadable
	}
	return ""
}

func authIsolationForFactory(factory OpenCodeFactory) string {
	if factory.OpenAIOAuthFile != "" || factory.OpenAIOAuthSession != nil {
		return contracts.AuthIsolationDedicatedFreshTokenFailStopV1
	}
	return ""
}

// RequireCleanOpenAIOAuthProviders accepts OpenAI plus OpenCode's credential-
// free built-in provider entry. The effective configuration separately pins
// enabled_providers=["openai"] and every requested model to openai/..., so the
// built-in catalogue entry is not model authority. Any credential-bearing or
// otherwise ambient provider remains a hard compatibility failure.
func RequireCleanOpenAIOAuthProviders(connected []string) error {
	seen := make(map[string]struct{}, len(connected))
	for _, provider := range connected {
		if _, duplicate := seen[provider]; duplicate {
			return fmt.Errorf("clean OAuth provider catalogue contains duplicate %q", provider)
		}
		seen[provider] = struct{}{}
		if provider != "openai" && provider != "opencode" {
			return fmt.Errorf("clean OAuth profile exposes ambient connected provider %q", provider)
		}
	}
	if _, ok := seen["openai"]; !ok {
		return fmt.Errorf("clean OAuth profile does not connect provider openai")
	}
	return nil
}

func requireRuntimeAgent(raw json.RawMessage, name string) error {
	var agents []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &agents); err != nil {
		return fmt.Errorf("decode effective OpenCode agent catalogue: %w", err)
	}
	matches := 0
	for _, agent := range agents {
		if agent.Name == name {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("effective OpenCode agent catalogue contains %d entries named %q, expected exactly one", matches, name)
	}
	return nil
}

// RequireRuntimeModelForProbe verifies that a provider/model selection is
// present and connected in a sanitized, read-only OpenCode provider catalog.
func RequireRuntimeModelForProbe(catalog client.ProviderCatalog, selection string) error {
	providerID, modelID, err := contracts.ParseModelSelection(selection)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeModelUnavailable, err)
	}
	connected := false
	for _, id := range catalog.Connected {
		if id == providerID {
			connected = true
			break
		}
	}
	if !connected {
		return fmt.Errorf("%w: %q", ErrRuntimeProviderDisconnected, providerID)
	}
	for _, provider := range catalog.All {
		if provider.ID != providerID {
			continue
		}
		if _, exists := provider.Models[modelID]; exists {
			return nil
		}
		for key, model := range provider.Models {
			if key == modelID || model.ID == modelID {
				return nil
			}
		}
		return fmt.Errorf("%w: provider %q does not expose model %q", ErrRuntimeModelUnavailable, providerID, modelID)
	}
	return fmt.Errorf("%w: provider catalog does not expose provider %q", ErrRuntimeModelUnavailable, providerID)
}

func durationOr(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}
