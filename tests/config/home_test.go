package config_test

import (
	"os"
	"testing"

	"spettro/internal/testhome"
)

// TestMain runs this package's tests in a temporary HOME, so nothing they
// save (config, keys, sessions) reaches the developer's real ~/.spettro.
func TestMain(m *testing.M) { os.Exit(testhome.Main(m)) }

// TestHomeIsIsolated fails if the tests above would run in the real home.
func TestHomeIsIsolated(t *testing.T) { testhome.Check(t) }
