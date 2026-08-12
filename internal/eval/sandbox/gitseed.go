package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/joeldevz/skynex/internal/safefs"
)

func validateGitSeed(seed GitSeed) error {
	groups := []struct {
		name  string
		files []SeedFile
	}{
		{"tracked", seed.Tracked}, {"staged", seed.Staged},
		{"untracked", seed.Untracked}, {"ignored", seed.Ignored},
	}
	seen := make(map[string]string)
	for _, group := range groups {
		for i, file := range group.files {
			path, err := safeRelative(file.Path)
			if err != nil || path == ".git" || strings.HasPrefix(path, ".git/") {
				return fmt.Errorf("invalid Git seed %s[%d] path %q", group.name, i, file.Path)
			}
			if previous, exists := seen[path]; exists {
				return fmt.Errorf("Git seed path %q appears in both %s and %s", path, previous, group.name)
			}
			seen[path] = group.name
			if (file.Content == nil) == (file.Digest == "") {
				return fmt.Errorf("Git seed %s[%d] must declare exactly one of content or digest", group.name, i)
			}
			if file.Digest != "" && !validSHA256(file.Digest) {
				return fmt.Errorf("Git seed %s[%d] has invalid digest", group.name, i)
			}
			if file.Mode&^os.ModePerm != 0 {
				return fmt.Errorf("Git seed %s[%d] has unsupported mode %#o", group.name, i, file.Mode)
			}
		}
	}
	return nil
}

func validSHA256(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func seedPaths(files []SeedFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return paths
}

func (w *Workspace) applySeedGroup(files []SeedFile) error {
	for _, file := range files {
		path, _ := safeRelative(file.Path)
		if file.Content == nil {
			if err := w.verifySeedFile(file); err != nil {
				return err
			}
			continue
		}
		mode := file.Mode
		if mode == 0 {
			mode = 0o644
		}
		parent := strings.TrimSuffix(path, "/"+filepathBase(path))
		if parent != path && parent != "" {
			if err := w.fixtureRoot.MkdirAll(parent, 0o700); err != nil {
				return fmt.Errorf("create seed parent for %q: %w", path, err)
			}
			if err := ensureRealDirectory(w.fixtureRoot, parent); err != nil {
				return err
			}
		}
		if err := safefs.WriteAtomic(w.fixtureRoot, path, file.Content, mode.Perm(), ".eval-seed-"); err != nil {
			return fmt.Errorf("write Git seed %q: %w", path, err)
		}
	}
	return nil
}

func filepathBase(path string) string {
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		return path[index+1:]
	}
	return path
}

func (w *Workspace) verifySeedFile(file SeedFile) error {
	path, err := safeRelative(file.Path)
	if err != nil {
		return err
	}
	data, err := safefs.ReadFileVerified(w.fixtureRoot, path, w.snapshotLimits.MaxFileBytes)
	if err != nil {
		return fmt.Errorf("read Git seed %q: %w", path, err)
	}
	expected := file.Digest
	if file.Content != nil {
		expected = digestRaw(file.Content)
	}
	if actual := digestRaw(data); actual != expected {
		return fmt.Errorf("Git seed %q digest mismatch: got %s, expected %s", path, actual, expected)
	}
	if file.Mode != 0 {
		info, err := w.fixtureRoot.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode().Perm() != file.Mode.Perm() {
			return fmt.Errorf("Git seed %q mode is %#o, expected %#o", path, info.Mode().Perm(), file.Mode.Perm())
		}
	}
	return nil
}

