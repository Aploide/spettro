package agent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"spettro/internal/jobs"
)

// offloadFloor is the minimum output size (in bytes, ~500 tokens) above which
// a tool result is persisted to the spool at execution time so compaction can
// later replace the in-context copy with a reference stub (see
// internal/compact stage 1) without losing information.
const offloadFloor = 2000

// Pi-style dual firebreaks for tool output in model history. Whichever limit
// is hit first wins. Matches packages/agent truncate.ts defaults.
const (
	defaultMaxOutputBytes = 50 * 1024
	defaultMaxOutputLines = 2000
)

// spoolFooterIDRe extracts the spool ID from the deterministic truncation
// footer written by spoolTruncate, so already-spooled outputs are not written
// to disk a second time by ensureSpooled.
var spoolFooterIDRe = regexp.MustCompile(`job-output \{"job_id":"(spool:\d+)"`)

// ensureSpooled guarantees that a tool result over the offload floor has a
// spool file backing it and returns the spool ID ("" for small outputs or on
// spool failure — offloading is best-effort). Outputs already truncated by
// spoolResult carry their ID in the footer (the spool holds the full,
// untruncated text); everything else is written as-is, which is the complete
// output since it was never cut.
func ensureSpooled(out string) string {
	if len(out) <= offloadFloor {
		return ""
	}
	if m := spoolFooterIDRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	id, err := jobs.Spool().Add(out)
	if err != nil {
		return ""
	}
	return id
}

// spoolFooterReserve is the budget slice held back for the truncation footer
// so the assembled result never exceeds the tool's history budget (downstream
// history truncation would otherwise cut the footer off).
const spoolFooterReserve = 200

// spoolResult enforces the per-tool history budget on a tool's output. Small
// outputs pass through untouched; oversized outputs are written in full to the
// session spool and replaced by their head (plus, for shell output, the tail)
// with a footer telling the model how to page the rest via job-output.
func (r *toolRuntime) spoolResult(toolName, out string) string {
	keepTail := toolName == "shell-exec" || toolName == "bash" || toolName == "pty-start" || toolName == "pty-write"
	return spoolIfLarge(out, r.historyLimit(toolName), keepTail)
}

func spoolIfLarge(out string, budget int, keepTail bool) string {
	if budget <= 0 {
		budget = defaultMaxOutputBytes
	}
	if !needsTruncate(out, budget, defaultMaxOutputLines) {
		return out
	}
	id, err := jobs.Spool().Add(out)
	if err != nil {
		// Spooling is best-effort; fall back to plain truncation.
		return truncateToBudget(out, budget, defaultMaxOutputLines, keepTail)
	}
	return spoolTruncate(out, budget, keepTail, id)
}

func needsTruncate(out string, budget, maxLines int) bool {
	if budget > 0 && len(out) > budget {
		return true
	}
	if maxLines > 0 && countLines(out) > maxLines {
		return true
	}
	return false
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if strings.HasSuffix(s, "\n") {
		return n
	}
	return n + 1
}

// spoolTruncate keeps the head (and, when keepTail is set, the tail) of out
// within budget and inserts a footer pointing at the spool. The cut points are
// a pure function of (out, budget, keepTail), so truncation is deterministic
// for a given output and prompt-cache prefixes stay stable.
func spoolTruncate(out string, budget int, keepTail bool, id string) string {
	bodyBudget := budget - spoolFooterReserve
	if bodyBudget < 0 {
		bodyBudget = 0
	}
	headBudget := bodyBudget
	tailBudget := 0
	headLines := defaultMaxOutputLines
	tailLines := 0
	if keepTail {
		tailBudget = bodyBudget / 4
		headBudget -= tailBudget
		tailLines = defaultMaxOutputLines / 4
		headLines -= tailLines
	}
	if headBudget < 0 {
		headBudget = 0
	}
	if headLines < 1 {
		headLines = 1
	}

	head := takeHead(out, headBudget, headLines)
	tail := ""
	if keepTail && tailBudget > 0 {
		tail = takeTail(out, tailBudget, tailLines)
		if len(head)+len(tail) >= len(out) {
			// Head+tail cover the whole output — nothing omitted.
			return out
		}
	}

	totalLines := countLines(out)
	omittedLines := totalLines - countLines(head) - countLines(tail)
	if omittedLines < 0 {
		omittedLines = 0
	}

	footer := fmt.Sprintf(
		"[truncated: %s of %s lines omitted; use job-output {\"job_id\":%q,\"offset\":%d} to read more]",
		groupDigits(omittedLines), groupDigits(totalLines), id, len(head))

	if tail == "" {
		return head + footer
	}
	return head + footer + "\n" + tail
}

func truncateToBudget(out string, budget, maxLines int, keepTail bool) string {
	if keepTail {
		return takeTail(out, budget, maxLines)
	}
	return takeHead(out, budget, maxLines)
}

// takeHead keeps complete lines from the start until byte or line limit.
func takeHead(out string, maxBytes, maxLines int) string {
	if maxBytes <= 0 || maxLines <= 0 {
		return ""
	}
	if !needsTruncate(out, maxBytes, maxLines) {
		return out
	}
	var b strings.Builder
	lines := 0
	start := 0
	for start < len(out) && lines < maxLines {
		nl := strings.IndexByte(out[start:], '\n')
		end := len(out)
		if nl >= 0 {
			end = start + nl + 1
		}
		line := out[start:end]
		if b.Len()+len(line) > maxBytes {
			break
		}
		b.WriteString(line)
		lines++
		start = end
		if nl < 0 {
			break
		}
	}
	return b.String()
}

// takeTail keeps complete lines from the end until byte or line limit.
func takeTail(out string, maxBytes, maxLines int) string {
	if maxBytes <= 0 || maxLines <= 0 || out == "" {
		return ""
	}
	if !needsTruncate(out, maxBytes, maxLines) {
		return out
	}
	// Work backwards by lines without allocating the full split when possible.
	trimmed := strings.TrimRight(out, "\n")
	parts := strings.Split(trimmed, "\n")
	var kept []string
	bytes := 0
	for i := len(parts) - 1; i >= 0 && len(kept) < maxLines; i-- {
		line := parts[i]
		add := len(line)
		if len(kept) > 0 {
			add++ // newline joiner
		}
		if bytes+add > maxBytes {
			break
		}
		kept = append(kept, line)
		bytes += add
	}
	if len(kept) == 0 {
		// Single oversized line: take a UTF-8-safe suffix.
		return suffixBytes(out, maxBytes)
	}
	// Reverse kept (it was collected end→start).
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, "\n")
}

func suffixBytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// groupDigits formats n with thousands separators (12400 -> "12,400").
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
