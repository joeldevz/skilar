package runner

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/joeldevz/skynex/internal/eval/contracts"
)

const (
	// ProvenanceExtensionEffectiveToolchainsDigest binds each retained sample to
	// the executable closure which was resolved before any setup or model work.
	ProvenanceExtensionEffectiveToolchainsDigest = "x-effective-toolchains-digest"
	ProvenanceExtensionEffectiveConfigDigest     = "x-effective-config-digest"
	ProvenanceExtensionEffectiveAgentsDigest     = "x-effective-agents-digest"
)

// ExecutableIdentity is the minimum reproducible identity for an executable.
// SelectionPath is the canonical absolute lookup result (and may itself be a
// symlink for toolchains); Path is the fully resolved regular-file target that
// is actually executed.
type ExecutableIdentity struct {
	Declaration   string `json:"declaration"`
	SelectionPath string `json:"selection_path"`
	Path          string `json:"path"`
	ContentDigest string `json:"content_digest"`
}

type executableResolutionMode uint8

const (
	strictExecutableSnapshot executableResolutionMode = iota
	executableClosureMember
)

// ExecutableSnapshot records one executable selected from a validated PATH.
// The captured search path is deliberately private: artifacts retain its
// canonical digest, not an ambient environment dump.
type ExecutableSnapshot struct {
	identity   ExecutableIdentity
	searchPath string
}

func (s *ExecutableSnapshot) Path() string {
	if s == nil {
		return ""
	}
	return s.identity.Path
}

func (s *ExecutableSnapshot) ContentDigest() string {
	if s == nil {
		return ""
	}
	return s.identity.ContentDigest
}

func (s *ExecutableSnapshot) Identity() ExecutableIdentity {
	if s == nil {
		return ExecutableIdentity{}
	}
	return s.identity
}

// Revalidate fails when PATH or the selected path/content changes. Callers use
// this immediately around launch boundaries; it is detection, not an fexecve
// primitive, so same-UID mutation in the final syscall window remains outside
// the trusted-local authority model.
func (s *ExecutableSnapshot) Revalidate() error {
	if s == nil {
		return fmt.Errorf("executable snapshot is required")
	}
	if current, ok := os.LookupEnv("PATH"); !ok || current != s.searchPath {
		return fmt.Errorf("PATH drifted after executable resolution")
	}
	current, err := resolveExecutableIdentity(s.identity.Declaration, s.searchPath, strictExecutableSnapshot)
	if err != nil {
		return err
	}
	if current.SelectionPath != s.identity.SelectionPath || current.Path != s.identity.Path || current.ContentDigest != s.identity.ContentDigest {
		return fmt.Errorf("executable %q drifted: got %s (%s), expected %s (%s)",
			s.identity.Declaration, current.Path, current.ContentDigest, s.identity.Path, s.identity.ContentDigest)
	}
	return nil
}

// ResolveExecutableSnapshot resolves a declaration once using the current,
// validated PATH. The returned path is absolute and canonical, and its final
// component is an executable regular file rather than a symlink.
func ResolveExecutableSnapshot(declaration string) (*ExecutableSnapshot, error) {
	searchPath, ok := os.LookupEnv("PATH")
	if !ok {
		return nil, fmt.Errorf("PATH is required to resolve executables")
	}
	if err := ValidateExecutableSearchPath(searchPath); err != nil {
		return nil, err
	}
	identity, err := resolveExecutableIdentity(declaration, searchPath, strictExecutableSnapshot)
	if err != nil {
		return nil, err
	}
	return &ExecutableSnapshot{identity: identity, searchPath: searchPath}, nil
}

// ExecutableClosure is the immutable, canonical set of executable identities
// used by the evaluator-owned setup/oracle/fake-MCP paths for a selected run.
type ExecutableClosure struct {
	searchPath          string
	canonicalSearchPath []string
	identities          []ExecutableIdentity
	byDeclaration       map[string]ExecutableIdentity
	digest              string
}

type executableClosurePayload struct {
	Kind        string               `json:"kind"`
	GoRuntime   string               `json:"go_runtime"`
	SearchPath  []string             `json:"search_path"`
	Executables []ExecutableIdentity `json:"executables"`
}

