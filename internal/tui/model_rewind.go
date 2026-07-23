package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/checkpoint"
	"spettro/internal/provider"
	"spettro/internal/session"
)

// rewindConversation is the conversation blob stored with every checkpoint:
// the display transcript plus the structured cross-turn history, so a restore
// brings back both what the user sees and what the model is fed.
type rewindConversation struct {
	Messages    []session.Message  `json:"messages"`
	ConvHistory []provider.Message `json:"conv_history"`
}

// ensureCheckpointer lazily opens the shadow repository for this project.
// Failure disables checkpointing for the session (banner shown once).
func (m *Model) ensureCheckpointer() *checkpoint.Checkpointer {
	if m.checkpointer != nil || m.checkpointerFailed {
		return m.checkpointer
	}
	cp, err := checkpoint.OpenWith(m.store.GlobalDir, m.cwd, checkpoint.Options{
		Disabled:      m.cfg.CheckpointingDisabled,
		MaxFileMB:     m.cfg.CheckpointMaxFileMB,
		RetentionDays: m.cfg.CheckpointRetentionDays,
		MaxGB:         m.cfg.CheckpointMaxGB,
		WarnGB:        m.cfg.CheckpointWarnGB,
	})
	if err != nil {
		m.checkpointerFailed = true
		return nil
	}
	if w := cp.Warning(); w != "" {
		m.showBanner(w, "warn")
	}
	m.checkpointer = cp
	return cp
}

