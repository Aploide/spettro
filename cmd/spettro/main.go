package main

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"spettro/internal/config"
	"spettro/internal/models"
	"spettro/internal/provider"
	"spettro/internal/storage"
	"spettro/internal/tui"
)

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		fatal("cwd error: %v", err)
	}

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

	if _, err := config.LoadAgentManifestForProject(cwd); err != nil {
		fatal("agent manifest error: %v", err)
	}

	// Load cached catalog immediately (fast disk read) so the model selector
	// is populated before the TUI starts.  Then refresh from the network in
	// the background – exactly like opencode's ModelsDev pattern.
	if cat, err := models.Load(); err == nil {
		pm.SetCatalog(cat)
	}
	for _, endpoint := range cfg.LocalEndpoints {
		localModels, err := provider.ProbeLocalServer(context.Background(), endpoint)
		if err != nil {
			continue
		}
		pm.AddLocalModels(localModels)
	}
	models.RefreshBackground(pm.SetCatalog)

	m := tui.New(cwd, cfg, store, pm)

	p := tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	if _, err := p.Run(); err != nil {
		fatal("runtime error: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