// ResolveExecutableClosure includes every declared allowed executable, every
// setup/oracle/fake-MCP argv[0], implicit Git used for seeded fixtures, and any
// evaluator-owned extra declarations supplied by the caller.
func ResolveExecutableClosure(testCases []contracts.Case, extras ...string) (*ExecutableClosure, error) {
	searchPath, ok := os.LookupEnv("PATH")
	if !ok {
		return nil, fmt.Errorf("PATH is required to resolve the executable closure")
	}
	if err := ValidateExecutableSearchPath(searchPath); err != nil {
		return nil, err
	}
	declarations := make(map[string]struct{})
	for _, declaration := range extras {
		if declaration != "" {
			declarations[declaration] = struct{}{}
		}
	}
	for _, testCase := range testCases {
		for _, declaration := range executableDeclarationsForCase(testCase) {
			declarations[declaration] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(declarations))
	for declaration := range declarations {
		ordered = append(ordered, declaration)
	}
	sort.Strings(ordered)
	closure := &ExecutableClosure{
		searchPath:    searchPath,
		byDeclaration: make(map[string]ExecutableIdentity, len(ordered)),
	}
	var err error
	closure.canonicalSearchPath, err = canonicalSearchPath(searchPath)
	if err != nil {
		return nil, err
	}
	for _, declaration := range ordered {
		identity, resolveErr := resolveExecutableIdentity(declaration, searchPath, executableClosureMember)
		if resolveErr != nil {
			return nil, resolveErr
		}
		closure.identities = append(closure.identities, identity)
		closure.byDeclaration[declaration] = identity
	}
	closure.digest, err = contracts.CanonicalDigest(executableClosurePayload{
		Kind: "effective-executable-closure-v1", GoRuntime: runtime.Version(),
		SearchPath:  append([]string(nil), closure.canonicalSearchPath...),
		Executables: append([]ExecutableIdentity(nil), closure.identities...),
	})
	if err != nil {
		return nil, fmt.Errorf("digest executable closure: %w", err)
	}
	return closure, nil
}

func (c *ExecutableClosure) Digest() string {
	if c == nil {
		return ""
	}
	return c.digest
}

func (c *ExecutableClosure) PathFor(declaration string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("executable closure is required")
	}
	identity, ok := c.byDeclaration[declaration]
	if !ok {
		return "", fmt.Errorf("executable %q is absent from the effective closure", declaration)
	}
	return identity.Path, nil
}

// validateCaseCoverage is an execution preflight: an Engine must never fall
// back to ambient PATH just because it was given a closure resolved for a
// different set of cases.
func (c *ExecutableClosure) validateCaseCoverage(testCase contracts.Case) error {
	if c == nil {
		return nil
	}
	for _, declaration := range executableDeclarationsForCase(testCase) {
		if _, err := c.PathFor(declaration); err != nil {
			return err
		}
	}
	return nil
}

func executableDeclarationsForCase(testCase contracts.Case) []string {
	declarations := make(map[string]struct{})
	for _, declaration := range testCase.Security.AllowedExecutables {
		declarations[declaration] = struct{}{}
	}
	commands := append(append([]contracts.Command(nil), testCase.Setup.Commands...), testCase.Oracle.Commands...)
	for _, command := range commands {
		if len(command.Argv) != 0 {
			declarations[command.Argv[0]] = struct{}{}
		}
	}
	for _, fake := range testCase.ToolPolicy.FakeMCPs {
		if fake.Command != nil && len(fake.Command.Argv) != 0 {
			declarations[fake.Command.Argv[0]] = struct{}{}
		}
	}
	if testCase.Fixture.InitialGit {
		declarations["git"] = struct{}{}
	}
	ordered := make([]string, 0, len(declarations))
	for declaration := range declarations {
		ordered = append(ordered, declaration)
	}
	sort.Strings(ordered)
	return ordered
}

