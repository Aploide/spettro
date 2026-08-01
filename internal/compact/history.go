package compact

import (
	"context"
	"fmt"
	"strings"

	"spettro/internal/budget"
	"spettro/internal/provider"
)

// SendFunc issues one summarization request. Callers decide the routing
// (internal utility model, fallback chain, plain active model) so this
// package stays free of provider-selection policy.
type SendFunc func(ctx context.Context, req provider.Request) (provider.Response, error)

// EstimateHistoryTokens approximates the request tokens a conversation will
// occupy, counting message content plus tool calls and tool results — the
// same accounting the in-loop compaction uses, so pre-turn checks and in-loop
// checks agree.
func EstimateHistoryTokens(system string, msgs []provider.Message) int {
	allContent := make([]string, 0, 1+len(msgs))
	allContent = append(allContent, system)
	for _, m := range msgs {
		allContent = append(allContent, m.Content)
		for _, tc := range m.ToolCalls {
			allContent = append(allContent, tc.Name, string(tc.Args))
		}
		for _, tr := range m.ToolResults {
			allContent = append(allContent, tr.Output)
		}
	}
	return budget.EstimateTokens(allContent...)
}

// CompactHistory summarizes the older portion of msgs into a single synthetic
// message when the estimated request size approaches the context window (or
// unconditionally when force is set, for explicit /compact). Preserves the
// first user turn (the task) and the most recent turns verbatim, replacing
// the middle with a model-produced summary. Returns the (possibly shortened)
// slice, whether it compacted, and any error.
//
// It applies the default auto-compaction policy (enabled, 85%). Callers that
// carry a user-configured policy should use CompactHistoryWithPolicy.
func CompactHistory(ctx context.Context, send SendFunc, system string, msgs []provider.Message, window int, force bool) ([]provider.Message, bool, error) {
	return CompactHistoryWithPolicy(ctx, send, system, msgs, window, force, Config{}, 0)
}

// CompactHistoryWithPolicy is CompactHistory with an explicit auto-compaction
// policy and the caller's consecutive-failure count. A zero-value cfg means
// "use defaults" (auto enabled at the default threshold); a non-zero cfg is
// honored as-is, so AutoEnabled=false disables the automatic trigger entirely
// (force still works). When failures has reached cfg.MaxFailures the
// automatic trigger pauses, matching Evaluate's semantics.
func CompactHistoryWithPolicy(ctx context.Context, send SendFunc, system string, msgs []provider.Message, window int, force bool, cfg Config, failures int) ([]provider.Message, bool, error) {
	if window <= 0 {
		window = 128000 // sane default so compaction always has a threshold
	}
	if cfg == (Config{}) {
		cfg = Config{AutoEnabled: true}
	}
	if !force {
		if len(msgs) <= 5 {
			return msgs, false, nil
		}
		estimate := EstimateHistoryTokens(system, msgs)
		eval := Evaluate(window, cfg, State{TokensUsed: estimate, ConsecutiveFailures: failures})
		// IsError acts as a backstop trigger only while auto compaction is on
		// and not paused after repeated failures; with the off switch set, the
		// run proceeds untouched (the budget validator's forced compaction
		// remains the last line of defense).
		if !eval.ShouldAutoCompact && !(eval.AutoDisabledReason == "" && eval.IsError) {
			return msgs, false, nil
		}
	}

	// Keep the first user turn (task) and the last K turns verbatim. A forced
	// compact keeps a shorter tail so an explicit /compact frees space even in
	// mid-sized conversations.
	keepLast := 4
	if force {
		keepLast = 2
	}
	cutEnd := len(msgs) - keepLast
	if cutEnd <= 1 {
		return msgs, false, nil
	}
	// Never split an assistant ToolCalls message from its following user
	// ToolResults message. Move the boundary forward (into the kept tail)
	// until it lands on a safe cut point.
	for cutEnd > 1 {
		if len(msgs[cutEnd-1].ToolCalls) > 0 && cutEnd < len(msgs) {
			cutEnd++
			continue
		}
		break
	}
	if cutEnd <= 1 || cutEnd >= len(msgs)+1 {
		return msgs, false, nil
	}
	if cutEnd <= 1 {
		return msgs, false, nil
	}

	// Stage 1 — lossless-by-reference: replace oversized tool results in the
	// middle with short stubs pointing at their spool files (the full output
	// stays re-readable via tool-output). If that alone brings the estimate
	// under the auto-compact threshold, stop before spending a summarizer
	// call; an explicit /compact (force) always proceeds to stage 2.
	msgs, offloaded := offloadToolResults(msgs, cutEnd)
	if offloaded > 0 && !force {
		estimate := EstimateHistoryTokens(system, msgs)
		eval := Evaluate(window, cfg, State{TokensUsed: estimate, ConsecutiveFailures: failures})
		if !eval.ShouldAutoCompact && !eval.IsError {
			return msgs, true, nil
		}
	}

	middle := msgs[1:cutEnd]
	if len(middle) == 0 {
		return msgs, false, nil
	}

	var sb strings.Builder
	sb.WriteString("Summarize this portion of an autonomous coding session. Preserve every decision, file changed, command run and its result, and all remaining work. Some tool results appear as [offloaded: ...] stubs referencing a stored output ID; carry those stubs (including the exact tool-output id) into the summary verbatim so the outputs stay re-readable. Output only the summary.\n\n")
	for _, m := range middle {
		sb.WriteString("--- turn ---\n")
		sb.WriteString(fmt.Sprintf("role: %s\n", m.Role))
		if m.Content != "" {
			sb.WriteString("content:\n")
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
		for _, tc := range m.ToolCalls {
			sb.WriteString(fmt.Sprintf("tool_call: %s args=%s\n", tc.Name, truncateStr(string(tc.Args), 200)))
		}
		for _, tr := range m.ToolResults {
			sb.WriteString(fmt.Sprintf("tool_result[%s]: %s\n", tr.Name, truncateStr(tr.Output, 500)))
		}
	}

	resp, err := send(ctx, provider.Request{Prompt: sb.String(), MaxTokens: 0})
	if err != nil {
		return msgs, false, fmt.Errorf("compaction summarizer: %w", err)
	}
	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		return msgs, false, fmt.Errorf("compaction: empty summary")
	}
	out := make([]provider.Message, 0, 2+keepLast)
	out = append(out, msgs[0])
	out = append(out, provider.Message{
		Role:    provider.RoleUser,
		Content: "[earlier progress summarized]\n" + summary,
	})
	out = append(out, msgs[cutEnd:]...)
	return out, true, nil
}

