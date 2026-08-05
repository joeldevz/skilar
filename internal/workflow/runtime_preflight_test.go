package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func preflightFixture(t *testing.T, caps RuntimeCapabilities) RuntimePreflight {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := DefaultRuntimePreflight()
	p.LookPath = func(string) (string, error) { return exe, nil }
	p.ResolveCapabilities = func(context.Context, string) (RuntimeCapabilities, error) { return caps, nil }
	p.AvailableBytes = func(string) (uint64, error) { return DefaultMinimumTempBytes, nil }
	return p
}

func TestRuntimePreflightRejectsCapabilityFailuresWithoutStarting(t *testing.T) {
	cases := []struct {
		name, want string
		caps       RuntimeCapabilities
		request    RuntimePreflightRequest
	}{
		{"model", "model_unavailable", RuntimeCapabilities{DefaultAgent: true, Models: map[string]bool{"good": true}}, RuntimePreflightRequest{Phase: "run", Model: "missing", ModelExplicit: true, ResultTransport: ResultTransportFileV1}},
		{"agent", "agent_unavailable", RuntimeCapabilities{DefaultAgent: true, Agents: map[string]bool{"good": true}}, RuntimePreflightRequest{Phase: "run", Agent: "missing", AgentExplicit: true, ResultTransport: ResultTransportFileV1}},
		{"transport", "result_transport_undeclared", RuntimeCapabilities{DefaultAgent: true}, RuntimePreflightRequest{Phase: "run", RequireResultFile: true}},
		{"default agent", "default_agent_unavailable", RuntimeCapabilities{}, RuntimePreflightRequest{Phase: "run", RequireResultFile: true, ResultTransport: ResultTransportFileV1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := preflightFixture(t, tc.caps)
			tc.request.WorkDir = t.TempDir()
			err := p.Check(context.Background(), tc.request)
			var got *RuntimePreflightError
			if !errors.As(err, &got) || got.Code != tc.want || got.Phase != "preflight" || !got.RetrySafe || got.MutationOutcome != "not_started" || got.NextAction.Operation == "" {
				t.Fatalf("err=%#v", err)
			}
		})
	}
}

func TestRuntimePreflightRejectsUnusableAndFullTemp(t *testing.T) {
	p := preflightFixture(t, RuntimeCapabilities{DefaultAgent: true})
	p.CreateTemp = func(string, string) (*os.File, error) { return nil, os.ErrPermission }
	err := p.Check(context.Background(), RuntimePreflightRequest{Phase: "run", RequireResultFile: true, ResultTransport: ResultTransportFileV1, WorkDir: t.TempDir()})
	var got *RuntimePreflightError
	if !errors.As(err, &got) || got.Code != "temp_unusable" {
		t.Fatalf("err=%v", err)
	}
	p = preflightFixture(t, RuntimeCapabilities{DefaultAgent: true})
	p.AvailableBytes = func(string) (uint64, error) { return 1, nil }
	err = p.Check(context.Background(), RuntimePreflightRequest{Phase: "review", RequireResultFile: true, ResultTransport: ResultTransportFileV1, WorkDir: t.TempDir()})
	if !errors.As(err, &got) || got.Code != "temp_space_insufficient" {
		t.Fatalf("err=%v", err)
	}
}

func TestRuntimePreflightHappyPath(t *testing.T) {
	p := preflightFixture(t, RuntimeCapabilities{DefaultAgent: true, Models: map[string]bool{"model": true}, Agents: map[string]bool{"agent": true}})
	if err := p.Check(context.Background(), RuntimePreflightRequest{Phase: "run", RequireResultFile: true, ResultTransport: ResultTransportFileV1, WorkDir: t.TempDir(), Model: "model", ModelExplicit: true, Agent: "agent", AgentExplicit: true}); err != nil {
		t.Fatal(err)
	}
}