func (c *ExecutableClosure) Revalidate() error {
	if c == nil {
		return fmt.Errorf("executable closure is required")
	}
	if current, ok := os.LookupEnv("PATH"); !ok || current != c.searchPath {
		return fmt.Errorf("PATH drifted after executable-closure resolution")
	}
	canonical, err := canonicalSearchPath(c.searchPath)
	if err != nil {
		return fmt.Errorf("revalidate canonical PATH: %w", err)
	}
	if len(canonical) != len(c.canonicalSearchPath) {
		return fmt.Errorf("canonical PATH drifted after executable-closure resolution")
	}
	for index := range canonical {
		if canonical[index] != c.canonicalSearchPath[index] {
			return fmt.Errorf("canonical PATH segment %d drifted from %q to %q", index, c.canonicalSearchPath[index], canonical[index])
		}
	}
	for _, expected := range c.identities {
		current, err := resolveExecutableIdentity(expected.Declaration, c.searchPath, executableClosureMember)
		if err != nil {
			return fmt.Errorf("revalidate executable %q: %w", expected.Declaration, err)
		}
		if current.SelectionPath != expected.SelectionPath || current.Path != expected.Path || current.ContentDigest != expected.ContentDigest {
			return fmt.Errorf("executable %q drifted: got %s (%s), expected %s (%s)",
				expected.Declaration, current.Path, current.ContentDigest, expected.Path, expected.ContentDigest)
		}
	}
	return nil
}

// ValidateExecutableSearchPath rejects the current-directory semantics of an
// empty PATH element and every relative search root.
func ValidateExecutableSearchPath(value string) error {
	if value == "" {
		return fmt.Errorf("PATH must not be empty")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("PATH contains NUL")
	}
	for index, segment := range strings.Split(value, string(os.PathListSeparator)) {
		if segment == "" {
			return fmt.Errorf("PATH segment %d is empty", index)
		}
		if !filepath.IsAbs(segment) {
			return fmt.Errorf("PATH segment %d is relative: %q", index, segment)
		}
	}
	return nil
}

func canonicalSearchPath(value string) ([]string, error) {
	parts := filepath.SplitList(value)
	result := make([]string, len(parts))
	for index, part := range parts {
		canonical, err := filepath.EvalSymlinks(filepath.Clean(part))
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("canonicalize PATH segment %q: %w", part, err)
			}
			canonical = filepath.Clean(part)
		}
		if !filepath.IsAbs(canonical) {
			return nil, fmt.Errorf("canonical PATH segment is not absolute: %q", canonical)
		}
		result[index] = canonical
	}
	return result, nil
}

func resolveExecutableIdentity(declaration, searchPath string, mode executableResolutionMode) (ExecutableIdentity, error) {
	if declaration == "" || strings.TrimSpace(declaration) != declaration || strings.IndexByte(declaration, 0) >= 0 {
		return ExecutableIdentity{}, fmt.Errorf("invalid executable declaration %q", declaration)
	}
	if err := ValidateExecutableSearchPath(searchPath); err != nil {
		return ExecutableIdentity{}, err
	}
	var selected string
	if filepath.IsAbs(declaration) {
		selected = declaration
	} else if strings.ContainsAny(declaration, `/\\`) {
		var err error
		selected, err = filepath.Abs(declaration)
		if err != nil {
			return ExecutableIdentity{}, fmt.Errorf("make executable path absolute %q: %w", declaration, err)
		}
	} else {
		// exec.LookPath is safe here because PATH was captured and is checked
		// before and after lookup. Execution uses the returned absolute path.
		if current, ok := os.LookupEnv("PATH"); !ok || current != searchPath {
			return ExecutableIdentity{}, fmt.Errorf("PATH changed while resolving executable %q", declaration)
		}
		resolved, err := exec.LookPath(declaration)
		if err != nil {
			return ExecutableIdentity{}, fmt.Errorf("resolve executable %q: %w", declaration, err)
		}
		if current, ok := os.LookupEnv("PATH"); !ok || current != searchPath {
			return ExecutableIdentity{}, fmt.Errorf("PATH changed while resolving executable %q", declaration)
		}
		selected = resolved
	}
	selection, target, err := canonicalRegularExecutable(selected, mode == executableClosureMember)
	if err != nil {
		return ExecutableIdentity{}, fmt.Errorf("resolve executable %q: %w", declaration, err)
	}
	digest, err := stableRegularFileDigest(target)
	if err != nil {
		return ExecutableIdentity{}, fmt.Errorf("digest executable %q: %w", declaration, err)
	}
	if mode == executableClosureMember {
		if err := requireNativeExecutable(target); err != nil {
			return ExecutableIdentity{}, fmt.Errorf("resolve executable %q: %w", declaration, err)
		}
	}
	return ExecutableIdentity{
		Declaration: declaration, SelectionPath: selection, Path: target, ContentDigest: digest,
	}, nil
}

