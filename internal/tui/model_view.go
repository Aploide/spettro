package tui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/compact"
	"spettro/internal/diff"
	"spettro/internal/jobs"
	"spettro/internal/session"
	"spettro/internal/version"
)

// approvalDiffCollapsedLines is how many diff lines a file-write/file-edit
// approval prompt shows before collapsing (ctrl+o expands).
const approvalDiffCollapsedLines = 16

// approvalDiffChromeLines is everything in the frame besides the diff when an
// approval dialog is open: header(1) + eyes(8) + separators(2) + status(1) +
// input box incl. borders(6) + approval label/reason/picker(6) + the 3-line
// minimum viewport, plus one row of slack.
const approvalDiffChromeLines = 28

// approvalDiffView renders the diff block of a pending file-write/file-edit
// approval, sized so the whole input box always fits the terminal. Both
// viewInput and recalcLayout call this, so the layout budget and the actual
// render can never disagree.
func (m Model) approvalDiffView(width int) string {
	if m.pendingAuth == nil || m.pendingAuth.request.Diff == "" {
		return ""
	}
	maxLines := approvalDiffCollapsedLines
	if m.approvalDiffExpanded {
		maxLines = 1 << 20 // no cap beyond what fits on screen
	}
	if fit := m.height - approvalDiffChromeLines; fit < maxLines {
		maxLines = fit
	}
	if maxLines < 3 {
		maxLines = 3
	}
	return diff.Render(m.pendingAuth.request.Diff, diff.Options{
		Width:      width - 6,
		MaxLines:   maxLines,
		ExpandHint: "(ctrl+o to expand)",
		Indent:     "  ",
	})
}

