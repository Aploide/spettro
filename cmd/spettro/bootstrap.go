package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"spettro/internal/config"
	"spettro/internal/models"
	"spettro/internal/provider"
	"spettro/internal/sandbox"
	"spettro/internal/spettro"
	"spettro/internal/storage"
)

// sessionModelsWait bounds how long a front-end that must answer with a model
// list (ACP session/new, the first headless submission) waits for the
// background model discovery. A slower endpoint's models arrive later
// through the front-end's own update path.
const sessionModelsWait = 2 * time.Second

// bootstrap is the process state every front-end (TUI, ACP, headless, goal)
// builds before it starts serving.
type bootstrap struct {
	store         *storage.Store
	manifest      config.AgentManifest
	sandboxPolicy sandbox.Policy
	cfg           config.UserConfig
	providers     *provider.Manager
}

// bootstrapSession runs the startup steps the front-ends share, in the order
// that keeps them off the network and does the least work twice:
//
//  1. Storage and the project manifest: local files only.
//  2. The sandbox policy and the parent confinement. On macOS confinement
//     re-execs the process under sandbox-exec, so everything before it runs
//     twice; it therefore comes before loading config and keys.
//  3. Config and the decrypted API keys.
//  4. The provider manager with the cached or embedded model catalog, and
//     the background catalog refresh. Local endpoints and the subscription
//     model list are left to startModelDiscovery.
//
// Errors carry the same prefixes the entry points printed before.
func bootstrapSession(cwd string, overrides sandbox.Overrides) (*bootstrap, error) {
	store, err := storage.New(cwd)
	if err != nil {
		return nil, fmt.Errorf("storage error: %w", err)
	}
	manifest, err := config.LoadAgentManifestForProject(cwd)
	if err != nil {
		return nil, fmt.Errorf("agent manifest error: %w", err)
	}
	policy, err := resolveSandboxPolicy(overrides, manifest)
	if err != nil {
		return nil, fmt.Errorf("sandbox error: %w", err)
	}
	confineParentProcess(policy, store, cwd)

	cfg, err := config.LoadFull()
	if err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}
	pm := provider.NewManager()
	pm.SetStreamAll(true)
	pm.SetAPIKeys(cfg.APIKeys)
	models.LoadAndRefresh(pm.SetCatalog)
	return &bootstrap{
		store:         store,
		manifest:      manifest,
		sandboxPolicy: policy,
		cfg:           cfg,
		providers:     pm,
	}, nil
}

// confineParentProcess write-confines the spettro process itself (and its
// in-process file tools) as defense-in-depth when the sandbox is enabled. On
// macOS this re-execs under sandbox-exec and does not return; on Linux it
// applies Landlock in place. Best-effort: the model's surface is already
// confined at the shell and file-tool layers, so a failure is a warning.
func confineParentProcess(policy sandbox.Policy, store *storage.Store, cwd string) {
	if !policy.Enabled() {
		return
	}
	writable := append([]string{store.GlobalDir, store.ProjectDir, cwd}, policy.ExtraWritable...)
	if err := sandbox.ConfineParent(writable); err != nil {
		fmt.Fprintf(os.Stderr, "warning: parent sandbox not applied: %v\n", err)
	}
}

// modelDiscovery fetches the model lists that need the network, local
// endpoint probes and the Spettro Subscription plan, in the background, so
// no front-end waits for them before its first frame or its initialize
// response.
//
// Ordering guarantees:
//   - Every result is applied to the provider manager (which has its own
//     lock) before Done is closed, so a reader that has seen Done closed sees
//     all of them.
//   - A result never overrides a change made after discovery started. The
//     user can remove or re-probe an endpoint, or sign out, while a probe is
//     still running; discovery records each list's change count before it
//     starts and applies a result only if the count is unchanged (see
//     provider.Manager.AddLocalModelsIfUnchanged).
//   - Probes run concurrently and apply their models as each one answers.
//     Once all have finished, the endpoints discovery added are applied once
//     more in config order (again only if nobody changed them since), so the
//     model picker's order does not depend on which server answered first.
type modelDiscovery struct {
	done chan struct{}
}

