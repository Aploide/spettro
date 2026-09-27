package agent

import (
	"log/slog"
	"strings"
	"unicode/utf8"

	"spettro/internal/provider"
)

// Replies that would end a turn without doing anything.
//
// The run loop treats a text reply with no tool calls as the final answer.
// Two kinds of reply end a turn that way without the model meaning to stop,
// and each gets at most one nudge per turn (a user message asking it to go
// on) instead of being accepted:
//
//   - Announce-only: the model says what it is about to do ("I'll start by
//     exploring the repository...") but sends no tool call, and has made
//     none this turn. The round-5 bench lost a Kimi run this way: its whole
//     turn was that one sentence.
//   - Dropped tool call: the provider reports finish reason tool-calls, yet
//     no tool call reached the loop. Something between the model and the
//     loop lost it; asking again is the only recovery.
//
// Both nudges are bounded to one per turn, so a model that keeps answering
// the same way ends the turn on its next reply exactly as it did before
// these nudges existed. Neither can loop.
const (
	announceOnlyNudge    = "You described what you will do but made no tool call. Continue by calling tools, or give your final answer."
	droppedToolCallNudge = "Your last response stopped for a tool call, but no tool call arrived. Send the tool call again, or give your final answer."
)

// announceMaxChars is the longest reply looksLikeAnnouncement considers.
// Real final answers that open with "I'll" or "Let me" are usually longer
// (they report what was done); a bare announcement is one or two sentences.
const announceMaxChars = 300

// announcePrefixes open a sentence that promises work instead of reporting
// it. They are matched case-insensitively at the start of the reply.
var announcePrefixes = []string{
	"i'll ", "i will ", "i'm going to ", "i am going to ", "let me ", "let's ",
	"first, i'll ", "first, let me ", "first i'll ", "first let me ",
	"now i'll ", "now let me ", "next, i'll ", "next i'll ", "i need to ",
	"i'm starting ", "i am starting ", "starting by ",
}

// announceVerbs are the actions an announcement promises: the kind of work
// that needs tools. Requiring one after the prefix keeps ordinary answers
// such as "Let me explain: ..." or "I'll leave it as is, the code is
// correct." from being mistaken for announcements. Entries are stems
// matched at the start of a word, so "explor" covers "explore" and
// "exploring".
var announceVerbs = []string{
	"start", "begin", "explor", "look", "check", "read", "examin", "inspect",
	"investigat", "search", "find", "open", "run", "review", "analy",
	"understand", "fix", "implement", "dig", "scan", "list", "locat",
	"reproduc",
}

// looksLikeAnnouncement reports whether text is a short promise of future
// tool work rather than an answer: at most announceMaxChars characters,
// opening with one of announcePrefixes followed (within the same sentence)
// by a word that starts with one of announceVerbs. The check is deliberately
// narrow; a miss only means the turn ends as it always did.
func looksLikeAnnouncement(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > announceMaxChars {
		return false
	}
	lower := strings.ToLower(text)
	// Typographic apostrophes are common in model output.
	lower = strings.ReplaceAll(lower, "’", "'")
	// "Let me know if you want me to run the tests." closes an answer.
	if strings.HasPrefix(lower, "let me know") {
		return false
	}
	for _, prefix := range announcePrefixes {
		rest, ok := strings.CutPrefix(lower, prefix)
		if !ok {
			continue
		}
		// Only the first clause counts: in "I'll keep it. Running the tests
		// showed nothing new." or "Let me explain: the list is empty" the
		// verb-like words after the break belong to the answer, not to the
		// promise. A trailing "..." is not a break ("Let me check...").
		if i := strings.IndexAny(rest, ".!?:;,\n"); i >= 0 && !strings.HasPrefix(rest[i:], "...") {
			rest = rest[:i]
		}
		// Match verbs at a word start only, so "run" does not match inside
		// "rerun" and "list" not inside "specialist".
		rest = " " + rest
		for _, verb := range announceVerbs {
			if strings.Contains(rest, " "+verb) {
				return true
			}
		}
	}
	return false
}

// droppedToolCall reports whether resp says it stopped for tool calls but
// carries none: the shape of a tool call lost before it reached the loop.
func droppedToolCall(resp provider.Response) bool {
	return len(resp.ToolCalls) == 0 && resp.FinishReason == provider.FinishToolCalls
}

// logDroppedToolCall records, at debug level, that a reply stopped for tool
// calls without carrying any and the loop nudged the model. The reply itself
// was logged just before by logReply, with the raw details.
func logDroppedToolCall(agentID string, step int) {
	slog.Debug("agent reply stopped for tool calls but none arrived; nudging once", "agent", agentID, "step", step)
}

// replyHeadChars bounds the reply text quoted in the debug log.
const replyHeadChars = 160

// logReply records one provider reply at debug level: why it stopped, what
// it carried, and the raw details a dropped tool call would show up in (see
// provider.ResponseDiagnostics). Debug records are discarded unless
// SPETTRO_DEBUG_LOG names a log file (see cmd/spettro), so this costs
// nothing in normal runs and never writes to the terminal.
func logReply(agentID string, step int, resp provider.Response) {
	reasoningChars := 0
	for _, r := range resp.Reasoning {
		reasoningChars += len(r.Text)
	}
	slog.Debug("agent reply",
		"agent", agentID,
		"step", step,
		"finish", string(resp.FinishReason),
		"raw_finish", resp.Diagnostics.RawFinishReason,
		"tool_calls", len(resp.ToolCalls),
		"tool_calls_seen", resp.Diagnostics.ToolCallsSeen,
		"unnamed_tool_calls", resp.Diagnostics.UnnamedToolCalls,
		"orphan_tool_deltas", resp.Diagnostics.OrphanToolDeltas,
		"text_chars", len(resp.Content),
		"reasoning_chars", reasoningChars,
		"output_tokens", resp.Usage.OutputTokens,
		"text_head", truncate(strings.TrimSpace(resp.Content), replyHeadChars),
	)
}
