package tui

import (
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

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

// injectAttachments appends each file attachment's content to the prompt.
// Image attachments (Kind="image") are skipped here; they are sent via the
// provider's image channel instead.
func (m Model) injectAttachments(prompt string) string {
	var sb strings.Builder
	sb.WriteString(prompt)
	for _, att := range m.attachments {
		if att.Kind != "file" {
			continue
		}
		content, err := os.ReadFile(att.Path)
		if err != nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("\n\n--- attached: %s ---\n", att.RelPath))
		sb.WriteString(string(content))
	}
	return sb.String()
}

// ensureClipboardTempDir creates a temp directory for pasted images on first
// use and stores its path in m.clipboardTempDir.
func (m *Model) ensureClipboardTempDir() error {
	if m.clipboardTempDir != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "spettro-clipboard-*")
	if err != nil {
		return err
	}
	m.clipboardTempDir = dir
	return nil
}

// renderAttachmentChips returns a line of chips for the input area showing
// both file and image attachments. Returns an empty string when there are none.
func (m Model) renderAttachmentChips(mc color.Color) string {
	if len(m.attachments) == 0 {
		return ""
	}
	var chips []string
	for i, att := range m.attachments {
		var label string
		if att.Kind == "image" {
			label = fmt.Sprintf("🖼 %s [%d]", att.RelPath, i+1)
		} else {
			label = fmt.Sprintf("📄 %s [%d]", filepath.Base(att.RelPath), i+1)
		}
		chip := lipgloss.NewStyle().
			Foreground(mc).
			Background(lipgloss.Color("#1F2937")).
			PaddingLeft(1).PaddingRight(1).
			Render(label)
		chips = append(chips, chip)
	}
	hint := styleMuted.Render("  ctrl+r removes last")
	return strings.Join(chips, " ") + hint
}
