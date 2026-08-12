package lowslugify

import "testing"

func TestSlugify(t *testing.T) {
	tests := map[string]string{
		"Hello World":       "hello-world",
		"  Already spaced ": "already-spaced",
		"Two---Separators":  "two-separators",
	}
	for input, want := range tests {
		if got := Slugify(input); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", input, got, want)
		}
	}
}
