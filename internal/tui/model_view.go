package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"spettro/internal/compact"
	"spettro/internal/version"
)

func (m Model) View() string {
	if !m.ready {
		return lipgloss.NewStyle().Foreground(colorMuted).Render("\n  loading…")
	}

	if m.showTrust {
		return m.viewTrust()
	}

	if m.showResume {
		return m.viewResume()
	}

	if m.showConnect {
		return m.viewConnect()
	}

	if m.showSelector {
		return m.viewSelector()
	}

	header := m.viewHeader()
	paneW := m.paneWidth()
	eyes := renderEyes(m.mode, m.eyeFrame, m.thinking, paneW)
	sep := m.viewSep(paneW)
	content := m.vp.View()
	inputArea := m.viewInput(paneW)
	statusBar := m.viewStatusBar(paneW)

	parts := []string{
		eyes,
		sep,
		content,
		sep,
	}
	sideW := m.sidePanelWidth()
	if sideW <= 0 {
		if pa := m.renderParallelAgents(); pa != "" {
			parts = append(parts, pa)
		}
	}
	parts = append(parts, inputArea, statusBar)
	mainPane := lipgloss.JoinVertical(lipgloss.Left, parts...)

	if sideW <= 0 {
		return lipgloss.JoinVertical(lipgloss.Left, header, mainPane)
	}
	sidePane := m.viewSidePanel(sideW)
	divider := lipgloss.NewStyle().Foreground(colorDim).Render("│")
	body := lipgloss.JoinHorizontal(lipgloss.Top, mainPane, divider, sidePane)
	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}

func (m Model) viewHeader() string {
	mc := m.currentColor()
	logo := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ spettro " + version.App)

	primaryIDs := primaryAgentIDs(m.manifest)
	var tabs []string
	for _, id := range primaryIDs {
		ag, ok := m.manifest.AgentByID(id)
		if !ok {
			continue
		}
		agColor := modeColor(ag.Color)
		if ag.ID == m.mode {
			tabs = append(tabs, lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#0D0D0D")).
				Background(agColor).
				PaddingLeft(1).PaddingRight(1).
				Render(ag.ID))
		} else {
			tabs = append(tabs, lipgloss.NewStyle().
				Foreground(colorMuted).
				PaddingLeft(1).PaddingRight(1).
				Render(ag.ID))
		}
	}
	center := strings.Join(tabs, " ")

	modelLabel := m.cfg.ActiveModel
	provLabel := m.cfg.ActiveProvider
	for _, mod := range m.providers.Models() {
		if mod.Provider == m.cfg.ActiveProvider && mod.Name == m.cfg.ActiveModel {
			if mod.DisplayName != "" {
				modelLabel = mod.DisplayName
			}
			if mod.ProviderName != "" {
				provLabel = mod.ProviderName
			}
			break
		}
	}
	if len(modelLabel) > 12 {
		modelLabel = modelLabel[:12]
	}
	permText := string(m.cfg.Permission)
	logoW := lipgloss.Width(logo)
	permW := lipgloss.Width(permText)
	maxMetaWidth := m.width - logoW - permW - 8
	if maxMetaWidth < 0 {
		maxMetaWidth = 0
	}
	metaText := truncateLabel(modelLabel+"  "+provLabel, maxMetaWidth)
	right := lipgloss.NewStyle().Foreground(mc).Render(permText)
	if metaText != "" {
		right = styleMuted.Render(metaText) + "  " + right
	}

	rightW := lipgloss.Width(right)
	availableCenter := m.width - logoW - rightW - 2
	if availableCenter < 0 {
		availableCenter = 0
	}
	if availableCenter > 0 && lipgloss.Width(center) > availableCenter {
		center = lipgloss.NewStyle().Foreground(mc).Bold(true).Render(m.mode)
	}
	centerBlock := ""
	if availableCenter > 0 {
		centerBlock = lipgloss.PlaceHorizontal(availableCenter, lipgloss.Center, center)
	}

	row := logo + " " + centerBlock
	if right != "" {
		row += " " + right
	}

	return lipgloss.NewStyle().
		Width(m.width).
		MaxWidth(m.width).
		Background(lipgloss.Color("#0D0D0D")).
		Render(row)
}

func (m Model) viewSep(width int) string {
	return lipgloss.NewStyle().
		Foreground(colorDim).
		Render(strings.Repeat("─", width))
}

