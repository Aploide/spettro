//go:build !ios

package main

import (
	"context"
	"errors"
	"os/signal"
	"syscall"

	"spettro/internal/acpserve"
	"spettro/internal/sandbox"
)

// runACP serves the Agent Client Protocol over stdio so ACP clients (Zed,
// Neovim plugins, ...) can drive Spettro as an external agent. stdout carries
// JSON-RPC exclusively; every diagnostic goes to stderr.
//
// The bootstrap itself lives in internal/acpserve because the iOS bridge
// (spettro/mobile) runs the identical setup over in-memory pipes. Leaving
// acpserve.Options.In/Out/Log unset is what selects stdio here.
func runACP(cwd string, sandboxOverrides sandbox.Overrides) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	err := acpserve.Run(ctx, acpserve.Options{
		CWD:              cwd,
		SandboxOverrides: sandboxOverrides,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fatal("%s", err)
	}
}
