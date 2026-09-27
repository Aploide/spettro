package tui

import (
	"os"
	"testing"

	"spettro/internal/testhome"
)

// TestMain runs the package's tests with HOME and the XDG directories in a
// temporary directory, so no test can touch the real ~/.spettro.
func TestMain(m *testing.M) { os.Exit(testhome.Main(m)) }

// TestHomeIsIsolated fails when the tests would run against the real home.
func TestHomeIsIsolated(t *testing.T) { testhome.AssertIsolated(t) }