// offloadFloor is the minimum tool-result size (bytes, ~500 tokens) worth
// replacing with a reference stub. It matches the execution-time spooling
// floor in internal/agent, so any result this large has a spool file backing
// it whenever SpoolID is set.
const offloadFloor = 2000

// offloadToolResults replaces every spool-backed tool result larger than the
// floor in msgs[1:cutEnd] with a short stub telling the model how to re-read
// the full output via the tool-output tool. The input slice is not mutated:
// changed messages are copied. Returns the (possibly new) slice and the
// number of results offloaded.
func offloadToolResults(msgs []provider.Message, cutEnd int) ([]provider.Message, int) {
	// Args digests for the stubs come from the assistant turns' tool calls,
	// keyed by call ID.
	argsByID := map[string]string{}
	for _, m := range msgs[:cutEnd] {
		for _, tc := range m.ToolCalls {
			argsByID[tc.ID] = truncateStr(string(tc.Args), 120)
		}
	}
	offloaded := 0
	out := msgs
	for i := 1; i < cutEnd; i++ {
		changed := false
		for _, tr := range msgs[i].ToolResults {
			if tr.SpoolID != "" && len(tr.Output) > offloadFloor && !strings.HasPrefix(tr.Output, "[offloaded:") {
				changed = true
			}
		}
		if !changed {
			continue
		}
		if len(out) == len(msgs) && &out[0] == &msgs[0] {
			out = make([]provider.Message, len(msgs))
			copy(out, msgs)
		}
		trs := make([]provider.ToolResult, len(msgs[i].ToolResults))
		copy(trs, msgs[i].ToolResults)
		for j, tr := range trs {
			if tr.SpoolID == "" || len(tr.Output) <= offloadFloor || strings.HasPrefix(tr.Output, "[offloaded:") {
				continue
			}
			trs[j].Output = offloadStub(tr, argsByID[tr.ID])
			offloaded++
		}
		out[i].ToolResults = trs
	}
	return out, offloaded
}

// offloadStub renders the replacement text for an offloaded tool result. It
// keeps just enough (tool, args digest, size, status, first/last line) for
// the model to judge whether re-reading the full output is worth a call. The
// whole stub stays well under the summarizer's 500-char result truncation so
// the tool-output ID always survives into the summary prompt.
func offloadStub(tr provider.ToolResult, argsDigest string) string {
	status := "ok"
	if tr.IsErr {
		status = "error"
	}
	lines := strings.Count(tr.Output, "\n") + 1
	head := firstLine(tr.Output)
	tail := lastLine(tr.Output)
	var sb strings.Builder
	fmt.Fprintf(&sb, "[offloaded: re-read with tool-output {\"id\":%q}] %s", tr.SpoolID, tr.Name)
	if argsDigest != "" {
		fmt.Fprintf(&sb, " args=%s", argsDigest)
	}
	fmt.Fprintf(&sb, " — %d chars, %d lines, status %s", len(tr.Output), lines, status)
	if head != "" {
		fmt.Fprintf(&sb, ", head: %q", head)
	}
	if tail != "" && tail != head {
		fmt.Fprintf(&sb, ", tail: %q", tail)
	}
	return sb.String()
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
	return s[:n] + "…"
}
