package contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// CanonicalDigest hashes deterministic JSON rather than a Go map's iteration
// order or the formatting of the source YAML.
func CanonicalDigest(value interface{}) (string, error) {
	data, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// CanonicalJSON returns whitespace-free JSON with recursively sorted object
// keys. Numbers are retained in the exact stable representation emitted by the
// standard library for the supplied Go value.
func CanonicalJSON(value interface{}) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var normalized interface{}
	if err := decoder.Decode(&normalized); err != nil {
		return nil, fmt.Errorf("decode canonical JSON: %w", err)
	}
	if err := expectJSONEOF(decoder); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := appendCanonicalJSON(&out, normalized); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func appendCanonicalJSON(out *bytes.Buffer, value interface{}) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		encoded, _ := json.Marshal(typed)
		out.Write(encoded)
	case json.Number:
		out.WriteString(typed.String())
	case []interface{}:
		out.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonicalJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]interface{}:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			out.Write(encoded)
			out.WriteByte(':')
			if err := appendCanonicalJSON(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON type %T", value)
	}
	return nil
}

func expectJSONEOF(decoder *json.Decoder) error {
	var extra interface{}
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("canonical JSON contains multiple values")
	}
	return fmt.Errorf("finish canonical JSON: %w", err)
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || value[:len("sha256:")] != "sha256:" {
		return false
	}
	hexValue := value[len("sha256:"):]
	decoded, err := hex.DecodeString(hexValue)
	return err == nil && hex.EncodeToString(decoded) == hexValue
}

// IsDigest reports whether value is a lowercase hexadecimal sha256 digest with
// the required algorithm prefix.
func IsDigest(value string) bool {
	return validDigest(value)
}