// View assembles the frame and declares terminal features (alt screen, mouse
// mode, focus reporting) on the returned tea.View, per the bubbletea v2
// declarative model.
func (m Model) View() tea.View {
	v := tea.NewView(m.viewContent())
	v.AltScreen = true
	// Focus/blur events drive m.terminalFocused (desktop notifications).
	v.ReportFocus = true
	// ctrl+t toggles mouse capture off so the terminal's native text
	// selection works (see the KeyPressMsg handler in update()).
	if m.mouseCaptureOff {
		v.MouseMode = tea.MouseModeNone
	} else {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

func (m Model) viewContent() string {
	if !m.ready {
		return lipgloss.NewStyle().Foreground(colorMuted).Render("\n  loading…")
	}

	// Render the overlay chosen by the single source of truth so View can
	// never disagree with update()'s key routing. A nil view (modalSetup,
	// legacy) falls through to the main pane.
	if h, ok := modalHandlers[m.activeModal()]; ok && h.view != nil {
		return h.view(m)
	}

	header := m.viewHeader()
	paneW := m.paneWidth()
	inputArea := m.viewInput(paneW)
	statusBar := m.viewStatusBar(paneW)
	sideW := m.sidePanelWidth()

	var parts []string
	if len(m.cmdItems) > 0 {
		// Overlay spans the full inner area. Fixed costs: header(1)+input(6)+status(1)=8.
		innerH := m.height - 8
		if innerH < 4 {
			innerH = 4
		}
		overlay := m.viewCmdOverlay(m.vp.Width(), innerH)
		parts = []string{overlay, inputArea, statusBar}
	} else {
		eyes := renderEyes(m.mode, m.eyeFrame, m.thinking, paneW)
		sep := m.viewSep(paneW)
		content := m.vp.View()
		parts = []string{eyes, sep, content, sep}
		if sideW <= 0 {
			if pa := m.renderParallelAgents(); pa != "" {
				parts = append(parts, pa)
			}
		}
		parts = append(parts, inputArea, statusBar)
	}

	mainPane := lipgloss.JoinVertical(lipgloss.Left, parts...)

	if sideW <= 0 {
		return lipgloss.JoinVertical(lipgloss.Left, header, mainPane)
	}
	sidePane := m.viewSidePanel(sideW)
	divider := lipgloss.NewStyle().Foreground(colorBorder).Render("│")
	body := lipgloss.JoinHorizontal(lipgloss.Top, mainPane, divider, sidePane)
	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}

// diagFillTitle builds a section header like "Title ╱╱╱╱╱╱╱╱╱╱╱" filling innerWidth.
func diagFillTitle(label string, innerWidth int) string {
	lw := lipgloss.Width(label)
	remaining := innerWidth - lw - 1
	if remaining <= 0 {
		return label
	}
	fill := lipgloss.NewStyle().Foreground(colorDim).Render(strings.Repeat("╱", remaining))
	return label + " " + fill
}

func (m Model) viewHeader() string {
	mc := m.currentColor()
	logo := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ spettro " + version.App)
	if planName := m.spettroPlanName(); planName != "" {
		sep := lipgloss.NewStyle().Bold(true).Foreground(mc).Render(" - ")
		logo += sep + renderPlanLabel(planName, m.eyeFrame)
	}

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
	thinkingTag := ""
	if level := strings.TrimSpace(m.cfg.ThinkingLevel); level != "" && level != "off" &&
		m.activeModelSupportsReasoning() {
		thinkingTag = "thinking:" + level
	}
	ultraTag := ""
	if m.cfg.UltraActive() {
		ultraTag = "ultra"
	}
	sandboxTag := ""
	if m.sandboxState != nil {
		if p := m.sandboxState.Policy(); p.Enabled() {
			sandboxTag = "sandbox:" + p.Short()
		}
	}
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
	if thinkingTag != "" {
		right = styleMuted.Render(thinkingTag) + "  " + right
	}
	if ultraTag != "" {
		right = lipgloss.NewStyle().Foreground(mc).Bold(true).Render(ultraTag) + "  " + right
	}
	if sandboxTag != "" {
		right = styleMuted.Render(sandboxTag) + "  " + right
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
		Background(colorHeaderBg).
		Render(row)
}

func (m Model) viewSep(width int) string {
	return lipgloss.NewStyle().
		Foreground(colorDim).
		Render(strings.Repeat("─", width))
}

// renderPlanLabel renders a plan name with its tier color.
// "max" animates through rainbow colors using the given frame counter.
func renderPlanLabel(plan string, frame int) string {
	label := strings.ToUpper(plan)
	switch plan {
	case "free":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#9CA3AF")).Render(label)
	case "lite":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F9FAFB")).Render(label)
	case "plus":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#86EFAC")).Render(label)
	case "pro":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#C4B5FD")).Render(label)
	case "max":
		rainbow := []color.Color{
			lipgloss.Color("#FF6B6B"), lipgloss.Color("#FF9E4F"), lipgloss.Color("#FFD93D"),
			lipgloss.Color("#6BCB77"), lipgloss.Color("#4D96FF"), lipgloss.Color("#C77DFF"),
		}
		var out string
		for i, ch := range label {
			c := rainbow[(i+frame)%len(rainbow)]
			out += lipgloss.NewStyle().Bold(true).Foreground(c).Render(string(ch))
		}
		return out
	default:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#9CA3AF")).Render(label)
	}
}

// dialogInnerWidth returns the usable content width for a dialog of dialogWidth,
// accounting for 2-char padding on each side.
func dialogInnerWidth(dialogWidth int) int {
	w := dialogWidth - 4
	if w < 4 {
		w = 4
	}
	return w
}

// viewCmdOverlay renders the /command suggestions as a centered overlay in the
// content area so the layout (eyes, viewport, input, status) never shifts.
func (m Model) viewCmdOverlay(width, height int) string {
	mc := m.currentColor()

	dialogWidth := width - 4
	if dialogWidth > 68 {
		dialogWidth = 68
	}
	if dialogWidth < 32 {
		dialogWidth = 32
	}
	innerW := dialogInnerWidth(dialogWidth)

	titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ commands")
	title := diagFillTitle(titleLabel, innerW)

	// Descriptions must fit on one line to prevent the dialog from growing taller
	// than the height passed to lipgloss.Place (which doesn't clip overflow).
	maxDescW := innerW - 18
	if maxDescW < 8 {
		maxDescW = 8
	}

	var rows []string
	for i, cmd := range m.cmdItems {
		desc := truncateLabel(cmd.desc, maxDescW)
		if i == m.cmdCursor {
			label := fmt.Sprintf("%-16s  %s", cmd.name, desc)
			rows = append(rows, lipgloss.NewStyle().
				Background(colorSelBg).
				Foreground(colorText).
				Bold(true).
				Width(innerW).
				Render(label))
		} else {
			nameStyle := lipgloss.NewStyle().Foreground(colorText)
			descStyle := lipgloss.NewStyle().Foreground(colorMuted)
			rows = append(rows, nameStyle.Render(fmt.Sprintf("%-16s", cmd.name))+"  "+descStyle.Render(desc))
		}
	}
	if len(m.cmdItems) == 0 {
		rows = append(rows, styleMuted.Render("  no matches"))
	}

	hint := styleMuted.Render("enter inserts  enter again runs")

	maxRows := len(rows)
	if height > 0 && maxRows > height-8 {
		maxRows = height - 8
	}
	if maxRows < 4 {
		maxRows = 4
	}
	start := 0
	if len(rows) > maxRows {
		start = m.cmdCursor - maxRows/2
		if start < 0 {
			start = 0
		}
		if start+maxRows > len(rows) {
			start = len(rows) - maxRows
		}
		rows = rows[start : start+maxRows]
	}

	dialog := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(dialogWidth+2).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			title,
			"",
			strings.Join(rows, "\n"),
			"",
			hint,
		))

	return lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
	)
}

