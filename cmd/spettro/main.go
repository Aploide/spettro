package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/jobs"
	"spettro/internal/models"
	"spettro/internal/provider"
	"spettro/internal/pty"
	"spettro/internal/sandbox"
	"spettro/internal/storage"
	"spettro/internal/tui"
	"spettro/internal/update"
)

func main() {
	// On Linux, this re-execs as a Landlock-confined sandbox child when asked
	// (see internal/sandbox); it must run before any flag parsing. No-op
	// otherwise.
	sandbox.RunChildIfRequested()

	// Subcommands run before flag parsing (the flag set below is for the
	// TUI/headless modes). `spettro clean` works entirely without the TUI.
	if len(os.Args) > 1 && os.Args[1] == "clean" {
		runClean(os.Args[2:])
		return
	}

	headless := flag.Bool("headless", false, "run as headless HTTP/SSE server (for Android)")
	acpMode := flag.Bool("acp", false, "run as Agent Client Protocol (ACP) agent over stdio (for editors like Zed)")
	cwdFlag := flag.String("cwd", "", "working directory (headless/acp modes only)")
	portFlag := flag.Int("port", 7878, "HTTP listen port (headless mode only)")
	bindFlag := flag.String("bind", "127.0.0.1", "bind host (headless mode only; 0.0.0.0 for LAN)")
	sandboxMode := flag.String("sandbox", "", "OS sandbox for agent shell commands: off|read-only|workspace-write (default: manifest setting, else off)")
	sandboxNet := flag.String("sandbox-net", "", "sandbox network policy: all|localhost|none|ports:443,8080 (localhost degrades to none on Linux)")
	goalFlag := flag.String("goal", "", "run in goal mode: execute autonomously until objective is met (headless only)")
	var sandboxAllowDirs stringListFlag
	flag.Var(&sandboxAllowDirs, "sandbox-allow-dir", "extra writable directory inside the sandbox (repeatable)")
	var sandboxReadDirs stringListFlag
	flag.Var(&sandboxReadDirs, "sandbox-allow-read-dir", "extra readable directory inside the sandbox, e.g. a toolchain cache (repeatable)")
	flag.Parse()

	sandboxOverrides := sandbox.Overrides{
		Mode:      *sandboxMode,
		Net:       *sandboxNet,
		AllowDirs: sandboxAllowDirs,
		ReadDirs:  sandboxReadDirs,
	}

	// Goal mode: autonomous run until objective is met
	if *goalFlag != "" {
		cwd := *cwdFlag
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				fatal("cwd error: %v", err)
			}
		}
		runHeadlessGoal(cwd, *goalFlag, sandboxOverrides)
		return
	}

	if *acpMode {
		cwd := *cwdFlag
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				fatal("cwd error: %v", err)
			}
		}
		runACP(cwd, sandboxOverrides)
		return
	}

	if *headless {
		cwd := *cwdFlag
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				fatal("cwd error: %v", err)
			}
		}
		runHeadless(cwd, *bindFlag, *portFlag, sandboxOverrides)
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

	manifest, err := config.LoadAgentManifestForProject(cwd)
	if err != nil {
		fatal("agent manifest error: %v", err)
	}
	sandboxPolicy, err := resolveSandboxPolicy(sandboxOverrides, manifest)
	if err != nil {
		fatal("sandbox error: %v", err)
	}
	sb := agent.NewSandboxState(sandboxPolicy)

	// Write-confine the spettro process itself (and its in-process file tools)
	// as defense-in-depth. On macOS this re-execs under sandbox-exec and does
	// not return; on Linux it applies Landlock in place. Done before the
	// catalog/network setup to avoid redoing that work after the macOS re-exec.
	// Best-effort: the model's surface is already confined at the shell and
	// file-tool layers, so a failure here is only a warning.
	if sandboxPolicy.Enabled() {
		writable := append([]string{store.GlobalDir, store.ProjectDir, cwd}, sandboxPolicy.ExtraWritable...)
		if err := sandbox.ConfineParent(writable); err != nil {
			fmt.Fprintf(os.Stderr, "warning: parent sandbox not applied: %v\n", err)
		}
	}

	// Load cached catalog immediately (fast disk read) so the model selector
	// is populated before the TUI starts.  Then refresh from the network in
	// the background – exactly like opencode's ModelsDev pattern.
	if cat, err := models.Load(); err == nil {
		pm.SetCatalog(cat)
	}
	for _, endpoint := range cfg.LocalEndpoints {
		localModels, err := provider.ProbeLocalServer(context.Background(), endpoint, cfg.APIKeys[endpoint])
		if err != nil {
			continue
		}
		pm.AddLocalModels(localModels)
	}
	models.RefreshBackground(pm.SetCatalog)

	m := tui.New(cwd, cfg, store, pm, sb)

	// Alt screen and mouse mode are declared on the tea.View in Model.View
	// (bubbletea v2 removed the imperative program options).
	p := tea.NewProgram(m)
	final, err := p.Run()
	// Background shell jobs are detached into their own process groups, so
	// they would outlive spettro unless killed explicitly on session exit.
	jobs.Default().KillAll()
	// Interactive PTY sessions are session state for the same reason.
	pty.Default().KillAll()
	// Spooled tool outputs are session state too; delete them with the session.
	jobs.Spool().Cleanup()
	if err != nil {
		fatal("runtime error: %v", err)
	}
	tui.PrintGoodbye(final)

	// /update installs the new binary in place before quitting; relaunch
	// into it now so the restart is seamless. On success this does not
	// return.
	if path := tui.RelaunchPath(final); path != "" {
		if err := update.Relaunch(path); err != nil {
			fmt.Fprintf(os.Stderr, "spettro was updated, but could not restart automatically: %v\nrun spettro again to use the new version.\n", err)
		}
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// resolveSandboxPolicy merges CLI overrides and the project manifest into the
// session's effective sandbox policy.
func resolveSandboxPolicy(o sandbox.Overrides, manifest config.AgentManifest) (sandbox.Policy, error) {
	return sandbox.ResolvePolicy(o, sandbox.ManifestPolicy{
		Mode:      string(manifest.Runtime.SandboxMode),
		Net:       manifest.Runtime.SandboxNet,
		AllowDirs: manifest.Runtime.SandboxAllowDirs,
		ReadDirs:  manifest.Runtime.SandboxAllowReadDirs,
	})
}

// stringListFlag is a repeatable string flag (e.g. --sandbox-allow-dir).
type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ",") }

func (s *stringListFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}
