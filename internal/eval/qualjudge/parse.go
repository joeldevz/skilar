package qualjudge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

type wireResult struct {
	Verdict    Verdict  `json:"verdict"`
	Score      *float64 `json:"score"`
	Confidence *float64 `json:"confidence"`
	Rationale  *string  `json:"rationale"`
}

func parseOutput(output []byte, maxBytes int) (wireResult, error) {
	if len(output) == 0 || len(output) > maxBytes {
		return wireResult{}, fmt.Errorf("%w: response size is outside the allowed range", ErrInvalidOutput)
	}
	if err := rejectDuplicateKeysAndTrailing(output); err != nil {
		return wireResult{}, fmt.Errorf("%w: response is not one unique-key JSON value", ErrInvalidOutput)
	}

	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	var parsed wireResult
	if err := decoder.Decode(&parsed); err != nil {
		return wireResult{}, fmt.Errorf("%w: response does not match the schema", ErrInvalidOutput)
	}
	if err := requireEOF(decoder); err != nil {
		return wireResult{}, fmt.Errorf("%w: trailing response content", ErrInvalidOutput)
	}
	if parsed.Verdict != VerdictPass && parsed.Verdict != VerdictFail && parsed.Verdict != VerdictInconclusive {
		return wireResult{}, fmt.Errorf("%w: unsupported verdict", ErrInvalidOutput)
	}
	if parsed.Score == nil || math.IsNaN(*parsed.Score) || math.IsInf(*parsed.Score, 0) || *parsed.Score < 0 || *parsed.Score > 1 {
		return wireResult{}, fmt.Errorf("%w: score must be between zero and one", ErrInvalidOutput)
	}
	if parsed.Confidence == nil || math.IsNaN(*parsed.Confidence) || math.IsInf(*parsed.Confidence, 0) || *parsed.Confidence < 0 || *parsed.Confidence > 1 {
		return wireResult{}, fmt.Errorf("%w: confidence must be between zero and one", ErrInvalidOutput)
	}
	if parsed.Rationale == nil || strings.TrimSpace(*parsed.Rationale) == "" || !utf8.ValidString(*parsed.Rationale) || len(*parsed.Rationale) > MaxRationaleBytes || containsUnsafeControl(*parsed.Rationale) {
		return wireResult{}, fmt.Errorf("%w: rationale is invalid", ErrInvalidOutput)
	}
	*parsed.Rationale = strings.TrimSpace(*parsed.Rationale)
	return parsed, nil
}

func rejectDuplicateKeysAndTrailing(value []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder, 0); err != nil {
		return err
	}
	return requireEOF(decoder)
}

func consumeJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return fmt.Errorf("JSON nesting limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := keys[key]; duplicate {
				return fmt.Errorf("duplicate object key")
			}
			keys[key] = struct{}{}
			if err := consumeJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("invalid object closing delimiter")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("invalid array closing delimiter")
		}
		return nil
	default:
		return fmt.Errorf("invalid opening delimiter")
	}
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("multiple JSON values")
}

func containsUnsafeControl(value string) bool {
	for _, char := range value {
		if char < 0x20 && char != '\n' && char != '\r' && char != '\t' {
			return true
		}
	}
	return false
}
