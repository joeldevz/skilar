package dirtyworktree

import "testing"

func TestMessage(t *testing.T) {
	if got := Message(); got != "after" {
		t.Fatalf("Message() = %q, want after", got)
	}
}
