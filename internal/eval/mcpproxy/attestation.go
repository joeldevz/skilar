// Package mcpproxy implements the evaluator-owned stdio boundary used to
// observe the exact MCP tools/list response consumed by OpenCode.
package mcpproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
)

const (
	AttestationSchemaVersion = 1
	maxAttestationBytes      = 64 << 10
	// ManifestEnvironment is evaluator-reserved and is intentionally removed
	// from the environment passed to the MCP child.
	ManifestEnvironment = "SKYNEX_EVAL_MCP_PROXY_MANIFEST"
)

var (
	ErrInvalidAttestation = errors.New("invalid MCP tool attestation")
	rawToolNamePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

// Attestation deliberately retains only identity metadata and raw MCP tool
// names. Tool descriptions, schemas, arguments, results, and child output are
// never copied into this evaluator-owned file.
type Attestation struct {
	SchemaVersion int      `json:"schema_version"`
	MCPName       string   `json:"mcp_name"`
	Nonce         string   `json:"nonce"`
	RawTools      []string `json:"raw_tools"`
}

type RuntimeBinding struct {
	MCPName         string `json:"mcp_name"`
	AttestationPath string `json:"attestation_path"`
	Nonce           string `json:"nonce"`
}

type runtimeManifest struct {
	SchemaVersion int              `json:"schema_version"`
	Bindings      []RuntimeBinding `json:"bindings"`
}

// VerifyAttestation loads a protected, canonical attestation and binds it to
// the fresh nonce and exact raw tool set expected for this MCP process.
func VerifyAttestation(path, mcpName, nonce string, expectedRawTools []string) (Attestation, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Attestation{}, fmt.Errorf("%w: file unavailable", ErrInvalidAttestation)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return Attestation{}, fmt.Errorf("%w: file must be a 0600 regular file", ErrInvalidAttestation)
	}
	if err := verifyProtectedFileIdentity(info); err != nil {
		return Attestation{}, fmt.Errorf("%w: unsafe file identity", ErrInvalidAttestation)
	}
	if info.Size() <= 0 || info.Size() > maxAttestationBytes {
		return Attestation{}, fmt.Errorf("%w: file has invalid size", ErrInvalidAttestation)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Attestation{}, fmt.Errorf("%w: read file", ErrInvalidAttestation)
	}
	var attestation Attestation
	if err := decodeStrictJSON(raw, &attestation); err != nil {
		return Attestation{}, fmt.Errorf("%w: decode file", ErrInvalidAttestation)
	}
	canonical, err := json.Marshal(attestation)
	if err != nil || !bytes.Equal(raw, append(canonical, '\n')) {
		return Attestation{}, fmt.Errorf("%w: file is not canonical", ErrInvalidAttestation)
	}
	want, err := canonicalRawTools(expectedRawTools)
	if err != nil {
		return Attestation{}, fmt.Errorf("%w: invalid expected tool set", ErrInvalidAttestation)
	}
	got, err := canonicalRawTools(attestation.RawTools)
	if err != nil || !slices.Equal(got, attestation.RawTools) {
		return Attestation{}, fmt.Errorf("%w: invalid attested tool set", ErrInvalidAttestation)
	}
	if attestation.SchemaVersion != AttestationSchemaVersion || attestation.MCPName != mcpName || attestation.Nonce != nonce || !slices.Equal(attestation.RawTools, want) {
		return Attestation{}, fmt.Errorf("%w: identity or tool set mismatch", ErrInvalidAttestation)
	}
	return attestation, nil
}

// WriteRuntimeManifest publishes the fresh per-process nonce/path bindings to
// the proxy without injecting them into the content-addressed OpenCode config.
func WriteRuntimeManifest(path string, bindings []RuntimeBinding) error {
	validated, err := validateRuntimeBindings(bindings)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(runtimeManifest{SchemaVersion: AttestationSchemaVersion, Bindings: validated})
	if err != nil {
		return fmt.Errorf("encode MCP proxy manifest")
	}
	return writeProtectedFileAtomically(path, append(raw, '\n'))
}

