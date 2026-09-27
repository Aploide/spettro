package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"spettro/internal/agent"
)

// cacheModel is a 1k-message transcript with tool calls, rendered once.
func cacheModel(t testing.TB, n int) Model {
	t.Helper()
	m := footerModel(140, 40)
	for i := 0; i < n; i++ {
		switch i % 4 {
		case 0:
			m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("question %d", i)})
		case 1:
			m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Content: fmt.Sprintf("## Answer %d\n\nsome **bold** text and `code`\n\n- a\n- b", i), Meta: "model · 1k tok"})
		case 2:
			m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Kind: "tool-stream", Tools: []ToolItem{{Name: "bash", Status: "success", Args: `{"command":"ls"}`, Output: strings.Repeat("out\n", 30)}}})
		default:
			m.messages = append(m.messages, ChatMessage{Role: RoleSystem, Content: fmt.Sprintf("note %d", i)})
		}
	}
	// Render everything up front: a budgeted refresh may leave placeholders.
	m.vp.SetBlocks(m.renderTranscriptBlocks())
	m.vp.GotoBottom()
	return m
}

// entryPointers snapshots which cache entry each message id maps to.
func entryPointers(m Model) map[uint64]*renderEntry {
	out := make(map[uint64]*renderEntry, len(m.renderCache.entries))
	for id, e := range m.renderCache.entries {
		out[id] = e
	}
	return out
}

// reRendered returns the ids whose entry changed between two snapshots.
func reRendered(before, after map[uint64]*renderEntry) []uint64 {
	var ids []uint64
	for id, e := range after {
		if before[id] != e {
			ids = append(ids, id)
		}
	}
	return ids
}

// Work-count guard: a streamed token re-renders exactly one block, the live
// draft, however long the transcript is.
func TestStreamChunkReRendersOneBlock(t *testing.T) {
	m := cacheModel(t, 400)
	m.thinking = true
	m.SetStreamChForTesting()
	nm, _ := m.Update(streamChunkMsg{chunk: agent.StreamChunk{Kind: agent.StreamKindAnswer, Delta: "first"}})
	m = nm.(Model)
	for i := 0; i < 5; i++ {
		before := entryPointers(m)
		nm, _ = m.Update(streamChunkMsg{chunk: agent.StreamChunk{Kind: agent.StreamKindAnswer, Delta: fmt.Sprintf(" tok%d", i)}})
		m = nm.(Model)
		changed := reRendered(before, entryPointers(m))
		if len(changed) != 1 || changed[0] != m.messages[len(m.messages)-1].id {
			t.Fatalf("chunk %d re-rendered %d blocks (%v), want only the draft", i, len(changed), changed)
		}
	}
	if !strings.Contains(ansi.Strip(m.vp.View()), "tok4") {
		t.Fatal("the streamed token is not on screen")
	}
}

// Work-count guard: a tool trace pair (running, then done) re-renders only
// the tool row it belongs to.
func TestToolTraceReRendersOnlyItsRow(t *testing.T) {
	m := cacheModel(t, 400)
	m.thinking = true
	tr := agent.ToolTrace{Name: "grep", Status: "running", Args: `{"pattern":"x"}`}
	nm, _ := m.Update(toolProgressMsg{trace: tr})
	m = nm.(Model)
	before := entryPointers(m)
	tr.Status, tr.Output = "success", "a.go:1: x"
	nm, _ = m.Update(toolProgressMsg{trace: tr})
	m = nm.(Model)
	if changed := reRendered(before, entryPointers(m)); len(changed) != 1 {
		t.Fatalf("completing a tool re-rendered %d blocks, want 1", len(changed))
	}
}

// A mode switch (shift+tab) re-renders nothing: messages keep the colour of
// the mode they were written in (decision D7), and new ones take the new
// mode's colour.
func TestModeSwitchKeepsHistoryColour(t *testing.T) {
	m := cacheModel(t, 40)
	m.mode = "coding"
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "old"})
	m.refreshViewport()
	before := entryPointers(m)
	m.mode = "plan"
	m.refreshViewport()
	if changed := reRendered(before, entryPointers(m)); len(changed) != 0 {
		t.Fatalf("a mode switch re-rendered %d blocks, want 0", len(changed))
	}
	m.messages = append(m.messages, ChatMessage{Role: RoleUser, Content: "new"})
	m.refreshViewport()
	if got := m.messages[len(m.messages)-1].paintMode; got != "plan" {
		t.Fatalf("a new message is painted for mode %q, want plan", got)
	}
	if got := m.messages[len(m.messages)-2].paintMode; got != "coding" {
		t.Fatalf("an old message was repainted for mode %q, want coding", got)
	}
}

