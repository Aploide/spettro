package compact

import (
	"fmt"

	"spettro/internal/provider"
)

// missingResultOutput is the synthetic result RepairPairing supplies for a
// tool call whose real result is no longer in the history.
const missingResultOutput = "[tool result not available: it was removed from the history]"

// ValidatePairing reports whether msgs is a history every provider accepts
// as far as tool use goes: it starts with a user turn, every assistant turn
// that issues tool calls is immediately followed by one user turn carrying
// exactly one result per call (matched by call ID), and no tool result
// appears anywhere else. Anthropic and OpenAI both reject a request that
// breaks any of these, so compaction must never produce one.
func ValidatePairing(msgs []provider.Message) error {
	if len(msgs) > 0 && msgs[0].Role != provider.RoleUser {
		return fmt.Errorf("history starts with a %s turn, not a user turn", msgs[0].Role)
	}
	for i, m := range msgs {
		if len(m.ToolCalls) > 0 {
			if m.Role != provider.RoleAssistant {
				return fmt.Errorf("message %d: tool calls on a %s turn", i, m.Role)
			}
			if i+1 >= len(msgs) || len(msgs[i+1].ToolResults) == 0 {
				return fmt.Errorf("message %d: %d tool call(s) with no tool results after them", i, len(m.ToolCalls))
			}
		}
		if len(m.ToolResults) == 0 {
			continue
		}
		if m.Role != provider.RoleUser {
			return fmt.Errorf("message %d: tool results on a %s turn", i, m.Role)
		}
		if i == 0 || len(msgs[i-1].ToolCalls) == 0 {
			return fmt.Errorf("message %d: tool results with no tool calls before them", i)
		}
		if !sameIDs(msgs[i-1].ToolCalls, m.ToolResults) {
			return fmt.Errorf("message %d: tool results do not match the calls of message %d", i, i-1)
		}
	}
	return nil
}

// sameIDs reports whether results answer exactly calls: the same call IDs,
// each as many times (IDs may be empty on backends that don't assign them).
func sameIDs(calls []provider.NativeTool, results []provider.ToolResult) bool {
	if len(calls) != len(results) {
		return false
	}
	want := make(map[string]int, len(calls))
	for _, tc := range calls {
		want[tc.ID]++
	}
	for _, tr := range results {
		if want[tr.ID] == 0 {
			return false
		}
		want[tr.ID]--
	}
	return true
}

// RepairPairing returns msgs with every tool-use pairing problem fixed, so
// ValidatePairing accepts the result whenever msgs starts with a user turn:
// results that answer no call of the preceding assistant turn are dropped (a
// results turn left empty keeps only its text, or goes), duplicate results
// for one call are dropped, and calls left without a result get a synthetic
// error result saying so. A valid history is returned unchanged (same
// backing array); otherwise the input is not mutated.
func RepairPairing(msgs []provider.Message) []provider.Message {
	if ValidatePairing(msgs) == nil {
		return msgs
	}
	out := make([]provider.Message, 0, len(msgs)+2)
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if len(m.ToolResults) > 0 {
			// Reaching a results turn here means no call turn claimed it:
			// orphaned results. Keep any text it carried.
			if m.Role == provider.RoleUser && m.Content != "" {
				m.ToolResults = nil
				m.FileStamps = nil
				out = append(out, m)
			}
			continue
		}
		out = append(out, m)
		if len(m.ToolCalls) == 0 || m.Role != provider.RoleAssistant {
			continue
		}
		var next *provider.Message
		if i+1 < len(msgs) && len(msgs[i+1].ToolResults) > 0 && msgs[i+1].Role == provider.RoleUser {
			next = &msgs[i+1]
			i++
		}
		out = append(out, answerCalls(m.ToolCalls, next))
	}
	return out
}

// answerCalls builds the results turn for calls: the matching results of
// next (in call order), a synthetic error result for any call next does not
// answer, and next's text and file stamps carried over.
func answerCalls(calls []provider.NativeTool, next *provider.Message) provider.Message {
	res := provider.Message{Role: provider.RoleUser}
	byID := map[string][]provider.ToolResult{}
	if next != nil {
		res.Content = next.Content
		res.FileStamps = next.FileStamps
		for _, tr := range next.ToolResults {
			byID[tr.ID] = append(byID[tr.ID], tr)
		}
	}
	res.ToolResults = make([]provider.ToolResult, 0, len(calls))
	for _, tc := range calls {
		if q := byID[tc.ID]; len(q) > 0 {
			res.ToolResults = append(res.ToolResults, q[0])
			byID[tc.ID] = q[1:]
			continue
		}
		res.ToolResults = append(res.ToolResults, provider.ToolResult{ID: tc.ID, Name: tc.Name, Output: missingResultOutput, IsErr: true})
	}
	return res
}

// isToolResults reports whether m is a user turn carrying tool results.
func isToolResults(m provider.Message) bool {
	return len(m.ToolResults) > 0
}

// isToolCall reports whether m is an assistant turn issuing tool calls.
func isToolCall(m provider.Message) bool {
	return m.Role == provider.RoleAssistant && len(m.ToolCalls) > 0
}

// tailStart returns the index where the verbatim tail of msgs begins: at
// least the last minTail messages plus the last keepExchanges tool
// exchanges (an assistant tool-call turn and its results turn), never
// reaching past msgs[0] (the task) and never more than a bounded number of
// messages, so a history whose only tool use is ancient still compacts. The
// returned cut never separates a results turn from its call turn.
func tailStart(msgs []provider.Message, keepExchanges, minTail int) int {
	n := len(msgs)
	start := max(n-minTail, 1)
	floor := max(n-(minTail+3*keepExchanges), 1)
	found := 0
	for i := n - 1; i >= floor && found < keepExchanges; i-- {
		if isToolCall(msgs[i]) {
			found++
			start = min(start, i)
		}
	}
	return safeCut(msgs, start)
}

// safeCut moves a cut index so msgs[start:] does not open with a results
// turn whose call turn would be cut away: back one message to include the
// call when that stays past msgs[0], forward past the results otherwise.
func safeCut(msgs []provider.Message, start int) int {
	start = max(start, 1)
	for start < len(msgs) && isToolResults(msgs[start]) {
		if start-1 >= 1 && isToolCall(msgs[start-1]) {
			return start - 1
		}
		start++
	}
	return min(start, len(msgs))
}
