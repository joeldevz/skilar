package adapters

import (
	"bytes"
	"strings"
	"testing"
)

func TestInstallOutputNormalIsQuietAndCompletionIsSingleOwner(t *testing.T) {
	var output bytes.Buffer
	reporter := NewReporter(&output, false)

	steps := []func() error{
		func() error { reporter.Detail("Installing skills"); return nil },
		func() error { reporter.Detail("Using embedded assets"); return nil },
		func() error { reporter.Detail("Backup path: /tmp/config.bak"); return nil },
		func() error { reporter.Detail("Copying config"); return nil },
		func() error { reporter.Detail("Installing dependencies"); return nil },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	reporter.Complete([]string{"Backup created", "Skills updated", "OpenCode configured", "State saved"})

	got := output.String()
	if strings.Contains(got, "Installing skills") || strings.Contains(got, "embedded assets") || strings.Contains(got, "/tmp/config.bak") {
		t.Fatalf("normal output leaked internal details: %q", got)
	}
	if strings.Count(got, "You're ready for launch.") != 1 || strings.Count(got, "opencode") != 1 {
		t.Fatalf("completion duplicated or missing: %q", got)
	}
	if strings.ContainsAny(got, "\x1b") {
		t.Fatalf("raw ANSI in output: %q", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 80 || strings.HasPrefix(line, "    ") {
			t.Fatalf("unhealthy line %q", line)
		}
	}
}

func TestInstallOutputVerboseIsLinear(t *testing.T) {
	var output bytes.Buffer
	reporter := NewReporter(&output, true)
	reporter.Detail("Installing skills")
	reporter.Detail("Backup path: /tmp/config.bak")

	got := output.String()
	if !strings.Contains(got, "Installing skills") || !strings.Contains(got, "/tmp/config.bak") {
		t.Fatalf("verbose output hid details: %q", got)
	}
	if strings.ContainsAny(got, "\x1b") {
		t.Fatalf("raw ANSI in verbose output: %q", got)
	}
}

func TestSanitizeTerminalRemovesControlSequences(t *testing.T) {
	malicious := "ok\x1b]2;evil-title\x07\x1b[31mRED\x1b[0m\x1b]8;;https://evil\x07click\x1b]8;;\x07\x00\x1f\x9b"
	got := sanitizeTerminal(malicious)
	if got != "okREDclick" {
		t.Fatalf("sanitized output = %q", got)
	}
}
