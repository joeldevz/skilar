package main

import (
	"fmt"
	"io"
	"os"

	"github.com/joeldevz/skynex/internal/adapters"
	"github.com/joeldevz/skynex/internal/paths"
)

type dependencyDependencies struct {
	opencodeDir func() string
	setup       func(string, bool) error
	output      io.Writer
}

func runDeps(args *cliArgs, deps dependencyDependencies) error {
	if args == nil {
		return fmt.Errorf("dependency arguments are required")
	}
	if deps.opencodeDir == nil {
		deps.opencodeDir = paths.OpencodeDir
	}
	if deps.setup == nil {
		deps.setup = adapters.SetupOpenCodeDependencies
	}
	if deps.output == nil {
		deps.output = io.Discard
	}
	target := deps.opencodeDir()
	if _, err := os.Stat(target); os.IsNotExist(err) {
		fmt.Fprintln(deps.output, "managed OpenCode installation not found; run `skynex install` first")
		return nil
	}
	if err := adapters.ValidateManagedOpenCode(target); err != nil {
		fmt.Fprintln(deps.output, "managed OpenCode installation is invalid; run `skynex install` first")
		return nil
	}
	if err := deps.setup(target, args.TrustScripts); err != nil {
		fmt.Fprintf(deps.output, "Dependency setup failed: %v\nRetry with: skynex deps\n", err)
		return fmt.Errorf("dependency setup failed: %w", err)
	}
	fmt.Fprintln(deps.output, "✓ OpenCode dependencies installed.")
	return nil
}
