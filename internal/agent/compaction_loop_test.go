package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	compactpkg "spettro/internal/compact"
	"spettro/internal/provider"
)

// toolHistory is a carried conversation of n file-read exchanges whose
// outputs are size chars each (spooled when spooled is set), ending with the
// previous turn's answer.
func toolHistory(n, size int, spooled bool) []provider.Message {
	msgs := []provider.Message{{Role: provider.RoleUser, Content: "Task:\nthe original task, verbatim"}}
	for i := range n {
		id := fmt.Sprintf("call_h%d", i)
		tr := provider.ToolResult{ID: id, Name: "file-read", Output: fmt.Sprintf("file %d\n", i) + strings.Repeat("x", size)}
		if spooled {
			tr.SpoolID = fmt.Sprintf("spool:%d", i)
		}
		msgs = append(msgs,
			provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.NativeTool{{ID: id, Name: "file-read", Args: json.RawMessage(fmt.Sprintf(`{"path":"f%d.go"}`, i))}}},
			provider.Message{Role: provider.RoleUser, ToolResults: []provider.ToolResult{tr}},
		)
	}
	return append(msgs, provider.Message{Role: provider.RoleAssistant, Content: "previous turn done"})
}

// requestMessages returns the messages of a recorded chat-completions body.
func requestMessages(body map[string]any) []map[string]any {
	raw, _ := body["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// In-loop compaction prunes old spooled outputs before it considers a
// summary: a history whose bulk is old tool output needs no summarizer call.
func TestRunToolLoopPrunesOldOutputsWithoutSummarizer(t *testing.T) {
	pm, url, ls := newLoopServer(t, loopReply{content: "done"})
	cfg := loopCfg(t, pm, url)
	cfg.Messages = toolHistory(12, 20000, true)
	cfg.ContextWindow = 70000
	res, err := runToolLoop(context.Background(), cfg)
	if err != nil || res.content != "done" {
		t.Fatalf("res = %q, err = %v", res.content, err)
	}
	reqs := ls.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 (no summarizer call)", len(reqs))
	}
	sent := fmt.Sprint(reqs[0]["messages"])
	if !strings.Contains(sent, "[output elided: ") || !strings.Contains(sent, `tool-output {"id":"spool:0"}`) {
		t.Fatal("old outputs were not replaced by re-readable stubs")
	}
	if strings.Contains(sent, compactpkg.SummaryHeader) {
		t.Fatal("pruning should have been enough")
	}
	if err := compactpkg.ValidatePairing(res.messages); err != nil {
		t.Fatalf("carried history invalid: %v", err)
	}
	if res.messages[0].Content != cfg.Messages[0].Content {
		t.Fatal("the original task changed")
	}
	// The newest outputs are still verbatim.
	last := res.messages[len(res.messages)-4]
	if len(last.ToolResults) != 1 || strings.HasPrefix(last.ToolResults[0].Output, "[output elided") {
		t.Fatal("the most recent tool output was pruned")
	}
}

// An overflow whose summarizer call fails still recovers: the forced pass
// falls back to a summary extracted from the transcript, keeping the task,
// the current request and valid pairing.
func TestRunToolLoopOverflowRecoversWhenSummarizerFails(t *testing.T) {
	fastRetries(t)
	pm, url, ls := newLoopServer(t,
		loopReply{status: http.StatusBadRequest, errMsg: "This model's maximum context length is 100000 tokens"},
		loopReply{status: http.StatusBadRequest, errMsg: "summarizer rejected the request"},
		loopReply{content: "done"},
	)
	cfg := loopCfg(t, pm, url)
	cfg.Messages = toolHistory(10, 3000, false)
	res, err := runToolLoop(context.Background(), cfg)
	if err != nil || res.content != "done" {
		t.Fatalf("res = %q, err = %v", res.content, err)
	}
	reqs := ls.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want overflow + summarizer + retry", len(reqs))
	}
	msgs := requestMessages(reqs[2])
	var sawSummary, sawTask, sawCurrent bool
	for _, m := range msgs {
		c := fmt.Sprint(m["content"])
		sawSummary = sawSummary || strings.Contains(c, compactpkg.SummaryHeader) && strings.Contains(c, "summarizer was unavailable")
		sawTask = sawTask || strings.Contains(c, "the original task, verbatim")
		sawCurrent = sawCurrent || strings.Contains(c, "do the task")
	}
	if !sawSummary || !sawTask || !sawCurrent {
		t.Fatalf("retried request: summary=%v task=%v current=%v", sawSummary, sawTask, sawCurrent)
	}
	if err := compactpkg.ValidatePairing(res.messages); err != nil {
		t.Fatalf("carried history invalid: %v", err)
	}
}
