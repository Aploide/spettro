// Package acpserve holds the ACP front-end bootstrap shared by every host
// that runs Spettro as an Agent Client Protocol agent: the `spettro --acp`
// CLI (stdio) and the in-process iOS bridge in spettro/mobile (in-memory
// pipes).
//
// It owns exactly the wiring that used to live in cmd/spettro/acp.go —
// storage, user config, the provider manager and its model catalog, the agent
// manifest and the sandbox policy — and then hands off to acp.Serve. Nothing
// here touches os.Stdin/os.Stdout/os.Stderr directly; the streams come in
// through Options so the same code path serves both hosts.
//
// This package must stay buildable for GOOS=ios: cmd/spettro is tagged !ios
// (it cannot link on iOS), so the bridge cannot reach runACP and this package
// is the only shared entry point.
package acpserve

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"spettro/internal/acp"
	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/models"
	"spettro/internal/platform"
	"spettro/internal/provider"
	"spettro/internal/sandbox"
	"spettro/internal/spettro"
	"spettro/internal/storage"
)

// Options configures one ACP serve session.
type Options struct {
	// CWD is the project directory. It becomes the fallback session cwd and
	// the root for per-project state (<cwd>/.spettro).
	CWD string
	// SandboxOverrides are the CLI-flag level sandbox settings, merged with
	// the project manifest by ResolveSandboxPolicy.
	SandboxOverrides sandbox.Overrides
	// In, Out and Log are passed straight through to acp.Options. Nil means
	// os.Stdin / os.Stdout / os.Stderr.
	In  io.Reader
	Out io.Writer
	Log io.Writer
	// Ready is passed straight through to acp.Options: it fires once the
	// bootstrap has succeeded and the agent is accepting input. Everything
	// Run does before that point is setup that can still fail.
	Ready func()
}

// Run performs the bootstrap and serves ACP until the client disconnects or
// ctx is cancelled. Errors are returned already prefixed with the stage that
// failed ("storage error: ...", "acp error: ...") so callers can report them
// verbatim; the serve error is wrapped, so errors.Is(err, context.Canceled)
// still identifies a clean shutdown.
func Run(ctx context.Context, opts Options) error {
	logOut := opts.Log
	if logOut == nil {
		logOut = os.Stderr
	}

	// One line, once per session, naming the degraded mode. On iOS this is
	// the host app's only visible signal that the missing shell/git/LSP tools
	// are a deliberate platform gate rather than a broken bootstrap — it
	// arrives on the same log channel the app already surfaces (the bridge's
	// OnLog), and it is what package 10's device checklist looks for first.
	// Desktop prints nothing: the condition is a build-time constant.
	if !platform.CanExec() {
		fmt.Fprintf(logOut, "capabilities: %s — shell, terminal, git, hooks and language servers are disabled; file tools are unaffected\n", platform.ExecUnavailableReason())
	}

	store, err := storage.New(opts.CWD)
	if err != nil {
		return fmt.Errorf("storage error: %w", err)
	}

	cfg, err := config.LoadFull()
	if err != nil {
		return fmt.Errorf("config error: %w", err)
	}

	pm := provider.NewManager()
	pm.SetAPIKeys(cfg.APIKeys)

	if cat, err := models.Load(); err == nil {
		pm.SetCatalog(cat)
	}
	for _, endpoint := range cfg.LocalEndpoints {
		if localModels, err := provider.ProbeLocalServer(ctx, endpoint, cfg.APIKeys[endpoint]); err == nil {
			pm.AddLocalModels(localModels)
		}
	}
	// Register the Spettro Subscription endpoint + models when signed in.
	if strings.TrimSpace(cfg.APIKeys[spettro.ProviderID]) != "" {
		pm.SetSpettro(spettro.InferenceBaseURL(), nil)
		if infos, err := spettro.ListModels(ctx, cfg.APIKeys[spettro.ProviderID]); err == nil {
			pm.SetSpettro(spettro.InferenceBaseURL(), SpettroModels(infos))
		}
	}
	models.RefreshBackground(pm.SetCatalog)

	// Don't run with a model whose provider has no credentials (fresh install
	// or removed key): fall back to the best connected model.
	cfg.ActiveProvider, cfg.ActiveModel = pm.ResolveActive(cfg.ActiveProvider, cfg.ActiveModel, cfg.APIKeys)

	manifest, err := config.LoadAgentManifestForProject(opts.CWD)
	if err != nil {
		return fmt.Errorf("agent manifest error: %w", err)
	}

	sandboxPolicy, err := ResolveSandboxPolicy(opts.SandboxOverrides, manifest)
	if err != nil {
		return fmt.Errorf("sandbox error: %w", err)
	}
	sb := agent.NewSandboxState(sandboxPolicy)

	// Write-confine the server process itself as defense-in-depth (best-effort;
	// the model surface is confined at the shell and file-tool layers).
	if sandboxPolicy.Enabled() {
		writable := append([]string{store.GlobalDir, store.ProjectDir, opts.CWD}, sandboxPolicy.ExtraWritable...)
		if err := sandbox.ConfineParent(writable); err != nil {
			fmt.Fprintf(logOut, "warning: parent sandbox not applied: %v\n", err)
		}
	}

	if err := acp.Serve(ctx, acp.Options{
		CWD:          opts.CWD,
		GlobalDir:    store.GlobalDir,
		Cfg:          cfg,
		Providers:    pm,
		Manifest:     manifest,
		SandboxState: sb,
		In:           opts.In,
		Out:          opts.Out,
		Log:          opts.Log,
		Ready:        opts.Ready,
	}); err != nil {
		return fmt.Errorf("acp error: %w", err)
	}
	return nil
}

// ResolveSandboxPolicy merges CLI overrides and the project manifest into the
// session's effective sandbox policy.
func ResolveSandboxPolicy(o sandbox.Overrides, manifest config.AgentManifest) (sandbox.Policy, error) {
	return sandbox.ResolvePolicy(o, sandbox.ManifestPolicy{
		Mode:      string(manifest.Runtime.SandboxMode),
		Net:       manifest.Runtime.SandboxNet,
		AllowDirs: manifest.Runtime.SandboxAllowDirs,
		ReadDirs:  manifest.Runtime.SandboxAllowReadDirs,
	})
}

// SpettroModels converts Spettro backend model entries into provider models
// tagged with the "spettro" provider.
func SpettroModels(infos []spettro.ModelInfo) []provider.Model {
	out := make([]provider.Model, 0, len(infos))
	for _, mi := range infos {
		out = append(out, provider.Model{
			Provider:     spettro.ProviderID,
			ProviderName: spettro.ProviderName,
			Name:         mi.ID,
			DisplayName:  mi.ID,
			ToolCall:     true,
			Vision:       mi.Vision,
			Context:      mi.ContextWindow,
		})
	}
	return out
}
