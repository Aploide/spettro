package agent_test

import (
	"strings"
	"testing"

	"spettro/internal/agent"
)

func TestTailTrimHistory_NoOpWhenUnderCap(t *testing.T) {
	in := "assistant(1): hi\ntool(1)[file-read]: ok\n"
	got := agent.TailTrimHistoryForTesting(in, 1024)
	if got != in {
		t.Fatalf("expected pass-through, got: %q", got)
	}
}

func TestTailTrimHistory_DropsHeadAndPrependsMarker(t *testing.T) {
	var b strings.Builder
	for range 1000 {
		b.WriteString("assistant(1): some old line that we'll drop very fast indeed\n")
	}
	for range 5 {
		b.WriteString("assistant(2): RECENT_LINE_TO_KEEP\n")
	}
	got := agent.TailTrimHistoryForTesting(b.String(), 512)
	if !strings.HasPrefix(got, "(history truncated)\n") {
		t.Fatalf("expected truncation marker prefix, got: %q", got[:80])
	}
	if !strings.Contains(got, "RECENT_LINE_TO_KEEP") {
		t.Fatalf("expected recent lines preserved, got: %q", got[len(got)-200:])
	}
	if len(got) > 512+len("(history truncated)\n")+128 {
		// 128 bytes of slack for the line-boundary snap.
		t.Fatalf("trimmed history too long: %d bytes", len(got))
	}
}

func TestTailTrimHistory_NegativeOrZeroDisables(t *testing.T) {
	in := strings.Repeat("x", 100_000)
	if got := agent.TailTrimHistoryForTesting(in, 0); got != in {
		t.Fatalf("0 maxBytes should be a no-op, got %d bytes", len(got))
	}
	if got := agent.TailTrimHistoryForTesting(in, -1); got != in {
		t.Fatalf("negative maxBytes should be a no-op, got %d bytes", len(got))
	}
}
