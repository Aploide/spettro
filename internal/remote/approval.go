package remote

import (
	"fmt"
	"unicode/utf8"

	"spettro/internal/termtext"
)

// Approval requests on the wire.
//
// A remote client (the Android app, a chat relay) may be where the user
// decides, so an approval_request event carries what is being approved in
// full: the whole command and, for a file change, the whole unified diff, not
// a preview. A person must be able to trust what they approve, and a command
// cut short can look harmless when its end is not.
//
// The text fields are the exact bytes, for a client to use as data. They can
// hold characters that do not show when drawn (a carriage return, an escape
// sequence, a bidi override, zero-width characters and variation selectors
// that carry a payload inside what reads as ""), so each also has
// <field>_hidden_chars, and when that is true, <field>_visible: the same text
// with every such character written out the way the TUI shows it
// (termtext.EscapeLines). A client showing the text to a person shows
// <field>_visible when there is one.
//
// Each text field is still bounded, by MaxApprovalFieldBytes, because the
// event is kept in the replay buffer and sent to every subscriber. The bound
// is far above any command or diff a person reads (the diff of the largest
// file the runtime diffs at all, 1 MiB per side, fits). When a field is cut
// anyway, the event says so twice: the text ends with an explicit
// "[truncated: ...]" line, and <field>_truncated is true, with
// <field>_bytes giving the full size, so a client can refuse to present the
// text as complete.

// MaxApprovalFieldBytes bounds each text field (command, diff) of an
// approval_request event.
const MaxApprovalFieldBytes = 4 << 20

// ApprovalRequest is what an approval_request event describes.
type ApprovalRequest struct {
	ToolID   string
	Command  string
	Reason   string
	Segments []string
	// Diff is the unified diff of a file change (file-write, file-edit);
	// empty for commands and network access.
	Diff string
}

// ApprovalEvent is the data of an approval_request event for req: tool_id,
// command, reason, segments, and diff when there is one, each text field
// with its size, truncation flag and hidden-character fields (see the
// comment above), and segments_visible, the segments written out, when one
// of them holds a hidden character. RequestApproval adds the approval_id a client answers
// with.
func ApprovalEvent(req ApprovalRequest) map[string]any {
	data := map[string]any{
		"tool_id":  req.ToolID,
		"reason":   req.Reason,
		"segments": req.Segments,
	}
	putApprovalText(data, "command", req.Command)
	if req.Diff != "" {
		putApprovalText(data, "diff", req.Diff)
	}
	// The segments are what "allow-always" remembers, so a client that lists
	// them gets them written out too when one holds hidden characters.
	for _, seg := range req.Segments {
		if termtext.HasHidden(seg) {
			visible := make([]string, len(req.Segments))
			for i, s := range req.Segments {
				visible[i] = termtext.EscapeLines(s)
			}
			data["segments_visible"] = visible
			break
		}
	}
	return data
}

// putApprovalText stores text under key, cut to MaxApprovalFieldBytes with an
// explicit note, and records key_bytes, key_truncated and key_hidden_chars,
// plus key_visible when text holds characters that do not show.
func putApprovalText(data map[string]any, key, text string) {
	clipped, cut := ClipApprovalText(text, MaxApprovalFieldBytes)
	data[key] = clipped
	data[key+"_bytes"] = len(text)
	data[key+"_truncated"] = cut
	hidden := termtext.HasHidden(text)
	data[key+"_hidden_chars"] = hidden
	if hidden {
		data[key+"_visible"], _ = ClipApprovalText(termtext.EscapeLines(text), MaxApprovalFieldBytes)
	}
}

// ClipApprovalText returns text unchanged when it is at most limit bytes.
// Otherwise it returns its first limit bytes (cut on a rune boundary) followed
// by a line saying how much is missing, and true. Callers that show an
// approval must never present a clipped text as the whole of it; the note is
// part of the text so that one rendered as-is cannot be mistaken for it.
func ClipApprovalText(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n[truncated: %d of %d bytes not shown; this is not the whole text]", len(text)-cut, len(text)), true
}
