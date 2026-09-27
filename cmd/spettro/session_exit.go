package main

import (
	"os"

	"spettro/internal/jobs"
	"spettro/internal/lsp"
	"spettro/internal/pty"
	"spettro/internal/shell"
)

// releaseSessionResources stops everything a session started that would
// otherwise outlive spettro, and deletes its temporary state. Every mode
// (TUI, ACP, headless remote, headless goal) must run it on the way out:
//
//   - background shell jobs (run_in_background) and interactive PTY
//     sessions run in their own process groups or sessions, so neither the
//     exit of spettro nor a closing terminal's SIGHUP reaches them (a dev
//     server would keep its port);
//   - foreground commands still running when spettro quits (SIGTERM, or a
//     quit while a tool call is in flight) are process trees of their own
//     for the same reason;
//   - spooled tool outputs live in a temp directory that is session state;
//   - language servers hold handles on workspace files, which matters before
//     the TUI's /update relaunch replaces anything.
//
// Every step is idempotent, so a deferred call after an earlier explicit
// one is harmless.
func releaseSessionResources() {
	jobs.Default().KillAll()
	shell.KillAllProcessTrees()
	pty.Default().KillAll()
	jobs.Spool().Cleanup()
	lsp.ShutdownAll()
}

// osExit is os.Exit, replaceable in tests.
var osExit = os.Exit

// exitSession releases the session's resources and exits with code. Use it
// instead of os.Exit anywhere a session may have started processes:
// os.Exit skips deferred calls, so a deferred releaseSessionResources alone
// would never run on that path.
func exitSession(code int) {
	releaseSessionResources()
	osExit(code)
}