// BindRuntimeManifest fills only the nonce and attestation path selected by
// the already-declared MCP name. Child argv and environment remain config
// authority and cannot be replaced by the manifest.
func BindRuntimeManifest(config Config, path string) (Config, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > maxAttestationBytes {
		return Config{}, fmt.Errorf("%w: runtime manifest unavailable", ErrInvalidProxyConfig)
	}
	if err := verifyProtectedFileIdentity(info); err != nil {
		return Config{}, fmt.Errorf("%w: unsafe runtime manifest", ErrInvalidProxyConfig)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("%w: read runtime manifest", ErrInvalidProxyConfig)
	}
	var manifest runtimeManifest
	if err := decodeStrictJSON(raw, &manifest); err != nil || manifest.SchemaVersion != AttestationSchemaVersion {
		return Config{}, fmt.Errorf("%w: decode runtime manifest", ErrInvalidProxyConfig)
	}
	canonical, err := json.Marshal(manifest)
	if err != nil || !bytes.Equal(raw, append(canonical, '\n')) {
		return Config{}, fmt.Errorf("%w: non-canonical runtime manifest", ErrInvalidProxyConfig)
	}
	bindings, err := validateRuntimeBindings(manifest.Bindings)
	if err != nil || !slices.Equal(bindings, manifest.Bindings) {
		return Config{}, fmt.Errorf("%w: invalid runtime manifest", ErrInvalidProxyConfig)
	}
	matched := false
	for _, binding := range bindings {
		if binding.MCPName == config.MCPName {
			config.AttestationPath = binding.AttestationPath
			config.Nonce = binding.Nonce
			matched = true
			break
		}
	}
	if !matched {
		return Config{}, fmt.Errorf("%w: runtime binding missing", ErrInvalidProxyConfig)
	}
	return validateConfig(config)
}

func validateRuntimeBindings(bindings []RuntimeBinding) ([]RuntimeBinding, error) {
	result := append([]RuntimeBinding(nil), bindings...)
	if result == nil {
		result = []RuntimeBinding{}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].MCPName < result[j].MCPName })
	for index, binding := range result {
		if !rawToolNamePattern.MatchString(binding.MCPName) || len(binding.Nonce) != 64 ||
			binding.AttestationPath == "" || !filepath.IsAbs(binding.AttestationPath) || filepath.Clean(binding.AttestationPath) != binding.AttestationPath {
			return nil, fmt.Errorf("%w: invalid runtime binding", ErrInvalidProxyConfig)
		}
		for _, char := range binding.Nonce {
			if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
				return nil, fmt.Errorf("%w: invalid runtime binding", ErrInvalidProxyConfig)
			}
		}
		if index > 0 && result[index-1].MCPName == binding.MCPName {
			return nil, fmt.Errorf("%w: duplicate runtime binding", ErrInvalidProxyConfig)
		}
	}
	return result, nil
}

func writeAttestationAtomically(path, mcpName, nonce string, rawTools []string) error {
	canonicalTools, err := canonicalRawTools(rawTools)
	if err != nil {
		return err
	}
	attestation := Attestation{
		SchemaVersion: AttestationSchemaVersion,
		MCPName:       mcpName,
		Nonce:         nonce,
		RawTools:      canonicalTools,
	}
	raw, err := json.Marshal(attestation)
	if err != nil {
		return fmt.Errorf("encode MCP tool attestation: %w", err)
	}
	raw = append(raw, '\n')
	return writeProtectedFileAtomically(path, raw)
}

func writeProtectedFileAtomically(path string, raw []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".mcp-attestation-")
	if err != nil {
		return fmt.Errorf("create MCP tool attestation: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict MCP tool attestation: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		return fmt.Errorf("write MCP tool attestation: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync MCP tool attestation: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close MCP tool attestation: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install MCP tool attestation: %w", err)
	}
	removeTemporary = false
	return nil
}

func canonicalRawTools(values []string) ([]string, error) {
	result := append([]string(nil), values...)
	if result == nil {
		result = []string{}
	}
	sort.Strings(result)
	for index, value := range result {
		if !rawToolNamePattern.MatchString(value) {
			return nil, fmt.Errorf("invalid raw MCP tool name")
		}
		if index > 0 && result[index-1] == value {
			return nil, fmt.Errorf("duplicate raw MCP tool name")
		}
	}
	return result, nil
}

// decodeStrictJSON rejects duplicate members at every object depth as well as
// trailing JSON before using encoding/json for the destination's shape.
func decodeStrictJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeUniqueJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected trailing JSON token %v", token)
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON object member")
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("invalid JSON array")
		}
	default:
		return fmt.Errorf("invalid JSON delimiter")
	}
	return nil
}
