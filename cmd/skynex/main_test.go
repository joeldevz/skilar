package main

import (
	"testing"
)

func TestNewUpdateInstallRequestPropagatesCleanupDeprecated(t *testing.T) {
	stateDir := t.TempDir()
	packages := []string{"skills"}
	targets := []string{"opencode"}
	versions := map[string]string{"skills": "latest"}

	for _, cleanupDeprecated := range []bool{true, false} {
		t.Run("cleanup="+boolString(cleanupDeprecated), func(t *testing.T) {
			request := newUpdateInstallRequest(packages, targets, versions, stateDir, cleanupDeprecated)
			if request.CleanupDeprecated != cleanupDeprecated {
				t.Fatalf("CleanupDeprecated = %v, want %v", request.CleanupDeprecated, cleanupDeprecated)
			}
			if request.StateDir != stateDir {
				t.Fatalf("StateDir = %q, want %q", request.StateDir, stateDir)
			}
			if request.Interactive {
				t.Fatal("update request must be non-interactive")
			}
		})
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
