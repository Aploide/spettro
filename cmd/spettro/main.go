package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/jobs"
	"spettro/internal/pty"
	"spettro/internal/sandbox"
	"spettro/internal/shell"
	"spettro/internal/tui"
	"spettro/internal/update"
	"spettro/internal/version"
)

func main() {
	// Answered before anything else runs, so it costs no more than the
	// package initialisers (see printVersionIfRequested).
	if printVersionIfRequested(os.Args[1:]) {
		return
	}

	// On Linux, this re-execs as a Landlock-confined sandbox child when asked
	// (see internal/sandbox); it must run before any flag parsing. No-op
	// otherwise.
	sandbox.RunChildIfRequested()

	// Opt-in debug log (SPETTRO_DEBUG_LOG); a no-op when the variable is unset.
	undoDebugLog := setupDebugLog()
	defer undoDebugLog()

	// Foreground shell commands, background jobs and PTY sessions run in
	// their own process groups or sessions, so the SIGHUP a closing terminal
	// sends spettro's group never reaches them; kill them on the way out
	// instead of leaving them orphaned (a dev server holding its port).
	shell.KillProcessTreesOnHangup(
		func() { jobs.Default().KillAll() },
		func() { pty.Default().KillAll() },
	)

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
	boot, err := bootstrapSession(cwd, sandboxOverrides)
	if err != nil {
		fatal("%v", err)
	}
	// Local endpoints answer in the background. Each answer, and each
	// catalog the background refresh applies, signals boot.modelsChanged,
	// which the TUI waits on (tui.WithModelUpdates) to redraw the model
	// lists and the header.
	discovery := startModelDiscovery(context.Background(), boot.cfg, boot.providers, false, boot.modelsChanged.notify)
	// tui.New replaces a configured model that cannot run with the best
	// connected one and saves that choice. When only a local endpoint can
	// supply it, give the probes a bounded chance to answer first, or the
	// saved choice would be "no model" on every launch.
	if fallbackNeedsDiscovery(boot.cfg, boot.providers) {
		discovery.Wait(sessionModelsWait)
	}
	sb := agent.NewSandboxState(boot.sandboxPolicy)

	opts := []tui.Option{tui.WithManifest(boot.manifest), tui.WithModelUpdates(boot.modelsChanged)}
	// The size Bubble Tea is about to read itself: with it the model is
	// ready before the first render, so that render is the real first frame
	// rather than a "loading…" placeholder (see tui.WithInitialSize).
	if w, h, err := term.GetSize(os.Stdout.Fd()); err == nil {
		opts = append(opts, tui.WithInitialSize(w, h))
	}
	m := tui.New(cwd, boot.cfg, boot.store, boot.providers, sb, opts...)

	// Alt screen and mouse mode are declared on the tea.View in Model.View
	// (bubbletea v2 removed the imperative program options).
	p := tea.NewProgram(m)
	final, err := p.Run()
	// Before the /update relaunch below, which may replace files.
	releaseSessionResources()
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

// printVersionIfRequested prints the version and reports true when the only
// argument is --version, -v or version.
func printVersionIfRequested(args []string) bool {
	if len(args) != 1 {
		return false
	}
	switch args[0] {
	case "--version", "-v", "-version", "version":
		fmt.Println("spettro " + version.App)
		return true
	}
	return false
}

// fatal reports an error and exits with status 1, releasing whatever the
// session started first (see exitSession).
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	exitSession(1)
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
