package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"spettro/internal/acp"
	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/models"
	"spettro/internal/provider"
	"spettro/internal/sandbox"
	"spettro/internal/spettro"
	"spettro/internal/storage"
)

// runACP serves the Agent Client Protocol over stdio so ACP clients (Zed,
// Neovim plugins, ...) can drive Spettro as an external agent. stdout carries
// JSON-RPC exclusively; every diagnostic goes to stderr.
func runACP(cwd string, sandboxOverrides sandbox.Overrides) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	store, err := storage.New(cwd)
	if err != nil {
		fatal("storage error: %v", err)
	}

	cfg, err := config.LoadFull()
	if err != nil {
		fatal("config error: %v", err)
	}

	pm := provider.NewManager()
	pm.SetAPIKeys(cfg.APIKeys)

	if cat, err := models.Load(); err == nil {
		pm.SetCatalog(cat)
	}
	for _, endpoint := range cfg.LocalEndpoints {
		if localModels, err := provider.ProbeLocalServer(ctx, endpoint); err == nil {
			pm.AddLocalModels(localModels)
		}
	}
	// Register the Spettro Subscription endpoint + models when signed in.
	if strings.TrimSpace(cfg.APIKeys[spettro.ProviderID]) != "" {
		pm.SetSpettro(spettro.InferenceBaseURL(), nil)
		if infos, err := spettro.ListModels(ctx, cfg.APIKeys[spettro.ProviderID]); err == nil {
			pm.SetSpettro(spettro.InferenceBaseURL(), spettroInfosToModels(infos))
		}
	}
	models.RefreshBackground(pm.SetCatalog)

	manifest, err := config.LoadAgentManifestForProject(cwd)
	if err != nil {
		fatal("agent manifest error: %v", err)
	}

	sandboxPolicy, err := resolveSandboxPolicy(sandboxOverrides, manifest)
	if err != nil {
		fatal("sandbox error: %v", err)
	}
	sb := agent.NewSandboxState(sandboxPolicy)

	// Write-confine the server process itself as defense-in-depth (best-effort;
	// the model surface is confined at the shell and file-tool layers).
	if sandboxPolicy.Enabled() {
		writable := append([]string{store.GlobalDir, store.ProjectDir, cwd}, sandboxPolicy.ExtraWritable...)
		if err := sandbox.ConfineParent(writable); err != nil {
			fmt.Fprintf(os.Stderr, "warning: parent sandbox not applied: %v\n", err)
		}
	}

	err = acp.Serve(ctx, acp.Options{
		CWD:          cwd,
		GlobalDir:    store.GlobalDir,
		Cfg:          cfg,
		Providers:    pm,
		Manifest:     manifest,
		SandboxState: sb,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fatal("acp error: %v", err)
	}
}
