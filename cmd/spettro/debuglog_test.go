package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// With SPETTRO_DEBUG_LOG set, debug records go to that file; unset, the
// default handler (which drops debug records) is left in place.
func TestSetupDebugLog(t *testing.T) {
	saved := slog.Default()
	t.Cleanup(func() { slog.SetDefault(saved) })

	t.Setenv(debugLogEnv, "")
	setupDebugLog()
	if slog.Default() != saved {
		t.Fatal("an unset SPETTRO_DEBUG_LOG must leave the default logger alone")
	}

	path := filepath.Join(t.TempDir(), "debug.log")
	t.Setenv(debugLogEnv, path)
	setupDebugLog()
	slog.Debug("probe record", "k", "v")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `msg="probe record" k=v`) {
		t.Fatalf("debug log = %q", data)
	}
}
