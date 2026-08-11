package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/joeldevz/skynex/schemas"
)

// DecodeRunResultJSON decodes one published eval-result v1 instance. The
// standard library cannot distinguish an omitted required boolean or number
// from its valid zero value, so this import boundary checks JSON presence and
// nullability before applying the typed semantic contract.
func DecodeRunResultJSON(data []byte) (RunResult, error) {
	root, err := requiredJSONObject(data, "result", []string{
		"schema_version", "run_id", "case_id", "variant", "repetition", "status", "provenance",
		"checks", "usage", "coordination", "timing", "evidence", "telemetry_complete", "error",
	}, map[string]struct{}{"error": {}})
	if err != nil {
		return RunResult{}, err
	}
	if err := validateRunResultJSONShape(root); err != nil {
		return RunResult{}, err
	}
	var result RunResult
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return RunResult{}, fmt.Errorf("decode result: %w", err)
	}
	if err := expectJSONEOF(decoder); err != nil {
		return RunResult{}, err
	}
	if err := result.Validate(); err != nil {
		return RunResult{}, err
	}
	if err := schemas.ValidateJSON(schemas.EvalResult, data); err != nil {
		return RunResult{}, fmt.Errorf("published result schema: %w", err)
	}
	return result, nil
}

func validateRunResultJSONShape(root map[string]json.RawMessage) error {
	provenance, err := requiredJSONObject(root["provenance"], "provenance", []string{
		"git_sha", "case_digest", "prompt_digest", "config_digest", "fixture_digest", "opencode_version",
		"model", "provider", "toolset_digest", "pricing_table_digest", "execution_mode", "network", "host",
	}, nil)
	if err != nil {
		return err
	}
	if _, err := requiredJSONObject(provenance["host"], "provenance.host", []string{"os", "arch"}, nil); err != nil {
		return err
	}
	if raw, exists := provenance["extensions"]; exists {
		if _, err := requiredJSONObject(raw, "provenance.extensions", nil, nil); err != nil {
			return err
		}
	}

	checks, err := requiredJSONArray(root["checks"], "checks")
	if err != nil {
		return err
	}
	for i, raw := range checks {
		field := fmt.Sprintf("checks[%d]", i)
		check, err := requiredJSONObject(raw, field, []string{"id", "type", "status", "hard", "summary", "requirement_ids", "evidence_ids"}, map[string]struct{}{"error": {}})
		if err != nil {
			return err
		}
		if _, err := requiredJSONArray(check["requirement_ids"], field+".requirement_ids"); err != nil {
			return err
		}
		if _, err := requiredJSONArray(check["evidence_ids"], field+".evidence_ids"); err != nil {
			return err
		}
		if rawError, exists := check["error"]; exists {
			if !isJSONNull(rawError) {
				if err := validateRunErrorJSONShape(rawError, field+".error"); err != nil {
					return err
				}
			}
		}
	}

	usage, err := requiredJSONObject(root["usage"], "usage", []string{"parent", "tree"}, nil)
	if err != nil {
		return err
	}
	if _, err := requiredJSONObject(usage["parent"], "usage.parent", []string{
		"first_input_tokens", "peak_input_tokens", "sum_input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
	}, nil); err != nil {
		return err
	}
	if _, err := requiredJSONObject(usage["tree"], "usage.tree", []string{
		"sum_input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "sessions",
	}, nil); err != nil {
		return err
	}
	if _, err := requiredJSONObject(root["coordination"], "coordination", []string{
		"tool_calls", "subagent_calls", "retries", "repeated_commands", "repeated_reads",
	}, nil); err != nil {
		return err
	}
	if _, err := requiredJSONObject(root["timing"], "timing", []string{"wall_ms", "model_ms"}, nil); err != nil {
		return err
	}

	evidence, err := requiredJSONObject(root["evidence"], "evidence", []string{
		"before_tree", "after_tree", "diff_digest", "trace_digest", "trace_path", "items",
	}, nil)
	if err != nil {
		return err
	}
	items, err := requiredJSONArray(evidence["items"], "evidence.items")
	if err != nil {
		return err
	}
	for i, raw := range items {
		if _, err := requiredJSONObject(raw, fmt.Sprintf("evidence.items[%d]", i), []string{"id", "kind", "source", "digest", "complete"}, nil); err != nil {
			return err
		}
	}

	if !isJSONNull(root["error"]) {
		if err := validateRunErrorJSONShape(root["error"], "error"); err != nil {
			return err
		}
	}
	return nil
}

func validateRunErrorJSONShape(raw json.RawMessage, field string) error {
	value, err := requiredJSONObject(raw, field, []string{"kind", "message", "retryable"}, nil)
	if err != nil {
		return err
	}
	if evidenceIDs, exists := value["evidence_ids"]; exists {
		if _, err := requiredJSONArray(evidenceIDs, field+".evidence_ids"); err != nil {
			return err
		}
	}
	return nil
}

func requiredJSONObject(raw json.RawMessage, field string, required []string, nullable map[string]struct{}) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, fieldError(field, "must be an object")
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fieldError(field, "must be an object: %v", err)
	}
	if value == nil {
		return nil, fieldError(field, "must be an object")
	}
	for _, name := range required {
		if _, exists := value[name]; !exists {
			return nil, fieldError(field+"."+name, "required field is missing")
		}
	}
	for name, child := range value {
		if !isJSONNull(child) {
			continue
		}
		if _, allowed := nullable[name]; !allowed {
			return nil, fieldError(field+"."+name, "must not be null")
		}
	}
	return value, nil
}

func requiredJSONArray(raw json.RawMessage, field string) ([]json.RawMessage, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, fieldError(field, "must be an array")
	}
	var value []json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fieldError(field, "must be an array: %v", err)
	}
	if value == nil {
		return nil, fieldError(field, "must be an array")
	}
	return value, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
