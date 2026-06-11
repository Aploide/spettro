package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"spettro/internal/config"
	"spettro/internal/models"
	"spettro/internal/provider"
	"spettro/internal/sandbox"
	"spettro/internal/storage"
	"spettro/internal/tui"
)

func main() {
	// On Linux, this re-execs as a Landlock-confined sandbox child when asked
	// (see internal/sandbox); it must run before any flag parsing. No-op
	// otherwise.
	sandbox.RunChildIfRequested()

	headless := flag.Bool("headless", false, "run as headless HTTP/SSE server (for Android)")
	cwdFlag := flag.String("cwd", "", "working directory (headless mode only)")
	portFlag := flag.Int("port", 7878, "HTTP listen port (headless mode only)")
	bindFlag := flag.String("bind", "127.0.0.1", "bind host (headless mode only; 0.0.0.0 for LAN)")
	flag.Parse()

	if *headless {
		cwd := *cwdFlag
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				fatal("cwd error: %v", err)
			}
		}
		runHeadless(cwd, *bindFlag, *portFlag)
		return
	}

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
	final, err := p.Run()
	if err != nil {
		fatal("runtime error: %v", err)
	}
	tui.PrintGoodbye(final)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
