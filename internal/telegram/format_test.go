package telegram

import (
	"strings"
	"testing"
)

func TestSplitForTelegramShort(t *testing.T) {
	if got := SplitForTelegram(""); got != nil {
		t.Errorf("empty input should return nil, got %v", got)
	}
	if got := SplitForTelegram("hello\n\n"); len(got) != 1 || got[0] != "hello" {
		t.Errorf("short input = %v", got)
	}
}

func TestSplitForTelegramLong(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString("this is a fairly long line of test output\n")
	}
	chunks := SplitForTelegram(b.String())
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	// The continuation prefix is applied after budgeting, so non-first chunks
	// may exceed MaxMessageLen by len("(...cont)\n"); the hard guarantee is
	// staying under Telegram's real 4096-byte sendMessage limit.
	const hardLimit = 4096
	for i, c := range chunks {
		if len(c) > hardLimit {
			t.Errorf("chunk %d exceeds Telegram limit: %d bytes", i, len(c))
		}
		if i > 0 && !strings.HasPrefix(c, "(...cont)\n") {
			t.Errorf("chunk %d missing continuation prefix", i)
		}
		if i < len(chunks)-1 && !strings.HasSuffix(c, "\n... (continued)") {
			t.Errorf("chunk %d missing continuation suffix", i)
		}
	}
	if strings.HasSuffix(chunks[len(chunks)-1], "(continued)") {
		t.Error("final chunk must not carry the continued suffix")
	}
}

func TestSplitForTelegramNoWhitespace(t *testing.T) {
	// A single giant token still has to make progress and stay under budget.
	chunks := SplitForTelegram(strings.Repeat("x", MaxMessageLen*3))
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 4096 {
			t.Errorf("chunk %d too big: %d", i, len(c))
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 100); got != "short" {
		t.Errorf("Truncate short = %q", got)
	}
	if got := Truncate("abcdef", 0); got != "abcdef" {
		t.Errorf("max<=0 disables truncation, got %q", got)
	}
	if got := Truncate("abcdef", 4); got != "abc…" {
		t.Errorf("Truncate = %q", got)
	}
	if got := Truncate("abcdef", 1); got != "a" {
		t.Errorf("Truncate max=1 = %q", got)
	}
}

func TestPrefix(t *testing.T) {
	if got := Prefix("◆", "body"); got != "◆ body" {
		t.Errorf("Prefix = %q", got)
	}
	if got := Prefix("", " body "); got != "body" {
		t.Errorf("empty tag = %q", got)
	}
	if got := Prefix(" ◆ ", ""); got != "◆" {
		t.Errorf("empty body = %q", got)
	}
}
