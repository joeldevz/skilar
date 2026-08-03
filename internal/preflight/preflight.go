package preflight

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/joeldevz/skynex/internal/models"
	"github.com/joeldevz/skynex/internal/paths"
	"github.com/joeldevz/skynex/internal/safefs"
)

// Run executes all preflight validations.
func Run(req *models.InstallRequest, cat *models.Catalog) []*models.ValidationIssue {
	return RunWithOptions(req, cat, Options{})
}

// Options controls side effects performed by preflight.
type Options struct {
	ReadOnly bool
}

// RunWithOptions executes preflight validations, optionally without filesystem mutations.
func RunWithOptions(req *models.InstallRequest, cat *models.Catalog, options Options) []*models.ValidationIssue {
	var issues []*models.ValidationIssue

	// Global: git must exist
	if _, err := exec.LookPath("git"); err != nil {
		issues = append(issues, &models.ValidationIssue{
			Level:   "error",
			Message: "git not found in PATH",
			FixHint: "Install git: https://git-scm.com/downloads",
		})
	}

	for _, pkgID := range req.Packages {
		pkg, ok := cat.Packages[pkgID]
		if !ok {
			issues = append(issues, &models.ValidationIssue{
				Level:     "error",
				PackageID: pkgID,
				Message:   fmt.Sprintf("unknown package: %s", pkgID),
			})
			continue
		}

		// Validate targets
		supported := make(map[string]bool)
		for _, t := range pkg.SupportedTargets {
			supported[t] = true
		}
		for _, target := range req.Targets {
			if !supported[target] {
				issues = append(issues, &models.ValidationIssue{
					Level:     "error",
					PackageID: pkgID,
					Target:    target,
					Message:   fmt.Sprintf("package %s does not support target %s", pkgID, target),
					FixHint:   fmt.Sprintf("Supported targets: %v", pkg.SupportedTargets),
				})
			}
		}

		// Neurox requirement — skip if neurox is also being installed in this session
		if pkg.RequiresNeurox {
			neuroxAlsoInstalling := false
			for _, p := range req.Packages {
				if p == "neurox" {
					neuroxAlsoInstalling = true
					break
				}
			}
			if !neuroxAlsoInstalling {
				if _, err := exec.LookPath("neurox"); err != nil {
					issues = append(issues, &models.ValidationIssue{
						Level:     "warning",
						PackageID: pkgID,
						Message:   "neurox not found in PATH (recommended for this package)",
						FixHint:   "Install neurox from https://github.com/joeldevz/neurox",
					})
				}
			}
		}

		// Target-specific checks
		for _, target := range req.Targets {
			switch {
			case pkgID == "skills" && target == "opencode":
				if _, err := exec.LookPath("bun"); err != nil {
					if _, err := exec.LookPath("npm"); err != nil {
						issues = append(issues, &models.ValidationIssue{
							Level:     "error",
							PackageID: pkgID,
							Target:    target,
							Message:   "neither bun nor npm found in PATH",
							FixHint:   "Install bun (https://bun.sh) or npm",
						})
					}
				}
				if options.ReadOnly {
					err, warning := checkWritable(paths.OpencodeDir(), true)
					if err != nil {
						issues = append(issues, &models.ValidationIssue{Level: "error", PackageID: pkgID, Target: target, Message: fmt.Sprintf("cannot write to %s", paths.OpencodeDir()), FixHint: err.Error()})
					}
					if warning != "" {
						issues = append(issues, &models.ValidationIssue{Level: "warning", PackageID: pkgID, Target: target, Message: warning})
					}
				}
			case pkgID == "skills" && target == "claude":
				claudeDir := paths.ClaudeDir()
				err, warning := checkWritable(claudeDir, options.ReadOnly)
				if err != nil {
					issues = append(issues, &models.ValidationIssue{
						Level:     "error",
						PackageID: pkgID,
						Target:    target,
						Message:   fmt.Sprintf("cannot write to %s", claudeDir),
						FixHint:   "Check permissions on " + claudeDir,
					})
				}
				if warning != "" {
					issues = append(issues, &models.ValidationIssue{Level: "warning", PackageID: pkgID, Target: target, Message: warning})
				}
			}
		}
	}
	if req.StateDir != "" {
		err, warning := checkWritable(req.StateDir, options.ReadOnly)
		if err != nil {
			issues = append(issues, &models.ValidationIssue{Level: "error", Message: fmt.Sprintf("cannot write to %s", req.StateDir), FixHint: err.Error()})
		}
		if warning != "" {
			issues = append(issues, &models.ValidationIssue{Level: "warning", Message: warning})
		}
	}

	return issues
}

// HasErrors returns true if any issue is an error.
func HasErrors(issues []*models.ValidationIssue) bool {
	for _, i := range issues {
		if i.Level == "error" {
			return true
		}
	}
	return false
}

// PrintIssues displays validation issues to stderr.
func PrintIssues(issues []*models.ValidationIssue, writers ...io.Writer) {
	out := io.Writer(os.Stderr)
	if len(writers) > 0 && writers[0] != nil {
		out = writers[0]
	}
	fmt.Fprintln(out, "\nPreflight validation:")
	for _, i := range issues {
		prefix := ""
		if i.PackageID != "" {
			prefix += "[" + i.PackageID + "]"
		}
		if i.Target != "" {
			prefix += "[" + i.Target + "]"
		}
		if prefix != "" {
			prefix += " "
		}
		fmt.Fprintf(out, "  %s%s: %s\n", prefix, i.Level, i.Message)
		if i.FixHint != "" {
			fmt.Fprintf(out, "    Fix: %s\n", i.FixHint)
		}
	}
}

func ensureWritable(dir string) error {
	cleaned := filepath.Clean(dir)
	if cleaned != dir {
		return fmt.Errorf("writable directory %q is not a clean path", dir)
	}

	if err := validateAncestors(cleaned); err != nil {
		return err
	}

	root, err := safefs.OpenOrCreate(cleaned, 0o700)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := safefs.ChmodRoot(root, 0o700); err != nil {
		return err
	}

	// Use a unique name so a user's existing file is never truncated or removed.
	tmp, f, err := safefs.TempFile(root, ".skynex-preflight-check-")
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	closeErr := f.Close()
	removeErr := root.Remove(tmp)
	if closeErr != nil || removeErr != nil {
		return errors.Join(closeErr, removeErr)
	}
	return nil
}

func checkWritable(dir string, readOnly bool) (error, string) {
	cleaned := filepath.Clean(dir)
	if cleaned != dir {
		return fmt.Errorf("writable directory %q is not a clean path", dir), ""
	}
	if err := validateAncestors(cleaned); err != nil {
		return err, ""
	}
	info, err := os.Lstat(cleaned)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("path is not a directory: %s", cleaned), ""
		}
		if readOnly {
			return nil, fmt.Sprintf("writability not proven in read-only mode: %s", cleaned)
		}
	}
	if !readOnly {
		return ensureWritable(dir), ""
	}
	return nil, fmt.Sprintf("writability not proven in read-only mode (directory does not exist): %s", cleaned)
}

// validateAncestors rejects symlinks in every existing component of path.
// Missing components are allowed so callers can create a new directory tree.
func validateAncestors(path string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("path component is a symlink: %s", current)
			}
		case os.IsNotExist(err):
			// The missing suffix is permitted; continue checking its ancestors.
		default:
			return err
		}

		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}
