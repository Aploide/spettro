package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PrintGoodbye prints the acting eyes and session stats to stdout after the
// TUI exits. It is a no-op when the session had no real activity.
func PrintGoodbye(final tea.Model) {
	m, ok := final.(Model)
	if !ok {
		return
	}

	duration := time.Since(m.startedAt)
	tokens := m.totalTokensUsed
	hasMessages := false
	for _, msg := range m.messages {
		if msg.Role == RoleUser {
			hasMessages = true
			break
		}
	}
	filesEdited := len(m.sessionEdits)

	if !hasMessages && tokens == 0 {
		return
	}

	eyeColor := lipgloss.Color("#BD93F9")
	eyeStyle := lipgloss.NewStyle().Foreground(eyeColor)
	mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280"))
	accentStyle := lipgloss.NewStyle().Foreground(eyeColor)

	fmt.Println()
	for _, line := range eyesActing {
		fmt.Println(eyeStyle.Render(line))
	}
	fmt.Println()

	sep := accentStyle.Render(" · ")

	parts := []string{
		accentStyle.Render(formatSessionDuration(duration)),
	}
	if tokens > 0 {
		parts = append(parts, mutedStyle.Render(formatSessionTokens(tokens)+" tokens"))
	}
	if filesEdited > 0 {
		label := "file"
		if filesEdited != 1 {
			label = "files"
		}
		parts = append(parts, mutedStyle.Render(fmt.Sprintf("%d %s edited", filesEdited, label)))
	}

	line := mutedStyle.Render("session finished") + sep + strings.Join(parts, sep)
	fmt.Println("  " + line)
	fmt.Println()
}

func formatSessionDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func formatSessionTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
