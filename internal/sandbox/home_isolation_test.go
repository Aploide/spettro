package sandbox

import (
	"os"
	"testing"

	"spettro/internal/testhome"
)

// TestMain lets a re-executed sandbox child apply its confinement and exec the
// real command before the test runner takes over; for a normal `go test`
// invocation (no child sentinel in os.Args) RunChildIfRequested is a no-op.
// Every test then runs with HOME set to a throwaway directory, so a sandboxed
// command can never write under the real home.
func TestMain(m *testing.M) {
	RunChildIfRequested()
	os.Exit(testhome.Main(m))
}

func TestHomeIsIsolated(t *testing.T) { testhome.Check(t) }
