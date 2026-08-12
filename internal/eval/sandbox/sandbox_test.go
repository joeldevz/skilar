package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const helperEnv = "SKYNEX_EVAL_HELPER"

func TestSandboxHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(97)
	}
	args := os.Args[separator+1:]
	switch args[0] {
	case "echo":
		fmt.Print(strings.Join(args[1:], "<arg>"))
	case "write":
		if len(args) != 3 {
			os.Exit(96)
		}
		if err := os.WriteFile(args[1], []byte(args[2]), 0o640); err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(95)
		}
	case "env-to-file":
		if len(args) != 3 {
			os.Exit(94)
		}
		if err := os.WriteFile(args[2], []byte(os.Getenv(args[1])), 0o600); err != nil {
			os.Exit(93)
		}
	case "spam":
		if len(args) != 2 {
			os.Exit(92)
		}
		count, _ := strconv.Atoi(args[1])
		_, _ = io.CopyN(os.Stdout, strings.NewReader(strings.Repeat("x", count)), int64(count))
		time.Sleep(time.Second)
	case "sleep":
		duration, _ := time.ParseDuration(args[1])
		time.Sleep(duration)
	case "spawn-marker":
		if len(args) != 3 {
			os.Exit(91)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestSandboxHelperProcess$", "--", "delayed-write", args[1], args[2])
		child.Env = os.Environ()
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(90)
		}
		time.Sleep(5 * time.Second)
	case "delayed-write":
		duration, _ := time.ParseDuration(args[2])
		time.Sleep(duration)
		_ = os.WriteFile(args[1], []byte("descendant survived"), 0o600)
	case "exit":
		code, _ := strconv.Atoi(args[1])
		os.Exit(code)
	default:
		os.Exit(89)
	}
	os.Exit(0)
}

func helperArgv(mode string, args ...string) []string {
	result := []string{os.Args[0], "-test.run=^TestSandboxHelperProcess$", "--", mode}
	return append(result, args...)
}

func testConfig(t *testing.T, source, parent string) Config {
	t.Helper()
	runner := DefaultRunnerConfig()
	runner.AllowedExecutables = []string{os.Args[0]}
	runner.AllowedEnv = []string{helperEnv}
	runner.Environment = map[string]string{helperEnv: "1"}
	runner.DefaultTimeout = 2 * time.Second
	runner.MaxTimeout = 5 * time.Second
	runner.Quiescence = 200 * time.Millisecond
	return Config{ParentDir: parent, SourceDir: source, Runner: runner}
}

