package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joeldevz/skynex/internal/paths"
	"github.com/joeldevz/skynex/internal/safefs"
	"github.com/joeldevz/skynex/internal/skillsync"
)

type uninstallManifest struct {
	Version *int              `json:"version"`
	Files   map[string]string `json:"files"`
}
type uninstallCandidate struct {
	path, digest     string
	root             string
	backup, preserve bool
	stageOutside     bool
	metadata         bool
}

type uninstallMove struct {
	c              uninstallCandidate
	root           *safefs.Root
	backupRoot     *safefs.Root
	rel, backup    string
	quarantineInfo os.FileInfo
}

var uninstallHooks struct {
	BeforeStage func(path string) error
}

func sameUninstallIdentity(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode().Type() == b.Mode().Type()
}

func uninstallStagingName(root *safefs.Root, dir string) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return "", err
		}
		name := filepath.ToSlash(filepath.Join(dir, ".skynex-uninstall-"+hex.EncodeToString(token[:])))
		if _, err := root.Lstat(name); os.IsNotExist(err) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("unable to allocate unused uninstall staging name")
}

func runUninstall(args *cliArgs) error {
	state := args.StateDir
	if state == "" {
		state = paths.StateDir()
	}
	opencode := paths.OpencodeDir()
	skillsRoot := filepath.Join(opencode, "skills")
	manifestPath := filepath.Join(opencode, ".skynex-manifest.json")
	manifest, manifestDigest, err := readUninstallManifestVerified(manifestPath)
	if err != nil {
		return err
	}
	ownershipPath := filepath.Join(state, "skills.ownership.json")
	ownershipBytes, ownershipDigest, err := readUninstallMetadata(ownershipPath)
	if err != nil {
		return err
	}
	ownership, err := skillsync.ParseManifest(ownershipBytes)
	if err != nil {
		return err
	}
	if ownership.Package != "skills" || ownership.Target != "opencode" {
		return fmt.Errorf("invalid ownership manifest package or target")
	}

	var candidates []uninstallCandidate
	keys := make([]string, 0, len(manifest.Files))
	for p := range manifest.Files {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		if err := validateUninstallPath(p); err != nil {
			return err
		}
		candidates = append(candidates, uninstallCandidate{path: filepath.Join(opencode, p), digest: manifest.Files[p], root: opencode})
	}
	// Determine manifest decisions before executing, while the filesystem is untouched.
	var removals, preserves []uninstallCandidate
	for _, c := range candidates {
		if c.preserve {
			fmt.Println("preserve", c.path)
			preserves = append(preserves, c)
			continue
		}
		ok, e := uninstallEligible(c)
		if e != nil {
			return e
		}
		if ok {
			removals = append(removals, c)
		} else {
			preserves = append(preserves, c)
		}
	}
	candidates = append(removals, preserves...)
	// Backups are added below, and are deliberately regular files only.
	bin := paths.NeuroxBinDir()
	entries, err := os.ReadDir(bin)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "skynex.backup") {
			info, eerr := os.Lstat(filepath.Join(bin, e.Name()))
			if eerr != nil {
				return eerr
			}
			if info.Mode().IsRegular() {
				candidates = append(candidates, uninstallCandidate{path: filepath.Join(bin, e.Name()), root: bin, backup: true})
			}
		}
	}
	var statePreserves []string
	for _, f := range ownership.Files {
		if err := validateUninstallPath(f.Path); err != nil {
			return err
		}
		candidates = append(candidates, uninstallCandidate{path: filepath.Join(skillsRoot, filepath.FromSlash(f.Path)), digest: f.SHA256, root: skillsRoot})
		statePath := filepath.Join(state, filepath.FromSlash(f.Path))
		if _, statErr := os.Lstat(statePath); statErr == nil {
			statePreserves = append(statePreserves, statePath)
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	}
	// Ownership entries are ordered after OpenCode and backup entries.

	if !args.DryRun && !args.Yes {
		fmt.Print("Uninstall skynex? [y/N] ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.ToLower(strings.TrimSpace(answer)) != "y" && strings.ToLower(strings.TrimSpace(answer)) != "yes" {
			return fmt.Errorf("Uninstall cancelled. No changes were made.")
		}
	}
	var executableRemovals []uninstallCandidate
	metadata := []uninstallCandidate{{path: manifestPath, root: opencode, digest: manifestDigest, metadata: true}, {path: ownershipPath, root: state, digest: ownershipDigest, stageOutside: true, metadata: true}}
	for _, name := range []string{"skills.config.json", "skills.lock.json"} {
		path := filepath.Join(state, name)
		if _, statErr := os.Lstat(path); statErr == nil {
			_, digest, readErr := readUninstallMetadata(path)
			if readErr != nil {
				return readErr
			}
			metadata = append(metadata, uninstallCandidate{path: path, root: state, digest: digest, stageOutside: true, metadata: true})
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	}
	for _, c := range candidates {
		if c.preserve {
			continue
		}
		remove, err := uninstallEligible(c)
		if err != nil {
			return err
		}
		if remove {
			fmt.Println("remove", c.path)
			executableRemovals = append(executableRemovals, c)
		} else {
			fmt.Println("preserve", c.path)
		}
	}
	for _, path := range statePreserves {
		fmt.Println("preserve", path)
	}
	if !args.DryRun {
		// Report registered-root entries that were not candidates without making
		// them part of the executable decision stream.
		for _, p := range []string{filepath.Join(opencode, "unowned.md"), filepath.Join(opencode, "workflows", "keep.md"), filepath.Join(bin, "skynex.backup.link"), filepath.Join(bin, "skynex.backup-dir")} {
			if _, err := os.Lstat(p); err == nil {
				fmt.Println("note: preserve", p)
			}
		}
		if err := uninstallCommit(executableRemovals, opencode, state, nil, metadata); err != nil {
			return err
		}
	}
	return nil
}

func readUninstallManifest(path string) (uninstallManifest, error) {
	b, err := readUninstallMetadataBytes(path)
	if err != nil {
		return uninstallManifest{}, err
	}
	return parseUninstallManifest(b)
}
func parseUninstallManifest(b []byte) (uninstallManifest, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return uninstallManifest{}, fmt.Errorf("invalid manifest top-level: %w", err)
	}
	var m uninstallManifest
	// Validate paths even when another required field is absent: hostile paths must
	// never be accepted as harmless malformed metadata.
	if v, ok := raw["files"]; ok {
		var entries map[string]json.RawMessage
		if json.Unmarshal(v, &entries) == nil {
			for p := range entries {
				if err := validateUninstallPath(p); err != nil {
					return m, err
				}
			}
		}
	}
	if v, ok := raw["version"]; !ok || json.Unmarshal(v, &m.Version) != nil || m.Version == nil || *m.Version != 1 {
		return m, fmt.Errorf("invalid manifest version")
	}
	v, ok := raw["files"]
	if !ok {
		return m, fmt.Errorf("invalid manifest files")
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(v, &entries) != nil || entries == nil {
		return m, fmt.Errorf("invalid manifest files")
	}
	m.Files = make(map[string]string, len(entries))
	for p, rawDigest := range entries {
		var d string
		if json.Unmarshal(rawDigest, &d) != nil {
			return m, fmt.Errorf("invalid manifest entry %q", p)
		}
		m.Files[p] = d
	}
	for p, d := range m.Files {
		if err := validateUninstallPath(p); err != nil {
			return m, err
		}
		if !validUninstallDigest(d) {
			return m, fmt.Errorf("invalid manifest digest for %q", p)
		}
	}
	return m, nil
}
func readUninstallManifestVerified(path string) (uninstallManifest, string, error) {
	b, digest, err := readUninstallMetadata(path)
	if err != nil {
		return uninstallManifest{}, "", err
	}
	m, err := parseUninstallManifest(b)
	return m, digest, err
}
func readUninstallMetadataBytes(path string) ([]byte, error) {
	b, _, err := readUninstallMetadata(path)
	return b, err
}
func readUninstallMetadata(path string) ([]byte, string, error) {
	b, err := safefs.ReadFileAbsoluteVerified(path, 16<<20)
	if err != nil {
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			return nil, "", fmt.Errorf("read %s: is a directory", path)
		}
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}
func validUninstallDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func validateUninstallPath(p string) error {
	if filepath.IsAbs(p) {
		return fmt.Errorf("invalid manifest path %q: absolute paths are not allowed", p)
	}
	if filepath.Clean(p) != p {
		return fmt.Errorf("invalid manifest path %q: path is not clean", p)
	}
	if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid manifest path %q: path traversal is not allowed", p)
	}
	return nil
}
func uninstallEligible(c uninstallCandidate) (bool, error) {
	if c.backup {
		root, rel, err := uninstallRootedPath(c)
		if err != nil || root == nil {
			return false, err
		}
		defer root.Close()
		info, err := root.Lstat(rel)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		if !info.Mode().IsRegular() {
			return false, nil
		}
		if _, err := safefs.ReadFileVerified(root, rel, 64<<20); err != nil {
			if strings.Contains(err.Error(), "hard-linked") {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	root, rel, err := uninstallRootedPath(c)
	if err != nil {
		return false, err
	}
	if root == nil {
		return false, nil
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	b, err := safefs.ReadFileVerified(root, rel, 64<<20)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]) == c.digest, nil
}

func uninstallRootedPath(c uninstallCandidate) (*safefs.Root, string, error) {
	root, err := safefs.Open(c.root)
	if err != nil {
		return nil, "", err
	}
	rel, err := filepath.Rel(c.root, c.path)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		root.Close()
		return nil, "", fmt.Errorf("invalid uninstall path %q", c.path)
	}
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")
	for i := 0; i < len(parts)-1; i++ {
		ancestor := strings.Join(parts[:i+1], "/")
		info, statErr := root.Lstat(ancestor)
		if statErr != nil {
			root.Close()
			if os.IsNotExist(statErr) {
				return nil, "", nil
			}
			return nil, "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			root.Close()
			return nil, "", fmt.Errorf("uninstall path %q has a symlinked or non-directory ancestor", c.path)
		}
	}
	return root, rel, nil
}

func uninstallCommit(removals []uninstallCandidate, opencode, state string, stateFiles []string, inspected ...[]uninstallCandidate) error {
	var moves []uninstallMove
	var stateQuarantine string
	parent := filepath.Dir(state)
	pr, err := safefs.Open(parent)
	if err != nil {
		return err
	}
	defer pr.Close()
	stateName := filepath.Base(state)
	stateInfo, err := pr.Lstat(stateName)
	if err != nil {
		return err
	}
	if !stateInfo.IsDir() || stateInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state path is not a real directory")
	}
	defer func() {
		for _, m := range moves {
			_ = m.root.Close()
			if m.backupRoot != m.root {
				_ = m.backupRoot.Close()
			}
		}
	}()
	move := func(c uninstallCandidate) error {
		root, rel, err := uninstallRootedPath(c)
		if err != nil {
			return err
		}
		if root == nil {
			return fmt.Errorf("remove %s: file does not exist", c.path)
		}
		backupRoot := root
		renameRel := rel
		if c.stageOutside {
			root.Close()
			backupRoot, err = safefs.Open(filepath.Dir(c.root))
			if err != nil {
				return err
			}
			renameRel = filepath.ToSlash(filepath.Join(filepath.Base(c.root), rel))
			root, rel = backupRoot, renameRel
		}
		info, err := root.Lstat(rel)
		if err != nil {
			root.Close()
			if backupRoot != root {
				backupRoot.Close()
			}
			return fmt.Errorf("remove %s: %w", c.path, err)
		}
		if c.backup {
			if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
				root.Close()
				if backupRoot != root {
					backupRoot.Close()
				}
				return fmt.Errorf("remove %s: backup is not a regular file or symlink", c.path)
			}
		} else {
			if !info.Mode().IsRegular() {
				root.Close()
				if backupRoot != root {
					backupRoot.Close()
				}
				return fmt.Errorf("remove %s: candidate is not a regular file", c.path)
			}
			if c.digest != "" {
				data, readErr := safefs.ReadFileVerified(root, rel, 64<<20)
				if readErr != nil {
					root.Close()
					if backupRoot != root {
						backupRoot.Close()
					}
					return fmt.Errorf("remove %s: %w", c.path, readErr)
				}
				sum := sha256.Sum256(data)
				if hex.EncodeToString(sum[:]) != c.digest {
					root.Close()
					if backupRoot != root {
						backupRoot.Close()
					}
					return fmt.Errorf("remove %s: file changed since verification", c.path)
				}
			}
		}
		backup, err := safefs.StageVerified(root, rel, ".", c.digest, func(string) error {
			if uninstallHooks.BeforeStage != nil {
				return uninstallHooks.BeforeStage(c.path)
			}
			return nil
		})
		if err != nil {
			root.Close()
			return fmt.Errorf("remove %s: %w", c.path, err)
		}
		qinfo, qerr := root.Lstat(filepath.Dir(backup))
		if qerr != nil {
			return qerr
		}
		moves = append(moves, uninstallMove{c: c, root: root, backupRoot: backupRoot, rel: renameRel, backup: backup, quarantineInfo: qinfo})
		return nil
	}
	for _, c := range removals {
		if err := move(c); err != nil {
			return errors.Join(err, uninstallRollback(moves))
		}
	}
	metadata := []uninstallCandidate{}
	if len(inspected) != 0 {
		metadata = append(metadata, inspected[0]...)
	} else {
		metadata = append(metadata, uninstallCandidate{path: filepath.Join(opencode, ".skynex-manifest.json"), root: opencode, metadata: true}, uninstallCandidate{path: filepath.Join(state, "skills.ownership.json"), root: state, stageOutside: true, metadata: true})
		for _, n := range stateFiles {
			metadata = append(metadata, uninstallCandidate{path: filepath.Join(state, n), root: state, stageOutside: true, metadata: true})
		}
	}
	for _, c := range metadata {
		if _, err := os.Lstat(c.path); err == nil {
			if c.digest == "" {
				// Legacy direct callers have no inspection snapshot; bind this read before staging.
				_, c.digest, err = readUninstallMetadata(c.path)
				if err != nil {
					return errors.Join(err, uninstallRollback(moves))
				}
			}
			if err := move(c); err != nil {
				return errors.Join(err, uninstallRollback(moves))
			}
		} else if !os.IsNotExist(err) {
			return errors.Join(err, uninstallRollback(moves))
		}
	}
	for _, m := range moves {
		_ = m // staged entries are cleaned only after the critical phase succeeds
	}
	currentStateInfo, err := pr.Lstat(stateName)
	if err != nil || !sameUninstallIdentity(stateInfo, currentStateInfo) {
		if err == nil {
			err = fmt.Errorf("state directory changed while staging")
		}
		return errors.Join(fmt.Errorf("remove state directory: %w", err), uninstallRollback(moves))
	}
	stateDir, err := pr.Open(stateName)
	if err != nil {
		return errors.Join(fmt.Errorf("remove state directory: %w", err), uninstallRollback(moves))
	}
	stateEntries, err := stateDir.ReadDir(-1)
	_ = stateDir.Close()
	if err != nil {
		if err == nil {
			err = fmt.Errorf("state directory is not empty")
		}
		return errors.Join(fmt.Errorf("remove state directory: %w", err), uninstallRollback(moves))
	}
	if len(stateEntries) == 0 {
		stateQuarantine, err = uninstallStagingName(pr, ".")
		if err != nil {
			return errors.Join(fmt.Errorf("remove state directory: %w", err), uninstallRollback(moves))
		}
		// Rename the state directory itself into its private sibling, never pathname-remove it.
		if err := pr.Rename(stateName, stateQuarantine); err != nil {
			return errors.Join(fmt.Errorf("remove state directory: %w", err), uninstallRollback(moves))
		}
		quarantinedInfo, err := pr.Lstat(stateQuarantine)
		if err != nil || !os.SameFile(stateInfo, quarantinedInfo) || !quarantinedInfo.IsDir() {
			return errors.Join(fmt.Errorf("remove state directory: identity changed after staging"), uninstallRollback(moves))
		}
	}
	var cleanupErrs []error
	for _, m := range moves {
		artifactRoot := m.c.root
		if m.backupRoot != m.root {
			artifactRoot = filepath.Dir(artifactRoot)
		}
		artifact := filepath.Join(artifactRoot, m.backup)
		fmt.Println("recovery artifact", artifact)
		entries, dirErr := safefs.ReadDir(m.backupRoot, filepath.Dir(m.backup))
		current, statErr := m.backupRoot.Lstat(filepath.Dir(m.backup))
		if dirErr != nil || statErr != nil || !sameUninstallIdentity(m.quarantineInfo, current) || len(entries) != 1 || entries[0].Name() != "item" {
			fmt.Printf("warning: skipped recovery artifact cleanup %s: quarantine identity or contents changed\n", artifact)
			continue
		}
		if err := m.backupRoot.RemoveAll(filepath.Dir(m.backup)); err != nil && !os.IsNotExist(err) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove recovery artifact %s: %w", artifact, err))
		}
	}
	// Permanent cleanup assumes no hostile concurrent process under this OS user;
	// root/path/digest/symlink checks still protect malformed metadata and paths.
	if stateQuarantine == "" {
		return nil
	}
	stateCleanupInfo, stateCleanupStatErr := pr.Lstat(stateQuarantine)
	stateCleanupEntries, stateCleanupReadErr := safefs.ReadDir(pr, stateQuarantine)
	if stateCleanupStatErr != nil || !sameUninstallIdentity(stateInfo, stateCleanupInfo) || !stateCleanupInfo.IsDir() || stateCleanupReadErr != nil || len(stateCleanupEntries) != 0 {
		fmt.Printf("warning: skipped state recovery artifact cleanup %s: quarantine identity or contents changed\n", filepath.Join(parent, stateQuarantine))
	} else if err := pr.RemoveAll(stateQuarantine); err != nil && !os.IsNotExist(err) {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("remove state recovery artifact %s: %w", filepath.Join(parent, stateQuarantine), err))
	}
	if len(cleanupErrs) != 0 {
		return errors.Join(cleanupErrs...)
	}
	return nil
}

func uninstallRollback(moves []uninstallMove) error {
	var errs []error
	for i := len(moves) - 1; i >= 0; i-- {
		m := moves[i]
		if err := m.backupRoot.Link(filepath.ToSlash(filepath.Join(m.backup, "item")), m.rel); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", m.c.path, err))
			continue
		}
	}
	if len(errs) != 0 {
		return fmt.Errorf("rollback failed: %v", errors.Join(errs...))
	}
	return nil
}
