package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// truncate cuts at a byte budget but never inside a rune: its output goes
// into traces ACP forwards and hook input, which must stay valid UTF-8.
func TestTruncateIsRuneSafe(t *testing.T) {
	s := strings.Repeat("é", 400) // 2 bytes each
	got := truncate(s, 601)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate split a rune: %q", got[590:])
	}
	if !strings.HasSuffix(got, "\n... (truncated)") || len(strings.TrimSuffix(got, "\n... (truncated)")) > 601 {
		t.Fatalf("truncate kept %d bytes", len(strings.TrimSuffix(got, "\n... (truncated)")))
	}
	if truncate("short", 10) != "short" {
		t.Fatal("a short string must be unchanged")
	}
}
