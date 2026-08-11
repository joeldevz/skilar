package baseline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const DefaultMaxJSONBytes int64 = 64 << 20

// IOOptions bounds imported data and controls strict top-level decoding.
type IOOptions struct {
	MaxBytes int64
	Strict   bool
}

// CanonicalJSON returns deterministic compact JSON. encoding/json sorts object
// keys; a terminal newline is deliberately excluded from digest material.
func CanonicalJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical JSON: %w", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return nil, fmt.Errorf("compact canonical JSON: %w", err)
	}
	return compact.Bytes(), nil
}

// SaveJSON writes a canonical JSON file through a same-directory temporary file,
// fsyncs it, atomically renames it, and enforces mode 0600. Existing symlink and
// non-regular targets are rejected.
func SaveJSON(path string, value any, options IOOptions) error {
	options = normalizeIOOptions(options)
	data, err := CanonicalJSON(value)
	if err != nil {
		return err
	}
	if int64(len(data)+1) > options.MaxBytes {
		return fmt.Errorf("JSON output exceeds %d bytes", options.MaxBytes)
	}
	directory := filepath.Dir(path)
	if directory == "" {
		directory = "."
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create result directory: %w", err)
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refuse non-regular result target %q", path)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect result target: %w", statErr)
	}
	temporary, err := os.CreateTemp(directory, ".skynex-eval-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary result: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("chmod temporary result: %w", err)
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary result: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace result atomically: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod result: %w", err)
	}
	if directoryHandle, openErr := os.Open(directory); openErr == nil {
		syncErr := directoryHandle.Sync()
		closeErr := directoryHandle.Close()
		if syncErr != nil {
			return fmt.Errorf("sync result directory: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close result directory: %w", closeErr)
		}
	}
	return nil
}

// LoadJSON reads a regular non-symlink file through a hard byte limit, rejects
// duplicate object keys, optionally rejects unknown struct fields, and requires
// exactly one JSON value.
func LoadJSON(path string, destination any, options IOOptions) error {
	options = normalizeIOOptions(options)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect JSON file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("refuse non-regular JSON file %q", path)
	}
	if info.Size() > options.MaxBytes {
		return fmt.Errorf("JSON input exceeds %d bytes", options.MaxBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open JSON file: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat opened JSON file: %w", err)
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		return fmt.Errorf("JSON file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, options.MaxBytes+1))
	if err != nil {
		return fmt.Errorf("read JSON file: %w", err)
	}
	if int64(len(data)) > options.MaxBytes {
		return fmt.Errorf("JSON input exceeds %d bytes", options.MaxBytes)
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if options.Strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode JSON: multiple values")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func normalizeIOOptions(options IOOptions) IOOptions {
	if options.MaxBytes <= 0 {
		options.MaxBytes = DefaultMaxJSONBytes
	}
	return options
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return fmt.Errorf("validate JSON keys: %w", err)
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("validate JSON keys: unexpected trailing token %v", token)
		}
		return fmt.Errorf("validate JSON keys: %w", err)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
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
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("invalid object closing delimiter")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("invalid array closing delimiter")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delimiter)
	}
	return nil
}
