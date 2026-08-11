package mcpproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/joeldevz/skynex/internal/eval/toolpolicy"
)

const maxStdioMessageBytes = 16 << 20

var ErrInvalidProxyConfig = errors.New("invalid MCP proxy configuration")

// Config is supplied only by the evaluator when it rewrites a declared local
// MCP command. The child receives a sanitized environment, never OpenCode's
// OAuth, HTTP basic-auth, XDG, loader, or shell-control variables.
type Config struct {
	MCPName         string
	AttestationPath string
	Nonce           string
	ExpectedTools   []string
	Environment     map[string]string
	Command         []string
}

// Run starts the declared MCP child, transparently relays its line-delimited
// JSON-RPC stream, and attests the real (possibly paginated) tools/list result
// before the final page is made visible to the same OpenCode process.
func Run(ctx context.Context, config Config, input io.Reader, output io.Writer) error {
	validated, err := validateConfig(config)
	if err != nil {
		return err
	}
	if ctx == nil || input == nil || output == nil {
		return fmt.Errorf("%w: nil runtime input", ErrInvalidProxyConfig)
	}
	childEnvironment, err := sanitizedChildEnvironment(validated.Environment)
	if err != nil {
		return err
	}

	childContext, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(childContext, validated.Command[0], validated.Command[1:]...)
	command.Env = childEnvironment
	command.Stderr = io.Discard
	childInput, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("prepare MCP child input")
	}
	childOutput, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("prepare MCP child output")
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start MCP child")
	}

	tracker := newListTracker(validated)
	clientDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() {
		err := relayClientRequests(input, childInput, tracker)
		_ = childInput.Close()
		clientDone <- err
	}()
	go func() {
		serverDone <- relayServerResponses(childOutput, output, tracker)
	}()

	var clientErr, serverErr error
	for serverDone != nil {
		select {
		case err := <-clientDone:
			clientErr = err
			clientDone = nil
			if err != nil {
				cancel()
			}
		case err := <-serverDone:
			serverErr = err
			serverDone = nil
			if err != nil {
				cancel()
			}
		case <-ctx.Done():
			cancel()
		}
	}
	// Wait only after stdout has reached EOF. os/exec may otherwise close the
	// StdoutPipe while the relay still owns it and truncate the final page.
	processErr := command.Wait()
	if serverErr != nil {
		return fmt.Errorf("relay MCP child response")
	}
	if clientErr != nil {
		return fmt.Errorf("relay MCP client request")
	}
	if processErr != nil {
		return fmt.Errorf("MCP child exited unsuccessfully")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func validateConfig(config Config) (Config, error) {
	config, err := validateDeclaredConfig(config)
	if err != nil {
		return Config{}, err
	}
	if len(config.Nonce) != 64 {
		return Config{}, fmt.Errorf("%w: invalid nonce", ErrInvalidProxyConfig)
	}
	for _, char := range config.Nonce {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return Config{}, fmt.Errorf("%w: invalid nonce", ErrInvalidProxyConfig)
		}
	}
	if config.AttestationPath == "" || !filepath.IsAbs(config.AttestationPath) || filepath.Clean(config.AttestationPath) != config.AttestationPath {
		return Config{}, fmt.Errorf("%w: invalid attestation path", ErrInvalidProxyConfig)
	}
	return config, nil
}

func validateDeclaredConfig(config Config) (Config, error) {
	if !rawToolNamePattern.MatchString(config.MCPName) {
		return Config{}, fmt.Errorf("%w: invalid MCP name", ErrInvalidProxyConfig)
	}
	tools, err := canonicalRawTools(config.ExpectedTools)
	if err != nil || len(tools) == 0 {
		return Config{}, fmt.Errorf("%w: invalid expected tools", ErrInvalidProxyConfig)
	}
	if len(config.Command) == 0 || !filepath.IsAbs(config.Command[0]) || filepath.Clean(config.Command[0]) != config.Command[0] {
		return Config{}, fmt.Errorf("%w: invalid child command", ErrInvalidProxyConfig)
	}
	if len(config.Command) > 256 {
		return Config{}, fmt.Errorf("%w: oversized child command", ErrInvalidProxyConfig)
	}
	commandBytes := 0
	for _, argument := range config.Command {
		if strings.IndexByte(argument, 0) >= 0 {
			return Config{}, fmt.Errorf("%w: invalid child command", ErrInvalidProxyConfig)
		}
		commandBytes += len(argument)
	}
	if commandBytes > 64<<10 {
		return Config{}, fmt.Errorf("%w: oversized child command", ErrInvalidProxyConfig)
	}
	if err := toolpolicy.ValidateFakeExecutionBoundary([]toolpolicy.FakeMCP{{
		Name: config.MCPName, Command: config.Command, Environment: config.Environment,
	}}); err != nil {
		return Config{}, fmt.Errorf("%w: unsafe child authority", ErrInvalidProxyConfig)
	}
	config.ExpectedTools = tools
	config.Command = append([]string(nil), config.Command...)
	config.Environment = cloneEnvironment(config.Environment)
	return config, nil
}