func canonicalRegularExecutable(path string, allowFinalSymlink bool) (selection string, target string, returnErr error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	abs = filepath.Clean(abs)
	canonicalParent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", "", err
	}
	selection = filepath.Join(canonicalParent, filepath.Base(abs))
	before, err := os.Lstat(selection)
	if err != nil {
		return "", "", err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		if !allowFinalSymlink {
			return "", "", fmt.Errorf("%q is a symlink", selection)
		}
		linkBefore, err := os.Readlink(selection)
		if err != nil {
			return "", "", err
		}
		target, err = filepath.EvalSymlinks(selection)
		if err != nil {
			return "", "", err
		}
		target, err = filepath.Abs(target)
		if err != nil {
			return "", "", err
		}
		target = filepath.Clean(target)
		after, err := os.Lstat(selection)
		if err != nil {
			return "", "", err
		}
		linkAfter, err := os.Readlink(selection)
		if err != nil {
			return "", "", err
		}
		if after.Mode()&os.ModeSymlink == 0 || !os.SameFile(before, after) || linkBefore != linkAfter {
			return "", "", fmt.Errorf("%q changed while resolving its symlink target", selection)
		}
	} else {
		if !before.Mode().IsRegular() {
			return "", "", fmt.Errorf("%q is not a regular file", selection)
		}
		target = selection
	}
	targetInfo, err := os.Lstat(target)
	if err != nil {
		return "", "", err
	}
	if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.Mode().IsRegular() {
		return "", "", fmt.Errorf("%q does not resolve to a non-symlink regular file", selection)
	}
	if runtime.GOOS != "windows" && targetInfo.Mode().Perm()&0o111 == 0 {
		return "", "", fmt.Errorf("%q is not executable", target)
	}
	return selection, target, nil
}

// requireNativeExecutable rejects scripts and wrappers because hashing only
// their bytes would not bind the interpreter (or its own executable closure).
// The evaluator's closure currently supports native ELF, PE, Mach-O and fat
// Mach-O executables; a future closure version can recursively bind shebangs.
func requireNativeExecutable(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var magic [4]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return fmt.Errorf("%q is not a supported native executable: %w", path, err)
	}
	native := string(magic[:]) == "\x7fELF" || string(magic[:2]) == "MZ"
	if !native {
		switch magic {
		case [4]byte{0xce, 0xfa, 0xed, 0xfe}, [4]byte{0xcf, 0xfa, 0xed, 0xfe},
			[4]byte{0xfe, 0xed, 0xfa, 0xce}, [4]byte{0xfe, 0xed, 0xfa, 0xcf},
			[4]byte{0xca, 0xfe, 0xba, 0xbe}, [4]byte{0xbe, 0xba, 0xfe, 0xca},
			[4]byte{0xca, 0xfe, 0xba, 0xbf}, [4]byte{0xbf, 0xba, 0xfe, 0xca}:
			native = true
		}
	}
	if !native {
		return fmt.Errorf("script/wrapper executable %q is unsupported because its interpreter closure is not pinned", path)
	}
	return nil
}

func stableRegularFileDigest(path string) (string, error) {
	const maxDigestBytes = int64(1 << 30)
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a non-symlink regular file", path)
	}
	if before.Size() < 0 || before.Size() > maxDigestBytes {
		return "", fmt.Errorf("%q exceeds the %d-byte digest limit", path, maxDigestBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	openedBefore, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, openedBefore) {
		return "", fmt.Errorf("%q changed while opening", path)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maxDigestBytes+1))
	if err != nil {
		return "", err
	}
	openedAfter, err := file.Stat()
	if err != nil {
		return "", err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if written != openedBefore.Size() || openedAfter.Size() != openedBefore.Size() ||
		!openedAfter.ModTime().Equal(openedBefore.ModTime()) || !os.SameFile(openedBefore, after) ||
		after.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%q changed while hashing", path)
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}
