package tui

import (
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/diff"
	"spettro/internal/session"
	"spettro/internal/termtext"
	"spettro/internal/theme"
)

const autoSaveMinInterval = 2 * time.Second

// autoSaveDebounced persists the session at most once per autoSaveMinInterval.
// Use it on high-frequency mutation paths (tool-stream updates, progress
// comments). Critical, low-frequency persistence points should call autoSave()
// directly so nothing is lost.
func (m *Model) autoSaveDebounced() {
	if !m.lastAutoSaveAt.IsZero() && time.Since(m.lastAutoSaveAt) < autoSaveMinInterval {
		return
	}
	m.autoSave()
}

// flushSave forces an unconditional save, ignoring the debounce window. It is
// the persistence safety-net invoked on exit so the final turn — which may
// have landed inside the debounce window — is never lost.
func (m *Model) flushSave() {
	m.autoSave()
}

func (m *Model) autoSave() {
	hasContent := false
	for _, msg := range m.messages {
		if msg.Role == RoleUser || msg.Role == RoleAssistant {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return
	}
	m.lastAutoSaveAt = time.Now()
	m.ensureSession()
	msgs := make([]session.Message, 0, len(m.messages))
	for _, msg := range m.messages {
		// Transient live-stream drafts are never persisted: their Kind is not
		// serialized, so they would otherwise leak into a mid-run save as a
		// plain assistant message.
		if msg.Role == RoleAssistant && (msg.Kind == kindThinkingStream || msg.Kind == kindAnswerStream) {
			continue
		}
		msgs = append(msgs, session.Message{
			Role:     string(msg.Role),
			Content:  msg.Content,
			Thinking: msg.Thinking,
			Meta:     msg.Meta,
			At:       msg.At,
		})
	}
	if len(msgs) == 0 {
		return
	}
	metadata := session.Metadata{
		ID:          m.sessionID,
		ProjectPath: m.cwd,
		ProjectHash: session.ProjectHash(m.cwd),
		StartedAt:   msgs[0].At,
	}
	if usage := m.providers.UsageSnapshot(); usage.Totals.Requests > 0 {
		metadata.Stats = &usage
	}
	if m.activeGoal != nil {
		metadata.Goal = &session.GoalRecord{
			Objective:       m.activeGoal.Objective,
			Iteration:       m.activeGoal.Iteration,
			NoProgress:      m.activeGoal.NoProgress,
			StartedAt:       m.activeGoal.StartedAt,
			MaxIterations:   m.activeGoal.MaxIterations,
			NoProgressLimit: m.activeGoal.NoProgressLimit,
			Active:          true,
		}
	}
	// Tasks are deliberately not part of the snapshot: the task files are
	// owned by session.UpsertTodo/SaveTodos, and m.todos is a render cache
	// that can lag behind tools writing mid-run.
	_ = session.Save(m.store.GlobalDir, session.State{
		Metadata: metadata,
		Messages: msgs,
	})
}

// refreshViewport re-renders the chat transcript into the viewport. It no
// longer persists the session: saving is decoupled (see autoSaveDebounced /
// autoSave) so that scroll, tick, and banner-only refreshes do not trigger a
// full session rewrite.
//
// The viewport only follows new output while it is already pinned to the
// bottom: a user who scrolled up to read earlier output keeps their position
// while tokens and tool traces stream in, and following resumes once they
// scroll back down (or send a new prompt, see scrollToBottom).
func (m *Model) refreshViewport() {
	follow := m.vp.AtBottom()
	m.vp.SetContent(m.renderMessages())
	if len(m.messages) == 0 {
		// A fresh session is nothing but the logo and the hint; scrolling to
		// the bottom of that would crop the art from the top on a short
		// terminal, which is exactly the screen that should look welcoming.
		m.vp.GotoTop()
		return
	}
	if follow {
		m.vp.GotoBottom()
	}
}

// scrollToBottom re-renders and unconditionally jumps to the latest output.
// Used when the user submits input, which should always bring the
// conversation back into view.
func (m *Model) scrollToBottom() {
	m.vp.SetContent(m.renderMessages())
	if len(m.messages) == 0 {
		m.vp.GotoTop()
		return
	}
	m.vp.GotoBottom()
}

func (m Model) renderPlanMessage(msg ChatMessage, mc color.Color) string {
	innerW := max(m.paneWidth()-8, 10)

	header := lipgloss.NewStyle().
		Foreground(mc).Bold(true).
		Render("◈ plan")

	var bodyParts []string
	if len(msg.Tools) > 0 {
		bodyParts = append(bodyParts, renderToolGroups(msg.Tools, innerW, m.showTools, m.showFullOutput, mc, m.isUserTool))
	}
	bodyParts = append(bodyParts, renderMarkdown(strings.TrimSpace(msg.Content), innerW))

	box := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(innerW+4).
		Padding(0, 1).
		Render(strings.Join(bodyParts, "\n"))

	return indent(header+"\n"+box, "  ")
}

func renderAssistantTextBlock(body string, width int) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	if width < 10 {
		width = 10
	}
	wrapped := lipgloss.NewStyle().Width(width).Render(body)
	return indent(wrapped, "  ")
}