func sanitizedChildEnvironment(declared map[string]string) ([]string, error) {
	values := make(map[string]string)
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "TZ", "HOME", "TMPDIR"} {
		if value, exists := os.LookupEnv(key); exists {
			if strings.IndexByte(value, 0) >= 0 {
				return nil, fmt.Errorf("%w: invalid inherited environment", ErrInvalidProxyConfig)
			}
			values[key] = value
		}
	}
	if pathValue := values["PATH"]; pathValue == "" || !absoluteSearchPath(pathValue) {
		return nil, fmt.Errorf("%w: unsafe inherited PATH", ErrInvalidProxyConfig)
	}
	for _, key := range []string{"HOME", "TMPDIR"} {
		value := values[key]
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return nil, fmt.Errorf("%w: unsafe inherited runtime directory", ErrInvalidProxyConfig)
		}
	}
	for key, value := range declared {
		if !validEnvironmentName(key) || strings.IndexByte(value, 0) >= 0 || reservedChildEnvironment(key) {
			return nil, fmt.Errorf("%w: unsafe declared child environment", ErrInvalidProxyConfig)
		}
		values[key] = value
	}
	for key, value := range map[string]string{
		"GOENV": "off", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off",
		"GOSUMDB": "off", "CGO_ENABLED": "0",
	} {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}

func reservedChildEnvironment(key string) bool {
	upper := strings.ToUpper(key)
	if upper == ManifestEnvironment || upper == "PATH" || upper == "HOME" || upper == "TMPDIR" || upper == "TEMP" || upper == "TMP" ||
		upper == "LANG" || upper == "LC_ALL" || upper == "TZ" || upper == "ENV" ||
		upper == "BASH_ENV" || upper == "SHELLOPTS" || upper == "BASHOPTS" || upper == "ZDOTDIR" ||
		upper == "NODE_OPTIONS" || upper == "NODE_PATH" || upper == "BUN_OPTIONS" || upper == "DENO_DIR" ||
		strings.HasPrefix(upper, "XDG_") || strings.HasPrefix(upper, "OPENCODE_") ||
		strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_") || strings.HasPrefix(upper, "GO") ||
		strings.HasPrefix(upper, "PYTHON") || strings.HasPrefix(upper, "RUBY") || strings.HasPrefix(upper, "PERL") {
		return true
	}
	for _, exact := range []string{
		"SSH_AUTH_SOCK", "SSH_AGENT_PID", "DOCKER_CONFIG", "REGISTRY_AUTH_FILE", "KUBECONFIG", "NETRC",
		"NPM_CONFIG_USERCONFIG", "PIP_CONFIG_FILE", "GNUPGHOME", "GOOGLE_APPLICATION_CREDENTIALS",
	} {
		if upper == exact {
			return true
		}
	}
	for _, prefix := range []string{"AWS_", "AZURE_", "GCP_", "GOOGLE_", "OCI_", "GH_", "GITHUB_", "GITLAB_"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	for _, fragment := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "PRIVATE_KEY", "AUTHORIZATION", "COOKIE"} {
		if strings.Contains(upper, fragment) {
			return true
		}
	}
	return false
}

func absoluteSearchPath(value string) bool {
	parts := filepath.SplitList(value)
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if part == "" || !filepath.IsAbs(part) {
			return false
		}
	}
	return true
}

func validEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for index, char := range value {
		if !(char == '_' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || index > 0 && char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

func cloneEnvironment(source map[string]string) map[string]string {
	if source == nil {
		return map[string]string{}
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

type listTracker struct {
	mu               sync.Mutex
	config           Config
	pending          map[string]string
	active           bool
	expectedCursor   string
	observedRawTools map[string]struct{}
}

func newListTracker(config Config) *listTracker {
	return &listTracker{config: config, pending: make(map[string]string), observedRawTools: make(map[string]struct{})}
}

func (t *listTracker) observeRequest(line []byte) error {
	var message rpcMessage
	if err := decodeStrictJSON(line, &message); err != nil {
		return fmt.Errorf("invalid MCP client JSON-RPC")
	}
	if message.Method != "tools/list" {
		return nil
	}
	if message.JSONRPC != "2.0" || len(message.ID) == 0 {
		return fmt.Errorf("invalid tools/list request")
	}
	id, err := rpcID(message.ID)
	if err != nil {
		return fmt.Errorf("invalid tools/list request id")
	}
	cursor, err := requestCursor(message.Params)
	if err != nil {
		return fmt.Errorf("invalid tools/list cursor")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) != 0 {
		return fmt.Errorf("overlapping tools/list request")
	}
	if _, duplicate := t.pending[id]; duplicate {
		return fmt.Errorf("duplicate tools/list request id")
	}
	if !t.active {
		if cursor != "" {
			return fmt.Errorf("unexpected initial tools/list cursor")
		}
		t.observedRawTools = make(map[string]struct{})
	} else if cursor == "" || cursor != t.expectedCursor {
		return fmt.Errorf("tools/list pagination cursor mismatch")
	}
	t.pending[id] = cursor
	return nil
}

// observeResponse returns true only for a valid final tools/list page. The
// caller must write the attestation before forwarding that page.
func (t *listTracker) observeResponse(line []byte) (bool, []string, error) {
	var message rpcMessage
	if err := decodeStrictJSON(line, &message); err != nil {
		return false, nil, fmt.Errorf("invalid MCP server JSON-RPC")
	}
	if len(message.ID) == 0 {
		return false, nil, nil
	}
	id, err := rpcID(message.ID)
	if err != nil {
		return false, nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, pending := t.pending[id]; !pending {
		return false, nil, nil
	}
	delete(t.pending, id)
	if message.JSONRPC != "2.0" || len(message.Error) != 0 || len(message.Result) == 0 {
		t.active = false
		t.expectedCursor = ""
		return false, nil, nil
	}
	tools, nextCursor, err := parseToolsListResult(message.Result)
	if err != nil {
		return false, nil, err
	}
	for _, tool := range tools {
		if _, duplicate := t.observedRawTools[tool]; duplicate {
			return false, nil, fmt.Errorf("duplicate raw MCP tool across tools/list pages")
		}
		t.observedRawTools[tool] = struct{}{}
	}
	if nextCursor != "" {
		t.active = true
		t.expectedCursor = nextCursor
		return false, nil, nil
	}
	observed := make([]string, 0, len(t.observedRawTools))
	for tool := range t.observedRawTools {
		observed = append(observed, tool)
	}
	sort.Strings(observed)
	if !slicesEqual(observed, t.config.ExpectedTools) {
		return false, nil, fmt.Errorf("MCP tools/list set differs from declared contract")
	}
	t.active = false
	t.expectedCursor = ""
	return true, observed, nil
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func rpcID(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", fmt.Errorf("missing JSON-RPC id")
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return "", err
		}
		return "s:" + value, nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return "", err
	}
	if _, err := number.Int64(); err != nil {
		return "", fmt.Errorf("JSON-RPC id must be an integer")
	}
	return "n:" + number.String(), nil
}

func requestCursor(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var params map[string]json.RawMessage
	if err := decodeStrictJSON(raw, &params); err != nil || params == nil {
		return "", fmt.Errorf("params must be an object")
	}
	rawCursor, exists := params["cursor"]
	if !exists {
		return "", nil
	}
	var cursor string
	if err := json.Unmarshal(rawCursor, &cursor); err != nil || cursor == "" || len(cursor) > 4096 {
		return "", fmt.Errorf("cursor must be a bounded non-empty string")
	}
	return cursor, nil
}

func parseToolsListResult(raw json.RawMessage) ([]string, string, error) {
	var result struct {
		Tools      []json.RawMessage `json:"tools"`
		NextCursor json.RawMessage   `json:"nextCursor"`
	}
	if err := decodeStrictJSON(raw, &result); err != nil || result.Tools == nil {
		return nil, "", fmt.Errorf("invalid tools/list result")
	}
	tools := make([]string, 0, len(result.Tools))
	seen := make(map[string]struct{}, len(result.Tools))
	for _, rawTool := range result.Tools {
		var tool struct {
			Name string `json:"name"`
		}
		if err := decodeStrictJSON(rawTool, &tool); err != nil || !rawToolNamePattern.MatchString(tool.Name) {
			return nil, "", fmt.Errorf("invalid raw MCP tool definition")
		}
		if _, duplicate := seen[tool.Name]; duplicate {
			return nil, "", fmt.Errorf("duplicate raw MCP tool definition")
		}
		seen[tool.Name] = struct{}{}
		tools = append(tools, tool.Name)
	}
	cursor := ""
	if len(result.NextCursor) != 0 {
		if err := json.Unmarshal(result.NextCursor, &cursor); err != nil || cursor == "" || len(cursor) > 4096 {
			return nil, "", fmt.Errorf("invalid tools/list next cursor")
		}
	}
	return tools, cursor, nil
}

func relayClientRequests(input io.Reader, output io.Writer, tracker *listTracker) error {
	return relayLines(input, output, func(line []byte) error { return tracker.observeRequest(line) })
}

func relayServerResponses(input io.Reader, output io.Writer, tracker *listTracker) error {
	return relayLines(input, output, func(line []byte) error {
		final, tools, err := tracker.observeResponse(line)
		if err != nil {
			return err
		}
		if final {
			if err := writeAttestationAtomically(tracker.config.AttestationPath, tracker.config.MCPName, tracker.config.Nonce, tools); err != nil {
				return err
			}
		}
		return nil
	})
}

func relayLines(input io.Reader, output io.Writer, inspect func([]byte) error) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), maxStdioMessageBytes)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if len(bytes.TrimSpace(line)) == 0 {
			return fmt.Errorf("empty MCP stdio message")
		}
		if err := inspect(line); err != nil {
			return err
		}
		if _, err := output.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
