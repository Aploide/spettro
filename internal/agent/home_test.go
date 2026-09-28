package agent

import (
	"testing"

	"spettro/internal/testhome"
)

// Guard: the package's tests run under the isolated home (see TestMain in
// llm_runtime_lsp_test.go), never the real one.
func TestHomeIsIsolated(t *testing.T) { testhome.Check(t) }
