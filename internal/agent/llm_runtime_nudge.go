package agent

import (
	"log/slog"
	"strings"
	"unicode"
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
// Both nudges are bounded to one per turn. A second announce-only reply, or
// a second dropped-call reply that carries text, ends the turn exactly as it
// did before these nudges existed. A second dropped-call reply with no text
// is an empty reply, so it goes on to the empty-reply handling in
// runToolLoop (bounded by maxEmptyReplies). Neither nudge can loop.
const (
	announceOnlyNudge    = "You described what you will do but made no tool call. Continue by calling tools, or give your final answer."
	droppedToolCallNudge = "Your last response stopped for a tool call, but no tool call arrived. Send the tool call again, or give your final answer."
)

// announceMaxChars is the longest reply looksLikeAnnouncement considers.
// Real final answers that open with "I'll" or "Let me" are usually longer
// (they report what was done); a bare announcement is one or two sentences.
const announceMaxChars = 300

// announceLeadIns may open the reply ahead of an announce prefix, as in
// "First, let me ..." or "Now I'll ...". They are matched case-insensitively.
var announceLeadIns = []string{"first, ", "first ", "now, ", "now ", "next, ", "next "}

// announcePrefixes open a sentence that promises work instead of reporting
// it. They are matched case-insensitively, after any lead-in.
var announcePrefixes = []string{
	"i'll need to ", "i'll ", "i will ", "i'm going to ", "i am going to ", "let me ", "let's ",
	"i need to ",
}

// startingPrefixes open a sentence in the progressive form ("I'm starting by
// reading ..."). The start verb is already part of the prefix, so what
// follows must say what the start is, exactly as after "I'll start" (see
// followsStart).
var startingPrefixes = []string{"i'm starting ", "i am starting ", "starting "}

// announceFillers are words allowed between the prefix and its verb:
// "I'll first read", "Let me quickly check", "I'll take a look", "I'll go
// ahead and run", "Let me have a look".
var announceFillers = map[string]bool{
	"first": true, "now": true, "quickly": true, "just": true, "also": true,
	"then": true, "briefly": true, "go": true, "ahead": true, "and": true,
	"take": true, "have": true, "a": true,
}

// announceVerbs are the actions an announcement promises: the kind of work
// that needs tools. Entries are stems matched at the start of a word, so
// "explor" covers "explore" and "exploring".
var announceVerbs = []string{
	"start", "begin", "explor", "look", "check", "read", "examin", "inspect",
	"investigat", "search", "find", "open", "run", "review", "analy",
	"understand", "fix", "implement", "dig", "scan", "list", "locat",
	"reproduc",
}

// looksLikeAnnouncement reports whether text is a short promise of future
// tool work rather than an answer or a question. All of these must hold:
//
//   - it is at most announceMaxChars characters long;
//   - it asks the user nothing and waits on nothing from them: it has no '?'
//     and no word "you" ("Could you paste the error?", "I'll run it once
//     you confirm." are legitimate ends of a turn, and nudging them would
//     push the model to act without the answer it asked for);
//   - it has no colon followed by more text, which introduces an answer
//     ("Let me explain: ...", "Let's look at it differently: the function
//     is O(n).");
//   - it opens with one of announcePrefixes (after an optional lead-in), and
//     the first word after the prefix, skipping announceFillers, is one of
//     announceVerbs. "start" and "begin" count only when followed by what is
//     being started ("start by exploring", not "start with the short
//     answer").
//
// Requiring the verb right after the prefix is what keeps "Let me explain
// how to run the tests" or "I'll leave it as is; the code is correct." out:
// a tool-work word later in the sentence does not make it a promise. The
// check is deliberately narrow and English-only; a miss only means the turn
// ends as it always did.
func looksLikeAnnouncement(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > announceMaxChars {
		return false
	}
	lower := strings.ToLower(text)
	// Typographic apostrophes are common in model output.
	lower = strings.ReplaceAll(lower, "’", "'")
	if addressesUser(lower) || strings.Contains(lower, ": ") || strings.Contains(lower, ":\n") {
		return false
	}
	for _, leadIn := range announceLeadIns {
		if rest, ok := strings.CutPrefix(lower, leadIn); ok {
			lower = rest
			break
		}
	}
	for _, prefix := range startingPrefixes {
		if rest, ok := strings.CutPrefix(lower, prefix); ok {
			return followsStart(announceWords(rest))
		}
	}
	for _, prefix := range announcePrefixes {
		if rest, ok := strings.CutPrefix(lower, prefix); ok {
			return promisesToolWork(announceWords(rest))
		}
	}
	return false
}

// addressesUser reports whether lower (lower-cased reply text) asks the user
// something or waits on them: it contains a question mark or the word "you"
// (also as "you'll", "you're", "you've", "you'd"). "Your" alone does not
// count: "Let me look at your config." is an ordinary announcement.
func addressesUser(lower string) bool {
	if strings.Contains(lower, "?") {
		return true
	}
	for _, w := range announceWords(lower) {
		if w == "you" || strings.HasPrefix(w, "you'") {
			return true
		}
	}
	return false
}

// announceWords splits lower-cased text into words: runs of letters and
// apostrophes. Punctuation, digits and path characters are separators.
func announceWords(lower string) []string {
	return strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\''
	})
}

// promisesToolWork reports whether words (the text after an announce prefix)
// start with an announce verb, after skipping announceFillers.
func promisesToolWork(words []string) bool {
	for len(words) > 0 && announceFillers[words[0]] {
		words = words[1:]
	}
	if len(words) == 0 || !isAnnounceVerb(words[0]) {
		return false
	}
	if strings.HasPrefix(words[0], "start") || strings.HasPrefix(words[0], "begin") {
		return followsStart(words[1:])
	}
	return true
}

// followsStart reports whether the words after "start"/"begin" name tool
// work: "by exploring", "with reading", "off by checking" or a verb directly
// ("start exploring"). "Let me start with the short answer" does not.
func followsStart(words []string) bool {
	for len(words) > 0 && (words[0] == "by" || words[0] == "with" || words[0] == "off") {
		words = words[1:]
	}
	return promisesToolWork(words)
}

// isAnnounceVerb reports whether word starts with one of announceVerbs.
func isAnnounceVerb(word string) bool {
	for _, verb := range announceVerbs {
		if strings.HasPrefix(word, verb) {
			return true
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
