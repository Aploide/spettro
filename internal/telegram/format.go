package telegram

import "strings"

// MaxMessageLen is the Telegram-side hard limit for a single sendMessage
// call. We keep a comfortable margin below 4096 to leave room for the
// "...(continued)" suffix when we split very large outputs.
const MaxMessageLen = 3800

// SplitForTelegram chunks a long text into pieces that each fit inside a
// single Bot API sendMessage call, splitting at line/word boundaries
// whenever possible. The order of the returned chunks matches the input.
//
// Each non-final chunk is suffixed with "... (continued)" and each
// non-first chunk is prefixed with "(...cont)" so the user can tell that
// what they see is a fragment of a larger message.
func SplitForTelegram(text string) []string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	if len(text) <= MaxMessageLen {
		return []string{text}
	}
	const (
		contSuffix = "\n... (continued)"
		contPrefix = "(...cont)\n"
	)
	chunks := splitWithBudget(text, MaxMessageLen-len(contSuffix))
	if len(chunks) == 0 {
		return nil
	}
	out := make([]string, 0, len(chunks))
	for i, c := range chunks {
		piece := c
		if i > 0 {
			piece = contPrefix + piece
		}
		if i != len(chunks)-1 {
			piece = piece + contSuffix
		}
		out = append(out, piece)
	}
	return out
}

// splitWithBudget greedily slices text into pieces of at most budget bytes,
// preferring to break on newlines, then spaces, then mid-word.
func splitWithBudget(text string, budget int) []string {
	if budget <= 0 {
		return nil
	}
	var out []string
	for len(text) > budget {
		cut := findCut(text, budget)
		chunk := strings.TrimRight(text[:cut], " \t\n")
		if chunk == "" {
			// Avoid emitting empty chunks if the text starts with a long
			// run of whitespace; advance by one rune-ish to make progress.
			chunk = text[:1]
			cut = 1
		}
		out = append(out, chunk)
		text = strings.TrimLeft(text[cut:], " \t\n")
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// findCut returns the largest split position <= budget that lands on a
// newline (preferred) or whitespace boundary. Falls back to budget exactly
// when no good boundary exists.
func findCut(text string, budget int) int {
	if budget >= len(text) {
		return len(text)
	}
	if idx := strings.LastIndex(text[:budget], "\n"); idx >= budget/2 {
		return idx + 1
	}
	if idx := strings.LastIndex(text[:budget], " "); idx >= budget/2 {
		return idx + 1
	}
	return budget
}

// Truncate clamps text to a single Bot API message, appending an ellipsis
// when content is dropped. Used for previews like user prompts in event
// notifications where we never want to split into multiple sends.
func Truncate(text string, max int) string {
	if max <= 0 || len(text) <= max {
		return text
	}
	if max <= 1 {
		return text[:max]
	}
	return text[:max-1] + "…"
}

// Prefix renders an emoji/text tag at the start of a Telegram message. It is
// applied AFTER chunking so multi-chunk messages all share the same prefix.
func Prefix(tag, body string) string {
	tag = strings.TrimSpace(tag)
	body = strings.TrimSpace(body)
	switch {
	case tag == "":
		return body
	case body == "":
		return tag
	default:
		return tag + " " + body
	}
}
