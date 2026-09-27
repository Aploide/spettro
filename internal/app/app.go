// Package app is the legacy line-oriented front-end behind internal/ui.
//
// Nothing in cmd/spettro constructs an App — the binary dispatches to `clean`,
// -headless, -acp or the Bubble Tea TUI — so this package's only importer is
// tests/app, which uses it to exercise the non-TUI command surface. Keep that
// in mind before spending effort here: the palette seeding below and the
// theming in internal/ui are parity work for a path no user runs.
package app

import (
	"bufio"
	"context"
	"io"
	"strings"

	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/storage"
	"spettro/internal/theme"
	"spettro/internal/ui"
)

type App struct {
	in  io.Reader
	out io.Writer

	mode        Mode
	cwd         string
	cfg         config.UserConfig
	store       *storage.Store
	providers   *provider.Manager
	manifest    config.AgentManifest
	pendingPlan string
	ui          *ui.Renderer
	setup       *setupWizard
	modelPicker *modelPicker
	reader      *bufio.Reader
}

type setupWizard struct {
	step     int
	provider string
	model    string
}

type modelPicker struct {
	filter string
	items  []provider.Model
}

func (a *App) persistUIState() {
	_ = a.updateConfig(func(cfg *config.UserConfig) error {
		cfg.LastAgentID = string(a.mode)
		return nil
	})
}

func (a *App) updateConfig(mut func(*config.UserConfig) error) error {
	cfg, err := config.Update(mut)
	if err != nil {
		return err
	}
	a.cfg = cfg
	if a.providers != nil {
		a.providers.SetAPIKeys(cfg.APIKeys)
	}
	return nil
}

func New(in io.Reader, out io.Writer, cwdFn func() (string, error)) (*App, error) {
	cwd, err := cwdFn()
	if err != nil {
		return nil, err
	}

	store, err := storage.New(cwd)
	if err != nil {
		return nil, err
	}

	cfg, err := config.LoadFull()
	if err != nil {
		return nil, err
	}

	// Resolve the palette before the renderer is built. This front-end writes
	// straight to a writer and has no event loop that could receive a reply to
	// an OSC 11 background query, so theme.Seed's answer — COLORFGBG, or the
	// dark fallback — is final and is never revised. Without this the legacy
	// REPL would render dark regardless of SPETTRO_THEME or the persisted
	// theme.
	theme.Set(theme.Seed(theme.Preferred(cfg.Theme)))

	pm := provider.NewManager()
	pm.SetStreamAll(true)
	pm.SetWireMode(cfg.ProviderWire)
	pm.SetAPIKeys(cfg.APIKeys)
	for _, endpoint := range cfg.LocalEndpoints {
		localModels, err := provider.ProbeLocalServer(context.Background(), endpoint, cfg.APIKeys[endpoint])
		if err != nil {
			continue
		}
		pm.AddLocalModels(localModels)
	}
	manifest, _ := config.LoadAgentManifestForProject(cwd)
	mode := Mode(manifest.DefaultAgent)
	if mode == "" {
		mode = ModePlanning
	}
	if cfg.LastAgentID != "" {
		if spec, ok := manifest.AgentByID(cfg.LastAgentID); ok && spec.Enabled {
			mode = Mode(cfg.LastAgentID)
		}
	}
	app := &App{
		in:        in,
		out:       out,
		mode:      mode,
		cwd:       cwd,
		cfg:       cfg,
		store:     store,
		providers: pm,
		manifest:  manifest,
		ui:        ui.NewRenderer(),
	}
	return app, nil
}

func (a *App) Run(ctx context.Context) error {
	a.reader = bufio.NewReader(a.in)
	reader := a.reader
	a.printLine(a.ui.Welcome())
	a.printLine(a.ui.Info(a.ui.Stage(string(a.mode))))
	a.printStatus()
	if strings.TrimSpace(a.cfg.APIKeys[a.cfg.ActiveProvider]) == "" {
		a.printLine(a.ui.Panel(string(a.mode), "Setup Required", "Run /setup to configure provider, model and encrypted API key storage."))
	}

	for {
		_, _ = io.WriteString(a.out, a.ui.Prompt(string(a.mode), a.cfg.ActiveProvider, a.cfg.ActiveModel)+" ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if a.setup != nil {
			if err := a.handleSetupInput(line); err != nil {
				a.printLine("setup error: " + err.Error())
			}
			continue
		}

		if a.modelPicker != nil {
			if err := a.handleModelPickerInput(line); err != nil {
				a.printLine("models error: " + err.Error())
			}
			continue
		}

		if IsModeSwitchInput(line) {
			a.mode = a.mode.Next()
			a.persistUIState()
			a.printLine(a.ui.Info(a.ui.Stage(string(a.mode))))
			a.printStatus()
			continue
		}

		if strings.HasPrefix(line, "/") {
			if err := a.handleCommand(line); err != nil {
				if err == io.EOF {
					return nil
				}
				a.printLine("error: " + err.Error())
			}
			continue
		}

		switch a.mode {
		case ModePlanning:
			if err := a.handlePlanning(ctx, line); err != nil {
				a.printLine("planning error: " + err.Error())
			}
		case ModeCoding:
			if err := a.handleCoding(ctx, line); err != nil {
				a.printLine("coding error: " + err.Error())
			}
		case ModeChat:
			if err := a.handleChat(ctx, line); err != nil {
				a.printLine("chat error: " + err.Error())
			}
		}
	}
}
