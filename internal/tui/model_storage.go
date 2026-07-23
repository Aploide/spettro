package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/checkpoint"
	"spettro/internal/jobs"
	"spettro/internal/storage"
)

// handleStorageCommand implements /storage (report) and /storage clean
// (interactive multi-select). Only reclaimable classes ever surface items;
// secrets and user content are not listed at all.
func (m Model) handleStorageCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	if len(fields) >= 2 && fields[1] == "clean" {
		return m.openStorageClean()
	}
	m.pushSystemMsg(m.renderStorageReport())
	m.refreshViewport()
	return m, nil
}

// storageInventory builds the report with the session policy from config and
// the live session/spool exempted.
func (m Model) storageInventory() storage.Report {
	storage.LiveSpoolDir = jobs.Spool().Dir()
	return storage.Inventory(m.store.GlobalDir, m.store.ProjectDir, storage.CleanOptions{
		SessionAgeDays:  m.cfg.CleanSessionAgeDays,
		KeepSessions:    m.cfg.CleanKeepSessions,
		ActiveSessionID: m.sessionID,
	})
}

func (m Model) renderStorageReport() string {
	report := m.storageInventory()
	// Shared renderer with `spettro clean` so TUI and CLI always agree.
	return "storage report (~/.spettro + project .spettro)\n\n" +
		storage.RenderReport(report) +
		"\nrun /storage clean to select what to delete"
}

func (m Model) openStorageClean() (tea.Model, tea.Cmd) {
	report := m.storageInventory()
	var items []storage.Item
	for _, c := range report.Classes {
		items = append(items, c.Items...)
	}
	if len(items) == 0 {
		m.showBanner("nothing cleanable found", "info")
		return m, nil
	}
	m.showStorageClean = true
	m.storageItems = items
	m.storageChecked = make([]bool, len(items))
	for i, it := range items {
		m.storageChecked[i] = it.Preselected
	}
	m.storageCursor = 0
	return m, nil
}

func (m Model) updateStorageClean(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.showStorageClean = false
	case "up", "shift+tab":
		if m.storageCursor > 0 {
			m.storageCursor--
		}
	case "down", "tab", "ctrl+n":
		if m.storageCursor < len(m.storageItems)-1 {
			m.storageCursor++
		}
	case "space":
		m.storageChecked[m.storageCursor] = !m.storageChecked[m.storageCursor]
	case "a":
		for i := range m.storageChecked {
			m.storageChecked[i] = true
		}
	case "n":
		for i := range m.storageChecked {
			m.storageChecked[i] = false
		}
	case "enter":
		return m.applyStorageClean()
	}
	return m, nil
}

func (m Model) applyStorageClean() (tea.Model, tea.Cmd) {
	var plan []storage.Item
	for i, it := range m.storageItems {
		if m.storageChecked[i] {
			plan = append(plan, it)
		}
	}
	m.showStorageClean = false
	if len(plan) == 0 {
		m.showBanner("nothing selected", "info")
		return m, nil
	}
	currentHistory := checkpoint.Dir(m.store.GlobalDir, m.cwd)
	freed, err := storage.Clean(plan)
	// If this project's own checkpoint store was deleted, drop the live
	// Checkpointer so the next snapshot lazily re-initializes a fresh repo
	// instead of writing into a deleted one.
	for _, it := range plan {
		if it.Path == currentHistory {
			m.checkpointer = nil
			m.checkpointerFailed = false
			break
		}
	}
	if err != nil {
		m.showBanner(fmt.Sprintf("cleaned %s, with errors: %v", storage.FormatBytes(freed), err), "warn")
	} else {
		m.showBanner(fmt.Sprintf("cleaned %s across %d item(s)", storage.FormatBytes(freed), len(plan)), "success")
	}
	m.refreshViewport()
	return m, nil
}

func (m Model) viewStorageClean() string {
	mc := m.currentColor()
	dialogWidth := 76
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 30 {
		dialogWidth = 30
	}

	var selCount int
	var selSize int64
	for i, it := range m.storageItems {
		if m.storageChecked[i] {
			selCount++
			selSize += it.Size
		}
	}

	// Header rows for class names mean row indices differ from item indices:
	// track the cursor's rendered row and window over rows (selector logic),
	// keeping the selection centered and always visible.
	var rows []string
	selectedRow := 0
	lastClass := ""
	for i, it := range m.storageItems {
		if it.ClassName != lastClass {
			lastClass = it.ClassName
			rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Bold(true).Render(it.ClassName))
		}
		box := "○"
		if m.storageChecked[i] {
			box = "●"
		}
		line := fmt.Sprintf("%s %8s  %s", box, storage.FormatBytes(it.Size), truncateLabel(it.Label, dialogWidth-16))
		if i == m.storageCursor {
			selectedRow = len(rows)
			rows = append(rows, lipgloss.NewStyle().Foreground(mc).Bold(true).Render("› ")+
				lipgloss.NewStyle().Foreground(colorText).Bold(true).Render(line))
		} else {
			rows = append(rows, "  "+lipgloss.NewStyle().Foreground(colorMuted).Render(line))
		}
	}
	maxRows := max(m.height-12, 4)
	if len(rows) > maxRows {
		start := max(selectedRow-maxRows/2, 0)
		if start+maxRows > len(rows) {
			start = len(rows) - maxRows
		}
		rows = rows[start : start+maxRows]
	}

	title := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("▣ storage clean")
	summary := lipgloss.NewStyle().Foreground(colorMuted).
		Render(fmt.Sprintf("%d selected — %s", selCount, storage.FormatBytes(selSize)))
	hint := styleMuted.Render("↑↓ move  space toggle  a all  n none  enter delete  esc cancel")

	dialog := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(dialogWidth+2).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			title, summary, "",
			strings.Join(rows, "\n"),
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
