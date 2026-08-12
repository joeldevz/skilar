package mcpproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListTrackerAttestsExactPaginatedRawTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attestation.json")
	config := Config{
		MCPName: "worker", AttestationPath: path, Nonce: testNonce,
		ExpectedTools: []string{"alpha", "omega"},
	}
	tracker := newListTracker(config)
	if err := tracker.observeRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	final, _, err := tracker.observeResponse([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"omega","description":"not attested"}],"nextCursor":"page-2"}}`))
	if err != nil || final {
		t.Fatalf("first page = final %v, err %v", final, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("attestation exists before final page: %v", err)
	}
	if err := tracker.observeRequest([]byte(`{"jsonrpc":"2.0","id":"second","method":"tools/list","params":{"cursor":"page-2"}}`)); err != nil {
		t.Fatal(err)
	}

	response := []byte(`{"jsonrpc":"2.0","id":"second","result":{"tools":[{"name":"alpha","inputSchema":{"type":"object"}}]}}`)
	var output observingWriter
	output.beforeWrite = func() {
		if _, err := VerifyAttestation(path, "worker", testNonce, []string{"alpha", "omega"}); err != nil {
			t.Errorf("attestation was not installed before final page forwarding: %v", err)
		}
	}
	if err := relayServerResponses(bytes.NewReader(append(response, '\n')), &output, tracker); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), append(response, '\n')) {
		t.Fatalf("forwarded response changed: %q", output.Bytes())
	}
}

func TestListTrackerRejectsMissingExtraDuplicateAndCursorDrift(t *testing.T) {
	tests := []struct {
		name     string
		expected []string
		response string
	}{
		{name: "missing", expected: []string{"alpha", "omega"}, response: `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"alpha"}]}}`},
		{name: "extra", expected: []string{"alpha"}, response: `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"alpha"},{"name":"omega"}]}}`},
		{name: "duplicate", expected: []string{"alpha"}, response: `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"alpha"},{"name":"alpha"}]}}`},
		{name: "duplicate JSON member", expected: []string{"alpha"}, response: `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"alpha","name":"alpha"}]}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := newListTracker(Config{MCPName: "worker", Nonce: testNonce, ExpectedTools: test.expected})
			if err := tracker.observeRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)); err != nil {
				t.Fatal(err)
			}
			if final, _, err := tracker.observeResponse([]byte(test.response)); err == nil || final {
				t.Fatalf("response accepted: final %v, err %v", final, err)
			}
		})
	}

	tracker := newListTracker(Config{MCPName: "worker", Nonce: testNonce, ExpectedTools: []string{"alpha"}})
	if err := tracker.observeRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)); err != nil {
		t.Fatal(err)
	}
	if final, _, err := tracker.observeResponse([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[],"nextCursor":"right"}}`)); err != nil || final {
		t.Fatalf("paginated response = final %v, err %v", final, err)
	}
	if err := tracker.observeRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"cursor":"wrong"}}`)); err == nil {
		t.Fatal("cursor drift was accepted")
	}
}

func TestSanitizedChildEnvironmentDropsOpenCodeOAuthAndControlVariables(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("OPENCODE_SERVER_PASSWORD", "must-not-pass")
	t.Setenv("XDG_DATA_HOME", "/credential-home")
	t.Setenv("LD_PRELOAD", "/tmp/ambient.so")
	t.Setenv("OPENAI_API_KEY", "must-not-pass")
	environment, err := sanitizedChildEnvironment(map[string]string{"SKX_FAKE_SCENARIO": "worker"})
	if err != nil {
		t.Fatal(err)
	}
	joined := "\n" + strings.Join(environment, "\n") + "\n"
	for _, forbidden := range []string{"OPENCODE_", "XDG_", "LD_PRELOAD", "OPENAI_API_KEY", "must-not-pass", "credential-home"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("sensitive environment survived: %s", forbidden)
		}
	}
	for _, required := range []string{"\nPATH=/usr/bin:/bin\n", "\nGOENV=off\n", "\nGOPROXY=off\n", "\nSKX_FAKE_SCENARIO=worker\n"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("sanitized environment lacks %q: %s", required, joined)
		}
	}
	if _, err := sanitizedChildEnvironment(map[string]string{"HOME": "/tmp/escape"}); err == nil {
		t.Fatal("declared HOME override was accepted")
	}
	for _, key := range []string{ManifestEnvironment, "OPENAI_API_KEY", "GITHUB_TOKEN"} {
		if _, err := sanitizedChildEnvironment(map[string]string{key: "must-not-pass"}); err == nil {
			t.Fatalf("declared sensitive key %q was accepted", key)
		}
	}
}

