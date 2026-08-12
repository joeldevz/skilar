package assets

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

func TestWorkflowPluginUsesDocumentedWakeAPIAndNeverDelivers(t *testing.T) {
	raw, err := os.ReadFile("../../opencode/plugins/skynex-workflow.ts")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := OpencodeFS()
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := fs.ReadFile(bundle, "plugins/skynex-workflow.ts")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"source": raw, "embedded": embedded} {
		text := string(content)
		for _, required := range []string{"shell.env", "session.idle", "showToast", "client.session.prompt", "startNotificationPolling", `"claim"`, `"ack"`, `"release"`} {
			if !strings.Contains(text, required) {
				t.Fatalf("%s missing %q", name, required)
			}
		}
		for _, forbidden := range []string{"promptAsync", "workflow review", "workflow deliver"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s forbidden %q", name, forbidden)
			}
		}
		for _, required := range []string{"automatically continue the next safe managed action", "genuine blocker", "human gate", "destructive ambiguity", "retries are exhausted"} {
			if !strings.Contains(text, required) {
				t.Fatalf("%s wake policy missing %q", name, required)
			}
		}
		for _, forbidden := range []string{"then report the result", "do not review or deliver automatically"} {
			if strings.Contains(strings.ToLower(text), forbidden) {
				t.Fatalf("%s contains report-only wake policy %q", name, forbidden)
			}
		}
	}
}
