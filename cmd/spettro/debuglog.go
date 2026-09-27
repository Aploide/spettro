package main

import (
	"fmt"
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
// nothing is written over the TUI. The file is never closed explicitly; it
// lives as long as the process, and the OS closes it on exit. A file that
// cannot be opened is reported on stderr and logging stays off.
func setupDebugLog() {
	path := strings.TrimSpace(os.Getenv(debugLogEnv))
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s: cannot open %s: %v\n", debugLogEnv, path, err)
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))
}
