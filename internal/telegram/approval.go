package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Approval notices.
//
// When the agent waits for an approval, every bound chat is told what it is
// waiting for. The decision itself is taken in the TUI, but the chat is
// where the user may be looking, so the notice must never present part of a
// command or diff as if it were the whole: a person must be able to trust
// what they approve.
//
// A command or diff that fits in a few messages is sent whole (Broadcast
// splits it into chunks marked "... (continued)"). A longer one is sent as
// its first lines, followed by a line saying how much is not shown, and the
// whole text follows as a file attachment (sendDocument). A text the event
// already carried cut (remote.ClipApprovalText, beyond several megabytes)
// keeps its "[truncated: ...]" line in both.

// approvalInlineBytes is the largest command or diff sent as chat text:
// three messages' worth. Past it the chat gets a head plus an attachment.
const approvalInlineBytes = 3 * MaxMessageLen

// approvalHeadLines and approvalHeadBytes bound the head shown in the chat
// when the whole text goes as an attachment.
const (
	approvalHeadLines = 40
	approvalHeadBytes = 2000
)

// Approval is a pending approval as the relay describes it.
type Approval struct {
	ToolID  string
	Command string
	Reason  string
	// Diff is the unified diff of a file change; empty for commands.
	Diff string
}

// Document is a file the relay sends next to a message.
type Document struct {
	Name    string
	Data    []byte
	Caption string
}

// FormatApproval renders the notice for a pending approval: the chat text
// and, when the command or diff is too long for it, the document carrying
// all of it (nil otherwise).
func FormatApproval(a Approval) (string, *Document) {
	tool := strings.TrimSpace(a.ToolID)
	if tool == "" {
		tool = "tool"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "‼ approval required: %s", tool)
	body, kind := a.Command, "command"
	if strings.TrimSpace(a.Diff) != "" {
		body, kind = a.Diff, "diff"
		// A file change's command is "<tool> <path>": short, and the only
		// place the path is named in full.
		fmt.Fprintf(&b, "\n  target: %s", strings.TrimSpace(a.Command))
	}
	if reason := strings.TrimSpace(a.Reason); reason != "" {
		fmt.Fprintf(&b, "\n  reason: %s", reason)
	}
	body = strings.TrimRight(body, "\n")
	lines := strings.Count(body, "\n") + 1
	var doc *Document
	if len(body) <= approvalInlineBytes {
		fmt.Fprintf(&b, "\n\n%s (%d %s):\n%s", kind, lines, plural(lines, "line", "lines"), body)
	} else {
		head := approvalHead(body)
		name := fmt.Sprintf("approval-%s-%s.txt", fileSafe(tool), kind)
		fmt.Fprintf(&b, "\n\n%s (%d %s, %d bytes), beginning:\n%s", kind, lines, plural(lines, "line", "lines"), len(body), head)
		fmt.Fprintf(&b, "\n[not the whole %s: only its first %d of %d bytes are shown here. The full %s is attached as %s]",
			kind, len(head), len(body), kind, name)
		doc = &Document{
			Name:    name,
			Data:    []byte(body + "\n"),
			Caption: fmt.Sprintf("Full %s of the pending %s approval (%d lines).", kind, tool, lines),
		}
	}
	b.WriteString("\n\nApprove or deny inside the TUI.")
	return b.String(), doc
}

// approvalHead is the beginning of body shown when the whole of it goes as
// an attachment: whole lines, at most approvalHeadLines of them and
// approvalHeadBytes bytes, or, when the first line alone is longer, that
// line's first approvalHeadBytes bytes (cut on a rune boundary). It is a
// prefix of body, with nothing added.
func approvalHead(body string) string {
	lines := strings.SplitN(body, "\n", approvalHeadLines+1)
	if len(lines) > approvalHeadLines {
		lines = lines[:approvalHeadLines]
	}
	n, size := 0, 0
	for n < len(lines) && size+len(lines[n]) <= approvalHeadBytes {
		size += len(lines[n]) + 1
		n++
	}
	if n > 0 {
		return strings.Join(lines[:n], "\n")
	}
	cut := min(approvalHeadBytes, len(body))
	for cut > 0 && cut < len(body) && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut]
}

// fileSafe keeps the letters, digits, '-' and '_' of s, for a file name.
func fileSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return -1
	}, s)
	if s == "" {
		return "tool"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// BroadcastApproval sends an approval notice (FormatApproval) to every bound
// chat: the text first, then the document when there is one, in that order
// in each chat. Failures are recorded like Broadcast's.
func (r *Relay) BroadcastApproval(text string, doc *Document) {
	r.Broadcast(text)
	if doc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, chatID := range r.BoundChats() {
		if _, err := r.client.SendDocument(ctx, chatID, doc.Name, doc.Data, doc.Caption); err != nil {
			r.recordSendErr(chatID, err)
			// The chat was told an attachment follows; say it did not.
			r.sendRaw(ctx, chatID, "⚠ could not attach the full text ("+err.Error()+"); review it in the TUI before approving.")
		}
	}
}
