package experiment

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/joeldevz/skynex/internal/eval/sandbox"
)

func TestInspectGitBundleCleanDirtyAndStaged(t *testing.T) {
	repo := newTestGitRepository(t)
	clean, err := InspectGitBundle(repo)
	if err != nil {
		t.Fatalf("inspect clean repository: %v", err)
	}
	if clean.Dirty || clean.DirtyPatchDigest != "" || !gitSHAPattern.MatchString(clean.GitSHA) {
		t.Fatalf("unexpected clean provenance: %+v", clean)
	}

	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unstaged, err := InspectGitBundle(repo)
	if err != nil {
		t.Fatalf("inspect dirty repository: %v", err)
	}
	if !unstaged.Dirty || !digestPattern.MatchString(unstaged.DirtyPatchDigest) || unstaged.GitSHA != clean.GitSHA {
		t.Fatalf("unexpected dirty provenance: %+v", unstaged)
	}

	runTestGit(t, repo, "add", "--", "tracked.txt")
	indexBefore, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	staged, err := InspectGitBundle(repo)
	if err != nil {
		t.Fatalf("inspect staged repository: %v", err)
	}
	indexAfter, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(indexAfter, indexBefore) {
		t.Fatal("Git inspection modified the index")
	}
	if !staged.Dirty || staged.GitSHA != clean.GitSHA {
		t.Fatalf("unexpected staged provenance: %+v", staged)
	}
	if staged.DirtyPatchDigest == unstaged.DirtyPatchDigest {
		t.Fatal("staging-only state transition did not change dirty_patch_digest")
	}
}

