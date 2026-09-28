package shell

import (
	"os"
	"testing"

	"spettro/internal/testhome"
)

// TestMain first lets a re-executed test binary act as the hangup helper
// (unix only, see runTestHelperIfRequested), then runs every test with HOME
// set to a throwaway directory. The login-environment capture starts the
// user's login shell, which reads the rc files under HOME, so a test must
// never see the real home.
func TestMain(m *testing.M) {
	runTestHelperIfRequested()
	os.Exit(testhome.Main(m))
}

func TestHomeIsIsolated(t *testing.T) { testhome.Check(t) }
