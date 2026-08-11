package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrIncompatibleAPI        = errors.New("OpenCode API is incompatible")
	ErrInvalidProviderCatalog = errors.New("OpenCode provider catalog is invalid")
)

// VerifyRequiredAPI validates the exact read/write routes used by the runner.
// It reads only the already captured OpenAPI document and performs no request.
func VerifyRequiredAPI(raw json.RawMessage) ([]string, error) {
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("%w: decode OpenAPI document: %v", ErrIncompatibleAPI, err)
	}
	if len(document.Paths) == 0 {
		return nil, fmt.Errorf("%w: OpenAPI document has no paths", ErrIncompatibleAPI)
	}
	available := make(map[string]map[string]bool, len(document.Paths))
	for path, operations := range document.Paths {
		canonical := canonicalAPIPath(path)
		if available[canonical] == nil {
			available[canonical] = make(map[string]bool)
		}
		for method := range operations {
			available[canonical][strings.ToUpper(method)] = true
		}
	}
	required := map[string][]string{
		"/session":               {"POST"},
		"/session/{}":            {"GET"},
		"/session/{}/children":   {"GET"},
		"/session/{}/message":    {"GET", "POST"},
		"/session/status":        {"GET"},
		"/global/event":          {"GET"},
		"/experimental/tool/ids": {"GET"},
		"/provider":              {"GET"},
	}
	verified := make([]string, 0, 9)
	for path, methods := range required {
		for _, method := range methods {
			if !available[path][method] {
				return nil, fmt.Errorf("%w: required route %s %s is absent", ErrIncompatibleAPI, method, path)
			}
			verified = append(verified, method+" "+path)
		}
	}
	sort.Strings(verified)
	return verified, nil
}

func canonicalAPIPath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			segments[index] = "{}"
		}
	}
	return strings.Join(segments, "/")
}
