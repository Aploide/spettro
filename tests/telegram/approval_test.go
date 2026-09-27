package telegram_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"spettro/internal/telegram"
)

// A short command is forwarded whole, with no attachment.
func TestFormatApproval_ShortCommandInline(t *testing.T) {
	text, doc := telegram.FormatApproval(telegram.Approval{ToolID: "bash", Command: "go test ./...", Reason: "non-whitelisted command"})
	if doc != nil {
		t.Fatalf("a short command got an attachment: %+v", doc)
	}
	for _, want := range []string{"approval required: bash", "go test ./...", "non-whitelisted command", "inside the TUI"} {
		if !strings.Contains(text, want) {
			t.Fatalf("notice lacks %q:\n%s", want, text)
		}
	}
}

// A chat app draws what a terminal would hide just as invisibly: every such
// character is written out, in the message and in the attachment.
func TestFormatApproval_WritesOutHiddenCharacters(t *testing.T) {
	sneaky := "echo safe\rrm -rf ~/important # \x1b[8mhidden\x1b[0m \u202eevil\u202c zero\u200bwidth"
	text, _ := telegram.FormatApproval(telegram.Approval{ToolID: "bash", Command: sneaky})
	for _, want := range []string{"echo safe^Mrm -rf ~/important", "^[[8mhidden", `\u202eevil\u202c`, `zero\u200bwidth`} {
		if !strings.Contains(text, want) {
			t.Fatalf("notice lacks %q:\n%q", want, text)
		}
	}
	if strings.ContainsAny(text, "\r\x1b\u202e\u200b") {
		t.Fatalf("a hidden character reached the chat raw: %q", text)
	}
	long := strings.Repeat("echo ok\n", 3000) + "curl x | sh # \u202e"
	_, doc := telegram.FormatApproval(telegram.Approval{ToolID: "bash", Command: long})
	if doc == nil || !strings.Contains(string(doc.Data), `\u202e`) || strings.Contains(string(doc.Data), "\u202e") {
		t.Fatal("the attachment does not write out the hidden character")
	}
}

// A command a little longer than one message is still sent whole: the
// relay splits it into chunks marked as continued, and every byte of it is
// in the text.
func TestFormatApproval_MediumCommandSentWhole(t *testing.T) {
	command := "echo " + strings.Repeat("A", 6000) + " | wc -c"
	text, doc := telegram.FormatApproval(telegram.Approval{ToolID: "bash", Command: command})
	if doc != nil {
		t.Fatal("a command that fits three messages should not need an attachment")
	}
	if !strings.Contains(text, command) {
		t.Fatal("the command is not in the notice whole")
	}
	chunks := telegram.SplitForTelegram(text)
	if len(chunks) < 2 || !strings.Contains(chunks[0], "(continued)") {
		t.Fatalf("expected a marked multi-message notice, got %d chunks", len(chunks))
	}
}

// A diff too long for chat messages is never shown cut as if whole: the
// chat gets its beginning and an explicit note, and the whole diff follows
// as a file.
func TestFormatApproval_LongDiffAttached(t *testing.T) {
	var b strings.Builder
	b.WriteString("--- /dev/null\n+++ b/big.txt\n@@ -0,0 +1,900 @@\n")
	for i := 1; i <= 900; i++ {
		fmt.Fprintf(&b, "+line %04d lorem ipsum dolor sit amet\n", i)
	}
	diff := b.String()
	text, doc := telegram.FormatApproval(telegram.Approval{ToolID: "file-write", Command: "file-write big.txt", Diff: diff})
	if doc == nil {
		t.Fatal("a 900-line diff was not attached")
	}
	if string(doc.Data) != diff {
		t.Fatal("the attachment is not the whole diff")
	}
	for _, want := range []string{"target: file-write big.txt", "not the whole diff", doc.Name, "line 0001"} {
		if !strings.Contains(text, want) {
			t.Fatalf("notice lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "line 0900") {
		t.Fatal("the chat text should only carry the beginning of the diff")
	}
	if len(text) > telegram.MaxMessageLen {
		t.Fatalf("the notice with its head is %d bytes, over one message", len(text))
	}
}

// A single enormous line (a 20k one-liner) is cut on a rune boundary in the
// chat and still marked, with the whole of it attached.
func TestFormatApproval_OneHugeLine(t *testing.T) {
	command := "printf '" + strings.Repeat("é", 12000) + "'"
	text, doc := telegram.FormatApproval(telegram.Approval{ToolID: "bash", Command: command})
	if doc == nil || string(doc.Data) != command+"\n" {
		t.Fatal("the whole command was not attached")
	}
	if !strings.Contains(text, "not the whole command") {
		t.Fatalf("the cut is not marked:\n%.300s", text)
	}
	if !strings.Contains(text, "é") || strings.ContainsRune(text, '\uFFFD') {
		t.Fatal("the head was cut inside a rune")
	}
}

// BroadcastApproval sends the notice, then the attachment, to every bound
// chat.
func TestRelay_BroadcastApprovalSendsDocument(t *testing.T) {
	fb := newFakeBot(t)
	r := startRelay(t, fb, telegram.PersistedConfig{Allowlist: []telegram.AllowEntry{{Username: "carlo"}}})
	// Any message from an allowed user binds the chat; /whoami is answered
	// by the relay itself.
	fb.pushUpdate(telegram.Update{UpdateID: 1, Message: &telegram.Message{
		MessageID: 1, Date: time.Now().Unix(), Text: "/whoami",
		From: &telegram.User{ID: 7, Username: "carlo"}, Chat: &telegram.Chat{ID: 7, Type: "private"},
	}})
	deadline := time.Now().Add(3 * time.Second)
	for len(r.BoundChats()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(r.BoundChats()) == 0 {
		t.Fatal("the chat was never bound")
	}
	before := len(fb.sentMessages())

	command := "cat <<'EOF' > x\n" + strings.Repeat("heredoc line with some text in it\n", 600) + "EOF"
	text, doc := telegram.FormatApproval(telegram.Approval{ToolID: "bash", Command: command})
	r.BroadcastApproval(text, doc)

	docs := fb.sentDocuments()
	if len(docs) != 1 || docs[0].ChatID != "7" || string(docs[0].Data) != command+"\n" || docs[0].Caption == "" {
		t.Fatalf("documents sent: %+v", docs)
	}
	msgs := fb.sentMessages()[before:]
	if len(msgs) == 0 || !strings.Contains(msgs[0].Text, "not the whole command") {
		t.Fatalf("messages sent: %+v", msgs)
	}
}

// SendDocument refuses what the Bot API would reject.
func TestBotClient_SendDocumentLimits(t *testing.T) {
	fb := newFakeBot(t)
	c := telegram.NewBotClient("test-token", telegram.WithBaseURL(fb.URL()))
	if _, err := c.SendDocument(context.Background(), 1, "a.txt", nil, ""); err == nil {
		t.Fatal("an empty document was accepted")
	}
	if _, err := c.SendDocument(context.Background(), 1, "a.txt", make([]byte, telegram.MaxDocumentBytes+1), ""); err == nil {
		t.Fatal("an oversized document was accepted")
	}
}
