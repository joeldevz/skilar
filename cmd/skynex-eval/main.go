package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(runCLI(ctx, os.Args[1:], defaultDependencies(), os.Stdout, os.Stderr))
}
