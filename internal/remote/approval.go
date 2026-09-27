package remote

import (
	"fmt"
	"unicode/utf8"
)

// Approval requests on the wire.
//
// A remote client (the Android app, a chat relay) may be where the user
// decides, so an approval_request event carries what is being approved in
// full: the whole command and, for a file change, the whole unified diff, not
// a preview. A person must be able to trust what they approve, and a command
// cut short can look harmless when its end is not.
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
// with its size and truncation flag (see the comment above).
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
	return data
}

// putApprovalText stores text under key, cut to MaxApprovalFieldBytes with an
// explicit note, and records key_bytes and key_truncated.
func putApprovalText(data map[string]any, key, text string) {
	clipped, cut := ClipApprovalText(text, MaxApprovalFieldBytes)
	data[key] = clipped
	data[key+"_bytes"] = len(text)
	data[key+"_truncated"] = cut
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