// ctrl+o re-renders only the messages that carry tool calls.
func TestToolToggleReRendersOnlyToolMessages(t *testing.T) {
	m := cacheModel(t, 200)
	before := entryPointers(m)
	m.showTools = true
	m.renderTranscriptBlocks()
	changed := reRendered(before, entryPointers(m))
	byID := map[uint64]ChatMessage{}
	for _, msg := range m.messages {
		byID[msg.id] = msg
	}
	for _, id := range changed {
		if len(byID[id].Tools) == 0 {
			t.Fatalf("message %d has no tools but was re-rendered by ctrl+o", id)
		}
	}
	if len(changed) != 50 {
		t.Fatalf("ctrl+o re-rendered %d blocks, want the 50 tool messages", len(changed))
	}
}

// A tool entry updated in place (same slice, new value) is re-rendered: the
// snapshot copies the tool slice rather than sharing it.
func TestInPlaceToolUpdateIsSeen(t *testing.T) {
	m := cacheModel(t, 8)
	idx := 2 // a tool-stream message
	m.messages[idx].Tools[0].Diff = "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new unique-marker\n"
	m.refreshViewport()
	if !strings.Contains(ansi.Strip(m.renderMessages()), "unique-marker") {
		t.Fatal("an in-place tool diff update was served stale from the cache")
	}
}

// A running pty tool's live tail is never cached: it changes without the
// message changing.
func TestLiveTailIsNotCached(t *testing.T) {
	m := cacheModel(t, 4)
	m.messages = append(m.messages, ChatMessage{Role: RoleAssistant, Kind: "tool-stream", Tools: []ToolItem{{Name: "pty-write", Status: "running", Args: `{"id":"nope"}`}}})
	m.refreshViewport()
	if _, cached := m.renderCache.entries[m.messages[len(m.messages)-1].id]; cached {
		t.Fatal("a message with a live pty tail was cached")
	}
}

// Dead entries (drafts cleared at run end) are evicted once they outnumber
// the slack.
func TestRenderCacheEvictsDeadEntries(t *testing.T) {
	m := cacheModel(t, 10)
	for i := 0; i < 200; i++ {
		m.messages = append(m.messages, ChatMessage{Role: RoleSystem, Content: fmt.Sprint("transient ", i)})
		m.refreshViewport()
		m.messages = m.messages[:len(m.messages)-1]
	}
	m.refreshViewport()
	if n := len(m.renderCache.entries); n > len(m.messages)+64 {
		t.Fatalf("cache holds %d entries for %d messages", n, len(m.messages))
	}
}

// The incremental answer draft shows the same text as rendering the whole
// draft at once, at every point of a stream (padding aside).
func TestAnswerDraftMatchesWholeRender(t *testing.T) {
	doc := strings.Join([]string{
		"# Title", "", "Intro paragraph with **bold** and `code`.", "",
		"```go", "func main() {", "", "\tprintln(1)", "}", "```", "",
		"| a | b |", "|---|---|", "| 1 | 2 |", "",
		"- item one", "- item two", "  - nested", "", "> a quote", "", "Last line.",
		"a line right after", "## heading", "text under it with a [link](https://example.com/x) and *italic*",
		"| x | y |", "|---|---|", "| 3 | 4 |", "text right after the table", "", "", "---", "",
		"1. first", "2. second with a long line that has to wrap at the width we render with here",
		"```", "unterminated fence line", "", "still code",
	}, "\n")
	const width = 60
	var d streamDraft
	for cut := 1; cut <= len(doc); cut++ {
		content := doc[:cut]
		got := d.answerLines(1, content, width)
		wantBlock := ""
		if strings.TrimSpace(content) != "" {
			wantBlock = renderAssistantTextBlock(renderMarkdown(content, width), width)
		}
		want := strings.Split(wantBlock, "\n")
		if wantBlock == "" {
			want = nil
		}
		if a, b := trimLines(got), trimLines(want); a != b {
			t.Fatalf("cut %d (%q): incremental draft differs\n--- incremental ---\n%s\n--- whole ---\n%s", cut, content[max(0, cut-20):], a, b)
		}
	}
}

