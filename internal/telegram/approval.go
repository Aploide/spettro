package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"spettro/internal/termtext"
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
//
// Every character that would not show as itself is written out, in the chat
// text and in the attachment alike (termtext.EscapeLines, the TUI's own
// escaping): a chat app draws a carriage return, a bidi override that
// reorders the line, or a string of variation selectors carrying a payload
// inside what reads as "" just as invisibly as a terminal would.

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
	tool := termtext.EscapeLines(strings.TrimSpace(a.ToolID))
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
		fmt.Fprintf(&b, "\n  target: %s", termtext.EscapeLines(strings.TrimSpace(a.Command)))
	}
	if reason := strings.TrimSpace(a.Reason); reason != "" {
		fmt.Fprintf(&b, "\n  reason: %s", termtext.EscapeLines(reason))
	}
	body = termtext.EscapeLines(strings.TrimRight(body, "\n"))
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

// Markers of an approval notice split over several messages
// (ApprovalMessages). A line cut in two gets a marker of its own: with one
// marker for both, "a" + "b" split mid-line and "a" and "b" on two lines,
// which a shell runs as two commands, would read the same.
const (
	approvalContNextLine = "\n... (continued in the next message)"
	approvalContSameLine = "\n... (this line continues in the next message)"
	approvalContPrefix   = "(...cont)\n"
)

// ApprovalMessages splits an approval notice (FormatApproval) into chat
// messages without losing, adding or hiding a character. SplitForTelegram
// is for prose: it drops the blanks at each break, so "./build/ ~/" (which
// deletes the home directory) and "./build/~/" (which does not) split into
// the same two messages there. Here a break falls, in order of preference:
// at a line break, which the break stands for (approvalContNextLine); inside
// a line between two characters that are not blanks, never inside an escape
// written by termtext (approvalContSameLine); and only in a line with no
// such place, just before a blank, which then starts the next message after
// its "(...cont)" line, where it shows as indentation. Every message but the
// first starts with that line and every one but the last ends with a
// marker, so no blank is left at either end of a message, where the chat
// app would trim it.
func ApprovalMessages(text string) []string {
	text = strings.TrimRight(text, "\n")
	if len(text) <= MaxMessageLen {
		return []string{text}
	}
	budget := MaxMessageLen - len(approvalContPrefix) - len(approvalContSameLine)
	var out []string
	for len(text) > MaxMessageLen {
		cut, atNewline := approvalCut(text, budget)
		marker := approvalContSameLine
		rest := text[cut:]
		if atNewline {
			marker = approvalContNextLine
			rest = text[cut+1:]
		}
		out = append(out, text[:cut]+marker)
		text = approvalContPrefix + rest
	}
	return append(out, text)
}

// approvalCut picks where ApprovalMessages ends a message holding the start
// of text, at most budget bytes in: the byte offset of the break and
// whether it is a line break (whose "\n" is dropped, the marker standing
// for it). See ApprovalMessages for the order of preference.
func approvalCut(text string, budget int) (int, bool) {
	budget = min(budget, len(text)-1)
	if i := strings.LastIndexByte(text[:budget+1], '\n'); i > 0 && i >= budget/2 {
		return i, true
	}
	blank := func(c byte) bool { return c == ' ' || c == '\n' }
	for cut := budget; cut >= max(budget/2, 1); cut-- {
		if utf8.RuneStart(text[cut]) && !blank(text[cut-1]) && !blank(text[cut]) && !insideEscape(text, cut) {
			return cut, false
		}
	}
	for cut := budget; cut >= 1; cut-- {
		if text[cut] == ' ' && !insideEscape(text, cut) {
			return cut, false
		}
	}
	for cut := budget; cut >= 1; cut-- {
		if utf8.RuneStart(text[cut]) {
			return cut, false
		}
	}
	return budget, false
}

// insideEscape reports whether offset cut of text falls inside an escape
// termtext wrote (at most ten bytes long: "\U000e0100").
func insideEscape(text string, cut int) bool {
	for start := max(cut-9, 0); start < cut; start++ {
		if n := termtext.EscapeLen(text[start:]); n > 0 && start+n > cut {
			return true
		}
	}
	return false
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
// chat, split by ApprovalMessages rather than as prose: the text first, then
// the document when there is one, in that order in each chat. Failures are
// recorded like Broadcast's.
func (r *Relay) BroadcastApproval(text string, doc *Document) {
	r.broadcastChunks(ApprovalMessages(text))
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
