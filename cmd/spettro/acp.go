package main

import (
	"context"
	"errors"
	"os/signal"
	"syscall"

	"spettro/internal/acp"
	"spettro/internal/agent"
	"spettro/internal/sandbox"
)

// runACP serves the Agent Client Protocol over stdio so ACP clients (Zed,
// Neovim plugins, ...) can drive Spettro as an external agent. stdout carries
// JSON-RPC exclusively; every diagnostic goes to stderr.
func runACP(cwd string, sandboxOverrides sandbox.Overrides) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// The editor closing the connection (or SIGTERM) ends Serve; whatever
	// the sessions started must not outlive the agent process.
	defer releaseSessionResources()

	boot, err := bootstrapSession(cwd, sandboxOverrides)
	if err != nil {
		fatal("%v", err)
	}
	// initialize must not wait for the network: local endpoints and the
	// subscription model list load in the background, and the bridge waits
	// for them (bounded) only where it reports a model list.
	discovery := startModelDiscovery(ctx, boot.cfg, boot.providers, true, nil)
	cfg := boot.cfg
	resolveActiveModel(&cfg, boot.providers)

	err = acp.Serve(ctx, acp.Options{
		CWD:          cwd,
		GlobalDir:    boot.store.GlobalDir,
		Cfg:          cfg,
		Providers:    boot.providers,
		Manifest:     boot.manifest,
		SandboxState: agent.NewSandboxState(boot.sandboxPolicy),
		ModelsReady:  discovery.Done(),
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fatal("acp error: %v", err)
	}
}
