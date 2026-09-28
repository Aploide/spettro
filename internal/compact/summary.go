package compact

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"spettro/internal/provider"
)

// SummaryHeader opens the synthetic user turn that holds a compaction
// summary. Compaction recognizes it on later passes (an earlier summary is
// merged into the new one, never kept as a "user message").
const SummaryHeader = "[earlier progress summarized]"

// summarizerSystem instructs the summarizer model. The sections are the ones
// a coding agent needs to resume work without re-reading the whole session.
const summarizerSystem = `You compact the history of an autonomous coding agent's session so it can keep working with a fresh context window. After compaction the agent sees: the original task (verbatim), your summary, the most recent user messages (verbatim) and its last few tool calls and results (verbatim). Everything else in the transcript you are given is replaced by your summary, so whatever you leave out is lost.

Write the summary in exactly these Markdown sections, terse but complete:

## Goal
What the user wants overall, with every constraint or refinement they added later. Quote exact requirements when the wording matters.

## Decisions and findings
Design decisions and why; approaches tried and rejected (and why); facts learned about the codebase: key files, functions, conventions, commands that work.

## Files modified
One bullet per file created, edited or deleted: path — what changed and why. Mark changes that are unfinished.

## Current state
Build, test and lint status as of the latest run: the exact commands, the exact names of failing tests, error messages and file:line locations. Write "not verified yet" if nothing was run.

## Next steps
The concrete remaining work in order, starting with what the agent was doing when the transcript ends.

## References
Stored outputs worth re-reading (spool:N IDs, with what they contain), URLs and other identifiers needed later. Write "none" if there are none.

Rules: keep exact identifiers, paths, commands and error text; never invent anything that is not in the transcript; merge a previous summary when one is given (keep its facts unless the transcript supersedes them); output only the summary.`

// renderCaps bounds how much of each item the summarizer transcript shows.
type renderCaps struct {
	content, args, result, errResult int
}

var (
	fullCaps = renderCaps{content: 12000, args: 8000, result: 6000, errResult: 10000}
	minCaps  = renderCaps{content: 1500, args: 200, result: 300, errResult: 600}
)

func (c renderCaps) halve() renderCaps {
	return renderCaps{
		content:   max(c.content/2, minCaps.content),
		args:      max(c.args/2, minCaps.args),
		result:    max(c.result/2, minCaps.result),
		errResult: max(c.errResult/2, minCaps.errResult),
	}
}

// transcriptBudget is the character budget of the summarizer prompt: about
// half the window in tokens (the summarizer may be a smaller model than the
// conversation's), within sane bounds.
func transcriptBudget(window int) int {
	return min(max(window*2, 16000), 600000)
}

// summaryPrompt builds the summarizer's user prompt: the original task and
// any earlier summary for context, the deterministic list of modified files,
// and the transcript of middle, shrinking per-item detail until it fits
// budget and dropping the oldest turns only as a last resort.
func summaryPrompt(task provider.Message, middle []provider.Message, focus string, budget int) string {
	var prev []string
	turns := make([]provider.Message, 0, len(middle))
	for _, m := range middle {
		if isSummary(m) {
			prev = append(prev, strings.TrimSpace(strings.TrimPrefix(m.Content, SummaryHeader)))
			continue
		}
		turns = append(turns, m)
	}

	var head strings.Builder
	head.WriteString("<original-task note=\"kept verbatim for the agent; shown here for context\">\n")
	head.WriteString(headTail(task.Content, 4000))
	head.WriteString("\n</original-task>\n\n")
	for _, p := range prev {
		head.WriteString("<previous-summary>\n")
		head.WriteString(p)
		head.WriteString("\n</previous-summary>\n\n")
	}
	if files := modifiedFiles(middle); len(files) > 0 {
		head.WriteString("<files-modified-by-edit-tools>\n")
		head.WriteString(strings.Join(files, "\n"))
		head.WriteString("\n</files-modified-by-edit-tools>\n\n")
	}
	var tail strings.Builder
	tail.WriteString("\nWrite the summary now.")
	if focus = strings.TrimSpace(focus); focus != "" {
		tail.WriteString(" Pay particular attention to: " + focus + ".")
	}

	const open, closing = "<transcript>\n", "</transcript>\n"
	room := budget - head.Len() - tail.Len() - len(open) - len(closing)
	names := callNames(middle)
	caps := fullCaps
	for {
		body := renderTurns(turns, names, caps)
		if len(body) <= room || caps == minCaps {
			if len(body) > room {
				body = dropOldest(turns, names, caps, room)
			}
			return head.String() + open + body + closing + tail.String()
		}
		caps = caps.halve()
	}
}