func TestRunRelaysRealChildResponseAndAttestsWithoutLeakingParentEnvironment(t *testing.T) {
	if os.Getenv("SKYNEX_MCP_PROXY_HELPER") == "1" {
		serveProxyTestHelper()
		return
	}
	root := t.TempDir()
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("TMPDIR", filepath.Join(root, "tmp"))
	for _, directory := range []string{os.Getenv("HOME"), os.Getenv("TMPDIR")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OPENCODE_SERVER_PASSWORD", "must-not-pass")
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "oauth"))
	path := filepath.Join(root, "attestation.json")
	input := bytes.NewBufferString("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\",\"params\":{}}\n")
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Run(ctx, Config{
		MCPName: "worker", AttestationPath: path, Nonce: testNonce,
		ExpectedTools: []string{"worker_result"},
		Environment:   map[string]string{"SKYNEX_MCP_PROXY_HELPER": "1"},
		Command:       []string{os.Args[0], "-test.run=^TestRunRelaysRealChildResponseAndAttestsWithoutLeakingParentEnvironment$"},
	}, input, &output)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAttestation(path, "worker", testNonce, []string{"worker_result"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"name":"worker_result"`) {
		t.Fatalf("child response was not relayed: %s", output.String())
	}
}

func serveProxyTestHelper() {
	defer os.Exit(0)
	if os.Getenv("OPENCODE_SERVER_PASSWORD") != "" || os.Getenv("XDG_DATA_HOME") != "" || os.Getenv("OPENAI_API_KEY") != "" ||
		os.Getenv("GOENV") != "off" || os.Getenv("GOPROXY") != "off" {
		os.Exit(3)
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || request.Method != "tools/list" {
			os.Exit(4)
		}
		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0", "id": request.ID,
			"result": map[string]any{"tools": []any{map[string]any{"name": "worker_result", "description": "public"}}},
		})
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		os.Exit(5)
	}
}

func TestParseArgsDoesNotAcceptReservedEnvironmentOrMissingSeparator(t *testing.T) {
	base := []string{
		"--mcp-name", "worker", "--tool", "worker_result", "--env", "HOME=/escape", "--", "/bin/false",
	}
	if _, err := ParseArgs(base); err != nil {
		// Reserved variables are rejected when the child environment is built,
		// keeping parsing free of ambient os.Environ dependencies.
		if !strings.Contains(err.Error(), "unsafe") {
			t.Fatalf("parse error = %v", err)
		}
	} else if _, err := sanitizedChildEnvironment(map[string]string{"HOME": "/escape"}); err == nil {
		t.Fatal("reserved child environment was accepted")
	}
	if _, err := ParseArgs(base[:len(base)-2]); err == nil {
		t.Fatal("missing child separator was accepted")
	}
	for _, args := range [][]string{
		{"--mcp-name", "worker", "--tool", "worker_result", "--", "/bin/sh", "-c", "exit 0"},
		{"--mcp-name", "worker", "--tool", "worker_result", "--env", "OPENAI_API_KEY=forbidden", "--", "/bin/false"},
		{"--mcp-name", "worker", "--tool", "worker_result", "--env", ManifestEnvironment + "=/tmp/forged", "--", "/bin/false"},
	} {
		if _, err := ParseArgs(args); err == nil {
			t.Fatal("unsafe original child authority was accepted by hidden proxy")
		}
	}
}

type observingWriter struct {
	bytes.Buffer
	beforeWrite func()
	wrote       bool
}

func (w *observingWriter) Write(value []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		w.beforeWrite()
	}
	return w.Buffer.Write(value)
}

func ExampleParseArgs() {
	_, _ = ParseArgs([]string{
		"--mcp-name", "worker", "--tool", "worker_result", "--", "/opt/eval/fake-mcp",
	})
	fmt.Println("parsed without protocol output")
	// Output: parsed without protocol output
}
