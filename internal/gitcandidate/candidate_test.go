package gitcandidate

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func newRepo(t *testing.T, objectFormat string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	args := []string{"init"}
	if objectFormat != "" {
		args = append(args, "--object-format="+objectFormat)
	}
	args = append(args, repo)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		if objectFormat != "" {
			t.Skipf("git does not support %s repositories: %v: %s", objectFormat, err, out)
		}
		t.Fatal(err)
	}
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "tracked.txt"), "base\n", 0o600)
	git(t, repo, "add", "tracked.txt")
	git(t, repo, "commit", "-m", "base")
	return repo
}

func TestFreezeUsesTemporaryIndexAndCapturesCandidateScope(t *testing.T) {
	repo := newRepo(t, "")
	write(t, filepath.Join(repo, "tracked.txt"), "changed\n", 0o600)
	write(t, filepath.Join(repo, "untracked.txt"), "new\n", 0o600)
	write(t, filepath.Join(repo, "executable.sh"), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(repo, ".gitignore"), "ignored.txt\n", 0o600)
	write(t, filepath.Join(repo, "ignored.txt"), "secret-ish\n", 0o600)
	write(t, filepath.Join(repo, ".skynex", "exports", "summary.md"), "generated\n", 0o600)
	if runtime.GOOS != "windows" {
		if err := os.Symlink("tracked.txt", filepath.Join(repo, "link")); err != nil {
			t.Fatal(err)
		}
	}
	// A staged entry matching the worktree proves the ordinary index remains byte-for-byte untouched.
	git(t, repo, "add", "tracked.txt")
	indexPath := gitOutput(t, repo, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(repo, indexPath)
	}
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	seal, err := CaptureContext(repo)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := Freeze(seal, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("freeze mutated the user's index")
	}
	entries := entryMap(candidate.Manifest)
	for _, path := range []string{"tracked.txt", "untracked.txt", "executable.sh", ".gitignore"} {
		if _, ok := entries[path]; !ok {
			t.Errorf("manifest missing %s", path)
		}
	}
	if entries["executable.sh"].Mode != "100755" {
		t.Fatalf("executable mode=%s", entries["executable.sh"].Mode)
	}
	if runtime.GOOS != "windows" && entries["link"].Mode != "120000" {
		t.Fatalf("symlink=%#v", entries["link"])
	}
	if _, ok := entries["ignored.txt"]; ok {
		t.Fatal("implicitly included ignored file")
	}
	if _, ok := entries[".skynex/exports/summary.md"]; ok {
		t.Fatal("included generated export")
	}
	if candidate.TreeOID == "" || candidate.PolicyHash == "" {
		t.Fatalf("candidate=%#v", candidate)
	}

	withIgnored, err := Freeze(seal, Policy{IncludeIgnored: []string{"ignored.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := entryMap(withIgnored.Manifest)["ignored.txt"]; !ok {
		t.Fatal("explicit ignored input missing")
	}
}

func TestDetectDriftWorktreeRefAndHEAD(t *testing.T) {
	t.Run("worktree", func(t *testing.T) {
		repo := newRepo(t, "")
		seal, _ := CaptureContext(repo)
		candidate, err := Freeze(seal, Policy{})
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(repo, "tracked.txt"), "drift\n", 0o600)
		drift, err := DetectDrift(candidate, Policy{})
		if err != nil {
			t.Fatal(err)
		}
		if !drift.Worktree || drift.HEAD || drift.BaseRef {
			t.Fatalf("drift=%#v", drift)
		}
	})
	t.Run("base ref", func(t *testing.T) {
		repo := newRepo(t, "")
		seal, _ := CaptureContext(repo)
		candidate, err := Freeze(seal, Policy{})
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(repo, "next.txt"), "next\n", 0o600)
		git(t, repo, "add", "next.txt")
		git(t, repo, "commit", "-m", "move ref")
		drift, err := DetectDrift(candidate, Policy{})
		if err != nil {
			t.Fatal(err)
		}
		if !drift.BaseRef {
			t.Fatalf("drift=%#v", drift)
		}
	})
	t.Run("head identity", func(t *testing.T) {
		repo := newRepo(t, "")
		seal, _ := CaptureContext(repo)
		candidate, err := Freeze(seal, Policy{})
		if err != nil {
			t.Fatal(err)
		}
		git(t, repo, "checkout", "-b", "other")
		drift, err := DetectDrift(candidate, Policy{})
		if err != nil {
			t.Fatal(err)
		}
		if !drift.HEAD {
			t.Fatalf("drift=%#v", drift)
		}
	})
}

func TestFreezeSHA256RepositoryWhenSupported(t *testing.T) {
	repo := newRepo(t, "sha256")
	seal, err := CaptureContext(repo)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := Freeze(seal, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if seal.ObjectFormat != "sha256" {
		t.Fatalf("format=%s", seal.ObjectFormat)
	}
	if len(candidate.TreeOID) != 64 {
		t.Fatalf("SHA-256 tree OID length=%d", len(candidate.TreeOID))
	}
}

func TestFreezeRejectsAmbiguousStagedOnlyState(t *testing.T) {
	repo := newRepo(t, "")
	write(t, filepath.Join(repo, "tracked.txt"), "staged\n", 0o600)
	git(t, repo, "add", "tracked.txt")
	write(t, filepath.Join(repo, "tracked.txt"), "worktree differs too\n", 0o600)
	seal, err := CaptureContext(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Freeze(seal, Policy{}); !errors.Is(err, ErrAmbiguousIndex) {
		t.Fatalf("error=%v", err)
	}
}

func entryMap(entries []ManifestEntry) map[string]ManifestEntry {
	result := make(map[string]ManifestEntry, len(entries))
	for _, entry := range entries {
		result[entry.Path] = entry
	}
	return result
}
func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSpace(out))
}