// dropOldest renders the newest turns that fit in room characters, noting how
// many older turns were left out. Each turn is rendered once and the longest
// fitting suffix is found from suffix lengths, so it stays linear in the size
// of the transcript however many turns it drops.
func dropOldest(turns []provider.Message, names map[string]string, caps renderCaps, room int) string {
	if len(turns) == 0 {
		return ""
	}
	parts := make([]string, len(turns))
	for i, m := range turns {
		parts[i] = renderTurn(m, names, caps)
	}
	// suffix[i] is the rendered length of turns[i:]. It has exactly one entry
	// per part (a running total from the end) rather than len(parts)+1 with a
	// zero sentinel, so the allocation size needs no arithmetic on a length
	// that comes from the transcript (CodeQL go/allocation-size-overflow).
	suffix := make([]int, len(parts))
	total := 0
	for i := len(parts) - 1; i >= 0; i-- {
		total += len(parts[i])
		suffix[i] = total
	}
	for skip := 1; skip < len(parts); skip++ {
		note := omittedNote(skip)
		if len(note)+suffix[skip] <= room {
			return note + strings.Join(parts[skip:], "")
		}
	}
	return omittedNote(len(parts)-1) + headTail(parts[len(parts)-1], max(room, 1000))
}

func omittedNote(n int) string {
	return fmt.Sprintf("[%d older turns omitted]\n", n)
}

// renderTurns writes turns as a plain-text transcript for the summarizer.
func renderTurns(turns []provider.Message, names map[string]string, caps renderCaps) string {
	var sb strings.Builder
	for _, m := range turns {
		writeTurn(&sb, m, names, caps)
	}
	return sb.String()
}

// renderTurn renders one turn of the summarizer transcript.
func renderTurn(m provider.Message, names map[string]string, caps renderCaps) string {
	var sb strings.Builder
	writeTurn(&sb, m, names, caps)
	return sb.String()
}

func writeTurn(sb *strings.Builder, m provider.Message, names map[string]string, caps renderCaps) {
	if m.Content != "" {
		fmt.Fprintf(sb, "[%s]\n%s\n", m.Role, headTail(m.Content, caps.content))
	}
	for _, tc := range m.ToolCalls {
		fmt.Fprintf(sb, "[tool call %s] %s\n", tc.Name, headTail(string(tc.Args), caps.args))
	}
	for _, tr := range m.ToolResults {
		name := tr.Name
		if name == "" {
			name = names[tr.ID]
		}
		status, limit := "ok", caps.result
		if tr.IsErr {
			status, limit = "error", caps.errResult
		}
		ref := ""
		if tr.SpoolID != "" {
			ref = ", full output stored as " + tr.SpoolID
		}
		fmt.Fprintf(sb, "[tool result %s, %s%s]\n%s\n", name, status, ref, headTail(tr.Output, limit))
	}
}

// callNames maps call IDs to tool names, for results that don't carry one.
func callNames(msgs []provider.Message) map[string]string {
	names := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			names[tc.ID] = tc.Name
		}
	}
	return names
}

// editTools are the tools whose "path" argument names a file they modify.
var editTools = map[string]bool{
	"file-write": true,
	"file-edit":  true,
	"multi-edit": true,
}

// modifiedFiles lists, from the tool log in msgs, every file an edit tool
// changed successfully, in first-touched order, as "- path (tool ×n, ...)".
// It is derived without a model, so the summary's file list can't silently
// lose an edited file.
func modifiedFiles(msgs []provider.Message) []string {
	failed := map[string]bool{}
	for _, m := range msgs {
		for _, tr := range m.ToolResults {
			if tr.IsErr {
				failed[tr.ID] = true
			}
		}
	}
	var order []string
	counts := map[string]map[string]int{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if !editTools[tc.Name] || tc.ArgsError != "" || (tc.ID != "" && failed[tc.ID]) {
				continue
			}
			var a struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(tc.Args, &a) != nil || strings.TrimSpace(a.Path) == "" {
				continue
			}
			if counts[a.Path] == nil {
				counts[a.Path] = map[string]int{}
				order = append(order, a.Path)
			}
			counts[a.Path][tc.Name]++
		}
	}
	out := make([]string, 0, len(order))
	for _, p := range order {
		tools := make([]string, 0, len(counts[p]))
		for t := range counts[p] {
			tools = append(tools, t)
		}
		slices.Sort(tools)
		parts := make([]string, 0, len(tools))
		for _, t := range tools {
			if n := counts[p][t]; n > 1 {
				parts = append(parts, fmt.Sprintf("%s ×%d", t, n))
			} else {
				parts = append(parts, t)
			}
		}
		out = append(out, fmt.Sprintf("- %s (%s)", p, strings.Join(parts, ", ")))
	}
	return out
}