func (m Model) viewCommandPalette(width int) string {
	if len(m.cmdItems) == 0 {
		return ""
	}
	mc := m.currentColor()
	var rows []string
	for i, cmd := range m.cmdItems {
		var nameStyle, descStyle lipgloss.Style
		if i == m.cmdCursor {
			nameStyle = lipgloss.NewStyle().Foreground(mc).Bold(true)
			descStyle = lipgloss.NewStyle().Foreground(colorText)
		} else {
			nameStyle = lipgloss.NewStyle().Foreground(colorText)
			descStyle = lipgloss.NewStyle().Foreground(colorMuted)
		}
		rows = append(rows, nameStyle.Render(fmt.Sprintf("%-14s", cmd.name))+"  "+descStyle.Render(cmd.desc))
	}
	body := strings.Join(rows, "\n")
	hint := styleMuted.Render("enter inserts  enter again runs")
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorBorder).
		Width(width - 4).
		PaddingLeft(2).PaddingRight(2).
		Render(body + "\n\n" + hint)
}

func (m Model) viewMentionPalette(width int) string {
	if len(m.mentionItems) == 0 {
		return ""
	}
	mc := m.currentColor()
	var rows []string
	for i, item := range m.mentionItems {
		style := lipgloss.NewStyle().Foreground(colorMuted)
		prefix := "  "
		if i == m.mentionCursor {
			prefix = lipgloss.NewStyle().Foreground(mc).Bold(true).Render("› ")
			style = lipgloss.NewStyle().Foreground(colorText).Bold(true)
		}
		rows = append(rows, prefix+style.Render(item))
	}
	title := lipgloss.NewStyle().Foreground(colorMuted).Bold(true).Render("available files")
	hint := styleMuted.Render("↑↓ navigate  enter inserts mention")
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorBorder).
		Width(width - 4).
		PaddingLeft(2).PaddingRight(2).
		Render(title + "\n\n" + strings.Join(rows, "\n") + "\n\n" + hint)
}

func (m Model) renderAskUserPrompt() string {
	if m.pendingQuestion == nil {
		return ""
	}
	req := m.pendingQuestion.request
	var lines []string
	lines = append(lines, styleMuted.Render("  "+req.Question))
	if strings.TrimSpace(req.Context) != "" {
		lines = append(lines, styleMuted.Render("  "+req.Context))
	}
	options := askUserOptions(req)
	if m.questionFreeform || len(options) == 0 {
		lines = append(lines, styleMuted.Render("  type your answer and press enter:"))
		lines = append(lines, m.ta.View())
		lines = append(lines, styleMuted.Render("  esc declines"))
		return strings.Join(lines, "\n")
	}
	lines = append(lines, m.renderApprovalPicker(
		"Choose an answer",
		options,
		m.questionCursor,
		m.currentColor(),
	))
	lines = append(lines, styleMuted.Render("  enter selects  esc declines"))
	return strings.Join(lines, "\n")
}

func (m Model) viewInput(width int) string {
	mc := m.currentColor()
	agentLabel := m.mode
	if spec, ok := m.manifest.AgentByID(m.mode); ok {
		agentLabel = spec.ID
	}
	prompt := modePrompt(m.mode)
	label := lipgloss.NewStyle().Foreground(mc).Bold(true).Render(prompt + " " + agentLabel)

	lines := []string{label}
	if m.showPlanApproval {
		lines = append(lines, m.renderApprovalPicker(
			"Execute this plan?",
			planApprovalOptions,
			m.planApprovalCursor,
			mc,
		))
		if m.pendingPlan != "" {
			lines = append(lines, m.ta.View())
		}
	} else if m.pendingQuestion != nil {
		lines = append(lines, m.renderAskUserPrompt())
	} else if m.pendingAuth != nil {
		cmd := formatApprovalCommandLabel(m.pendingAuth.request.Command)
		lines = append(lines, styleWarn.Render("  "+cmd))
		if strings.TrimSpace(m.pendingAuth.request.Reason) != "" {
			lines = append(lines, styleMuted.Render("  why: "+m.pendingAuth.request.Reason))
		}
		if len(m.pendingAuth.request.Segments) > 0 && m.cfg.ShowPermissionDebug {
			lines = append(lines, styleMuted.Render("  segments: "+strings.Join(m.pendingAuth.request.Segments, " | ")))
		}
		if m.approvalCursor == 3 {
			lines = append(lines, styleMuted.Render("  type what to do instead, then press enter:"))
			lines = append(lines, m.ta.View())
		} else {
			lines = append(lines, m.renderApprovalPicker(
				"allow this command?",
				shellApprovalOptions,
				m.approvalCursor,
				lipgloss.Color("#F59E0B"),
			))
		}
	} else {
		lines = append(lines, m.ta.View())
	}
	boxStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(width - 2).
		PaddingLeft(1).PaddingRight(1)

	inner := strings.Join(lines, "\n")
	inputBox := boxStyle.Render(inner)

	palette := m.viewCommandPalette(width)
	mentionPalette := m.viewMentionPalette(width)
	if palette == "" && mentionPalette == "" {
		return inputBox
	}
	var blocks []string
	if palette != "" {
		blocks = append(blocks, palette)
	}
	if mentionPalette != "" {
		blocks = append(blocks, mentionPalette)
	}
	blocks = append(blocks, inputBox)
	return lipgloss.JoinVertical(lipgloss.Left, blocks...)
}

