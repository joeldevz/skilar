package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/joeldevz/skynex/internal/eval/mcpproxy"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if len(os.Args) > 1 && os.Args[1] == "__mcp-proxy" {
		// This evaluator-owned stdio subcommand is intentionally absent from
		// help and emits no diagnostics: stdout is the MCP protocol stream, and
		// command/env values must never be copied to stderr.
		config, err := mcpproxy.ParseArgs(os.Args[2:])
		if err != nil {
			os.Exit(2)
		}
		config, err = mcpproxy.BindRuntimeManifest(config, os.Getenv(mcpproxy.ManifestEnvironment))
		if err != nil {
			os.Exit(2)
		}
		if err := mcpproxy.Run(ctx, config, os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		return
	}
	os.Exit(runCLI(ctx, os.Args[1:], defaultDependencies(), os.Stdout, os.Stderr))
}