func TestInspectGitBundleWithExecutableRejectsUnsafePaths(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "git")
	if err := os.WriteFile(executable, []byte("not invoked\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "git-link")
	if err := os.Symlink(executable, symlink); err != nil {
		t.Skipf("create executable symlink: %v", err)
	}
	unclean := directory + string(os.PathSeparator) + "." + string(os.PathSeparator) + "git"

	for _, test := range []struct {
		name string
		path string
		want string
	}{
		{name: "relative", path: "git", want: "must be absolute"},
		{name: "unclean", path: unclean, want: "must be clean"},
		{name: "missing", path: filepath.Join(directory, "missing"), want: "stat"},
		{name: "directory", path: directory, want: "not a regular file"},
		{name: "symlink", path: symlink, want: "symbolic link"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := InspectGitBundleWithExecutable(directory, test.path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}

	if runtime.GOOS != "windows" {
		nonExecutable := filepath.Join(directory, "git-no-exec")
		if err := os.WriteFile(nonExecutable, []byte("not invoked\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := InspectGitBundleWithExecutable(directory, nonExecutable); err == nil || !strings.Contains(err.Error(), "not executable") {
			t.Fatalf("non-executable error = %v", err)
		}
	}
}

func TestInspectGitBundleWithExecutableIgnoresPATHDrift(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git executable is unavailable")
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err = filepath.EvalSymlinks(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	repo := newTestGitRepository(t)

	// The pinned API must keep using the captured absolute path even after the
	// ambient search authority no longer contains Git.
	t.Setenv("PATH", t.TempDir())
	observed, err := InspectGitBundleWithExecutable(repo, gitPath)
	if err != nil {
		t.Fatalf("inspect with pinned Git after PATH drift: %v", err)
	}
	if observed.Dirty || !gitSHAPattern.MatchString(observed.GitSHA) {
		t.Fatalf("unexpected provenance after PATH drift: %+v", observed)
	}
}

func TestVerifyBundlesEnforcesGitProvenanceAndHasNoDuplicateBundles(t *testing.T) {
	base, repo, manifest := newGitManifestWorkspace(t)
	clean, err := InspectGitBundle(repo)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Harness.GitSHA = clean.GitSHA

	frozen, err := manifest.VerifyBundles(base, sandbox.SnapshotLimits{})
	if err != nil {
		t.Fatalf("verify clean bundles: %v", err)
	}
	gotNames := make([]string, len(frozen.Bundles))
	for index, bundle := range frozen.Bundles {
		gotNames[index] = bundle.Name
	}
	if want := []string{"harness", "control", "candidate"}; !reflect.DeepEqual(gotNames, want) {
		t.Fatalf("verified bundle names = %v, want %v", gotNames, want)
	}
	if err := frozen.VerifyUnchanged(); err != nil {
		t.Fatalf("recheck clean bundles: %v", err)
	}

	missingSHA := manifest
	missingSHA.Harness.GitSHA = ""
	if _, err := missingSHA.VerifyBundles(base, sandbox.SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "git_sha is required") {
		t.Fatalf("missing Git SHA error = %v", err)
	}

	wrongSHA := manifest
	wrongSHA.Harness.GitSHA = differentHex(clean.GitSHA)
	if _, err := wrongSHA.VerifyBundles(base, sandbox.SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "git_sha mismatch") {
		t.Fatalf("Git SHA mismatch error = %v", err)
	}

	cleanWithPatch := manifest
	cleanWithPatch.Harness.DirtyPatchDigest = "sha256:" + strings.Repeat("a", 64)
	if _, err := cleanWithPatch.VerifyBundles(base, sandbox.SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "declared for a clean") {
		t.Fatalf("clean dirty-patch error = %v", err)
	}
}

func TestVerifyBundlesRequiresExactDirtyPatchDigest(t *testing.T) {
	base, repo, manifest := newGitManifestWorkspace(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Harness.Digest = digestTestBundle(t, repo)
	dirty, err := InspectGitBundle(repo)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Harness.GitSHA = dirty.GitSHA

	if _, err := manifest.VerifyBundles(base, sandbox.SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "dirty_patch_digest is required") {
		t.Fatalf("missing dirty patch error = %v", err)
	}
	manifest.Harness.DirtyPatchDigest = differentDigest(dirty.DirtyPatchDigest)
	if _, err := manifest.VerifyBundles(base, sandbox.SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "dirty_patch_digest mismatch") {
		t.Fatalf("dirty patch mismatch error = %v", err)
	}
	manifest.Harness.DirtyPatchDigest = dirty.DirtyPatchDigest
	if _, err := manifest.VerifyBundles(base, sandbox.SnapshotLimits{}); err != nil {
		t.Fatalf("verify exact dirty provenance: %v", err)
	}
}

func TestVerifyUnchangedDetectsStagingOnlyGitDrift(t *testing.T) {
	base, repo, manifest := newGitManifestWorkspace(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Harness.Digest = digestTestBundle(t, repo)
	unstaged, err := InspectGitBundle(repo)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Harness.GitSHA = unstaged.GitSHA
	manifest.Harness.DirtyPatchDigest = unstaged.DirtyPatchDigest
	frozen, err := manifest.VerifyBundles(base, sandbox.SnapshotLimits{})
	if err != nil {
		t.Fatalf("freeze dirty bundle: %v", err)
	}

	// Staging changes only .git. DigestTree deliberately excludes .git, so this
	// proves VerifyUnchanged independently enforces the Git status provenance.
	runTestGit(t, repo, "add", "--", "tracked.txt")
	if current := digestTestBundle(t, repo); current != manifest.Harness.Digest {
		t.Fatalf("content digest changed during staging: %s != %s", current, manifest.Harness.Digest)
	}
	if err := frozen.VerifyUnchanged(); err == nil || !strings.Contains(err.Error(), "dirty_patch_digest mismatch") {
		t.Fatalf("staging-only drift error = %v", err)
	}
}

func TestValidateDirtyPatchDigestRequiresGitSHA(t *testing.T) {
	manifest := validManifest()
	manifest.Harness.DirtyPatchDigest = "sha256:" + strings.Repeat("a", 64)
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "dirty_patch_digest requires git_sha") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestGitInspectionEnvironmentDoesNotInheritGitConfiguration(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", "/must-not-survive")
	t.Setenv("GIT_EXTERNAL_DIFF", "/must-not-survive")
	t.Setenv("SSH_AUTH_SOCK", "/must-not-survive")

	environment := gitInspectionEnvironment()
	joined := "\x00" + strings.Join(environment, "\x00") + "\x00"
	for _, forbidden := range []string{
		"GIT_CONFIG_COUNT=", "GIT_CONFIG_KEY_0=", "GIT_CONFIG_VALUE_0=",
		"GIT_EXTERNAL_DIFF=", "SSH_AUTH_SOCK=",
	} {
		if strings.Contains(joined, "\x00"+forbidden) {
			t.Fatalf("ambient environment variable survived: %s", forbidden)
		}
	}
	for _, required := range []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0",
	} {
		if !strings.Contains(joined, "\x00"+required+"\x00") {
			t.Fatalf("required sanitized environment missing: %s", required)
		}
	}
}

func newGitManifestWorkspace(t *testing.T) (string, string, Manifest) {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, "harness")
	newTestGitRepositoryAt(t, repo)
	for _, name := range []string{"control", "candidate"} {
		root := filepath.Join(base, name)
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "bundle.txt"), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := validManifest()
	manifest.Harness = FrozenBundle{Root: "harness", Digest: digestTestBundle(t, repo)}
	manifest.Control = FrozenBundle{Root: "control", Digest: digestTestBundle(t, filepath.Join(base, "control"))}
	manifest.Candidate = FrozenBundle{Root: "candidate", Digest: digestTestBundle(t, filepath.Join(base, "candidate"))}
	return base, repo, manifest
}

func newTestGitRepository(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	newTestGitRepositoryAt(t, repo)
	return repo
}

func newTestGitRepositoryAt(t *testing.T, repo string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable is unavailable")
	}
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("clean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "--", "tracked.txt")
	runTestGit(t, repo, "commit", "--quiet", "-m", "initial")
}

func runTestGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git executable is unavailable")
	}
	commandArgs := []string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "commit.gpgsign=false",
		"-c", "user.name=Skynex Test",
		"-c", "user.email=eval@invalid.local",
		"-C", repo,
	}
	commandArgs = append(commandArgs, args...)
	command := exec.Command(gitPath, commandArgs...)
	command.Env = gitInspectionEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func digestTestBundle(t *testing.T, root string) string {
	t.Helper()
	snapshot, err := sandbox.DigestTree(root, sandbox.SnapshotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.Digest
}

func differentHex(value string) string {
	replacement := byte('0')
	if value[0] == replacement {
		replacement = '1'
	}
	return string(replacement) + value[1:]
}

func differentDigest(value string) string {
	hex := strings.TrimPrefix(value, "sha256:")
	return "sha256:" + differentHex(hex)
}
