package installer

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/joeldevz/skynex/internal/models"
)

// Destinations contains the filesystem roots touched by an installation plan.
// Keeping them injectable makes plans testable without changing runtime paths.
type Destinations struct {
	ClaudeDir         string
	ClaudeConfigFile  string
	OpencodeDir       string
	StateDir          string
	StateConfigFile   string
	StateLockFile     string
	OwnershipManifest string
}

// OperationKind identifies a coarse-grained Plan v1 operation.
type OperationKind string

const (
	InstallTarget     OperationKind = "install-target"
	CleanupDeprecated OperationKind = "cleanup-deprecated"
	WriteState        OperationKind = "write-state"
)

// Operation is one deterministic, non-executable installation operation.
type Operation struct {
	Kind        OperationKind
	PackageID   string
	Target      string
	Destination string
}

// Plan is the versioned, coarse-grained installation plan.
type Plan struct {
	Version      int
	Operations   []Operation
	destinations Destinations
}

// Build creates a deterministic Plan v1 and validates package/target choices.
func Build(req *models.InstallRequest, cat *models.Catalog, destinations Destinations) (*Plan, error) {
	if req == nil || cat == nil {
		return nil, fmt.Errorf("install request and catalog are required")
	}
	packages := append([]string(nil), req.Packages...)
	targets := append([]string(nil), req.Targets...)
	sort.Strings(packages)
	sort.Strings(targets)

	plan := &Plan{Version: 1, destinations: destinations}
	for _, packageID := range packages {
		pkg, ok := cat.Packages[packageID]
		if !ok {
			return nil, fmt.Errorf("unknown package: %s", packageID)
		}
		supported := make(map[string]bool, len(pkg.SupportedTargets))
		for _, target := range pkg.SupportedTargets {
			supported[target] = true
		}
		for _, target := range targets {
			if !supported[target] {
				return nil, fmt.Errorf("package %s does not support target %s", packageID, target)
			}
			plan.Operations = append(plan.Operations, Operation{
				Kind:        InstallTarget,
				PackageID:   packageID,
				Target:      target,
				Destination: destinationForTarget(target, destinations),
			})
		}
	}
	if req.CleanupDeprecated {
		plan.Operations = append(plan.Operations, Operation{Kind: CleanupDeprecated, Destination: destinations.StateDir})
	}
	plan.Operations = append(plan.Operations,
		Operation{Kind: WriteState, Destination: filepath.Join(destinations.StateDir, "skills.config.json")},
		Operation{Kind: WriteState, Destination: filepath.Join(destinations.StateDir, "skills.lock.json")},
	)
	return plan, nil
}

// NewPlan is an alias for Build for callers that prefer constructor semantics.
func NewPlan(req *models.InstallRequest, cat *models.Catalog, destinations Destinations) (*Plan, error) {
	return Build(req, cat, destinations)
}

func destinationForTarget(target string, destinations Destinations) string {
	switch target {
	case "claude":
		return destinations.ClaudeDir
	case "opencode":
		return destinations.OpencodeDir
	default:
		return ""
	}
}

// RenderText writes a stable human-readable representation of a plan.
func (p *Plan) RenderText(w io.Writer) error {
	if p == nil {
		return fmt.Errorf("plan is nil")
	}
	if _, err := fmt.Fprintf(w, "Plan v%d\n", p.Version); err != nil {
		return err
	}
	for i, operation := range p.Operations {
		if _, err := fmt.Fprintf(w, "%d. %s", i+1, operation.Kind); err != nil {
			return err
		}
		if operation.PackageID != "" {
			if _, err := fmt.Fprintf(w, " package=%s", operation.PackageID); err != nil {
				return err
			}
		}
		if operation.Target != "" {
			if _, err := fmt.Fprintf(w, " target=%s", operation.Target); err != nil {
				return err
			}
		}
		if operation.Destination != "" {
			if _, err := fmt.Fprintf(w, " destination=%s", operation.Destination); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return nil
}

// RenderText writes a plan without requiring callers to use a method value.
func RenderText(w io.Writer, p *Plan) error {
	return p.RenderText(w)
}