func (m Model) viewMentionPalette(width int) string {
	if len(m.mentionItems) == 0 {
		return ""
	}
	boxW := width - 4
	innerW := dialogInnerWidth(boxW)
	titleLabel := lipgloss.NewStyle().Foreground(colorMuted).Bold(true).Render("available files")
	title := diagFillTitle(titleLabel, innerW)
	var rows []string
	for i, item := range m.mentionItems {
		if i == m.mentionCursor {
			rows = append(rows, lipgloss.NewStyle().
				Background(colorSelBg).
				Foreground(colorText).
				Bold(true).
				Width(innerW).
				Render("› "+item))
		} else {
			rows = append(rows, lipgloss.NewStyle().Foreground(colorMuted).Render("  "+item))
		}
	}
	hint := styleMuted.Render("↑↓ navigate  enter inserts mention")
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorBorder).
		Width(boxW + 2).
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
	} else if m.showSteerChoice {
		lines = append(lines, styleMuted.Render("  "+truncateLabel(m.steerPending, 100)))
		lines = append(lines, m.renderApprovalPicker(
			"agent is running — deliver this message how?",
			steerChoiceOptions,
			m.steerCursor,
			mc,
		))
		lines = append(lines, styleMuted.Render("  enter selects  esc keeps typing"))
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
		if block := m.approvalDiffView(width); block != "" {
			lines = append(lines, block)
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
		if chips := m.renderAttachmentChips(mc); chips != "" {
			lines = append(lines, chips)
		}
		if m.showAttachPrompt {
			lines = append(lines, styleMuted.Render("  attach file: (esc cancels)"))
		}
		lines = append(lines, m.ta.View())
	}
	boxStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(width).
		PaddingLeft(1).PaddingRight(1)

	inner := strings.Join(lines, "\n")
	inputBox := boxStyle.Render(inner)

	// cmd overlay is shown in content area; only @mention inline popup stays here
	mentionPalette := m.viewMentionPalette(width)
	if mentionPalette == "" {
		return inputBox
	}
	return lipgloss.JoinVertical(lipgloss.Left, mentionPalette, inputBox)
}

