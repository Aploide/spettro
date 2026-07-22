package notify

import "testing"

// Send is best-effort by design: with no notification daemon (or no binary on
// PATH at all) it must return without panicking or reporting an error.
func TestSendIsBestEffort(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	Send("title", "body")
	Send("", "")
}