func materializeTestWorkspace(t *testing.T) *Workspace {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "original.txt"), []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	workspace, err := Materialize(context.Background(), testConfig(t, source, t.TempDir()))
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	t.Cleanup(func() {
		if err := workspace.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return workspace
}

func TestMaterializeAcceptsDeclaredNonZeroSetupExit(t *testing.T) {
	source := t.TempDir()
	config := testConfig(t, source, t.TempDir())
	config.Setup = []Command{{
		ID: "expected-red", Argv: helperArgv("exit", "7"), ExpectedExit: []int{7},
	}}
	workspace, err := Materialize(context.Background(), config)
	if err != nil {
		t.Fatalf("Materialize() rejected declared setup exit: %v", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}

	config = testConfig(t, source, t.TempDir())
	config.Setup = []Command{{
		ID: "unexpected-red", Argv: helperArgv("exit", "7"), ExpectedExit: []int{1},
	}}
	if _, err := Materialize(context.Background(), config); err == nil {
		t.Fatal("Materialize() accepted an undeclared setup exit")
	}
}

func TestRunnerUsesPinnedExecutablePathForDeclaration(t *testing.T) {
	source := t.TempDir()
	config := testConfig(t, source, t.TempDir())
	pinned, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	config.Runner.AllowedExecutables = []string{"pinned-tool"}
	config.Runner.ExecutablePaths = map[string]string{"pinned-tool": pinned}
	config.Setup = []Command{{
		ID: "pinned", Argv: []string{"pinned-tool", "-test.run=^TestSandboxHelperProcess$", "--", "echo", "exact"},
	}}
	workspace, err := Materialize(context.Background(), config)
	if err != nil {
		t.Fatalf("Materialize() did not use pinned executable: %v", err)
	}
	defer workspace.Close()
	if len(workspace.SetupResults) != 1 || workspace.SetupResults[0].Stdout != "exact" {
		t.Fatalf("pinned setup result = %+v", workspace.SetupResults)
	}
}

func TestDigestTreeCanonicalAndRejectsLinks(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, root := range []string{left, right} {
		if err := os.Mkdir(filepath.Join(root, "dir"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "dir", "file.txt"), []byte("same"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	leftDigest, err := DigestTree(left, SnapshotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := DigestTree(right, SnapshotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest.Digest != rightDigest.Digest {
		t.Fatalf("canonical digests differ: %s != %s", leftDigest.Digest, rightDigest.Digest)
	}

	symlinkRoot := t.TempDir()
	if err := os.Symlink(filepath.Join(left, "dir", "file.txt"), filepath.Join(symlinkRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := DigestTree(symlinkRoot, SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("DigestTree() symlink error = %v", err)
	}

	hardlinkRoot := t.TempDir()
	first := filepath.Join(hardlinkRoot, "first")
	if err := os.WriteFile(first, []byte("linked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(hardlinkRoot, "second")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := DigestTree(hardlinkRoot, SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "hardlink") {
		t.Fatalf("DigestTree() hardlink error = %v", err)
	}
}

func TestDigestTreeRejectsSymlinkedRootAndTraversal(t *testing.T) {
	realRoot := t.TempDir()
	link := filepath.Join(t.TempDir(), "fixture-link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	if _, err := DigestTree(link, SnapshotLimits{}); err == nil {
		t.Fatal("DigestTree() accepted a symlinked root")
	}
	workspace := materializeTestWorkspace(t)
	for _, dir := range []string{"../outside", "/tmp", "%2e%2e/outside", "nested/%2foutside"} {
		result := workspace.Run(context.Background(), Command{ID: "traversal", Argv: helperArgv("echo", "x"), Dir: dir})
		if result.Started || result.Error == "" {
			t.Errorf("Run(cwd=%q) = %+v, want pre-start rejection", dir, result)
		}
	}
}

func TestMaterializeCopiesPrivatelyAndLeavesSourceUnchanged(t *testing.T) {
	source, parent := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "source.txt"), []byte("source"), 0o640); err != nil {
		t.Fatal(err)
	}
	sourceBefore, err := DigestTree(source, SnapshotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig(t, source, parent)
	config.ExpectedSourceDigest = sourceBefore.Digest
	workspace, err := Materialize(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	runPath := workspace.RunPath()
	info, err := os.Stat(runPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("run root mode = %#o, want 0700", info.Mode().Perm())
	}
	if err := os.WriteFile(filepath.Join(workspace.Path(), "nested", "source.txt"), []byte("candidate"), 0o640); err != nil {
		t.Fatal(err)
	}
	unchanged, err := workspace.SourceUnchanged()
	if err != nil || !unchanged {
		t.Fatalf("SourceUnchanged() = %v, %v", unchanged, err)
	}
	data, err := os.ReadFile(filepath.Join(source, "nested", "source.txt"))
	if err != nil || string(data) != "source" {
		t.Fatalf("source file = %q, %v", data, err)
	}
	after, err := workspace.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(Diff(workspace.Before, after)) != 1 {
		t.Fatalf("Diff() = %+v, want one workspace-only change", Diff(workspace.Before, after))
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runPath); !os.IsNotExist(err) {
		t.Fatalf("private run root still exists after Close(): %v", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestMaterializeRunsSetupBeforeInitialSnapshot(t *testing.T) {
	source := t.TempDir()
	config := testConfig(t, source, t.TempDir())
	config.Setup = []Command{{
		ID: "setup", Argv: helperArgv("write", "setup.txt", "prepared"),
		Env: map[string]string{helperEnv: "1"},
	}}
	workspace, err := Materialize(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	entry, ok := workspace.Before.Entry("setup.txt")
	if !ok || entry.SHA256 != contentSHA256("prepared") {
		t.Fatalf("before snapshot does not contain setup output: %+v, %v", entry, ok)
	}
	after, err := workspace.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if changes := Diff(workspace.Before, after); len(changes) != 0 {
		t.Fatalf("setup was treated as candidate diff: %+v", changes)
	}
}

func TestInitialGitIsDeterministic(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	var commits []string
	for range 2 {
		config := testConfig(t, source, t.TempDir())
		config.InitialGit = true
		workspace, err := Materialize(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		result := workspace.Run(context.Background(), Command{ID: "head", Argv: []string{"git", "rev-parse", "HEAD"}})
		if !result.Successful() {
			t.Fatalf("git rev-parse failed: %+v", result)
		}
		commits = append(commits, strings.TrimSpace(result.Stdout))
		if err := workspace.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if commits[0] != commits[1] {
		t.Fatalf("deterministic commits differ: %q != %q", commits[0], commits[1])
	}
}

func TestGitSeedCreatesExactDirtyWorktreeAndStatusEvidence(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	source := t.TempDir()
	files := map[string]string{
		"tracked-source.txt":  "source tracked",
		"staged-existing.txt": "baseline bytes",
		".gitignore":          "ignored.txt\n",
	}
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(source, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	config := testConfig(t, source, t.TempDir())
	config.InitialGit = true
	config.GitSeed = GitSeed{
		Tracked: []SeedFile{
			{Path: "tracked-source.txt", Digest: digestBytes([]byte("source tracked"))},
			{Path: "tracked-added.txt", Content: []byte("tracked added"), Mode: 0o644},
		},
		Staged:    []SeedFile{{Path: "staged-existing.txt", Content: []byte("staged bytes"), Mode: 0o644}},
		Untracked: []SeedFile{{Path: "untracked.txt", Content: []byte("untracked bytes"), Mode: 0o644}},
		Ignored:   []SeedFile{{Path: "ignored.txt", Content: []byte("ignored bytes"), Mode: 0o644}},
	}
	workspace, err := Materialize(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	if !validSHA256(workspace.InitialGitStatus.Digest) || len(workspace.InitialGitStatus.Raw) == 0 ||
		!validGitObjectID(workspace.InitialGitStatus.Head) || !validSHA256(workspace.InitialGitStatus.IndexDigest) ||
		len(workspace.InitialGitStatus.IndexRaw) == 0 || !validSHA256(workspace.InitialGitStatus.StateDigest) {
		t.Fatalf("missing porcelain-v2 status evidence: %+v", workspace.InitialGitStatus)
	}
	status := make(map[string]GitStatusEntry)
	for _, entry := range workspace.InitialGitStatus.Entries {
		status[entry.Path] = entry
	}
	if _, dirty := status["tracked-source.txt"]; dirty {
		t.Fatalf("tracked source is dirty: %+v", status["tracked-source.txt"])
	}
	if _, dirty := status["tracked-added.txt"]; dirty {
		t.Fatalf("tracked seed is dirty: %+v", status["tracked-added.txt"])
	}
	if staged := status["staged-existing.txt"]; staged.IndexStatus == '.' || staged.WorktreeStatus != '.' {
		t.Fatalf("staged seed status = %+v", staged)
	}
	if untracked := status["untracked.txt"]; untracked.Kind != "untracked" {
		t.Fatalf("untracked seed status = %+v", untracked)
	}
	if ignored := status["ignored.txt"]; ignored.Kind != "ignored" {
		t.Fatalf("ignored seed status = %+v", ignored)
	}
	if data, err := os.ReadFile(filepath.Join(workspace.Path(), "ignored.txt")); err != nil || string(data) != "ignored bytes" {
		t.Fatalf("ignored seed content = %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path(), "candidate.txt"), []byte("allowed worktree-only change"), 0o644); err != nil {
		t.Fatal(err)
	}
	afterWorktree, err := workspace.CaptureGitStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	comparison := CompareGitState(workspace.InitialGitStatus, afterWorktree)
	if !comparison.Preserved() {
		t.Fatalf("worktree-only addition changed the immutable Git basis: %+v", comparison)
	}
	if result := workspace.Run(context.Background(), Command{ID: "stage-candidate", Argv: []string{"git", "add", "--", "candidate.txt"}}); !result.Successful() {
		t.Fatalf("git add failed: %+v", result)
	}
	afterAdd, err := workspace.CaptureGitStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	comparison = CompareGitState(workspace.InitialGitStatus, afterAdd)
	if comparison.Preserved() || comparison.IndexPreserved {
		t.Fatalf("index mutation was not detected: %+v", comparison)
	}
	if result := workspace.Run(context.Background(), Command{ID: "commit-candidate", Argv: []string{"git", "commit", "--no-gpg-sign", "-m", "candidate"}}); !result.Successful() {
		t.Fatalf("git commit failed: %+v", result)
	}
	afterCommit, err := workspace.CaptureGitStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	comparison = CompareGitState(workspace.InitialGitStatus, afterCommit)
	if comparison.Preserved() || comparison.HeadPreserved {
		t.Fatalf("HEAD mutation was not detected: %+v", comparison)
	}
	unchanged, err := workspace.SourceUnchanged()
	if err != nil || !unchanged {
		t.Fatalf("source changed while Git seed was applied: %v, %v", unchanged, err)
	}
}

func TestCopyVerifiedTreeFreezesIntoEmptyDestination(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "bundle"), []byte("immutable input"), 0o640); err != nil {
		t.Fatal(err)
	}
	copied, err := CopyVerifiedTree(source, destination, SnapshotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest, err := DigestTree(source, SnapshotLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if copied.Digest != sourceDigest.Digest {
		t.Fatalf("copied digest = %s, source = %s", copied.Digest, sourceDigest.Digest)
	}
	if err := os.WriteFile(filepath.Join(destination, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyVerifiedTree(source, destination, SnapshotLimits{}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("CopyVerifiedTree() nonempty destination error = %v", err)
	}
}

func TestRunPassesMetacharactersLiterallyAndDoesNotInheritSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	workspace := materializeTestWorkspace(t)
	marker := filepath.Join(workspace.Path(), "pwned")
	literal := "$(touch " + marker + "); * | > &"
	result := workspace.Run(context.Background(), Command{
		ID: "literal", Argv: helperArgv("echo", literal), Env: map[string]string{helperEnv: "1"},
	})
	if !result.Successful() || !strings.Contains(result.Stdout, literal) {
		t.Fatalf("literal command result = %+v", result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("shell metacharacters were interpreted: %v", err)
	}
	secretFile := "secret.txt"
	result = workspace.Run(context.Background(), Command{
		ID: "env", Argv: helperArgv("env-to-file", "OPENAI_API_KEY", secretFile), Env: map[string]string{helperEnv: "1"},
	})
	if !result.Successful() {
		t.Fatalf("environment probe failed: %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(workspace.Path(), secretFile))
	if err != nil || len(data) != 0 {
		t.Fatalf("credential leaked into command environment: %q, %v", data, err)
	}
}

func TestRunEnforcesExecutableAndOutputCaps(t *testing.T) {
	workspace := materializeTestWorkspace(t)
	unlisted := workspace.Run(context.Background(), Command{ID: "unlisted", Argv: []string{"definitely-not-allowed"}})
	if unlisted.Started || !strings.Contains(unlisted.Error, "not allowlisted") {
		t.Fatalf("unlisted executable result = %+v", unlisted)
	}
	workspace.runner.config.MaxStdoutBytes = 64
	result := workspace.Run(context.Background(), Command{
		ID: "spam", Argv: helperArgv("spam", "4096"), Env: map[string]string{helperEnv: "1"},
	})
	if !result.OutputLimitExceeded || !result.StdoutTruncated || len(result.Stdout) != 64 || result.Successful() {
		t.Fatalf("output-capped result = %+v (stdout len %d)", result, len(result.Stdout))
	}
}

func TestRunTimeoutKillsParentAndDescendant(t *testing.T) {
	workspace := materializeTestWorkspace(t)
	marker := filepath.Join(workspace.Path(), "survived.txt")
	result := workspace.Run(context.Background(), Command{
		ID: "timeout", Argv: helperArgv("spawn-marker", marker, "400ms"),
		Env: map[string]string{helperEnv: "1"}, Timeout: 100 * time.Millisecond,
	})
	if !result.TimedOut || result.Successful() {
		t.Fatalf("timeout result = %+v", result)
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant survived process-group timeout: %v", err)
	}
}

func TestRunnerRejectsExplicitShellAndCredentialEnvironment(t *testing.T) {
	source, parent := t.TempDir(), t.TempDir()
	config := testConfig(t, source, parent)
	config.Runner.AllowedExecutables = append(config.Runner.AllowedExecutables, "sh")
	if _, err := Materialize(context.Background(), config); err == nil || !strings.Contains(err.Error(), "shell executable") {
		t.Fatalf("Materialize() shell error = %v", err)
	}
	config = testConfig(t, source, parent)
	config.Runner.AllowedEnv = append(config.Runner.AllowedEnv, "API_TOKEN")
	if _, err := Materialize(context.Background(), config); err == nil || !strings.Contains(err.Error(), "credential-like") {
		t.Fatalf("Materialize() credential error = %v", err)
	}
}

func contentSHA256(value string) string {
	// Kept local to avoid coupling sandbox tests to judge helpers.
	return digestBytes([]byte(value))
}
