package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"

	"spettro/internal/theme"
)

func (m Model) sidePanelWidth() int {
	if !m.showSidePanel {
		return 0
	}
	if m.width < 110 {
		return 0
	}
	w := max(m.width/3, 34)
	if w > 54 {
		w = 54
	}
	return w
}

func (m Model) paneWidth() int {
	sw := m.sidePanelWidth()
	if sw <= 0 {
		return m.width
	}
	w := m.width - sw - 1
	if w < 40 {
		return m.width
	}
	return w
}

func (m Model) sidePanelItems() []sidePanelItem {
	items := make([]sidePanelItem, 0, len(m.activityFeed))
	for _, entry := range slices.Backward(m.activityFeed) {

		if entry.Kind != "tool" && entry.Kind != "command" {
			continue
		}
		if strings.TrimSpace(entry.Title) == "" && strings.TrimSpace(entry.Detail) == "" && strings.TrimSpace(entry.Body) == "" {
			continue
		}
		items = append(items, sidePanelItem{
			Kind:   entry.Kind,
			ID:     entry.ID,
			Title:  entry.Title,
			Detail: entry.Detail,
			Body:   entry.Body,
			Agent:  entry.AgentID,
			Status: entry.Status,
		})
	}
	return items
}

func (m Model) sidePanelGitSummary(width int) (string, int) {
	if strings.TrimSpace(m.gitBranch) == "" {
		return "", 0
	}

	added, deleted := 0, 0
	for _, f := range m.modifiedFiles {
		added += f.Added
		deleted += f.Deleted
	}

	repo := filepath.Base(m.cwd)
	branch := truncateLabel(m.gitBranch, max(12, width-20))
	repo = truncateLabel(repo, max(10, width/2))

	line := strings.Join([]string{
		lipgloss.NewStyle().Foreground(theme.Current().TextMuted).Render("⎇"),
		lipgloss.NewStyle().Bold(true).Foreground(theme.Current().Text).Render(branch),
		styleMuted.Render(repo),
		lipgloss.NewStyle().Bold(true).Foreground(theme.Current().SuccessBright).Render(fmt.Sprintf("+%d", added)),
		lipgloss.NewStyle().Bold(true).Foreground(theme.Current().Error).Render(fmt.Sprintf("-%d", deleted)),
	}, " ")
	return line, 2
}

func (m Model) sideListGeometry() (startY, rows int) {
	width := m.sidePanelWidth()
	reserved := m.sidePanelReservedRows(width)
	rows = m.sidePanelListBudget(width)
	return 5 + reserved, rows
}

func (m Model) sidePanelInnerHeight() int {
	h := max(m.height-4, 12)
	return h
}

// sidePanelCursor clamps the stored cursor to the current item list.
func (m Model) sidePanelCursor(items []sidePanelItem) int {
	cursor := max(m.sideCursor, 0)
	if cursor >= len(items) {
		cursor = len(items) - 1
	}
	return cursor
}

// sidePanelListBudget is the number of display lines the activity list gets,
// consistent with the budget viewSidePanel actually renders with.
func (m Model) sidePanelListBudget(width int) int {
	items := m.sidePanelItems()
	innerHeight := m.sidePanelInnerHeight() - sidePanelHintRows
	reserved := m.sidePanelReservedRows(width)
	metaLines := 3
	if len(items) > 0 {
		metaLines = len(m.sidePanelDetailMeta(items[m.sidePanelCursor(items)]))
	}
	listBudget, _ := m.sidePanelBudgets(innerHeight, reserved, metaLines)
	return listBudget
}

// sidePanelList renders the full activity list (agent-group headers included)
// and returns the visible window of display lines, centered on the cursor's
// rendered row like the model selector, plus the row→item mapping for mouse
// clicks. Windowing over display lines — not item indices — is what keeps the
// cursor on screen when headers make the two diverge.
func (m Model) sidePanelList(items []sidePanelItem, width, maxRows int) (visible []string, rowToItem []int) {
	lines, rowMap, selectedRow := m.sidePanelLines(items, width)
	if len(lines) > maxRows && maxRows > 0 {
		start := max(selectedRow-maxRows/2, 0)
		if start+maxRows > len(lines) {
			start = len(lines) - maxRows
		}
		lines = lines[start : start+maxRows]
		rowMap = rowMap[start : start+maxRows]
	}
	return lines, rowMap
}

