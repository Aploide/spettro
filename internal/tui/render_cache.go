package tui

import (
	"image/color"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/theme"
)

// renderCacheState memoizes the rendered transcript so a refresh re-renders
// only the messages that changed.
//
//   - What it caches: the rendered rows of every chat message (entries), the
//     logo block that opens the transcript (banner), and the stable part of
//     the live answer and thinking drafts (draft, see streamDraft).
//   - Key: a message's id (ChatMessage.id, assigned on first render). An
//     entry is reused only while the message's render-relevant fields equal
//     the snapshot taken when it was rendered, and the layout it was
//     rendered for still holds: the pane width, plus the ctrl+o/ctrl+g
//     toggles for a message that carries tool calls. The fields are compared
//     with ==, which for a string that was not replaced is a pointer
//     comparison, so a hit costs a few nanoseconds per field and nothing is
//     hashed. (The FNV and then maphash content hashes this replaced read
//     every byte of every message on every streamed token: 15 ms and 13 MB
//     per refresh at 1k messages with 200 tool calls; see
//     render_cache_bench_test.go.)
//   - Invalidation: per entry, automatically, by the comparison above; a
//     message with a running pty tool is never cached, because its live
//     tail comes from the pty session, not from the message. applyTheme
//     drops the whole cache (a theme change repaints everything).
//   - Owner: the Bubble Tea Update goroutine. renderTranscriptBlocks is
//     never called from View or from a tea.Cmd. The Model holds a pointer so
//     the cache survives Bubble Tea's value copies of the Model.
type renderCacheState struct {
	// width is the pane width of the last render (tests read it).
	width int
	// entries maps a message id to its last rendering.
	entries map[uint64]*renderEntry
	banner  bannerCache
	draft   streamDraft
	// fillPending reports that the last budgeted render left placeholders
	// (see renderTranscript); fillArmed that a renderFillMsg is on its way.
	fillPending bool
	fillArmed   bool
}

// nextMessageID numbers chat messages on their first render. It is
// process-wide rather than per cache, so ids stay unique when the cache is
// dropped (applyTheme) while the messages keep the ids they already have.
var nextMessageID atomic.Uint64

// renderEntry is one message's last rendering and what it was rendered from.
type renderEntry struct {
	snap       messageSnapshot
	width      int
	showTools  bool
	fullOutput bool
	paintMode  string
	lines      []string
}

// bannerCache is the rendered logo for one (mode, width); the theme is
// covered by applyTheme dropping the whole cache.
type bannerCache struct {
	mode  string
	width int
	frame int
	lines []string
}

// messageSnapshot copies every ChatMessage field that renderMessageBlock
// reads. Slices are copied, because tool entries are updated in place
// (updateToolStreamMessage, attachToolDiff): a snapshot sharing the backing
// array would change along with the message and never see the difference.
type messageSnapshot struct {
	role     Role
	kind     string
	content  string
	thinking string
	meta     string
	tools    []ToolItem
	images   []string
}

func snapshotOf(msg *ChatMessage) messageSnapshot {
	return messageSnapshot{
		role:     msg.Role,
		kind:     msg.Kind,
		content:  msg.Content,
		thinking: msg.Thinking,
		meta:     msg.Meta,
		tools:    append([]ToolItem(nil), msg.Tools...),
		images:   append([]string(nil), msg.Images...),
	}
}

// matches reports whether msg still renders as the snapshot did.
func (s *messageSnapshot) matches(msg *ChatMessage) bool {
	if s.role != msg.Role || s.kind != msg.Kind || s.content != msg.Content ||
		s.thinking != msg.Thinking || s.meta != msg.Meta ||
		len(s.tools) != len(msg.Tools) || len(s.images) != len(msg.Images) {
		return false
	}
	for i := range s.tools {
		if s.tools[i] != msg.Tools[i] {
			return false
		}
	}
	for i := range s.images {
		if s.images[i] != msg.Images[i] {
			return false
		}
	}
	return true
}

// reusable reports whether e can stand in for msg at the current layout.
func (e *renderEntry) reusable(msg *ChatMessage, width int, showTools, fullOutput bool) bool {
	if e.width != width || e.paintMode != msg.paintMode || !e.snap.matches(msg) {
		return false
	}
	if len(msg.Tools) > 0 && (e.showTools != showTools || e.fullOutput != fullOutput) {
		return false
	}
	return true
}

// hasLiveTail reports whether msg draws a running pty tool's live terminal
// tail, which changes without the message changing.
func hasLiveTail(msg *ChatMessage) bool {
	for _, t := range msg.Tools {
		if t.Status == "running" {
			switch t.Name {
			case "pty-start", "pty-write", "pty-kill":
				return true
			}
		}
	}
	return false
}

