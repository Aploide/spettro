package main

import (
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// With SPETTRO_DEBUG_LOG set, debug records go to that file; unset, the
// default handler (which drops debug records) is left in place. Undoing the
// setup restores the standard log package too, which slog.SetDefault
// redirected.
func TestSetupDebugLog(t *testing.T) {
	savedWriter, savedFlags := log.Writer(), log.Flags()
	t.Run("unset", func(t *testing.T) {
		saved := slog.Default()
		t.Setenv(debugLogEnv, "")
		t.Cleanup(setupDebugLog())
		if slog.Default() != saved {
			t.Fatal("an unset SPETTRO_DEBUG_LOG must leave the default logger alone")
		}
	})
	t.Run("set", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "debug.log")
		t.Setenv(debugLogEnv, path)
		// Cleanups run last-in first-out: the undo registered here closes
		// the file before t.TempDir removes it (Windows refuses to delete
		// an open file).
		t.Cleanup(setupDebugLog())
		slog.Debug("probe record", "k", "v")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `msg="probe record" k=v`) {
			t.Fatalf("debug log = %q", data)
		}
	})
	if log.Writer() != savedWriter || log.Flags() != savedFlags {
		t.Fatalf("the standard log package was left redirected: writer %T, flags %d", log.Writer(), log.Flags())
	}
}
