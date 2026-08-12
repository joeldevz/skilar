package lowcomplete

import "testing"

func TestSubmitButtonColor(t *testing.T) {
	if SubmitButtonColor != "#EF4444" {
		t.Fatalf("SubmitButtonColor = %q, want red #EF4444", SubmitButtonColor)
	}
}