func trimLines(lines []string) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimRight(ansi.Strip(l), " ")
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// The thinking draft shows the same text as renderThinkingBlock.
func TestThinkingDraftMatchesWholeRender(t *testing.T) {
	doc := "\n\nfirst line of reasoning\nsecond line that is long enough to wrap at a narrow width for sure\n\nafter a blank line\nlast"
	const width = 30
	var d streamDraft
	for cut := 1; cut <= len(doc); cut++ {
		content := doc[:cut]
		got := d.thinkingLines(1, content, width)
		want := renderThinkingBlock(content, width, true)
		var wantLines []string
		if want != "" {
			wantLines = strings.Split(want, "\n")
		}
		if a, b := trimLines(got), trimLines(wantLines); a != b {
			t.Fatalf("cut %d: incremental thinking differs\n--- incremental ---\n%s\n--- whole ---\n%s", cut, a, b)
		}
	}
}

// An answer reset (the step turned into a tool call) starts the draft over.
func TestAnswerDraftRestartsWhenTextIsReplaced(t *testing.T) {
	var d streamDraft
	d.answerLines(1, "para one\n\npara two\n\nthree", 40)
	got := trimLines(d.answerLines(1, "other text", 40))
	if strings.Contains(got, "para one") || !strings.Contains(got, "other text") {
		t.Fatalf("a replaced draft kept its old text: %q", got)
	}
}

// draftText hands out strings that never change under a copy of the
// message that fell behind.
func TestDraftTextCopiesStayIntact(t *testing.T) {
	var d draftText
	a := d.appendTo("", "abc")
	b := d.appendTo(a, "def")
	stale := d.appendTo(a, "XYZ") // an old copy of the message appends too
	if a != "abc" || b != "abcdef" || stale != "abcXYZ" {
		t.Fatalf("draft strings changed: %q %q %q", a, b, stale)
	}
	if next := d.appendTo(b, "g"); next != "abcdefg" || b != "abcdef" {
		t.Fatalf("appending after a stale copy corrupted text: %q %q", next, b)
	}
}

// The draft buffer grows in place: streaming a long answer does not copy
// the whole text per token.
func TestDraftTextAppendAllocations(t *testing.T) {
	var d draftText
	s := d.appendTo("", strings.Repeat("x", 1024))
	if n := testing.AllocsPerRun(100, func() { s = d.appendTo(s, "tok ") }); n > 0.1 {
		t.Fatalf("appending a token allocated %v times on average", n)
	}
}

// A refresh that has to re-render a large transcript (a width change, a
// toggle, /resume) stops at the frame budget, newest messages first, and
// the fill that follows over the next frames converges on the full render.
func TestBudgetedRenderFillsInOverFrames(t *testing.T) {
	m := cacheModel(t, 1000)
	before := m.renderMessages()
	m.width -= 7 // every block must be re-rendered
	m = m.recalcLayout()
	m.refreshViewport()
	if !m.renderCache.fillPending {
		t.Skip("the whole transcript rendered inside one frame budget on this machine")
	}
	if !m.vp.AtBottom() {
		t.Fatal("a budgeted render lost the bottom of the transcript")
	}
	full := m
	full.renderCache = nil
	want := full.renderMessages()
	if m.fillCmd() == nil {
		t.Fatal("no fill was scheduled while placeholders remain")
	}
	frames := 0
	for m.renderCache.fillPending {
		if frames++; frames > 1000 {
			t.Fatal("the fill never completes")
		}
		nm, _ := m.Update(renderFillMsg{})
		m = nm.(Model)
		if m.renderCache.fillPending && !m.renderCache.fillArmed {
			t.Fatal("a fill frame left placeholders without scheduling the next one")
		}
	}
	if got := m.renderMessages(); got != want || got == before {
		t.Fatalf("after %d fill frames the transcript does not match a full render at the new width", frames)
	}
}
