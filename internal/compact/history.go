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
	if !force {
		if len(msgs) <= 5 {
			return msgs, false, nil
		}
		if cfg == (Config{}) {
			cfg = Config{AutoEnabled: true}
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
	middle := msgs[1:cutEnd]
	if len(middle) == 0 {
		return msgs, false, nil
	}

	var sb strings.Builder
	sb.WriteString("Summarize this portion of an autonomous coding session. Preserve every decision, file changed, command run and its result, and all remaining work. Output only the summary.\n\n")
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

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