// swarmSpecID strips the per-instance suffix from a swarm member name
// ("code#3" → "code") so manifest lookups (color, spec) keep working for
// uniquely-named Ultra sub-agents.
func swarmSpecID(id string) string {
	if i := strings.IndexByte(id, '#'); i > 0 {
		return id[:i]
	}
	return id
}

// latestAgentActivity returns the most recent tool-activity title recorded for
// the given agent instance — "what this agent is doing right now".
func (m Model) latestAgentActivity(agentID string) string {
	for _, it := range slices.Backward(m.activityFeed) {

		if it.AgentID == agentID && it.Kind == "tool" && it.ID != "agent" {
			return it.Title
		}
	}
	return ""
}

// sidePanelSwarmLines renders the swarm section of the side panel: a header
// with the fan-out's progress meter and one row per Ultra sub-agent showing
// what it is doing right now. Finished members stay listed so the panel shows
// the whole fan-out, not a list that shrinks as the swarm succeeds.
func (m Model) sidePanelSwarmLines(width int) []string {
	s := m.swarmSummary()
	if len(s.members) == 0 {
		return nil
	}
	budget := max(12, width-2)
	lines := []string{
		s.titleLine(budget),
		progressBar(min(20, max(8, budget-12)), s.done, s.failed, len(s.members)) + " " +
			styleMuted.Render(fmt.Sprintf("%d/%d", s.done+s.failed, len(s.members))),
	}
	for _, a := range s.members {
		lines = append(lines, m.swarmMemberRow(a, budget))
	}
	return lines
}

// sidePanelWorkflowLines renders the workflow phase tree. The side panel has
// the vertical room the footer block does not, so this is the full tree with
// no row cap.
func (m Model) sidePanelWorkflowLines(width int) []string {
	return m.workflowTreeLines(max(12, width-2), 0)
}

// sidePanelReservedRows is the vertical space the git summary, workflow tree
// and swarm sections occupy above the activity list (each block includes its
// leading separator line).
func (m Model) sidePanelReservedRows(width int) int {
	_, rows := m.sidePanelGitSummary(width)
	if lines := m.sidePanelWorkflowLines(width); len(lines) > 0 {
		rows += len(lines) + 1
	}
	if lines := m.sidePanelSwarmLines(width); len(lines) > 0 {
		rows += len(lines) + 1
	}
	return rows
}

func activityAgentLabel(agent string) string {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return "agent"
	}
	if agent == "tui" {
		return "session"
	}
	return agent
}

// sidePanelLines renders every item as display lines with agent-group
// headers, returning the row→item mapping (-1 for headers) and the rendered
// row index of the cursor.
func (m Model) sidePanelLines(items []sidePanelItem, width int) ([]string, []int, int) {
	cursor := m.sidePanelCursor(items)
	lines := make([]string, 0, len(items)+4)
	rowToItem := make([]int, 0, len(items)+4)
	selectedRow := 0
	rowBudget := max(12, width-6)
	prevAgent := ""
	pal := theme.Current()
	for idx, it := range items {
		agent := activityAgentLabel(it.Agent)
		if agent != prevAgent {
			header := lipgloss.NewStyle().Foreground(pal.TextMuted).Bold(true).Render("  " + truncateLabel(agent, max(6, rowBudget-2)))
			lines = append(lines, header)
			rowToItem = append(rowToItem, -1)
			prevAgent = agent
		}
		prefix := "    "
		titleStyle := lipgloss.NewStyle().Foreground(pal.TextMuted)
		if idx == cursor {
			selectedRow = len(lines)
			prefix = lipgloss.NewStyle().Foreground(m.currentColor()).Bold(true).Render("›   ")
			titleStyle = lipgloss.NewStyle().Foreground(pal.Text).Bold(true)
		}
		detailColor := pal.TextDim
		switch it.Status {
		case "running":
			detailColor = m.currentColor()
		case "error", "failed":
			detailColor = pal.Error
		case "changed":
			detailColor = pal.SuccessBright
		default:
			if it.Kind == "file" {
				detailColor = pal.SuccessBright
			}
			if it.Kind == "command" {
				detailColor = pal.Info
			}
		}
		titleRaw := strings.ReplaceAll(strings.TrimSpace(it.Title), "\n", " ")
		detailRaw := strings.ReplaceAll(strings.TrimSpace(it.Detail), "\n", " ")
		labelBudget := max(4, rowBudget-3)
		label := truncateLabel(titleRaw, labelBudget)
		row := prefix + "└ " + titleStyle.Render(label)
		if detailRaw != "" {
			baseWidth := lipgloss.Width("└ "+label) + 1
			detailBudget := rowBudget - baseWidth
			if detailBudget > 0 {
				detail := lipgloss.NewStyle().Foreground(detailColor).Render(truncateLabel(detailRaw, detailBudget))
				row += " " + detail
			}
		}
		lines = append(lines, row)
		rowToItem = append(rowToItem, idx)
	}
	return lines, rowToItem, selectedRow
}