// fallbackSummary is the summary compaction writes without a model, when the
// summarizer fails on a forced compaction (an overflowing context must still
// shrink): the user's messages, the files the edit tools changed, the last
// commands and the last errors, straight from the transcript.
func fallbackSummary(middle []provider.Message) string {
	var sb strings.Builder
	sb.WriteString("(The summarizer was unavailable; this record was extracted from the transcript.)\n")
	var prev, users []string
	for _, m := range middle {
		switch {
		case isSummary(m):
			prev = append(prev, strings.TrimSpace(strings.TrimPrefix(m.Content, SummaryHeader)))
		case isUserText(m):
			users = append(users, "- "+headTail(strings.TrimSpace(m.Content), 600))
		}
	}
	for _, p := range prev {
		sb.WriteString("\n## Earlier summary\n")
		sb.WriteString(p)
		sb.WriteString("\n")
	}
	if len(users) > 0 {
		sb.WriteString("\n## User messages\n")
		sb.WriteString(strings.Join(users, "\n"))
		sb.WriteString("\n")
	}
	if files := modifiedFiles(middle); len(files) > 0 {
		sb.WriteString("\n## Files modified\n")
		sb.WriteString(strings.Join(files, "\n"))
		sb.WriteString("\n")
	}
	names := callNames(middle)
	var cmds, errs, spools []string
	for _, m := range middle {
		for _, tc := range m.ToolCalls {
			if IsShellTool(tc.Name) {
				var a struct {
					Command string `json:"command"`
				}
				if json.Unmarshal(tc.Args, &a) == nil && a.Command != "" {
					cmds = append(cmds, "- "+truncateStr(a.Command, 200))
				}
			}
		}
		for _, tr := range m.ToolResults {
			name := tr.Name
			if name == "" {
				name = names[tr.ID]
			}
			if tr.IsErr {
				errs = append(errs, fmt.Sprintf("- %s: %s", name, headTail(tr.Output, 400)))
			}
			if tr.SpoolID != "" {
				spools = append(spools, fmt.Sprintf("- %s (%s)", tr.SpoolID, name))
			}
		}
	}
	section := func(title string, items []string, keep int) {
		if len(items) == 0 {
			return
		}
		if len(items) > keep {
			items = items[len(items)-keep:]
		}
		sb.WriteString("\n## " + title + "\n")
		sb.WriteString(strings.Join(items, "\n"))
		sb.WriteString("\n")
	}
	section("Last commands run", cmds, 10)
	section("Last errors", errs, 3)
	section("Stored outputs (re-read with tool-output)", spools, 10)
	return strings.TrimSpace(sb.String())
}

// isSummary reports whether m is a compaction summary turn.
func isSummary(m provider.Message) bool {
	return m.Role == provider.RoleUser && strings.HasPrefix(m.Content, SummaryHeader)
}

// isUserText reports whether m is a user message proper: a user turn with
// text and no tool results that is not a compaction summary.
func isUserText(m provider.Message) bool {
	return m.Role == provider.RoleUser && len(m.ToolResults) == 0 && strings.TrimSpace(m.Content) != "" && !isSummary(m)
}

// IsShellTool reports whether name is the shell tool, under its canonical
// name (bash) or a retired one (shell-exec, bash-output). The summary lists
// the commands those calls ran. compact cannot import the agent package
// that owns the retired-name table (agent imports compact), so the names
// are written out here; a test in internal/agent checks that they match
// agent.LegacyToolNames("bash").
func IsShellTool(name string) bool {
	switch name {
	case "bash", "shell-exec", "bash-output":
		return true
	}
	return false
}

// headTail shortens s to about n bytes, keeping the first two thirds and the
// last third (errors and results usually sit at the end of an output) with a
// marker saying how much was cut. Cuts land on UTF-8 boundaries.
func headTail(s string, n int) string {
	if len(s) <= n || n <= 0 {
		return s
	}
	h := runeFloor(s, n*2/3)
	t := runeCeil(s, len(s)-n/3)
	if t <= h {
		return s
	}
	return s[:h] + fmt.Sprintf("\n…[%d chars omitted]…\n", t-h) + s[t:]
}

func runeFloor(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

func runeCeil(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncateStr(strings.TrimSpace(s), 80)
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n \t")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return truncateStr(strings.TrimSpace(s), 80)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:runeFloor(s, n)] + "…"
}
