package version

import "testing"

func TestAppDefault(t *testing.T) {
	if App == "" {
		t.Error("version.App must never be empty; ldflags override an explicit default")
	}
}