// transcriptHasLiveTail reports whether any message in the transcript draws
// a live pty tail, which the animation tick then keeps repainting.
func (m Model) transcriptHasLiveTail() bool {
	for i := len(m.messages) - 1; i >= 0; i-- {
		if hasLiveTail(&m.messages[i]) {
			return true
		}
	}
	return false
}

// emptyTranscriptHint is the row shown under the logo before the first
// message.
const emptyTranscriptHint = "  no messages yet — type a prompt or /help"

// renderFrameBudget bounds the rendering one refresh does when many
// messages need it at once: a width change, ctrl+o or ctrl+g, a theme
// switch, /resume. At 1k messages with 200 large tool calls those re-render
// everything, which took 0.25-0.9 s (TestToggleLatency in the perf harness)
// and froze the UI for that long. Past the budget the rest is filled in over
// the following frames (renderFillMsg), newest first.
const renderFrameBudget = 12 * time.Millisecond

// minRenderPerPass is how many messages a budgeted pass renders however
// slow they are, so a fill always makes progress, and so a transcript with
// only a few changed messages (every refresh in a test, most in real use)
// is always rendered whole.
const minRenderPerPass = 4

// renderFillMsg continues a budgeted render that left placeholders.
type renderFillMsg struct{}

// renderTranscriptBlocks renders the whole transcript as blocks of rows, one
// per message after the logo. Unchanged messages come from the cache (see
// renderCacheState).
func (m *Model) renderTranscriptBlocks() [][]string {
	return m.renderTranscript(0)
}

// renderTranscript renders the transcript for lineView.SetBlocks. With a
// budget > 0 it stops rendering after about that long (having rendered at
// least minRenderPerPass messages), from the newest message back, and puts
// a placeholder in each block it did not get to: the message's previous
// rendering when there is one (right text, maybe the old width or toggles),
// else one muted "…" row. It then sets fillPending, and Update schedules a
// renderFillMsg that continues on the next frame. Until the fill completes
// the scroll height is approximate; a view following the bottom stays at
// the bottom, whose messages are rendered first.
func (m *Model) renderTranscript(budget time.Duration) [][]string {
	c := m.renderCache
	if c == nil {
		c = &renderCacheState{}
		m.renderCache = c
	}
	width := m.paneWidth()
	c.width = width

	// The logo opens the scrollback rather than sitting above it, so it
	// scrolls out of the way as the conversation grows.
	banner := c.bannerLines(m.mode, m.transcriptWidth(), m.eyeIntroFrame)
	if len(m.messages) == 0 {
		return [][]string{banner, {styleMuted.Render(emptyTranscriptHint)}}
	}
	if c.entries == nil {
		c.entries = make(map[uint64]*renderEntry, len(m.messages))
	}

	blocks := make([][]string, len(m.messages)+1)
	blocks[0] = banner
	var misses []int
	for i := range m.messages {
		msg := &m.messages[i]
		if msg.id == 0 {
			msg.id = nextMessageID.Add(1)
		}
		if msg.paintMode == "" {
			// Decision D7: a message keeps the colour of the mode it was
			// written in, so a mode switch (shift+tab) re-renders nothing.
			msg.paintMode = m.mode
		}
		if e := c.entries[msg.id]; e != nil && e.reusable(msg, width, m.showTools, m.showFullOutput) {
			blocks[i+1] = e.lines
		} else {
			misses = append(misses, i)
		}
	}

	c.fillPending = false
	var colors map[string]color.Color // mode colours, filled on the first miss
	start := time.Now()
	for n := len(misses) - 1; n >= 0; n-- {
		i := misses[n]
		msg := &m.messages[i]
		rendered := len(misses) - 1 - n
		if budget > 0 && rendered >= minRenderPerPass && time.Since(start) > budget {
			blocks[i+1] = placeholderLines(c.entries[msg.id])
			c.fillPending = true
			continue
		}
		blocks[i+1] = m.renderMessageLines(c, msg, width, &colors)
	}
	c.evictStale(m.messages)
	return blocks
}

// placeholderLines stands in for a message a budgeted render did not get
// to: its previous rendering, or one muted row.
func placeholderLines(prev *renderEntry) []string {
	if prev != nil {
		return prev.lines
	}
	return []string{styleMuted.Render("  …")}
}

// fillCmd returns the command that continues a budgeted render, once, or
// nil when nothing is left to fill.
func (m *Model) fillCmd() tea.Cmd {
	c := m.renderCache
	if c == nil || !c.fillPending || c.fillArmed {
		return nil
	}
	c.fillArmed = true
	return func() tea.Msg { return renderFillMsg{} }
}

