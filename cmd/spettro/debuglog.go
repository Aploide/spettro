package main

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
)

// debugLogEnv names the environment variable that turns on debug logging:
// SPETTRO_DEBUG_LOG=/path/to/spettro-debug.log appends slog records at debug
// level to that file. The agent loop logs every provider reply there (finish
// reason, raw finish reason, tool-call counts, the head of the text), which
// is how a tool call lost between the provider and the loop is diagnosed.
const debugLogEnv = "SPETTRO_DEBUG_LOG"

// setupDebugLog installs the debug log file named by SPETTRO_DEBUG_LOG as the
// default slog handler. Without it the default handler stays in place: it
// drops debug records and the few info records go to stderr as before, so
// nothing is written over the TUI. A file that cannot be opened is reported
// on stderr and logging stays off.
//
// Installing a slog handler also redirects the standard log package (see
// slog.SetDefault), so the returned function undoes both: it restores the
// previous slog default, log output and log flags, then closes the file.
// It is a no-op when logging was not turned on. main defers it, which only
// matters on a normal return (os.Exit skips it and the OS closes the file);
// tests rely on it to close the file before their temp directory is removed,
// which Windows refuses while the file is open.
func setupDebugLog() (undo func()) {
	path := strings.TrimSpace(os.Getenv(debugLogEnv))
	if path == "" {
		return func() {}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s: cannot open %s: %v\n", debugLogEnv, path, err)
		return func() {}
	}
	prevDefault, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() {
		// SetDefault leaves the log package alone when handed its built-in
		// default handler, so the log output and flags are restored by hand.
		slog.SetDefault(prevDefault)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		_ = f.Close()
	}
}