func (m Model) renderParallelAgents() string {
	active := make([]parallelAgentEntry, 0, len(m.parallelAgents))
	for _, a := range m.parallelAgents {
		if a.Status == "running" {
			active = append(active, a)
		}
	}
	if len(active) == 0 && len(m.todos) == 0 {
		return ""
	}
	frame := spinnerFrames[m.tickCount%len(spinnerFrames)]
	var lines []string
	if len(active) > 0 {
		lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(colorMuted).Render("  agents"))
		lines = append(lines, lipgloss.NewStyle().Foreground(m.currentColor()).Render("  ● orchestrator: "+m.mode+" (running)"))
	}
	for _, a := range active {
		var dot string
		var style lipgloss.Style
		agentColor := modeColor("")
		if spec, ok := m.manifest.AgentByID(a.ID); ok {
			agentColor = modeColor(spec.Color)
		}
		switch a.Status {
		case "running":
			dot = frame
			style = lipgloss.NewStyle().Foreground(agentColor).Bold(true)
		case "done":
			dot = "●"
			style = lipgloss.NewStyle().Foreground(agentColor)
		case "error", "failed":
			dot = "✗"
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444"))
		default:
			dot = "○"
			style = lipgloss.NewStyle().Foreground(colorMuted)
		}
		label := a.ID
		if a.Instance > 1 {
			label = fmt.Sprintf("%s [%d]", a.ID, a.Instance)
		}
		task := a.Task
		if len(task) > 50 {
			task = task[:47] + "..."
		}
		line := style.Render(fmt.Sprintf("  %s %-18s", dot, label)) +
			lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280")).Render(task)
		lines = append(lines, line)
	}
	if len(m.todos) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(colorMuted).Render("  todos"))
		for _, td := range m.todos {
			icon := "○"
			color := colorMuted
			switch td.Status {
			case "completed", "done":
				icon = "✓"
				color = lipgloss.Color("#10B981")
			case "in_progress", "running":
				icon = frame
				color = lipgloss.Color("#F59E0B")
			case "blocked", "failed", "cancelled":
				icon = "!"
				color = lipgloss.Color("#EF4444")
			}
			label := td.Content
			if len(label) > 56 {
				label = label[:53] + "..."
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(color).Render("  "+icon+" ")+styleMuted.Render(label))
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) contextWindow() int {
	for _, mod := range m.providers.Models() {
		if mod.Provider == m.cfg.ActiveProvider && mod.Name == m.cfg.ActiveModel {
			return mod.Context
		}
	}
	return 0
}

func contextWindowDefault(providerName string) int {
	switch providerName {
	case "anthropic":
		return 200_000
	case "openai":
		return 128_000
	case "google":
		return 1_000_000
	default:
		return 128_000
	}
}

func (m Model) autoCompactIfNeeded() tea.Cmd {
	if m.thinking || m.totalTokensUsed == 0 {
		return nil
	}
	window := m.contextWindow()
	if window == 0 {
		window = contextWindowDefault(m.cfg.ActiveProvider)
	}
	eval := compact.Evaluate(window, compact.Config{
		AutoEnabled:      m.cfg.AutoCompactEnabled,
		AutoThresholdPct: m.cfg.AutoCompactThresholdPct,
		MaxFailures:      m.cfg.AutoCompactMaxFailures,
	}, compact.State{
		TokensUsed:          m.totalTokensUsed,
		ConsecutiveFailures: m.autoCompactFailures,
	})
	if !eval.ShouldAutoCompact {
		return nil
	}
	if len(m.messages) < 3 {
		return nil
	}
	_, cmd := m.runCompactWithMode("preserve all key decisions, code changes, and action items", true)
	return cmd
}

func formatTokenCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func (m Model) viewStatusBar(width int) string {
	left := m.statusBarMessage()

	window := m.contextWindow()
	if window == 0 {
		window = contextWindowDefault(m.cfg.ActiveProvider)
	}
	eval := compact.Evaluate(window, compact.Config{
		AutoEnabled:      m.cfg.AutoCompactEnabled,
		AutoThresholdPct: m.cfg.AutoCompactThresholdPct,
		MaxFailures:      m.cfg.AutoCompactMaxFailures,
	}, compact.State{
		TokensUsed:          m.totalTokensUsed,
		ConsecutiveFailures: m.autoCompactFailures,
	})
	used := m.totalTokensUsed
	var ctxColor lipgloss.Color
	switch {
	case eval.IsError:
		ctxColor = lipgloss.Color("#EF4444")
	case eval.IsWarning:
		ctxColor = lipgloss.Color("#F59E0B")
	default:
		ctxColor = lipgloss.Color("#6B7280")
	}
	ctxLabel := fmt.Sprintf("%s / %s ctx", formatTokenCount(used), formatTokenCount(eval.EffectiveWindow))
	if !m.cfg.AutoCompactEnabled {
		ctxLabel += " (auto off)"
	}
	right := lipgloss.NewStyle().Foreground(ctxColor).Render(ctxLabel)

	leftWidth := width - lipgloss.Width(right) - 2
	if leftWidth < 0 {
		leftWidth = 0
	}
	leftPadded := lipgloss.NewStyle().Width(leftWidth).Render(left)

	bar := leftPadded + right + " "
	return lipgloss.NewStyle().
		Width(width).
		Background(lipgloss.Color("#0D0D0D")).
		PaddingLeft(1).
		Render(bar)
}

func (m Model) statusBarMessage() string {
	if m.banner != "" {
		return renderStatusBanner(m.banner, m.bannerKind)
	}
	hints := []string{
		styleMuted.Render("shift+tab: mode"),
		styleMuted.Render("ctrl+b: panel"),
		styleMuted.Render("ctrl+o: context"),
	}
	if m.mouseCaptureOff {
		hints = append(hints, styleWarn.Render("ctrl+t: mouse off"))
	} else {
		hints = append(hints, styleMuted.Render("ctrl+t: select"))
	}
	return strings.Join(hints, styleDim.Render("  ·  "))
}

func renderStatusBanner(text, kind string) string {
	prefix := "• "
	style := styleMuted
	switch kind {
	case "error":
		prefix = "✗ "
		style = styleError
	case "warn":
		prefix = "! "
		style = styleWarn
	case "success":
		prefix = "✓ "
		style = styleSuccess
	}
	return style.Render(prefix + text)
}

func (m Model) sidePanelWidth() int {
	if !m.showSidePanel {
		return 0
	}
	if m.width < 110 {
		return 0
	}
	w := m.width / 3
	if w < 34 {
		w = 34
	}
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
	for i := len(m.activityFeed) - 1; i >= 0; i-- {
		entry := m.activityFeed[i]
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
		lipgloss.NewStyle().Foreground(colorMuted).Render("⎇"),
		lipgloss.NewStyle().Bold(true).Foreground(colorText).Render(branch),
		styleMuted.Render(repo),
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#22C55E")).Render(fmt.Sprintf("+%d", added)),
		lipgloss.NewStyle().Bold(true).Foreground(colorError).Render(fmt.Sprintf("-%d", deleted)),
	}, " ")
	return line, 2
}

func (m Model) sideListGeometry() (startY, rows int) {
	_, gitRows := m.sidePanelGitSummary(m.sidePanelWidth())
	_, _, rows = m.sidePanelWindow(m.sidePanelItems(), m.sidePanelInnerHeight(), gitRows)
	return 5 + gitRows, rows
}

func (m Model) sidePanelInnerHeight() int {
	h := m.height - 4
	if h < 12 {
		h = 12
	}
	return h
}

