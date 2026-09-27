package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"

	"spettro/internal/termtext"
	"spettro/internal/theme"
)

// Terminal size the side panel needs. Below either, the panel is not drawn
// (ctrl+b still toggles the setting, and the panel comes back when the
// terminal grows): the transcript needs the width, and the panel's frame,
// its sections and its three rows of key hints cannot be squeezed into
// fewer rows without pushing the frame past the bottom of the terminal.
const (
	sidePanelMinTerminalWidth  = 110
	sidePanelMinTerminalHeight = 15
)

// sidePanelWidth is the width of the side panel column, or 0 when it is not
// drawn: switched off, or a terminal below the size the panel needs.
func (m Model) sidePanelWidth() int {
	if !m.showSidePanel {
		return 0
	}
	if m.width < sidePanelMinTerminalWidth || m.height < sidePanelMinTerminalHeight {
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

// sidePanelGitSummary is the "⎇ branch repo +added -deleted" section of the
// side panel and the number of rows it occupies, its leading blank separator
// included (0 when there is no git branch to show).
//
// It is one row when it fits inside the panel frame. A long branch name and
// repository name together often do not, and cutting both to share one row
// left neither readable, so the branch then gets a row of its own and the
// repository shares the second row with the line counts.
func (m Model) sidePanelGitSummary(width int) (string, int) {
	if strings.TrimSpace(m.gitBranch) == "" {
		return "", 0
	}

	added, deleted := 0, 0
	for _, f := range m.modifiedFiles {
		added += f.Added
		deleted += f.Deleted
	}
	pal := theme.Current()
	contentW := sidePanelContentWidth(width)
	branch := termtext.SingleLine(m.gitBranch)
	repo := filepath.Base(m.cwd)
	counts := lipgloss.NewStyle().Bold(true).Foreground(pal.SuccessBright).Render(fmt.Sprintf("+%d", added)) + " " +
		lipgloss.NewStyle().Bold(true).Foreground(pal.Error).Render(fmt.Sprintf("-%d", deleted))
	icon := lipgloss.NewStyle().Foreground(pal.TextMuted).Render("⎇") + " "
	branchStyle := lipgloss.NewStyle().Bold(true).Foreground(pal.Text)

	oneRow := icon + branchStyle.Render(branch) + " " + styleMuted.Render(repo) + " " + counts
	if lipgloss.Width(oneRow) <= contentW {
		return oneRow, 2
	}
	branchRow := icon + branchStyle.Render(termtext.Fit(branch, contentW-lipgloss.Width(icon)))
	repoRoom := max(contentW-lipgloss.Width(counts)-1, 4)
	repoRow := styleMuted.Render(termtext.Fit(repo, repoRoom)) + " " + counts
	return branchRow + "\n" + repoRow, 3
}

func (m Model) sideListGeometry() (startY, rows int) {
	width := m.sidePanelWidth()
	reserved := m.sidePanelReservedRows(width)
	rows = m.sidePanelListBudget(width)
	return 5 + reserved, rows
}

// sidePanelInnerHeight is the number of rows of the side panel column below
// the header, less the frame's border and one row of slack: the panel is
// drawn only on a terminal at least sidePanelMinTerminalHeight rows tall
// (sidePanelWidth), so this is never below 11.
func (m Model) sidePanelInnerHeight() int {
	return m.height - 4
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

// sidePanelList returns the visible window of the activity list's display
// lines (agent-group headers included), centered on the cursor's row like
// the model selector, plus the row→item mapping for mouse clicks (-1 for a
// header). Windowing over display lines — not item indices — is what keeps
// the cursor on screen when headers make the two diverge.
//
// The layout pass is plain bookkeeping; only the rows in the window are
// styled. Styling every item on every frame cost 64 ms per frame at 10k
// items (BenchmarkSidePanelScale in the perf harness).
func (m Model) sidePanelList(items []sidePanelItem, width, maxRows int) (visible []string, rowToItem []int) {
	rowToItem, selectedRow := sidePanelRowLayout(items, m.sidePanelCursor(items))
	start, end := 0, len(rowToItem)
	if len(rowToItem) > maxRows && maxRows > 0 {
		start = max(selectedRow-maxRows/2, 0)
		if start+maxRows > len(rowToItem) {
			start = len(rowToItem) - maxRows
		}
		end = start + maxRows
	}
	rowToItem = rowToItem[start:end]
	return m.sidePanelStyledRows(items, rowToItem, width), rowToItem
}

// sidePanelRowLayout lays the activity list out as display rows: a header
// row (-1) whenever the agent changes, then one row per item. It returns the
// row→item mapping and the row of the item at cursor.
func sidePanelRowLayout(items []sidePanelItem, cursor int) ([]int, int) {
	rowToItem := make([]int, 0, len(items)+4)
	selectedRow := 0
	prevAgent := ""
	for idx, it := range items {
		if agent := activityAgentLabel(it.Agent); agent != prevAgent {
			rowToItem = append(rowToItem, -1)
			prevAgent = agent
		}
		if idx == cursor {
			selectedRow = len(rowToItem)
		}
		rowToItem = append(rowToItem, idx)
	}
	return rowToItem, selectedRow
}

// sidePanelStyledRows renders the rows of a sidePanelRowLayout window. A
// header row (-1) names the agent of the item on the row below it.
func (m Model) sidePanelStyledRows(items []sidePanelItem, rowToItem []int, width int) []string {
	cursor := m.sidePanelCursor(items)
	// A row is the 4-cell cursor prefix plus up to rowBudget cells of
	// "└ title detail", which has to fit the room inside the panel frame.
	rowBudget := max(12, sidePanelContentWidth(width)-4)
	pal := theme.Current()
	lines := make([]string, 0, len(rowToItem))
	for r, idx := range rowToItem {
		if idx < 0 {
			agent := ""
			if r+1 < len(rowToItem) && rowToItem[r+1] >= 0 {
				agent = activityAgentLabel(items[rowToItem[r+1]].Agent)
			}
			header := lipgloss.NewStyle().Foreground(pal.TextMuted).Bold(true).Render("  " + truncateLabel(agent, max(6, rowBudget-2)))
			lines = append(lines, header)
			continue
		}
		lines = append(lines, m.sidePanelItemRow(items[idx], idx == cursor, rowBudget))
	}
	return lines
}

// sidePanelItemRow renders one activity row: the cursor mark, the title and
// the status-coloured detail, cut to rowBudget cells after the prefix.
func (m Model) sidePanelItemRow(it sidePanelItem, selected bool, rowBudget int) string {
	pal := theme.Current()
	prefix := "    "
	titleStyle := lipgloss.NewStyle().Foreground(pal.TextMuted)
	if selected {
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
	// Titles and details carry tool arguments (a command, a path) and are
	// folded onto the row as plain text: see termtext.SingleLine.
	titleRaw := termtext.SingleLine(it.Title)
	detailRaw := termtext.SingleLine(it.Detail)
	labelBudget := max(4, rowBudget-3)
	label := truncateLabel(titleRaw, labelBudget)
	row := prefix + "└ " + titleStyle.Render(label)
	if detailRaw != "" {
		baseWidth := lipgloss.Width("└ "+label) + 1
		if detailBudget := rowBudget - baseWidth; detailBudget > 0 {
			detail := lipgloss.NewStyle().Foreground(detailColor).Render(truncateLabel(detailRaw, detailBudget))
			row += " " + detail
		}
	}
	return row
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

// sidePanelReservedRows is the vertical space the git summary, workflow tree,
// swarm and task sections occupy above the activity list (each block includes
// its leading separator line). It must count exactly what
// sidePanelHeaderParts draws after the title and subtitle.
func (m Model) sidePanelReservedRows(width int) int {
	_, rows := m.sidePanelGitSummary(width)
	if lines := m.sidePanelWorkflowLines(width); len(lines) > 0 {
		rows += len(lines) + 1
	}
	if lines := m.sidePanelSwarmLines(width); len(lines) > 0 {
		rows += len(lines) + 1
	}
	if lines := m.sidePanelTodoLines(); len(lines) > 0 {
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
		styleMuted.Render("type: " + termtext.SingleLine(selected.Kind)),
		styleMuted.Render("id: " + termtext.SingleLine(selected.ID)),
	}
	if selected.Agent != "" {
		details = append(details, styleMuted.Render("agent: "+termtext.SingleLine(selected.Agent)))
	}
	return details
}

// sidePanelDetailBody is the detail pane of the selected activity entry:
// the entry's full body (arguments and output) while ctrl+o is on, else a
// two-row summary of it. The body is raw tool output; renderMarkdown makes
// every line of it plain text (no escape sequences, tabs or carriage
// returns) before styling it, and the collapsed summary is folded the same
// way by termtext.SingleLine.
func (m Model) sidePanelDetailBody(selected sidePanelItem, width int) string {
	detailsBody := strings.TrimSpace(selected.Detail)
	if m.showTools && strings.TrimSpace(selected.Body) != "" {
		detailsBody = strings.TrimSpace(selected.Body)
	}
	if !m.showTools && strings.TrimSpace(selected.Body) != "" {
		detailsBody = truncateLabel(termtext.SingleLine(selected.Body), max(24, width*2))
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

// sidePanelTodoRows caps the task list in the side panel. The footer block
// that shows tasks when the panel is hidden is skipped while the panel is
// open (viewContent), so the panel has to carry them; a long plan still must
// not squeeze the activity list out, hence the cap and the "… N more" row.
const sidePanelTodoRows = 8

// sidePanelTodoLines is the task section of the side panel: the same rows
// the footer shows (live tasks first, completed ones only counted), or nil
// when there is nothing left to do.
func (m Model) sidePanelTodoLines() []string {
	if len(m.todos) == 0 {
		return nil
	}
	return m.todoLines(sidePanelTodoRows)
}

// sidePanelHeaderParts is everything above the activity list: title,
// subtitle, and the git, workflow, swarm and task sections, each preceded by
// a blank separator row. sidePanelReservedRows counts the same sections, so
// the list budget and the mouse hit-testing agree with what is drawn.
func (m Model) sidePanelHeaderParts(width int) []string {
	subtitle := "Operational tool activity"
	switch {
	case m.workflow != nil:
		subtitle = "Workflow · phase-by-phase progress"
	case m.cfg.UltraActive():
		subtitle = "Ultra swarm · per-agent activity"
	}
	if m.activityDropped > 0 {
		subtitle += fmt.Sprintf(" · %d earlier dropped", m.activityDropped)
	}
	parts := []string{
		lipgloss.NewStyle().Bold(true).Render("Activity"),
		styleMuted.Render(subtitle),
	}
	if gitSummary, _ := m.sidePanelGitSummary(width); gitSummary != "" {
		parts = append(parts, "", gitSummary)
	}
	for _, section := range [][]string{
		m.sidePanelWorkflowLines(width),
		m.sidePanelSwarmLines(width),
		m.sidePanelTodoLines(),
	} {
		if len(section) > 0 {
			parts = append(parts, "")
			parts = append(parts, section...)
		}
	}
	return parts
}

// sidePanelBox draws the panel's frame around body. The frame is exactly
// width cells wide (lipgloss v2 counts border and padding inside Width) and
// innerHeight+2 rows tall. Body lines are cut to the room inside the frame
// rather than left for the box to wrap: a wrapped line would make the panel
// taller than the rows the layout gave it and widen the frame past the
// terminal edge.
func sidePanelBox(body string, width, innerHeight int) string {
	contentW := sidePanelContentWidth(width)
	lines := strings.Split(clampLines(body, innerHeight), "\n")
	for i, line := range lines {
		// JoinVertical pads every line with spaces to the widest one; that
		// padding is not content and must not earn a line a "…".
		lines[i] = termtext.Fit(strings.TrimRight(line, " "), contentW)
	}
	return lipgloss.NewStyle().
		Width(width).
		Height(innerHeight+2).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(theme.Current().Border).
		Padding(0, 1).
		Render(strings.Join(lines, "\n"))
}

// sidePanelContentWidth is the room inside the panel's frame: the column
// width minus the border and the one-cell padding on each side.
func sidePanelContentWidth(width int) int {
	return max(width-4, 1)
}

func (m Model) viewSidePanel(width int) string {
	innerHeight := m.sidePanelInnerHeight() - sidePanelHintRows
	reserved := m.sidePanelReservedRows(width)
	items := m.sidePanelItems()
	hints := m.sidePanelHintsView()
	contentParts := m.sidePanelHeaderParts(width)

	if len(items) == 0 {
		contentParts = append(contentParts, "")
		for _, line := range termtext.Wrap("Observability is on. Commands, edits, and other tool activity will appear here.", sidePanelContentWidth(width)) {
			contentParts = append(contentParts, styleMuted.Render(line))
		}
		body := lipgloss.JoinVertical(lipgloss.Left, contentParts...)
		return lipgloss.JoinVertical(lipgloss.Left, sidePanelBox(body, width, innerHeight), hints)
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

	contentParts = append(contentParts, "", listBlock, "", detailsBlock)
	content := lipgloss.JoinVertical(lipgloss.Left, contentParts...)
	return lipgloss.JoinVertical(lipgloss.Left, sidePanelBox(content, width, innerHeight), hints)
}