// renderMessageLines renders one message and caches the result.
func (m *Model) renderMessageLines(c *renderCacheState, msg *ChatMessage, width int, colors *map[string]color.Color) []string {
	var lines []string
	switch {
	case msg.Role == RoleAssistant && msg.Kind == kindAnswerStream:
		lines = c.draft.answerLines(msg.id, msg.Content, m.paneWidth()-8)
	case msg.Role == RoleAssistant && msg.Kind == kindThinkingStream:
		lines = c.draft.thinkingLines(msg.id, msg.Content, m.paneWidth()-8)
	default:
		mc, ok := (*colors)[msg.paintMode]
		if !ok {
			if *colors == nil {
				*colors = map[string]color.Color{}
			}
			mc = m.colorForMode(msg.paintMode)
			(*colors)[msg.paintMode] = mc
		}
		lines = strings.Split(m.renderMessageBlock(*msg, mc), "\n")
	}
	if hasLiveTail(msg) {
		return lines
	}
	c.entries[msg.id] = &renderEntry{
		snap:       snapshotOf(msg),
		width:      width,
		showTools:  m.showTools,
		fullOutput: m.showFullOutput,
		paintMode:  msg.paintMode,
		lines:      lines,
	}
	return lines
}

// evictStale drops the entries of messages that left the transcript (live
// drafts cleared at run end, tool rows merged into their neighbour). It only
// rebuilds the map once the dead entries outnumber a small slack, so a
// refresh normally costs no map writes beyond the re-rendered messages.
func (c *renderCacheState) evictStale(messages []ChatMessage) {
	const slack = 64
	if len(c.entries) <= len(messages)+slack {
		return
	}
	live := make(map[uint64]*renderEntry, len(messages))
	for i := range messages {
		if e, ok := c.entries[messages[i].id]; ok {
			live[messages[i].id] = e
		}
	}
	c.entries = live
}

// bannerLines returns the logo block for this mode, width, and intro frame.
func (c *renderCacheState) bannerLines(mode string, width, frame int) []string {
	if c.banner.lines == nil || c.banner.mode != mode || c.banner.width != width || c.banner.frame != frame {
		c.banner = bannerCache{mode: mode, width: width, frame: frame, lines: strings.Split(renderEyesAt(mode, width, frame), "\n")}
	}
	return c.banner.lines
}

// colorForMode is the accent colour of the agent mode id, as currentColor is
// for the active one.
func (m Model) colorForMode(mode string) color.Color {
	if spec, ok := m.manifest.AgentByID(mode); ok {
		return modeColor(spec.Color)
	}
	return modeColor(mode)
}

// renderMessages renders the whole transcript as one string, blocks joined
// by a blank row: exactly what the viewport shows, for tests and callers
// that want the text.
func (m *Model) renderMessages() string {
	blocks := m.renderTranscriptBlocks()
	parts := make([]string, len(blocks))
	for i, b := range blocks {
		parts[i] = strings.Join(b, "\n")
	}
	return strings.Join(parts, "\n\n")
}

// streamDraft renders the live answer or thinking draft incrementally.
//
// A draft grows by a few bytes per streamed token, and rendering the whole
// of it again for every token made a long answer quadratic: 5 ms per token
// at 32 KB of markdown (BenchmarkStreamChunk/live32KB in the perf harness).
// The draft is instead cut into segments at stable boundaries (for an
// answer, the end of a completed line outside a code fence or table; for
// thinking, the end of the last line holding text), each completed segment
// is rendered once and kept, and only the line still being written is
// rendered per token.
//
// Segments are rendered independently, so a draft can differ from the
// whole-text rendering in invisible ways (the width a block is padded to);
// the final message replaces the draft at run end and is rendered whole.
//
// It is part of renderCacheState and has the same owner. Keyed by the
// message id, the kind and the width; a draft whose text no longer starts
// with the rendered part (an answer reset) starts over.
type streamDraft struct {
	id    uint64
	kind  string
	width int
	// done is the prefix of the draft whose rows are in lines.
	done  string
	lines []string
	// first reports whether the next segment is the draft's first, whose
	// leading blank lines are dropped as the whole-text render does.
	first bool
}

// reset starts the draft over for the message id, kind and width.
func (d *streamDraft) reset(id uint64, kind string, width int) {
	*d = streamDraft{id: id, kind: kind, width: width, first: true}
}

// sync prepares d for content, starting over when it belongs to another
// draft or content no longer extends the rendered prefix.
func (d *streamDraft) sync(id uint64, kind, content string, width int) {
	if d.id != id || d.kind != kind || d.width != width || !strings.HasPrefix(content, d.done) {
		d.reset(id, kind, width)
	}
}