// startModelDiscovery starts the background discovery for cfg's local
// endpoints and, when discoverSubscription is set and the user is signed in,
// the Spettro Subscription models. The subscription endpoint is registered
// immediately (no network), so inference with a subscription model resolves
// before its model list arrives. It must be called before the front-end can
// change the provider manager's model lists, since it reads their change
// counts as the baseline.
func startModelDiscovery(ctx context.Context, cfg config.UserConfig, pm *provider.Manager, discoverSubscription bool) *modelDiscovery {
	d := &modelDiscovery{done: make(chan struct{})}
	subscriptionKey := strings.TrimSpace(cfg.APIKeys[spettro.ProviderID])
	discoverSubscription = discoverSubscription && subscriptionKey != ""
	var subscriptionGen uint64
	if discoverSubscription {
		pm.SetSpettro(spettro.InferenceBaseURL(), nil)
		subscriptionGen = pm.SpettroGeneration()
	}
	endpoints := cfg.LocalEndpoints
	startGen := make([]uint64, len(endpoints))
	for i, endpoint := range endpoints {
		startGen[i] = pm.LocalModelsGeneration(provider.LocalProviderID(endpoint))
	}

	go func() {
		defer close(d.done)
		var wg sync.WaitGroup
		// Written by probe i only, read after wg.Wait.
		applied := make([][]provider.Model, len(endpoints))
		appliedGen := make([]uint64, len(endpoints))
		for i, endpoint := range endpoints {
			wg.Go(func() {
				localModels, err := provider.ProbeLocalServer(ctx, endpoint, cfg.APIKeys[endpoint])
				if err != nil {
					return
				}
				if gen, ok := pm.AddLocalModelsIfUnchanged(localModels, startGen[i]); ok {
					applied[i], appliedGen[i] = localModels, gen
				}
			})
		}
		if discoverSubscription {
			wg.Go(func() {
				if infos, err := spettro.ListModels(ctx, subscriptionKey); err == nil {
					pm.SetSpettroIfUnchanged(spettro.InferenceBaseURL(), spettro.ProviderModels(infos), subscriptionGen)
				}
			})
		}
		wg.Wait()
		for i, localModels := range applied {
			if localModels != nil {
				pm.AddLocalModelsIfUnchanged(localModels, appliedGen[i])
			}
		}
	}()
	return d
}

// Done is closed once every discovery request has finished and its models
// are in the provider manager.
func (d *modelDiscovery) Done() <-chan struct{} { return d.done }

// Wait blocks until discovery finishes or timeout passes, and reports
// whether it finished.
func (d *modelDiscovery) Wait(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-d.done:
		return true
	case <-timer.C:
		return false
	}
}

// fallbackNeedsDiscovery reports whether resolving cfg's active model
// (provider.Manager.ResolveActive) can pick a model that discovery has not
// delivered yet. That is the case only when the configured provider has no
// credentials, local endpoints are configured, and no connected model is
// known now: local models come last in the preference order, so any model
// already connected would be chosen over them anyway.
func fallbackNeedsDiscovery(cfg config.UserConfig, pm *provider.Manager) bool {
	if len(cfg.LocalEndpoints) == 0 || provider.HasCredentials(cfg.APIKeys, cfg.ActiveProvider) {
		return false
	}
	_, known := pm.PreferredModel(cfg.APIKeys)
	return !known
}

// resolveActiveModel replaces a configured model whose provider has no
// credentials (fresh install, removed key) with the best connected model
// the provider manager knows now. It does not wait for model discovery: ACP
// uses the result only as the fallback when a fresh config read fails, and
// the headless server reloads its config before every submission.
func resolveActiveModel(cfg *config.UserConfig, pm *provider.Manager) {
	cfg.ActiveProvider, cfg.ActiveModel = pm.ResolveActive(cfg.ActiveProvider, cfg.ActiveModel, cfg.APIKeys)
}
