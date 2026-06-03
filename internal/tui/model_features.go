package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"spettro/internal/notify"
)

// ---------------------------------------------------------------------------
// Desktop notifications
// ---------------------------------------------------------------------------

// maybeNotify fires a desktop notification when the agent finishes, but only
// if the terminal is not currently focused or the run took more than 10 s.
func (m *Model) maybeNotify(runErr error) {
	elapsed := time.Since(m.agentStartAt)
	if elapsed < 10*time.Second && m.terminalFocused {
		return
	}
	if runErr != nil {
		notify.Send("Spettro", "Agent finished with an error")
	} else {
		notify.Send("Spettro", "Agent finished")
	}
}

// ---------------------------------------------------------------------------
// File attachments (ctrl+f)
// ---------------------------------------------------------------------------

// addAttachment resolves path relative to cwd and appends an attachmentItem.
func (m *Model) addAttachment(rawPath string) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return
	}
	abs := rawPath
	if !filepath.IsAbs(rawPath) {
		abs = filepath.Join(m.cwd, rawPath)
	}
	abs = filepath.Clean(abs)
	if _, err := os.Stat(abs); err != nil {
		m.showBanner("file not found: "+rawPath, "error")
		return
	}
	rel, err := filepath.Rel(m.cwd, abs)
	if err != nil {
		rel = rawPath
	}
	// Prevent duplicates
	for _, existing := range m.attachments {
		if existing.Path == abs {
			m.showBanner("already attached: "+rel, "info")
			return
		}
	}
	m.attachments = append(m.attachments, attachmentItem{
		Kind:    "file",
		Path:    abs,
		RelPath: filepath.ToSlash(rel),
	})
	m.showBanner(fmt.Sprintf("attached: %s (%d total)", rel, len(m.attachments)), "success")
}

// injectAttachments appends each attachment's content to the prompt.
func (m Model) injectAttachments(prompt string) string {
	if len(m.attachments) == 0 {
		return prompt
	}
	var sb strings.Builder
	sb.WriteString(prompt)
	for _, att := range m.attachments {
		content, err := os.ReadFile(att.Path)
		if err != nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("\n\n--- attached: %s ---\n", att.RelPath))
		sb.WriteString(string(content))
	}
	return sb.String()
}

// renderAttachmentChips returns a line of file chips for the input area.
// Returns an empty string when there are no attachments.
func (m Model) renderAttachmentChips(mc lipgloss.Color) string {
	if len(m.attachments) == 0 {
		return ""
	}
	var chips []string
	for i, att := range m.attachments {
		chip := lipgloss.NewStyle().
			Foreground(mc).
			Background(lipgloss.Color("#1F2937")).
			PaddingLeft(1).PaddingRight(1).
			Render(fmt.Sprintf("📄 %s [%d]", filepath.Base(att.RelPath), i+1))
		chips = append(chips, chip)
	}
	hint := styleMuted.Render("  ctrl+r removes last")
	return strings.Join(chips, " ") + hint
}

