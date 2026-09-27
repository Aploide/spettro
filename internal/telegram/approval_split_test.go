package telegram

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// rejoin undoes ApprovalMessages: the markers and "(...cont)" lines come
// off, a line break goes back where approvalContNextLine stood.
func rejoin(t *testing.T, msgs []string) string {
	t.Helper()
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 {
			if !strings.HasPrefix(m, approvalContPrefix) {
				t.Fatalf("message %d lacks the continuation line: %q", i, m[:min(len(m), 40)])
			}
			m = m[len(approvalContPrefix):]
		}
		switch {
		case i == len(msgs)-1:
			b.WriteString(m)
		case strings.HasSuffix(m, approvalContNextLine):
			b.WriteString(strings.TrimSuffix(m, approvalContNextLine) + "\n")
		case strings.HasSuffix(m, approvalContSameLine):
			b.WriteString(strings.TrimSuffix(m, approvalContSameLine))
		default:
			t.Fatalf("message %d ends without a marker: %q", i, m[max(len(m)-40, 0):])
		}
	}
	return b.String()
}

// Two commands that differ by one space must not give the same chat
// messages. SplitForTelegram drops the blanks at a break, so
// "rm -rf ./x…x/ ~/…" (which reaches the home directory) and
// "rm -rf ./x…x/~/…" split into the same two messages.
func TestApprovalMessagesTellCommandsApartThatDifferByASpace(t *testing.T) {
	for n := 3000; n < 3900; n += 7 {
		dir := "./" + strings.Repeat("x", n) + "/"
		tail := "~/" + strings.Repeat("z", 3000)
		a, _ := FormatApproval(Approval{ToolID: "bash", Command: "rm -rf " + dir + " " + tail})
		b, _ := FormatApproval(Approval{ToolID: "bash", Command: "rm -rf " + dir + tail})
		ma, mb := ApprovalMessages(a), ApprovalMessages(b)
		if strings.Join(ma, "|") == strings.Join(mb, "|") {
			t.Fatalf("n=%d: the two commands give the same messages", n)
		}
		if rejoin(t, ma) != a || rejoin(t, mb) != b {
			t.Fatalf("n=%d: the messages do not hold the notice exactly", n)
		}
	}
}

// Every message fits, is valid UTF-8, keeps no blank at either end where
// the chat app would trim it, never splits an escape, and together they
// hold the notice exactly: for random texts of blanks, newlines, words,
// escapes and multi-byte characters.
func TestApprovalMessagesAreLossless(t *testing.T) {
	pieces := []string{" ", "  ", "\n", "\n\n", "word", "x", "\\u00a0", "^M", "\\U000e0100", "é", "日本", "⇥   ", strings.Repeat("y", 500)}
	rng := rand.New(rand.NewSource(1))
	for round := range 300 {
		var b strings.Builder
		b.WriteString("header")
		for b.Len() < 4000+rng.Intn(12000) {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		b.WriteString("end")
		text := b.String()
		msgs := ApprovalMessages(text)
		for i, m := range msgs {
			if len(m) > MaxMessageLen || !utf8.ValidString(m) {
				t.Fatalf("round %d message %d: %d bytes, valid %v", round, i, len(m), utf8.ValidString(m))
			}
			if strings.TrimSpace(m) != m {
				t.Fatalf("round %d message %d has a blank at an end: %q … %q", round, i, m[:min(20, len(m))], m[max(len(m)-20, 0):])
			}
		}
		for i := 0; i+1 < len(msgs); i++ {
			body := strings.TrimSuffix(strings.TrimSuffix(msgs[i], approvalContSameLine), approvalContNextLine)
			for _, esc := range []string{"\\u00a", "\\u00", "\\u0", "\\U000e010", "\\U000e", "\\", "^"} {
				if strings.HasSuffix(body, esc) && strings.HasSuffix(msgs[i], approvalContSameLine) {
					t.Fatalf("round %d: message %d splits an escape: %q", round, i, body[max(len(body)-20, 0):])
				}
			}
		}
		if got := rejoin(t, msgs); got != text {
			t.Fatalf("round %d: rejoined text differs", round)
		}
	}
}