// renderGlare produces a shimmer that sweeps left-to-right across text.
// frame drives the position; agentColor sets the gradient's base hue.
func renderGlare(text string, frame int, agentColor color.Color) string {
	runes := []rune(text)
	n := len(runes)
	if n == 0 {
		return ""
	}
	// one position step every 3 frames (150 ms at 50 ms/frame)
	padding := 6
	cycleLen := n + padding
	pos := (frame/3)%cycleLen - padding/2

	grad := glareGradient(agentColor)

	var sb strings.Builder
	for i, r := range runes {
		dist := i - pos
		if dist < 0 {
			dist = -dist
		}
		var fg color.Color
		switch {
		case dist == 0:
			fg = grad[0]
		case dist == 1:
			fg = grad[1]
		case dist == 2:
			fg = grad[2]
		case dist <= 4:
			fg = grad[3]
		default:
			fg = grad[4]
		}
		sb.WriteString(lipgloss.NewStyle().Foreground(fg).Render(string(r)))
	}
	return sb.String()
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
	var lines []string
	if len(active) > 0 {
		lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(colorMuted).Render("  agents"))
		orchText := "orchestrator: " + m.mode
		lines = append(lines, "  "+renderGlare(orchText, m.eyeFrame, m.currentColor()))
	}
	for _, a := range active {
		agentColor := modeColor("")
		if spec, ok := m.manifest.AgentByID(swarmSpecID(a.ID)); ok {
			agentColor = modeColor(spec.Color)
		}
		label := a.ID
		if a.Instance > 1 {
			label = fmt.Sprintf("%s [%d]", a.ID, a.Instance)
		}
		task := a.Task
		if len(task) > 50 {
			task = task[:47] + "..."
		}

		var line string
		switch a.Status {
		case "running":
			combined := fmt.Sprintf("%-20s %s", label, task)
			line = "  " + renderGlare(combined, m.eyeFrame, agentColor)
		case "done":
			line = lipgloss.NewStyle().Foreground(agentColor).Render(fmt.Sprintf("  ● %-18s", label)) +
				lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280")).Render(task)
		case "error", "failed":
			line = lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444")).Render(fmt.Sprintf("  ✗ %-18s", label)) +
				lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280")).Render(task)
		default:
			line = lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("  ○ %-18s", label)) +
				lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280")).Render(task)
		}
		lines = append(lines, line)
	}
	if len(m.todos) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(colorMuted).Render("  todos"))
		blockedIDs := session.BlockedIDs(m.todos)
		for _, td := range m.todos {
			status := td.Status
			// A pending task gated by incomplete dependencies renders as
			// blocked so the graph state is visible at a glance.
			if _, gated := blockedIDs[td.ID]; gated && status == "pending" {
				status = "blocked"
			}
			var line string
			switch status {
			case "completed", "done":
				label := td.Content
				if len(label) > 56 {
					label = label[:53] + "..."
				}
				line = lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981")).Render("  ✓ ") + styleMuted.Render(label)
			case "in_progress", "running":
				label := td.Content
				if len(label) > 56 {
					label = label[:53] + "..."
				}
				line = "  " + renderGlare(label, m.eyeFrame, lipgloss.Color("#F59E0B"))
			case "blocked", "failed", "cancelled":
				label := td.Content
				if len(label) > 56 {
					label = label[:53] + "..."
				}
				line = lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444")).Render("  ! ") + styleMuted.Render(label)
			default:
				label := td.Content
				if len(label) > 56 {
					label = label[:53] + "..."
				}
				line = lipgloss.NewStyle().Foreground(colorMuted).Render("  ○ ") + styleMuted.Render(label)
			}
			lines = append(lines, line)
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

// evaluateCompact is the single source of truth for context-pressure
// evaluation. Every gauge / warning / auto-compaction / blocking decision goes
// through here so they all read the same occupancy estimate (contextTokens,
// NOT the cumulative cost) against the same window and config.
func (m Model) evaluateCompact() compact.Evaluation {
	window := m.contextWindow()
	if window == 0 {
		window = contextWindowDefault(m.cfg.ActiveProvider)
	}
	return compact.Evaluate(window, compact.Config{
		AutoEnabled:      m.cfg.AutoCompactEnabled,
		AutoThresholdPct: m.cfg.AutoCompactThresholdPct,
		MaxFailures:      m.cfg.AutoCompactMaxFailures,
	}, compact.State{
		TokensUsed:          m.contextTokens,
		ConsecutiveFailures: m.autoCompactFailures,
	})
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
	if m.thinking || m.contextTokens == 0 {
		return nil
	}
	eval := m.evaluateCompact()
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

	eval := m.evaluateCompact()
	// The gauge shows occupancy (how full the window is), not cumulative cost.
	used := m.contextTokens
	var ctxColor color.Color
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
	// Live prompt-cache cue: hit rate of the LAST request. A sudden drop means
	// the cached prefix broke — visible without running /stats.
	if label, healthy := m.cacheIndicator(); label != "" {
		cacheColor := lipgloss.Color("#F59E0B")
		if healthy {
			cacheColor = lipgloss.Color("#10B981")
		}
		right = lipgloss.NewStyle().Foreground(cacheColor).Render(label) + "  " + right
	}
	if n := jobs.Default().RunningCount(); n > 0 {
		label := fmt.Sprintf("◉ %d bg job", n)
		if n > 1 {
			label += "s"
		}
		right = lipgloss.NewStyle().Foreground(lipgloss.Color("#22C55E")).Render(label) + "  " + right
	}

	leftWidth := width - lipgloss.Width(right) - 2
	if leftWidth < 0 {
		leftWidth = 0
	}
	leftPadded := lipgloss.NewStyle().Width(leftWidth).Render(left)

	bar := leftPadded + right + " "
	return lipgloss.NewStyle().
		Width(width).
		Background(colorHeaderBg).
		PaddingLeft(1).
		Render(bar)
}

func (m Model) statusBarMessage() string {
	if m.banner != "" {
		return renderStatusBanner(m.banner, m.bannerKind)
	}
	if g := m.activeGoal; g != nil {
		elapsed := time.Since(g.StartedAt).Round(time.Second)
		// Compact format: objective, iteration, time, state
		obj := truncateLabel(g.Objective, 40)
		progress := fmt.Sprintf("iter %d", g.Iteration)
		if g.NoProgress > 0 {
			progress = fmt.Sprintf("iter %d · %d/%d", g.Iteration, g.NoProgress, g.NoProgressLimit)
		}
		state := "running"
		if !m.thinking {
			state = "paused"
		}
		return styleSuccess.Render(fmt.Sprintf("◈ %s · %s · %s · %s", obj, progress, elapsed, state))
	}
	return ""
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
