package reporter

import (
	"fmt"

	"github.com/joeldevz/skynex/internal/eval/baseline"
	"github.com/joeldevz/skynex/internal/eval/runner"
)

// Save writes any machine-readable report as canonical, atomic, mode-0600 JSON.
func Save(value any, path string) error {
	if value == nil {
		return fmt.Errorf("result is nil")
	}
	return baseline.SaveJSON(path, value, baseline.IOOptions{})
}

// Load reads bounded JSON. Strict controls rejection of unknown struct fields.
func Load(path string, destination any, strict bool) error {
	if destination == nil {
		return fmt.Errorf("destination is nil")
	}
	return baseline.LoadJSON(path, destination, baseline.IOOptions{Strict: strict})
}

// SaveResult is the legacy runner adapter.
func SaveResult(result *runner.SuiteResult, path string) error {
	return Save(result, path)
}

// LoadResult is the legacy runner adapter. It remains strict and size-bounded.
func LoadResult(path string) (*runner.SuiteResult, error) {
	var result runner.SuiteResult
	if err := Load(path, &result, true); err != nil {
		return nil, err
	}
	return &result, nil
}
