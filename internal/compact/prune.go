package compact

import (
	"encoding/json"
	"fmt"
	"strings"

	"spettro/internal/budget"
	"spettro/internal/provider"
)

// Pruning is compaction's cheap first stage (OpenCode-style): old, large
// tool outputs are replaced in place by a short stub instead of summarizing
// the conversation. It needs no model call, keeps every turn and every
// tool call/result pairing intact, and the full output of a spooled result
// stays re-readable with the tool-output tool, so it is often all a long
// tool-heavy run needs.

// offloadFloor is the minimum tool-result size (bytes, ~500 tokens) worth
// replacing with a stub. It matches the execution-time spooling floor in
// internal/agent, so any result this large has a spool file backing it
// whenever SpoolID is set.
const offloadFloor = 2000

// argFloor is the minimum length of a string inside an old tool call's
// arguments (file-write content, file-edit old/new text) worth eliding.
const argFloor = 2000

// elidedPrefix opens every pruned tool output; offloadedPrefix is the stub
// prefix earlier versions wrote. Outputs starting with either are never
// pruned again.
const (
	elidedPrefix    = "[output elided:"
	offloadedPrefix = "[offloaded:"
)

// pruneTier selects how aggressive a pruning pass is.
type pruneTier int

const (
	// pruneSpooled replaces spool-backed outputs only (lossless by
	// reference), sparing the most recent outputs up to the protect budget.
	pruneSpooled pruneTier = iota
	// pruneAll also replaces outputs with no spool copy (keeping a head and
	// tail excerpt) and elides large strings in old tool-call arguments,
	// sparing nothing before the verbatim tail.
	pruneAll
)

// protectTokens is how many tokens of the most recent tool output the
// spooled tier leaves alone, beyond the verbatim tail: about a fifth of the
// window, between 4k and 40k tokens (OpenCode protects 40k).
func protectTokens(window int) int {
	return min(max(window/5, 4000), 40000)
}

func isPruned(output string) bool {
	return strings.HasPrefix(output, elidedPrefix) || strings.HasPrefix(output, offloadedPrefix)
}

// pruneToolOutputs replaces large tool outputs in msgs[1:end] with stubs
// according to tier. For pruneSpooled the most recent outputs before end
// totalling up to protect tokens are spared. The input slice is not mutated:
// changed messages are copied. Returns the (possibly new) slice and the
// number of outputs and arguments replaced.
func pruneToolOutputs(msgs []provider.Message, end int, tier pruneTier, protect int) ([]provider.Message, int) {
	end = min(end, len(msgs))
	if end <= 1 {
		return msgs, 0
	}
	// Spare the newest outputs first: walk back from the cut, summing output
	// sizes, and prune only at or before the message where the running total
	// passes the protect budget.
	pruneBefore := end
	if tier == pruneSpooled && protect > 0 {
		used := 0
		pruneBefore = 1
		for i := end - 1; i >= 1; i-- {
			for _, tr := range msgs[i].ToolResults {
				used += budget.EstimateTokens(tr.Output)
			}
			if used > protect {
				pruneBefore = i + 1
				break
			}
		}
	}

	// Stubs carry an args digest, keyed by call ID.
	argsByID := map[string]string{}
	for _, m := range msgs[:end] {
		for _, tc := range m.ToolCalls {
			argsByID[tc.ID] = truncateStr(string(tc.Args), 120)
		}
	}
	pruned := 0
	out := msgs
	// cow copies the message slice before the first change.
	cow := func() {
		if &out[0] == &msgs[0] {
			out = make([]provider.Message, len(msgs))
			copy(out, msgs)
		}
	}
	for i := 1; i < pruneBefore; i++ {
		m := msgs[i]
		if len(m.ToolResults) > 0 {
			var trs []provider.ToolResult
			for j, tr := range m.ToolResults {
				if !prunable(tr, tier) {
					continue
				}
				if trs == nil {
					trs = make([]provider.ToolResult, len(m.ToolResults))
					copy(trs, m.ToolResults)
				}
				trs[j].Output = elisionStub(tr, argsByID[tr.ID])
				pruned++
			}
			if trs != nil {
				cow()
				out[i].ToolResults = trs
			}
		}
		if tier == pruneAll && len(m.ToolCalls) > 0 {
			var tcs []provider.NativeTool
			for j, tc := range m.ToolCalls {
				args, ok := elideArgs(tc.Args)
				if !ok {
					continue
				}
				if tcs == nil {
					tcs = make([]provider.NativeTool, len(m.ToolCalls))
					copy(tcs, m.ToolCalls)
				}
				tcs[j].Args = args
				pruned++
			}
			if tcs != nil {
				cow()
				out[i].ToolCalls = tcs
			}
		}
	}
	return out, pruned
}

func prunable(tr provider.ToolResult, tier pruneTier) bool {
	if len(tr.Output) <= offloadFloor || isPruned(tr.Output) {
		return false
	}
	return tr.SpoolID != "" || tier == pruneAll
}

// elisionStub renders the replacement text for a pruned tool output. It
// keeps just enough (size, spool ID, tool, args digest, status, first and
// last line) for the model to judge whether re-reading the full output is
// worth a call. A result with no spool copy keeps a longer head and tail
// excerpt instead, since nothing else of it survives.
func elisionStub(tr provider.ToolResult, argsDigest string) string {
	status := "ok"
	if tr.IsErr {
		status = "error"
	}
	lines := strings.Count(tr.Output, "\n") + 1
	var sb strings.Builder
	if tr.SpoolID != "" {
		fmt.Fprintf(&sb, "%s %d chars, %s — re-read with tool-output {\"id\":%q}] %s", elidedPrefix, len(tr.Output), tr.SpoolID, tr.SpoolID, tr.Name)
	} else {
		fmt.Fprintf(&sb, "%s %d chars, no stored copy] %s", elidedPrefix, len(tr.Output), tr.Name)
	}
	if argsDigest != "" {
		fmt.Fprintf(&sb, " args=%s", argsDigest)
	}
	fmt.Fprintf(&sb, " — %d lines, status %s", lines, status)
	if tr.SpoolID == "" {
		sb.WriteString(", excerpt:\n")
		sb.WriteString(headTail(tr.Output, 600))
		return sb.String()
	}
	head, tail := firstLine(tr.Output), lastLine(tr.Output)
	if head != "" {
		fmt.Fprintf(&sb, ", head: %q", head)
	}
	if tail != "" && tail != head {
		fmt.Fprintf(&sb, ", tail: %q", tail)
	}
	return sb.String()
}

// elideArgs replaces every string longer than argFloor inside a tool call's
// JSON arguments with a short marker, keeping the object's shape (so the
// call still replays as valid JSON with the same keys). Reports whether
// anything changed.
func elideArgs(args json.RawMessage) (json.RawMessage, bool) {
	if len(args) <= argFloor {
		return args, false
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return args, false
	}
	v, changed := elideValue(v)
	if !changed {
		return args, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return args, false
	}
	return b, true
}

func elideValue(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		if len(x) > argFloor {
			return fmt.Sprintf("[elided from history: %d chars — the file on disk is authoritative, re-read it]", len(x)), true
		}
	case map[string]any:
		changed := false
		for k, e := range x {
			if ne, ok := elideValue(e); ok {
				x[k] = ne
				changed = true
			}
		}
		return x, changed
	case []any:
		changed := false
		for i, e := range x {
			if ne, ok := elideValue(e); ok {
				x[i] = ne
				changed = true
			}
		}
		return x, changed
	}
	return v, false
}