func clampLines(s string, maxLines int) string {
	if maxLines <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	if maxLines == 1 {
		return truncateLabel(strings.TrimSpace(lines[0]), 48)
	}
	clipped := append([]string(nil), lines[:maxLines-1]...)
	clipped = append(clipped, styleMuted.Render("…"))
	return strings.Join(clipped, "\n")
}

func clampOffset(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func scrollBlock(content string, height, offset int) (string, int, int) {
	if height <= 0 {
		return "", 0, 0
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return "", 0, 0
	}
	lines := strings.Split(content, "\n")
	maxOffset := max(0, len(lines)-height)
	offset = clampOffset(offset, 0, maxOffset)
	end := min(len(lines), offset+height)
	return strings.Join(lines[offset:end], "\n"), offset, maxOffset
}

func (m Model) sidePanelDetailMeta(selected sidePanelItem) []string {
	details := []string{
		lipgloss.NewStyle().Bold(true).Foreground(theme.Current().TextMuted).Render("Details"),
		styleMuted.Render("type: " + selected.Kind),
		styleMuted.Render("id: " + selected.ID),
	}
	if selected.Agent != "" {
		details = append(details, styleMuted.Render("agent: "+selected.Agent))
	}
	return details
}

func (m Model) sidePanelDetailBody(selected sidePanelItem, width int) string {
	detailsBody := strings.TrimSpace(selected.Detail)
	if m.showTools && strings.TrimSpace(selected.Body) != "" {
		detailsBody = strings.TrimSpace(selected.Body)
	}
	if !m.showTools && strings.TrimSpace(selected.Body) != "" {
		detailsBody = truncateLabel(strings.ReplaceAll(strings.TrimSpace(selected.Body), "\n", " "), max(24, width*2))
	}
	lines := []string{}
	if detailsBody != "" {
		lines = append(lines, renderMarkdown(detailsBody, max(20, width-4)))
	}
	if !m.showTools {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, styleMuted.Render("ctrl+o expands full context"))
	}
	return strings.Join(lines, "\n")
}

func (m Model) sidePanelBudgets(innerHeight, gitRows, detailMetaLines int) (listLines, detailBodyRows int) {
	// Reserved lines:
	// activity title/subtitle (2), optional git block (2), section separators (2),
	// details metadata, metadata/body separator (1), scroll footer (1).
	reserved := 2 + 2 + detailMetaLines + 1 + 1 + gitRows
	available := max(innerHeight-reserved, 7)
	listLines = max(4, min(12, available/2))
	detailBodyRows = max(3, available-listLines)
	return listLines, detailBodyRows
}

const sidePanelHintRows = 3

func (m Model) sidePanelHintsView() string {
	sep := styleRule.Render(" • ")
	line1 := strings.Join([]string{
		styleMuted.Render("shift+tab: mode"),
		styleMuted.Render("ctrl+b: panel"),
	}, sep)
	line2 := strings.Join([]string{
		styleMuted.Render("ctrl+o: context"),
		styleMuted.Render("ctrl+y: copy"),
	}, sep)
	var line3 string
	if m.mouseCaptureOff {
		line3 = styleWarn.Render("ctrl+t: mouse off")
	} else {
		line3 = styleMuted.Render("ctrl+t: select")
	}
	return strings.Join([]string{line1, line2, line3}, "\n")
}

