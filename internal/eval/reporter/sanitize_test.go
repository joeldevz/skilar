package reporter

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeTerminalRemovesEscapeSequencesAndEscapesControls(t *testing.T) {
	input := "safe\x1b[31mred\x1b[0m\x1b]0;owned\x07title\x1bPpayload\x1b\\done\rback\b\u202e"
	got := SanitizeTerminal(input)
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\x07') || strings.ContainsRune(got, '\b') || strings.ContainsRune(got, '\r') {
		t.Fatalf("raw terminal control survived: %q", got)
	}
	for _, want := range []string{"safered", "titledone", `\r`, `\x08`, `\u202e`} {
		if !strings.Contains(got, want) {
			t.Fatalf("sanitized output %q does not contain %q", got, want)
		}
	}
}

func TestSanitizeTerminalBoundsAndRepairsInvalidUTF8(t *testing.T) {
	input := string([]byte{'a', 0xff, 'b'}) + strings.Repeat("x", 100)
	got := SanitizeTerminalN(input, 24)
	if len(got) > 24 || !utf8.ValidString(got) || !strings.Contains(got, `\xff`) {
		t.Fatalf("unsafe bounded output: %q (%d bytes)", got, len(got))
	}
}

func TestSanitizeTerminalHandlesUnterminatedControlString(t *testing.T) {
	got := SanitizeTerminal("before\x1b]unterminated payload")
	if got != "before" {
		t.Fatalf("unterminated OSC leaked: %q", got)
	}
}