func (m Model) sidePanelWindow(items []sidePanelItem, innerHeight, gitRows int) (cursor, start, rows int) {
	if len(items) == 0 {
		return 0, 0, 4
	}
	cursor = m.sideCursor
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(items) {
		cursor = len(items) - 1
	}
	availableRows := innerHeight - 10 - gitRows
	if availableRows < 6 {
		availableRows = 6
	}
	rows = min(max(4, availableRows/2), max(4, len(items)))
	start = m.sideScroll
	maxStart := max(0, len(items)-rows)
	if start > maxStart {
		start = maxStart
	}
	if cursor < start {
		start = cursor
	}
	if cursor >= start+rows {
		start = cursor - rows + 1
	}
	return cursor, start, rows
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

func (m Model) sidePanelLines(items []sidePanelItem, width, cursor, start, rows int) ([]string, []int) {
	lines := make([]string, 0, rows+4)
	rowToItem := make([]int, 0, rows+4)
	rowBudget := max(12, width-6)
	prevAgent := ""
	renderedItems := 0
	for idx := start; idx < len(items) && renderedItems < rows; idx++ {
		it := items[idx]
		agent := activityAgentLabel(it.Agent)
		if agent != prevAgent {
			header := lipgloss.NewStyle().Foreground(colorMuted).Bold(true).Render("  " + truncateLabel(agent, max(6, rowBudget-2)))
			lines = append(lines, header)
			rowToItem = append(rowToItem, -1)
			prevAgent = agent
		}
		prefix := "    "
		titleStyle := lipgloss.NewStyle().Foreground(colorMuted)
		if idx == cursor {
			prefix = lipgloss.NewStyle().Foreground(m.currentColor()).Bold(true).Render("›   ")
			titleStyle = lipgloss.NewStyle().Foreground(colorText).Bold(true)
		}
		detailColor := colorDim
		switch it.Status {
		case "running":
			detailColor = m.currentColor()
		case "error", "failed":
			detailColor = colorError
		case "changed":
			detailColor = lipgloss.Color("#22C55E")
		default:
			if it.Kind == "file" {
				detailColor = lipgloss.Color("#22C55E")
			}
			if it.Kind == "command" {
				detailColor = lipgloss.Color("#60A5FA")
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
		renderedItems++
	}
	return lines, rowToItem
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
		lipgloss.NewStyle().Bold(true).Foreground(colorMuted).Render("Details"),
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
	available := innerHeight - reserved
	if available < 7 {
		available = 7
	}
	listLines = max(4, min(12, available/2))
	detailBodyRows = max(3, available-listLines)
	return listLines, detailBodyRows
}

func (m Model) sidePanelDetailMaxScroll(width int) int {
	items := m.sidePanelItems()
	if len(items) == 0 {
		return 0
	}
	innerHeight := m.sidePanelInnerHeight()
	_, gitRows := m.sidePanelGitSummary(width)
	cursor, _, _ := m.sidePanelWindow(items, innerHeight, gitRows)
	selected := items[cursor]
	meta := m.sidePanelDetailMeta(selected)
	_, detailBodyRows := m.sidePanelBudgets(innerHeight, gitRows, len(meta))
	body := m.sidePanelDetailBody(selected, width)
	_, _, maxOffset := scrollBlock(body, detailBodyRows, m.sideDetailScroll)
	return maxOffset
}

func (m Model) viewSidePanel(width int) string {
	innerHeight := m.sidePanelInnerHeight()
	gitSummary, gitRows := m.sidePanelGitSummary(width)
	items := m.sidePanelItems()
	if len(items) == 0 {
		parts := []string{
			lipgloss.NewStyle().Bold(true).Render("Activity"),
			styleMuted.Render("Operational tool activity"),
		}
		if gitSummary != "" {
			parts = append(parts, "", gitSummary)
		}
		parts = append(parts, "", styleMuted.Render("Observability is on. Commands, edits, and other tool activity will appear here."))
		body := lipgloss.JoinVertical(lipgloss.Left, parts...)
		box := lipgloss.NewStyle().
			Width(width).
			Height(innerHeight).
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Padding(0, 1).
			Render(clampLines(body, innerHeight))
		return box
	}

	cursor, start, rows := m.sidePanelWindow(items, innerHeight, gitRows)
	lines, _ := m.sidePanelLines(items, width, cursor, start, rows)
	selected := items[cursor]
	detailMeta := m.sidePanelDetailMeta(selected)
	listLinesBudget, detailBodyRows := m.sidePanelBudgets(innerHeight, gitRows, len(detailMeta))

	listBlock := clampLines(strings.Join(lines, "\n"), listLinesBudget)

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
		styleMuted.Render("Operational tool activity"),
	}
	if gitSummary != "" {
		contentParts = append(contentParts, "", gitSummary)
	}
	contentParts = append(contentParts, "", listBlock, "", detailsBlock)
	content := lipgloss.JoinVertical(lipgloss.Left, contentParts...)
	content = clampLines(content, innerHeight)

	return lipgloss.NewStyle().
		Width(width).
		Height(innerHeight).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorBorder).
		Padding(0, 1).
		Render(content)
}
