// Package schemas exposes the published evaluation schemas as executable,
// embedded Draft 2020-12 contracts. Keeping the validator beside the JSON
// documents makes the files in this directory the single source of truth for
// import boundaries, including installed binaries which have no source tree.
package schemas

import (
	"bytes"
	_ "embed"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	EvalCase       = "eval-case.schema.json"
	EvalExperiment = "eval-experiment.schema.json"
	EvalResult     = "eval-result.schema.json"
)

//go:embed eval-case.schema.json
var evalCaseSchema []byte

//go:embed eval-experiment.schema.json
var evalExperimentSchema []byte

//go:embed eval-result.schema.json
var evalResultSchema []byte

type schemaSource struct {
	url  string
	data []byte
}

var published = map[string]schemaSource{
	EvalCase: {
		url:  "https://github.com/joeldevz/skynex/schemas/eval-case.schema.json",
		data: evalCaseSchema,
	},
	EvalExperiment: {
		url:  "https://github.com/joeldevz/skynex/schemas/eval-experiment.schema.json",
		data: evalExperimentSchema,
	},
	EvalResult: {
		url:  "https://github.com/joeldevz/skynex/schemas/eval-result.schema.json",
		data: evalResultSchema,
	},
}

var (
	compileOnce sync.Once
	compiled    map[string]*jsonschema.Schema
	compileErr  error
)

// ValidateJSON parses one JSON instance without losing integer precision and
// validates it against the named published schema.
func ValidateJSON(name string, data []byte) error {
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parse instance for published %s: %w", name, err)
	}
	return Validate(name, instance)
}

// Validate validates an already decoded JSON-compatible instance against the
// named published schema.
func Validate(name string, instance any) error {
	compileOnce.Do(compilePublished)
	if compileErr != nil {
		return compileErr
	}
	schema, ok := compiled[name]
	if !ok {
		return fmt.Errorf("unknown published schema %q", name)
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("instance violates published %s: %w", name, err)
	}
	return nil
}

func compilePublished() {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)

	for name, source := range published {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(source.data))
		if err != nil {
			compileErr = fmt.Errorf("parse published %s: %w", name, err)
			return
		}
		if err := compiler.AddResource(source.url, document); err != nil {
			compileErr = fmt.Errorf("register published %s: %w", name, err)
			return
		}
	}

	compiled = make(map[string]*jsonschema.Schema, len(published))
	for name, source := range published {
		schema, err := compiler.Compile(source.url)
		if err != nil {
			compileErr = fmt.Errorf("compile published %s as Draft 2020-12: %w", name, err)
			return
		}
		compiled[name] = schema
	}
}
