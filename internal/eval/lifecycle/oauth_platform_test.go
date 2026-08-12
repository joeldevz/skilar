package lifecycle

import (
	"strings"
	"testing"
)

func TestCleanOAuthCapsuleIsExplicitlyLinuxOnly(t *testing.T) {
	if err := requireCleanOAuthPlatform("linux"); err != nil {
		t.Fatalf("linux clean OAuth capsule rejected: %v", err)
	}
	for _, goos := range []string{"darwin", "windows", "freebsd"} {
		err := requireCleanOAuthPlatform(goos)
		if err == nil || !strings.Contains(err.Error(), cleanOAuthCapsuleVersion) || !strings.Contains(err.Error(), goos) {
			t.Fatalf("%s clean OAuth capsule error = %v", goos, err)
		}
	}
}
