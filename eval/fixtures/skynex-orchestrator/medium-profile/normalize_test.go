package mediumprofile

import "testing"

func TestNormalizeProfile(t *testing.T) {
	got := NormalizeProfile(Profile{
		DisplayName: "  Ada   Lovelace  ",
		Email:       " ADA@EXAMPLE.COM ",
	})
	if got.DisplayName != "Ada Lovelace" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
	if got.Email != "ada@example.com" {
		t.Errorf("Email = %q", got.Email)
	}
}