// renderThinkingBlock renders the model's reasoning as a dim, italic block. When
// live is true a small "streaming" cue marks the in-progress thinking.
func renderThinkingBlock(text string, width int, live bool) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if width < 10 {
		width = 10
	}
	thinkStyle := lipgloss.NewStyle().Foreground(theme.Current().TextDim).Italic(true)
	header := "  thinking"
	if live {
		header += " …"
	}
	wrapped := lipgloss.NewStyle().Width(width).Render(text)
	return thinkStyle.Render(header + "\n" + indent(wrapped, "  │ "))
}

func renderUserTextBlock(body string, width int, prefix string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	if width < 10 {
		width = 10
	}
	lines := strings.Split(lipgloss.NewStyle().Width(width).Render(body), "\n")
	for i, line := range lines {
		if i == 0 {
			lines[i] = prefix + line
		} else {
			lines[i] = strings.Repeat(" ", lipgloss.Width(prefix)) + line
		}
	}
	return strings.Join(lines, "\n")
}

// renderCacheState memoizes per-message rendered blocks so renderMessages does
// not re-run the markdown regex over the whole transcript on every frame. The
// cache is keyed by a content hash of each message's render-relevant fields and
// scoped to the layout params (width / showTools / color); any change to those
// params invalidates the whole cache. Entries for messages no longer present
// are evicted by rebuilding the map on each call, bounding its size to the
// current transcript.
//
// Access is single-threaded: renderMessages is only ever called from the Bubble
// Tea Update goroutine, never from a background tea.Cmd.
type renderCacheState struct {
	width      int
	showTools  bool
	fullOutput bool
	color      string
	blocks     map[uint64]string
}

// renderMessageBlock renders a single chat message to its display string. It is
// the pure, cacheable unit of renderMessages.
func (m Model) renderMessageBlock(msg ChatMessage, mc color.Color) string {
	switch msg.Role {
	case RoleUser:
		prefix := lipgloss.NewStyle().Foreground(mc).Bold(true).Render("  › ")
		// StableWidth: a prompt pasted with an emoji ZWJ sequence would
		// otherwise measure differently here and in the terminal.
		text := lipgloss.NewStyle().Foreground(theme.Current().Text).Render(termtext.StableWidth(msg.Content))
		var entry strings.Builder
		entry.WriteString(renderUserTextBlock(text, m.paneWidth()-8, prefix))
		for i := range msg.Images {
			imgLabel := styleMuted.Render(fmt.Sprintf("     [Image #%d]", i+1))
			entry.WriteString("\n")
			entry.WriteString(imgLabel)
		}
		return entry.String()
	case RoleAssistant:
		if msg.Kind == "plan" {
			return m.renderPlanMessage(msg, mc)
		}
		if msg.Kind == kindThinkingStream {
			return renderThinkingBlock(msg.Content, m.paneWidth()-8, true)
		}
		body := renderMarkdown(msg.Content, m.paneWidth()-8)
		var entryLines []string
		if len(msg.Tools) > 0 {
			entryLines = append(entryLines, renderToolGroups(msg.Tools, m.transcriptWidth(), m.showTools, m.showFullOutput, mc, m.isUserTool))
		}
		if strings.TrimSpace(msg.Content) != "" {
			entryLines = append(entryLines, renderAssistantTextBlock(body, m.paneWidth()-8))
		}
		if msg.Meta != "" {
			entryLines = append(entryLines, styleMuted.Render(termtext.Fit("  "+termtext.SingleLine(msg.Meta), m.transcriptWidth())))
		}
		return strings.Join(entryLines, "\n")
	case RoleSystem:
		if msg.Kind == "diff" {
			return diff.Render(msg.Content, diff.Options{
				Width:  m.paneWidth() - 8,
				Indent: "    ",
			})
		}
		return lipgloss.NewStyle().
			Foreground(theme.Current().TextMuted).
			Render(indent(strings.Join(wrapSystemText(msg.Content, m.paneWidth()-8), "\n"), "    "))
	}
	return ""
}

