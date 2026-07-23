package agent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"spettro/internal/jobs"
)

// offloadFloor is the minimum output size (in bytes, ~500 tokens) above which
// a tool result is persisted to the spool at execution time so compaction can
// later replace the in-context copy with a reference stub (see
// internal/compact stage 1) without losing information.
const offloadFloor = 2000

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
	if budget <= 0 || len(out) <= budget {
		return out
	}
	id, err := jobs.Spool().Add(out)
	if err != nil {
		// Spooling is best-effort; fall back to plain truncation.
		return truncate(out, budget)
	}
	return spoolTruncate(out, budget, keepTail, id)
}

// spoolTruncate keeps the head (and, when keepTail is set, the tail) of out
// within budget and inserts a footer pointing at the spool. The cut points are
// a pure function of (out, budget, keepTail), so truncation is deterministic
// for a given output and prompt-cache prefixes stay stable.
func spoolTruncate(out string, budget int, keepTail bool, id string) string {
	headBudget := budget - spoolFooterReserve
	tailBudget := 0
	if keepTail {
		tailBudget = headBudget / 4
		headBudget -= tailBudget
	}
	if headBudget < 0 {
		headBudget = 0
	}

	head := out[:min(headBudget, len(out))]
	// Snap to line boundaries so we never hand the model half a line.
	if i := strings.LastIndexByte(head, '\n'); i > 0 {
		head = head[:i+1]
	}
	tail := ""
	if tailBudget > 0 && len(out)-tailBudget > len(head) {
		tail = out[len(out)-tailBudget:]
		if i := strings.IndexByte(tail, '\n'); i >= 0 && i < len(tail)-1 {
			tail = tail[i+1:]
		}
	}

	totalLines := strings.Count(out, "\n") + 1
	omitted := out[len(head) : len(out)-len(tail)]
	omittedLines := strings.Count(omitted, "\n")
	if len(omitted) > 0 && !strings.HasSuffix(omitted, "\n") && tail == "" {
		omittedLines++
	}

	footer := fmt.Sprintf(
		"[truncated: %s of %s lines omitted; use job-output {\"job_id\":%q,\"offset\":%d} to read more]",
		groupDigits(omittedLines), groupDigits(totalLines), id, len(head))

	if tail == "" {
		return head + footer
	}
	return head + footer + "\n" + tail
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