func digestRaw(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (w *Workspace) CaptureGitStatus(ctx context.Context) (GitStatusEvidence, error) {
	statusResult := w.Run(ctx, Command{
		ID:   "git.status",
		Argv: []string{"git", "status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=matching"},
	})
	if !statusResult.Successful() {
		return GitStatusEvidence{}, fmt.Errorf("git status failed: exit=%d %s %s", statusResult.ExitCode, statusResult.Error, statusResult.Stderr)
	}
	raw := []byte(statusResult.Stdout)
	entries, err := parsePorcelainV2(raw)
	if err != nil {
		return GitStatusEvidence{}, err
	}
	headResult := w.Run(ctx, Command{
		ID:   "git.head",
		Argv: []string{"git", "rev-parse", "--verify", "HEAD"},
	})
	if !headResult.Successful() {
		return GitStatusEvidence{}, fmt.Errorf("git HEAD inspection failed: exit=%d %s %s", headResult.ExitCode, headResult.Error, headResult.Stderr)
	}
	headFields := strings.Fields(headResult.Stdout)
	if len(headFields) != 1 || !validGitObjectID(headFields[0]) {
		return GitStatusEvidence{}, fmt.Errorf("git HEAD inspection returned an invalid object ID")
	}
	indexResult := w.Run(ctx, Command{
		ID:   "git.index",
		Argv: []string{"git", "ls-files", "--stage", "-z"},
	})
	if !indexResult.Successful() {
		return GitStatusEvidence{}, fmt.Errorf("git index inspection failed: exit=%d %s %s", indexResult.ExitCode, indexResult.Error, indexResult.Stderr)
	}
	indexRaw := []byte(indexResult.Stdout)
	evidence := GitStatusEvidence{
		Digest: digestRaw(raw), Raw: append([]byte(nil), raw...),
		Head: headFields[0], IndexDigest: digestRaw(indexRaw), IndexRaw: append([]byte(nil), indexRaw...),
		Entries: entries,
	}
	evidence.StateDigest = digestGitState(evidence.Head, evidence.Digest, evidence.IndexDigest)
	return evidence, nil
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func digestGitState(head, statusDigest, indexDigest string) string {
	// All fields are length-bounded hexadecimal values, but explicit NUL tags
	// also make the framing self-describing should their representation evolve.
	return digestRaw([]byte("head\x00" + head + "\x00status\x00" + statusDigest + "\x00index\x00" + indexDigest + "\x00"))
}

// CompareGitState verifies the immutable repository basis and every dirty
// record that existed before the candidate ran. It deliberately permits new
// worktree-only records because expected_diff owns the allowed-change policy.
func CompareGitState(before, after GitStatusEvidence) GitStateComparison {
	comparison := GitStateComparison{
		Complete: before.StateDigest != "" && after.StateDigest != "" &&
			before.Head != "" && after.Head != "" &&
			before.IndexDigest != "" && after.IndexDigest != "",
		HeadPreserved:  before.Head != "" && before.Head == after.Head,
		IndexPreserved: before.IndexDigest != "" && before.IndexDigest == after.IndexDigest,
	}
	afterByPath := make(map[string]GitStatusEntry, len(after.Entries))
	for _, entry := range after.Entries {
		afterByPath[entry.Path] = entry
	}
	comparison.InitialEntriesPreserved = true
	for _, entry := range before.Entries {
		observed, present := afterByPath[entry.Path]
		if !present || observed != entry {
			comparison.InitialEntriesPreserved = false
			break
		}
	}
	return comparison
}

func parsePorcelainV2(raw []byte) ([]GitStatusEntry, error) {
	records := bytes.Split(raw, []byte{0})
	entries := make([]GitStatusEntry, 0, len(records))
	for i := 0; i < len(records); i++ {
		record := string(records[i])
		if record == "" {
			continue
		}
		entry := GitStatusEntry{}
		switch {
		case strings.HasPrefix(record, "? "):
			entry.Kind, entry.Path = "untracked", strings.TrimPrefix(record, "? ")
		case strings.HasPrefix(record, "! "):
			entry.Kind, entry.Path = "ignored", strings.TrimPrefix(record, "! ")
		case strings.HasPrefix(record, "1 "):
			fields := strings.SplitN(record, " ", 9)
			if len(fields) != 9 || len(fields[1]) != 2 {
				return nil, fmt.Errorf("malformed porcelain-v2 ordinary record")
			}
			entry.Kind, entry.IndexStatus, entry.WorktreeStatus, entry.Path = "ordinary", fields[1][0], fields[1][1], fields[8]
		case strings.HasPrefix(record, "2 "):
			fields := strings.SplitN(record, " ", 10)
			if len(fields) != 10 || len(fields[1]) != 2 || i+1 >= len(records) {
				return nil, fmt.Errorf("malformed porcelain-v2 rename record")
			}
			entry.Kind, entry.IndexStatus, entry.WorktreeStatus, entry.Path = "rename", fields[1][0], fields[1][1], fields[9]
			i++
			entry.OriginalPath = string(records[i])
		case strings.HasPrefix(record, "u "):
			fields := strings.SplitN(record, " ", 11)
			if len(fields) != 11 || len(fields[1]) != 2 {
				return nil, fmt.Errorf("malformed porcelain-v2 unmerged record")
			}
			entry.Kind, entry.IndexStatus, entry.WorktreeStatus, entry.Path = "unmerged", fields[1][0], fields[1][1], fields[10]
		default:
			return nil, fmt.Errorf("unknown porcelain-v2 record kind")
		}
		path, err := safeRelative(entry.Path)
		if err != nil {
			return nil, fmt.Errorf("unsafe Git status path: %w", err)
		}
		entry.Path = path
		if entry.OriginalPath != "" {
			original, err := safeRelative(entry.OriginalPath)
			if err != nil {
				return nil, fmt.Errorf("unsafe Git status original path: %w", err)
			}
			entry.OriginalPath = original
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (w *Workspace) verifyGitSeed(ctx context.Context, seed GitSeed, status GitStatusEvidence) error {
	byPath := make(map[string]GitStatusEntry, len(status.Entries))
	for _, entry := range status.Entries {
		byPath[entry.Path] = entry
	}
	for _, file := range seed.Tracked {
		if err := w.verifySeedFile(file); err != nil {
			return err
		}
		if entry, dirty := byPath[file.Path]; dirty {
			return fmt.Errorf("tracked seed %q is not clean (%s %c%c)", file.Path, entry.Kind, entry.IndexStatus, entry.WorktreeStatus)
		}
		result := w.Run(ctx, Command{ID: "git.verify-tracked", Argv: []string{"git", "ls-files", "--error-unmatch", "--", file.Path}})
		if !result.Successful() {
			return fmt.Errorf("tracked seed %q is not tracked", file.Path)
		}
	}
	for _, file := range seed.Staged {
		if err := w.verifySeedFile(file); err != nil {
			return err
		}
		entry, ok := byPath[file.Path]
		if !ok || entry.Kind == "untracked" || entry.Kind == "ignored" || entry.IndexStatus == '.' || entry.WorktreeStatus != '.' {
			return fmt.Errorf("staged seed %q does not have an exact staged-only status", file.Path)
		}
	}
	for _, file := range seed.Untracked {
		if err := w.verifySeedFile(file); err != nil {
			return err
		}
		if entry, ok := byPath[file.Path]; !ok || entry.Kind != "untracked" {
			return fmt.Errorf("untracked seed %q is not reported as untracked", file.Path)
		}
	}
	for _, file := range seed.Ignored {
		if err := w.verifySeedFile(file); err != nil {
			return err
		}
		if entry, ok := byPath[file.Path]; !ok || entry.Kind != "ignored" {
			return fmt.Errorf("ignored seed %q is not reported as ignored", file.Path)
		}
		result := w.Run(ctx, Command{ID: "git.verify-ignored", Argv: []string{"git", "check-ignore", "-q", "--", file.Path}})
		if !result.Successful() {
			return fmt.Errorf("ignored seed %q is not ignored", file.Path)
		}
	}
	return nil
}