// wrapSystemText wraps a system message (command output such as /help,
// /skills, /stats) to width cells. Those messages are mostly two-column
// listings, "  /models p:m    set model directly", so a row too long for the
// pane is wrapped with a hanging indent at its second column: the wrapped
// part lines up under the description instead of starting at column 0,
// where it read as a new entry. A row with no second column hangs at its own
// indent. Rows carrying escape sequences (already styled) are wrapped as
// before, by lipgloss.
func wrapSystemText(content string, width int) []string {
	width = max(width, 10)
	var out []string
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "\x1b") {
			out = append(out, strings.Split(lipgloss.NewStyle().Width(width).Render(line), "\n")...)
			continue
		}
		line = strings.ReplaceAll(line, "\t", "    ")
		if ansi.StringWidth(line) <= width {
			out = append(out, line)
			continue
		}
		cut := systemTextHangColumn(line)
		if cut == 0 || ansi.StringWidth(line[:cut]) > width/2 {
			// Leading spaces are one byte a cell, so this is also a width.
			cut = min(len(line)-len(strings.TrimLeft(line, " ")), width/2)
		}
		prefixW := ansi.StringWidth(line[:cut])
		parts := termtext.Wrap(line[cut:], width-prefixW)
		out = append(out, line[:cut]+parts[0])
		pad := strings.Repeat(" ", prefixW)
		for _, part := range parts[1:] {
			out = append(out, pad+part)
		}
	}
	return out
}

// systemTextHangColumn returns the byte offset of a listing row's second
// column: the text after the first run of two or more spaces that follows
// the row's first word. It returns 0 when the row has no second column.
func systemTextHangColumn(line string) int {
	lead := len(line) - len(strings.TrimLeft(line, " "))
	gap := strings.Index(line[lead:], "  ")
	if lead == len(line) || gap <= 0 {
		return 0
	}
	col := lead + gap
	for col < len(line) && line[col] == ' ' {
		col++
	}
	if col == len(line) {
		return 0
	}
	return col
}

// renderKeySeed seeds messageRenderKey. The keys only ever live in this
// process's render cache, so a per-process random seed is all that is needed.
var renderKeySeed = maphash.MakeSeed()

// messageRenderKey hashes every field that influences how a message renders.
// Length prefixes guard against boundary collisions (e.g. "ab"+"c" vs
// "a"+"bc"). The layout params (width/showTools/color) are NOT folded in here —
// they scope the whole cache and invalidate it wholesale on change.
//
// This runs for every message on every refresh, cache hit or not, so it has
// to stay cheap on a transcript full of huge tool calls (a file-write carries
// the whole file). maphash hashes a string in place; the FNV hasher it
// replaced copied every field into a new byte slice first, which made one
// refresh of 200 such messages cost tens of milliseconds and megabytes of
// garbage, on every streamed token.
func messageRenderKey(msg ChatMessage) uint64 {
	var h maphash.Hash
	h.SetSeed(renderKeySeed)
	writeHashField := func(s string) {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(len(s)))
		_, _ = h.Write(buf[:])
		_, _ = h.WriteString(s)
	}
	writeHashField(string(msg.Role))
	writeHashField(msg.Kind)
	writeHashField(msg.Content)
	writeHashField(msg.Thinking)
	writeHashField(msg.Meta)
	for _, t := range msg.Tools {
		writeHashField(t.Name)
		writeHashField(t.Status)
		writeHashField(t.Args)
		writeHashField(t.Output)
		writeHashField(t.Diff)
		if t.Open {
			writeHashField("open")
		}
	}
	for _, img := range msg.Images {
		writeHashField(img)
	}
	return h.Sum64()
}

