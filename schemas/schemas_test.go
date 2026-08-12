package schemas

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublishedSchemasCompileAsDraft2020_12(t *testing.T) {
	compileOnce.Do(compilePublished)
	if compileErr != nil {
		t.Fatalf("compile published schemas: %v", compileErr)
	}
	if len(compiled) != len(published) {
		t.Fatalf("compiled %d schemas, want %d", len(compiled), len(published))
	}

	for name, source := range published {
		name, source := name, source
		t.Run(name, func(t *testing.T) {
			var metadata struct {
				Draft string `json:"$schema"`
				ID    string `json:"$id"`
			}
			if err := json.Unmarshal(source.data, &metadata); err != nil {
				t.Fatalf("decode published schema: %v", err)
			}
			if metadata.Draft != "https://json-schema.org/draft/2020-12/schema" {
				t.Fatalf("$schema = %q, want Draft 2020-12", metadata.Draft)
			}
			if metadata.ID != source.url {
				t.Fatalf("$id = %q, registered URL = %q", metadata.ID, source.url)
			}
			if compiled[name] == nil {
				t.Fatal("schema was not compiled")
			}
			if err := ValidateJSON(name, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "instance violates published") {
				t.Fatalf("empty invalid instance error = %v", err)
			}
		})
	}
}
