package tui

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"image/color"
	"io"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"spettro/internal/diff"
	"spettro/internal/session"
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
func (m *Model) refreshViewport() {
	m.vp.SetContent(m.renderMessages())
	m.vp.GotoBottom()
}

func (m Model) renderPlanMessage(msg ChatMessage, mc color.Color) string {
	innerW := m.paneWidth() - 8
	if innerW < 10 {
		innerW = 10
	}

	header := lipgloss.NewStyle().
		Foreground(mc).Bold(true).
		Render("◈ plan")

	var bodyParts []string
	if len(msg.Tools) > 0 {
		bodyParts = append(bodyParts, renderToolGroups(msg.Tools, m.showTools, m.showFullOutput, mc))
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
	thinkStyle := lipgloss.NewStyle().Foreground(colorDim).Italic(true)
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
	blocks    map[uint64]string
}

// renderMessageBlock renders a single chat message to its display string. It is
// the pure, cacheable unit of renderMessages.
func (m Model) renderMessageBlock(msg ChatMessage, mc color.Color) string {
	switch msg.Role {
	case RoleUser:
		prefix := lipgloss.NewStyle().Foreground(mc).Bold(true).Render("  › ")
		text := lipgloss.NewStyle().Foreground(colorText).Render(msg.Content)
		entry := renderUserTextBlock(text, m.paneWidth()-8, prefix)
		for i := range msg.Images {
			imgLabel := styleMuted.Render(fmt.Sprintf("     [Image #%d]", i+1))
			entry += "\n" + imgLabel
		}
		return entry
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
			entryLines = append(entryLines, renderToolGroups(msg.Tools, m.showTools, m.showFullOutput, mc))
		}
		if strings.TrimSpace(msg.Content) != "" {
			entryLines = append(entryLines, renderAssistantTextBlock(body, m.paneWidth()-8))
		}
		if msg.Meta != "" {
			entryLines = append(entryLines, styleMuted.Render("  "+msg.Meta))
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
			Foreground(colorMuted).
			PaddingLeft(4).
			Width(m.paneWidth() - 4).
			Render(msg.Content)
	}
	return ""
}

// messageRenderKey hashes every field that influences how a message renders.
// Length prefixes guard against boundary collisions (e.g. "ab"+"c" vs
// "a"+"bc"). The layout params (width/showTools/color) are NOT folded in here —
// they scope the whole cache and invalidate it wholesale on change.
func messageRenderKey(msg ChatMessage) uint64 {
	h := fnv.New64a()
	writeHashField := func(s string) {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(len(s)))
		_, _ = h.Write(buf[:])
		_, _ = io.WriteString(h, s)
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
	if len(m.messages) == 0 {
		return styleMuted.Render("  no messages yet — type a prompt or /help")
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

	parts := make([]string, 0, len(m.messages))
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

func (m Model) recalcLayout() Model {
	eyesH := len(eyesActing)
	headerH := 1
	sepH := 2
	statusH := 1

	inputH := 6
	if len(m.attachments) > 0 {
		inputH++
	}
	if m.showPlanApproval {
		inputH += 2 + len(planApprovalOptions)
	} else if m.pendingAuth != nil {
		inputH += 2 + len(shellApprovalOptions)
		if block := m.approvalDiffView(m.paneWidth()); block != "" {
			inputH += lipgloss.Height(block)
		}
	}
	if len(m.mentionItems) > 0 {
		inputH += 5 + len(m.mentionItems)
	}

	parallelH := 0
	if m.sidePanelWidth() <= 0 {
		if pa := m.renderParallelAgents(); pa != "" {
			parallelH = lipgloss.Height(pa)
		}
	}

	fixed := headerH + eyesH + sepH + inputH + statusH + parallelH
	contentH := m.height - fixed
	if contentH < 3 {
		contentH = 3
	}
	vpW := m.paneWidth() - 2
	if vpW < 10 {
		vpW = 10
	}

	m.vp.SetWidth(vpW)
	m.vp.SetHeight(contentH)
	m.ta.SetWidth(m.paneWidth() - 6)

	return m
}
