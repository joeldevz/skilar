package assets

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

func TestNeuroxPluginSourceAndEmbeddedContract(t *testing.T) {
	source, err := os.ReadFile("../../opencode/plugins/neurox.ts")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := fs.ReadFile(bundle, "plugins/neurox.ts")
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"source": source, "embedded": embedded} {
		text := string(raw)
		for _, want := range []string{"NEUROX_PORT", "NEUROX_BIN", "UNTRUSTED MEMORY DATA", "127.0.0.1", "sanitizeNamespace"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s plugin missing %q", name, want)
			}
		}
		for _, bad := range []string{"/home/clasing", "http://0.0.0.0"} {
			if strings.Contains(text, bad) {
				t.Fatalf("%s plugin contains %q", name, bad)
			}
		}
	}
}