func (m *Model) renderMessages() string {
	// The logo opens the scrollback rather than sitting above it, so it
	// scrolls out of the way as the conversation grows. It is recomputed on
	// every call — it is not a ChatMessage and never enters the block cache —
	// which is what lets a mode or theme switch repaint it.
	banner := m.eyesBanner()

	if len(m.messages) == 0 {
		return banner + "\n\n" + styleMuted.Render("  no messages yet — type a prompt or /help")
	}

	mc := m.currentColor()
	width := m.paneWidth()
	color := colorCacheKey(mc)

	// Reuse the prior cache only when the layout params match; otherwise start
	// fresh so width/showTools/color changes fully re-render.
	var prev map[uint64]string
	if m.renderCache != nil && m.renderCache.width == width &&
		m.renderCache.showTools == m.showTools && m.renderCache.fullOutput == m.showFullOutput &&
		m.renderCache.color == color {
		prev = m.renderCache.blocks
	}
	next := make(map[uint64]string, len(m.messages))

	parts := make([]string, 0, len(m.messages)+1)
	parts = append(parts, banner)
	for _, msg := range m.messages {
		key := messageRenderKey(msg)
		block, ok := next[key]
		if !ok {
			if block, ok = prev[key]; !ok {
				block = m.renderMessageBlock(msg, mc)
			}
			next[key] = block
		}
		parts = append(parts, block)
	}

	m.renderCache = &renderCacheState{
		width:      width,
		showTools:  m.showTools,
		fullOutput: m.showFullOutput,
		color:      color,
		blocks:     next,
	}

	return strings.Join(parts, "\n\n")
}

// transcriptWidth is the width of the conversation viewport: the pane minus
// a column of margin on each side. Anything placed in the transcript must fit
// in it; the viewport cuts wider rows without a trace.
func (m Model) transcriptWidth() int {
	return max(m.paneWidth()-2, 10)
}

// eyesBanner is the static logo block that opens the scrollback. It is sized
// to the viewport rather than the pane, because that is the width it is
// centred inside.
func (m Model) eyesBanner() string {
	return renderEyesStatic(m.mode, m.transcriptWidth())
}

func (m Model) recalcLayout() Model {
	headerH := 1
	sepH := 2
	statusH := 1

	// The input area is measured, not estimated. It holds the textarea or,
	// in its place, the plan/steer/approval picker or the question form,
	// plus attachment chips and the @mention palette; each of those used to
	// have a hand-kept row estimate here, and every one of them drifted from
	// what viewInput draws at some point (a wrapped command, a palette one
	// row taller), pushing the frame past the bottom of the terminal. The
	// dialogs keep themselves inside the terminal (questionBlockBudget,
	// approvalLayout), so measuring can never squeeze the conversation away.
	m.ta.SetWidth(m.paneWidth() - 6)
	inputH := lipgloss.Height(m.viewInput(m.paneWidth()))

	fixed := headerH + sepH + inputH + statusH + m.parallelFooterHeight() + m.workingIndicatorHeight()
	// At least one transcript row, even when the chrome alone fills the
	// terminal: a larger floor would only push the frame further past the
	// bottom edge on a tiny window.
	contentH := max(m.height-fixed, 1)
	// A transcript that was following the latest output keeps following it
	// through a resize of the pane. Without this, anything that shrinks the
	// pane (an approval dialog opening, the todo list or the working
	// indicator appearing) left the old offset behind: the view stopped at
	// the bottom of the old height, the newest rows sat below it, and since
	// refreshViewport only follows a view already at the bottom, nothing
	// new was shown again until the user scrolled.
	follow := m.vp.AtBottom() && len(m.messages) > 0
	m.vp.SetWidth(m.transcriptWidth())
	m.vp.SetHeight(contentH)
	if follow {
		m.vp.GotoBottom()
	}

	return m
}
