package jobs

import (
	"os"
	"testing"

	"spettro/internal/testhome"
)

// TestMain runs every test of the package with HOME and the XDG directories
// pointed at a temporary directory, so no test can touch the user's real
// ~/.spettro.
func TestMain(m *testing.M) { os.Exit(testhome.Main(m)) }

// Guard: the package's tests run under the isolated home, never the real one.
func TestHomeIsIsolated(t *testing.T) { testhome.Check(t) }