func (m Model) sidePanelDetailMaxScroll(width int) int {
	items := m.sidePanelItems()
	if len(items) == 0 {
		return 0
	}
	innerHeight := m.sidePanelInnerHeight() - sidePanelHintRows
	reserved := m.sidePanelReservedRows(width)
	selected := items[m.sidePanelCursor(items)]
	meta := m.sidePanelDetailMeta(selected)
	_, detailBodyRows := m.sidePanelBudgets(innerHeight, reserved, len(meta))
	body := m.sidePanelDetailBody(selected, width)
	_, _, maxOffset := scrollBlock(body, detailBodyRows, m.sideDetailScroll)
	return maxOffset
}

func (m Model) viewSidePanel(width int) string {
	innerHeight := m.sidePanelInnerHeight() - sidePanelHintRows
	gitSummary, _ := m.sidePanelGitSummary(width)
	workflowLines := m.sidePanelWorkflowLines(width)
	swarmLines := m.sidePanelSwarmLines(width)
	reserved := m.sidePanelReservedRows(width)
	items := m.sidePanelItems()
	hints := m.sidePanelHintsView()
	subtitle := "Operational tool activity"
	switch {
	case m.workflow != nil:
		subtitle = "Workflow · phase-by-phase progress"
	case m.cfg.UltraActive():
		subtitle = "Ultra swarm · per-agent activity"
	}
	if len(items) == 0 {
		parts := []string{
			lipgloss.NewStyle().Bold(true).Render("Activity"),
			styleMuted.Render(subtitle),
		}
		if gitSummary != "" {
			parts = append(parts, "", gitSummary)
		}
		if len(workflowLines) > 0 {
			parts = append(parts, "")
			parts = append(parts, workflowLines...)
		}
		if len(swarmLines) > 0 {
			parts = append(parts, "")
			parts = append(parts, swarmLines...)
		}
		parts = append(parts, "", styleMuted.Render("Observability is on. Commands, edits, and other tool activity will appear here."))
		body := lipgloss.JoinVertical(lipgloss.Left, parts...)
		box := lipgloss.NewStyle().
			Width(width+2).
			Height(innerHeight+2).
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(theme.Current().Border).
			Padding(0, 1).
			Render(clampLines(body, innerHeight))
		return lipgloss.JoinVertical(lipgloss.Left, box, hints)
	}

	selected := items[m.sidePanelCursor(items)]
	detailMeta := m.sidePanelDetailMeta(selected)
	listLinesBudget, detailBodyRows := m.sidePanelBudgets(innerHeight, reserved, len(detailMeta))
	lines, _ := m.sidePanelList(items, width, listLinesBudget)

	listBlock := strings.Join(lines, "\n")

	detailBody := m.sidePanelDetailBody(selected, width)
	detailWindow, detailOffset, detailMax := scrollBlock(detailBody, detailBodyRows, m.sideDetailScroll)
	detailFooter := styleMuted.Render("scroll: none")
	if detailMax > 0 {
		detailFooter = styleMuted.Render(fmt.Sprintf("scroll: %d/%d  (mouse wheel)", detailOffset+1, detailMax+1))
	}
	detailsBlockParts := append([]string(nil), detailMeta...)
	if strings.TrimSpace(detailWindow) != "" {
		detailsBlockParts = append(detailsBlockParts, "")
		detailsBlockParts = append(detailsBlockParts, detailWindow)
	}
	detailsBlockParts = append(detailsBlockParts, "")
	detailsBlockParts = append(detailsBlockParts, detailFooter)
	detailsBlock := strings.Join(detailsBlockParts, "\n")

	contentParts := []string{
		lipgloss.NewStyle().Bold(true).Render("Activity"),
		styleMuted.Render(subtitle),
	}
	if gitSummary != "" {
		contentParts = append(contentParts, "", gitSummary)
	}
	if len(workflowLines) > 0 {
		contentParts = append(contentParts, "")
		contentParts = append(contentParts, workflowLines...)
	}
	if len(swarmLines) > 0 {
		contentParts = append(contentParts, "")
		contentParts = append(contentParts, swarmLines...)
	}
	contentParts = append(contentParts, "", listBlock, "", detailsBlock)
	content := lipgloss.JoinVertical(lipgloss.Left, contentParts...)
	content = clampLines(content, innerHeight)

	box := lipgloss.NewStyle().
		Width(width+2).
		Height(innerHeight+2).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(theme.Current().Border).
		Padding(0, 1).
		Render(content)
	return lipgloss.JoinVertical(lipgloss.Left, box, hints)
}
