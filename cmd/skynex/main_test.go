package main

import (
	"testing"
)

func TestCompactInstallErrorStripsTerminalControlsAndSuggestsVerbose(t *testing.T) {
	for _, raw := range []string{
		"validate \x1b[?2026h\x1b[1;31mopencode\x1b[0m",
		"validate ^[[?2026h^[[1;31mopencode^[[0m",
		"validate \x1b[?2026;2$y\x1b[?2027;2$y\x1b]11;rgb:1/2/3\x07opencode",
	} {
		err := compactInstallError(messageError(raw), false)
		if err != "Installation failed: validate opencode (rerun with --verbose for details)" {
			t.Fatalf("compact error for %q = %q", raw, err)
		}
	}
}

type messageError string

func (e messageError) Error() string { return string(e) }

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

func TestParseArgsFromDryRun(t *testing.T) {
	args := parseArgsFrom([]string{"install", "--dry-run", "--package", "skills", "--target", "claude"})
	if !args.Install || !args.DryRun {
		t.Fatalf("install/dry-run flags not parsed: %#v", args)
	}
	if len(args.Packages) != 1 || args.Packages[0] != "skills" {
		t.Fatalf("packages = %#v", args.Packages)
	}
}

func TestParseArgsFromVerbose(t *testing.T) {
	args := parseArgsFrom([]string{"install", "--verbose"})
	if !args.Install || !args.Verbose {
		t.Fatalf("verbose flag not parsed: %#v", args)
	}
}

func TestParseArgsFromForce(t *testing.T) {
	args := parseArgsFrom([]string{"install", "--force"})
	if !args.Install || !args.Force {
		t.Fatalf("install/force flags not parsed: %#v", args)
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestAdvisorModelFlagIsNotPublicCLIState(t *testing.T) {
	args := parseArgsFrom([]string{"install", "--advisor-model", "legacy/model"})
	if args.ParseError == "" {
		t.Fatal("retired --advisor-model flag was silently accepted")
	}
}
