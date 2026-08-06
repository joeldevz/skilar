package assets

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var retiredAdvisorPaths = []string{
	"agents/advisor.md",
	"plugins/advisor.ts",
	"skills/_shared/advisor-protocol.md",
}

func assertAdvisorRetired(t *testing.T, root fs.FS) {
	t.Helper()
	for _, path := range retiredAdvisorPaths {
		if _, err := fs.Stat(root, path); !os.IsNotExist(err) {
			t.Errorf("retired Advisor asset %q remains: %v", path, err)
		}
	}
	raw, err := fs.ReadFile(root, "opencode.json")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(raw))
	for _, forbidden := range []string{"\"advisor\"", "advisor_consult"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("OpenCode config retains %q", forbidden)
		}
	}
}

func TestSourceBundleRetiresAdvisor(t *testing.T) {
	assertAdvisorRetired(t, os.DirFS(filepath.Join("..", "..", "opencode")))
}

func TestEmbeddedBundleRetiresAdvisor(t *testing.T) {
	root, err := OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	assertAdvisorRetired(t, root)
}
