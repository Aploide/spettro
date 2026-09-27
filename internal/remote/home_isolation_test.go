package remote

import (
	"os"
	"testing"

	"spettro/internal/testhome"
)

// TestMain runs every test in this package with HOME set to a throwaway
// directory, so no test can read or rewrite the real ~/.spettro.
func TestMain(m *testing.M) { os.Exit(testhome.Main(m)) }

func TestHomeIsIsolated(t *testing.T) { testhome.Check(t) }
