package tui

// Render pipeline benchmarks. They correspond to the performance plan's TUI
// harness (unit B): BenchmarkStreamChunk/1kmsg_200tools (a streamed token:
// Update and View), BenchmarkToolTrace1k (a running/done tool trace pair),
// TestToggleLatency (ctrl+o at 1k/200: the first frame after the key) and
// BenchmarkViewOnly (a frame with nothing changed). The session is the
// harness's: 1,000 messages, 200 of them tool calls with 800 lines of output
// (every fourth a file edit with a 300-line diff), side panel open, 180x50.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
)

func benchMarkdown(i, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Step %d\n\nHere is **what I found** in `internal/tui/state_render.go` and why it matters.\n\n", i)
	for b.Len() < n {
		b.WriteString("- The renderer walks every message and re-wraps the transcript, see [docs](https://example.com/x).\n")
		b.WriteString("\n```go\nfunc hot() { for i := 0; i < n; i++ { s += x } }\n```\n\n")
		b.WriteString("| col a | col b |\n|---|---|\n| `x` | 1 |\n\n")
		b.WriteString("Plain paragraph text that goes on describing the change in some detail, long enough to wrap.\n")
	}
	return b.String()
}

func benchSession(b *testing.B) Model {
	b.Helper()
	m := footerModel(180, 50)
	m.showSidePanel = true
	var out, diffText strings.Builder
	for i := 0; i < 800; i++ {
		fmt.Fprintf(&out, "%05d: some tool output line with path/to/file_%d.go:%d\n", i, i%97, i)
	}
	diffText.WriteString("--- a/x.go\n+++ b/x.go\n@@ -1,100 +1,100 @@\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&diffText, "%sline %d\n", [3]string{" ", "-", "+"}[i%3], i)
	}
	tools := 0
	for i := 0; i < 1000; i++ {
		switch i % 3 {
		case 0:
			m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("please look at item %d", i)})
		case 1:
			m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: benchMarkdown(i, 1500), Meta: "model · 1.2k tok"})
		default:
			m.messages = append(m.messages, ChatMessage{Role: RoleSystem, Content: fmt.Sprintf("note %d", i)})
		}
		if i%5 == 0 && tools < 200 {
			tools++
			ti := ToolItem{Name: "bash", Status: "success", Args: fmt.Sprintf(`{"command":"go test -run X%d"}`, i), Output: out.String(), Seq: tools}
			if tools%4 == 0 {
				ti = ToolItem{Name: "file-edit", Status: "success", Args: fmt.Sprintf(`{"path":"x%d.go"}`, i), Output: "ok", Diff: diffText.String(), Seq: tools}
			}
			m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Kind: "tool-stream", Tools: []ToolItem{ti}})
			m.upsertActivity(activityItem{Key: fmt.Sprint("k", i), Kind: "tool", ID: ti.Name, Title: ti.Name, Detail: ti.Args, Status: "done"})
		}
	}
	m = m.recalcLayout()
	// Steady state: the whole transcript rendered, as after a budgeted fill.
	m.vp.SetBlocks(m.renderTranscriptBlocks())
	m.vp.GotoBottom()
	nm, _ := m.Update(tea.FocusMsg{}) // creates the frame memo
	return nm.(Model)
}

func BenchmarkRenderStreamChunk(b *testing.B) {
	m := benchSession(b)
	m.thinking = true
	m.agentStartAt = time.Now()
	m.SetStreamChForTesting()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nm, _ := m.Update(streamChunkMsg{chunk: agent.StreamChunk{Kind: agent.StreamKindAnswer, Delta: " tok"}})
		m = nm.(Model)
		benchSink = m.View().Content
	}
}

func BenchmarkRenderToolTracePair(b *testing.B) {
	m := benchSession(b)
	m.thinking = true
	m.agentStartAt = time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr := agent.ToolTrace{Name: "bash", Status: "running", Args: fmt.Sprintf(`{"command":"seq %d"}`, i)}
		nm, _ := m.Update(toolProgressMsg{trace: tr})
		m = nm.(Model)
		benchSink = m.View().Content
		tr.Status, tr.Output = "success", "1\n2\n3"
		nm, _ = m.Update(toolProgressMsg{trace: tr})
		m = nm.(Model)
		benchSink = m.View().Content
	}
}

// BenchmarkRenderToggleFirstFrame is ctrl+o and back: the Update and the
// first frame, with the budgeted fill of the rest left to later frames.
func BenchmarkRenderToggleFirstFrame(b *testing.B) {
	m := benchSession(b)
	key := tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nm, _ := m.Update(key)
		m = nm.(Model)
		benchSink = m.View().Content
	}
}

func BenchmarkRenderIdleFrame(b *testing.B) {
	m := benchSession(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink = m.View().Content
	}
}