// answerLines renders an answer draft: its markdown, as renderMessageBlock
// renders an assistant message.
func (d *streamDraft) answerLines(id uint64, content string, width int) []string {
	d.sync(id, kindAnswerStream, content, width)
	if split := answerBoundary(content, len(d.done)); split > len(d.done) {
		// The segment ends with its line break, which is not part of it.
		d.appendSegment(renderAnswerSegment(content[len(d.done):split-1], width), false)
		d.done = content[:split]
	}
	tail := renderAnswerSegment(content[len(d.done):], width)
	return d.withTail(tail, false)
}

// thinkingLines renders a thinking draft as renderThinkingBlock does, with
// the live marker on its header.
func (d *streamDraft) thinkingLines(id uint64, content string, width int) []string {
	d.sync(id, kindThinkingStream, content, width)
	// The boundary is the line break before the last line holding text, so
	// trailing blank lines stay in the tail, which is trimmed as the
	// whole-text render trims them, until text follows them.
	lastText := len(strings.TrimRight(content, " \t\n"))
	if nl := strings.LastIndexByte(content[:lastText], '\n'); nl >= len(d.done) {
		seg := content[len(d.done):nl]
		if d.first {
			seg = strings.TrimLeft(seg, " \t\n")
		}
		if seg != "" || !d.first {
			d.appendSegment(renderThinkingSegment(seg, width), false)
		}
		d.done = content[:nl+1]
	}
	tail := content[len(d.done):]
	if d.first {
		tail = strings.TrimLeft(tail, " \t\n")
	}
	var tailLines []string
	if strings.TrimSpace(tail) != "" {
		tailLines = renderThinkingSegment(strings.TrimRight(tail, " \t\n"), width)
	}
	if len(d.lines) == 0 && len(tailLines) == 0 {
		return nil
	}
	header := lipgloss.NewStyle().Foreground(theme.Current().TextDim).Italic(true).Render("  thinking …")
	out := make([]string, 0, 1+len(d.lines)+len(tailLines))
	out = append(out, header)
	out = append(out, d.lines...)
	return append(out, tailLines...)
}

// appendSegment adds a completed segment's rows; blankBetween separates
// consecutive segments with the blank row their boundary stood for.
func (d *streamDraft) appendSegment(rows []string, blankBetween bool) {
	if len(rows) == 0 {
		return
	}
	if blankBetween && len(d.lines) > 0 {
		d.lines = append(d.lines, "")
	}
	d.lines = append(d.lines, rows...)
	d.first = false
}

// withTail returns the completed rows followed by the rows of the segment
// still being written, in a new slice (d.lines keeps growing in place).
func (d *streamDraft) withTail(tail []string, blankBetween bool) []string {
	out := make([]string, 0, len(d.lines)+1+len(tail))
	out = append(out, d.lines...)
	if blankBetween && len(d.lines) > 0 && len(tail) > 0 {
		out = append(out, "")
	}
	return append(out, tail...)
}

// answerBoundary returns the offset just past the last line break in
// content (scanning from start) after which rendering can be split: the end
// of a completed, non-blank line that is outside a code fence and is not a
// table row. renderMarkdown renders every such line on its own (only fences
// and tables span lines), so rendering the text before and after the split
// separately gives the same rows. It returns start when there is none yet.
// The scan starts outside any fence, which holds because start is always a
// previous split (or 0).
func answerBoundary(content string, start int) int {
	split := start
	inFence := false
	for pos := start; pos < len(content); {
		nl := strings.IndexByte(content[pos:], '\n')
		if nl < 0 {
			break // the last line is still being written
		}
		end := pos + nl + 1
		trim := strings.TrimSpace(content[pos : pos+nl])
		if strings.HasPrefix(trim, "```") {
			inFence = !inFence
		}
		if !inFence && trim != "" && !isTableRow(trim) {
			split = end
		}
		pos = end
	}
	return split
}

// renderAnswerSegment renders one answer segment as an assistant message's
// text block.
func renderAnswerSegment(seg string, width int) []string {
	if strings.TrimSpace(seg) == "" {
		return nil
	}
	block := renderAssistantTextBlock(renderMarkdown(seg, width), width)
	if block == "" {
		return nil
	}
	return strings.Split(block, "\n")
}

// renderThinkingSegment renders thinking text as the rows under the
// "thinking" header of renderThinkingBlock.
func renderThinkingSegment(seg string, width int) []string {
	if width < 10 {
		width = 10
	}
	thinkStyle := lipgloss.NewStyle().Foreground(theme.Current().TextDim).Italic(true)
	wrapped := lipgloss.NewStyle().Width(width).Render(seg)
	return strings.Split(thinkStyle.Render(indent(wrapped, "  │ ")), "\n")
}