// conversationSnapshot serializes the current conversation state for storage
// with a checkpoint.
func (m Model) conversationSnapshot() []byte {
	msgs := make([]session.Message, 0, len(m.messages))
	for _, msg := range m.messages {
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
	raw, err := json.Marshal(rewindConversation{
		Messages:    msgs,
		ConvHistory: m.convHistory,
	})
	if err != nil {
		return nil
	}
	return raw
}

// renderCheckpointsInfo builds the /checkpoints report: snapshot count and
// shadow-store disk usage for this project plus the total across all projects
// under ~/.spettro/history/.
func (m *Model) renderCheckpointsInfo() string {
	cp := m.ensureCheckpointer()
	if cp == nil {
		return "checkpointing unavailable (disabled in config, or git not installed)"
	}
	items, _ := cp.List()
	var b strings.Builder
	fmt.Fprintf(&b, "checkpoints: %d for this project\n", len(items))
	fmt.Fprintf(&b, "disk usage:  %s (this project)  %s (all projects)\n",
		formatBytes(cp.Size()), formatBytes(checkpoint.TotalSize(m.store.GlobalDir)))
	b.WriteString("store:       " + checkpoint.Dir(m.store.GlobalDir, m.cwd))
	return b.String()
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func (m Model) openRewind() (tea.Model, tea.Cmd) {
	cp := m.ensureCheckpointer()
	if cp == nil {
		m.showBanner("checkpointing unavailable (is git installed?)", "warn")
		return m, nil
	}
	items, err := cp.List()
	if err != nil {
		m.showBanner("failed to list checkpoints: "+err.Error(), "error")
		return m, nil
	}
	if len(items) == 0 {
		m.showBanner("no checkpoints yet — they are taken before each file-modifying tool", "info")
		return m, nil
	}
	// Each checkpoint is taken *before* its tool call runs, so the edits of
	// turn i land between checkpoint i and checkpoint i+1. Showing a row's
	// own FilesChanged (diff vs the previous checkpoint) would count earlier
	// manual edits too; instead show what happened *after* each checkpoint —
	// the next checkpoint's diff, or for the newest row a live diff against
	// the working tree. That is exactly what rewinding that row undoes.
	counts := make([]int, len(items))
	for i := range items {
		if i < len(items)-1 {
			counts[i] = items[i+1].FilesChanged
		} else {
			counts[i] = cp.ChangesSince(items[i].ID)
		}
	}
	m.showRewind = true
	m.rewindItems = items
	m.rewindCounts = counts
	// Newest at the bottom, cursor starting on the most recent step.
	m.rewindCursor = len(items) - 1
	m.rewindModePick = false
	m.rewindModeCursor = 0
	m.ensureRewindWindow()
	return m, nil
}

var rewindModes = []string{
	"restore conversation and files",
	"restore files only",
	"restore conversation only",
}

func (m Model) updateRewind(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.rewindModePick {
		switch msg.String() {
		case "esc", "ctrl+c":
			m.rewindModePick = false
		case "up", "shift+tab":
			if m.rewindModeCursor > 0 {
				m.rewindModeCursor--
			}
		case "down", "ctrl+n", "tab":
			if m.rewindModeCursor < len(rewindModes)-1 {
				m.rewindModeCursor++
			}
		case "enter":
			return m.applyRewind(m.rewindItems[m.rewindCursor], m.rewindModeCursor)
		}
		return m, nil
	}
	switch msg.String() {
	case "esc", "ctrl+c":
		m.showRewind = false
	case "up", "shift+tab":
		if m.rewindCursor > 0 {
			m.rewindCursor--
		}
		m.ensureRewindWindow()
	case "down", "ctrl+n", "tab":
		if m.rewindCursor < len(m.rewindItems)-1 {
			m.rewindCursor++
		}
		m.ensureRewindWindow()
	case "pgup":
		m.rewindCursor = max(0, m.rewindCursor-max(1, m.rewindMaxRows()-2))
		m.ensureRewindWindow()
	case "pgdown":
		m.rewindCursor = min(len(m.rewindItems)-1, m.rewindCursor+max(1, m.rewindMaxRows()-2))
		m.ensureRewindWindow()
	case "home":
		m.rewindCursor = 0
		m.ensureRewindWindow()
	case "end":
		if len(m.rewindItems) > 0 {
			m.rewindCursor = len(m.rewindItems) - 1
		}
		m.ensureRewindWindow()
	case "enter":
		if len(m.rewindItems) > 0 {
			m.rewindModePick = true
			m.rewindModeCursor = 0
		}
	}
	return m, nil
}

func (m Model) applyRewind(cp checkpoint.Checkpoint, mode int) (tea.Model, tea.Cmd) {
	restoreFiles := mode == 0 || mode == 1
	restoreConv := mode == 0 || mode == 2
	if restoreFiles {
		if err := m.checkpointer.RestoreFiles(cp.ID); err != nil {
			m.showRewind = false
			m.showBanner("rewind failed: "+err.Error(), "error")
			return m, nil
		}
	}
	if restoreFiles && len(cp.SkippedLarge) > 0 {
		m.showBanner(fmt.Sprintf("%d large file(s) were not snapshotted and are unaffected by this rewind", len(cp.SkippedLarge)), "warn")
	}
	if restoreConv {
		blob, err := m.checkpointer.Conversation(cp.ConvKey())
		if err != nil || len(blob) == 0 {
			m.showRewind = false
			m.showBanner("no conversation stored for this checkpoint", "warn")
			if restoreFiles {
				m.showBanner("files restored; no conversation stored for this checkpoint", "warn")
			}
			m.refreshModifiedFiles()
			return m, nil
		}
		var conv rewindConversation
		if err := json.Unmarshal(blob, &conv); err != nil {
			m.showRewind = false
			m.showBanner("rewind failed: corrupt conversation snapshot", "error")
			return m, nil
		}
		m.convHistory = conv.ConvHistory
		m.messages = make([]ChatMessage, 0, len(conv.Messages))
		for _, cm := range conv.Messages {
			m.messages = append(m.messages, ChatMessage{
				Role:     Role(cm.Role),
				Content:  cm.Content,
				Thinking: cm.Thinking,
				Meta:     cm.Meta,
				At:       cm.At,
			})
		}
		m.refreshViewport()
		m.autoSave()
	}
	m.showRewind = false
	m.refreshModifiedFiles()
	what := "conversation and files"
	if !restoreConv {
		what = "files"
	} else if !restoreFiles {
		what = "conversation"
	}
	m.showBanner(fmt.Sprintf("rewound %s to %s (before %s)", what, cp.At.Format("15:04:05"), cp.Tool), "success")
	return m, nil
}

func (m *Model) ensureRewindWindow() {
	if len(m.rewindItems) == 0 {
		m.rewindCursor, m.rewindScroll = 0, 0
		return
	}
	if m.rewindCursor < 0 {
		m.rewindCursor = 0
	}
	if m.rewindCursor >= len(m.rewindItems) {
		m.rewindCursor = len(m.rewindItems) - 1
	}
	maxRows := m.rewindMaxRows()
	maxStart := max(0, len(m.rewindItems)-maxRows)
	if m.rewindScroll < 0 {
		m.rewindScroll = 0
	}
	if m.rewindScroll > maxStart {
		m.rewindScroll = maxStart
	}
	if m.rewindCursor < m.rewindScroll {
		m.rewindScroll = m.rewindCursor
	}
	if m.rewindCursor >= m.rewindScroll+maxRows {
		m.rewindScroll = m.rewindCursor - maxRows + 1
	}
}

func (m Model) rewindMaxRows() int {
	return max(4, m.height-12)
}

func (m Model) viewRewind() string {
	mc := m.currentColor()
	dialogWidth := 72
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 30 {
		dialogWidth = 30
	}

	var body, hint string
	if m.rewindModePick {
		var rows []string
		for i, opt := range rewindModes {
			if i == m.rewindModeCursor {
				rows = append(rows, lipgloss.NewStyle().Foreground(mc).Bold(true).Render("› ")+
					lipgloss.NewStyle().Foreground(colorText).Bold(true).Render(opt))
			} else {
				rows = append(rows, "  "+lipgloss.NewStyle().Foreground(colorMuted).Render(opt))
			}
		}
		body = strings.Join(rows, "\n")
		hint = styleMuted.Render("↑↓ choose  enter restore  esc back")
	} else {
		var rows []string
		for i, cp := range m.rewindItems {
			isSelected := i == m.rewindCursor
			timeStr := cp.At.Format("2006-01-02 15:04:05")
			files := fmt.Sprintf("%d file(s) edited", m.rewindCounts[i])
			preview := strings.ReplaceAll(cp.Prompt, "\n", " ")
			if preview == "" {
				preview = "(no prompt)"
			}
			var prefix string
			var timeStyle, previewStyle lipgloss.Style
			if isSelected {
				prefix = lipgloss.NewStyle().Foreground(mc).Bold(true).Render("› ")
				timeStyle = lipgloss.NewStyle().Foreground(colorText).Bold(true)
				previewStyle = lipgloss.NewStyle().Foreground(colorMuted)
			} else {
				prefix = "  "
				timeStyle = lipgloss.NewStyle().Foreground(colorMuted)
				previewStyle = lipgloss.NewStyle().Foreground(colorDim)
			}
			meta := timeStr + "  " + files
			budget := max(8, dialogWidth-lipgloss.Width(prefix)-lipgloss.Width(meta)-6)
			rows = append(rows, prefix+timeStyle.Render(meta)+"  "+previewStyle.Render(truncateLabel(preview, budget)))
		}
		maxRows := m.rewindMaxRows()
		if len(rows) > maxRows {
			start := min(max(0, m.rewindScroll), len(rows)-maxRows)
			rows = rows[start : start+maxRows]
		}
		body = strings.Join(rows, "\n")
		hint = styleMuted.Render("↑↓ navigate  enter restore  esc close")
	}

	title := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ rewind to checkpoint")
	dialog := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(dialogWidth+2).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			body,
			"",
			hint,
		))

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
	)
}
