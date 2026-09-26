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
	"spettro/internal/pty"
	"spettro/internal/session"
	"spettro/internal/theme"
	"spettro/internal/version"
)

// approvalDiffCollapsedLines is how many diff lines a file-write/file-edit
// approval prompt shows before collapsing (ctrl+o expands).
const approvalDiffCollapsedLines = 16

// approvalDiffChromeLines is everything in the frame besides the diff when an
// approval dialog is open: header(1) + separators(2) + status(1) + the
// working indicator(1) + input box incl. borders(6) + approval
// label/reason/picker(6) + the 3-line minimum viewport, plus one row of
// slack. The eye art no longer costs anything here — it moved into the
// scrollback.
const approvalDiffChromeLines = 21

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
	content := m.viewContent()
	if m.textSel.dragging {
		content = applySelectionHighlight(content, m.textSel)
	}
	v := tea.NewView(content)
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
		return lipgloss.NewStyle().Foreground(theme.Current().TextMuted).Render("\n  loading…")
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

	// The working indicator sits directly above the input box on every path
	// that draws one, so a run is visible whether or not the command overlay
	// is open. It is "" when idle and costs no row then.
	indicator := m.viewWorkingIndicator(paneW)

	var parts []string
	if len(m.cmdItems) > 0 {
		// Overlay spans the full inner area. Fixed costs: header(1)+input(6)+status(1)=8,
		// plus the indicator row while a run is in flight.
		innerH := max(m.height-8-m.workingIndicatorHeight(), 4)
		overlay := m.viewCmdOverlay(m.vp.Width(), innerH)
		parts = []string{overlay}
	} else {
		sep := m.viewSep(paneW)
		content := m.vp.View()
		parts = []string{sep, content, sep}
		if sideW <= 0 {
			if pa := m.renderParallelAgents(); pa != "" {
				parts = append(parts, pa)
			}
		}
	}
	if indicator != "" {
		parts = append(parts, indicator)
	}
	parts = append(parts, inputArea, statusBar)

	mainPane := lipgloss.JoinVertical(lipgloss.Left, parts...)

	if sideW <= 0 {
		return lipgloss.JoinVertical(lipgloss.Left, header, mainPane)
	}
	sidePane := m.viewSidePanel(sideW)
	divider := lipgloss.NewStyle().Foreground(theme.Current().Border).Render("│")
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
	fill := lipgloss.NewStyle().Foreground(theme.Current().Rule).Render(strings.Repeat("╱", remaining))
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
	pal := theme.Current()
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
				Foreground(pal.TextOnAccent).
				Background(agColor).
				PaddingLeft(1).PaddingRight(1).
				Render(ag.ID))
		} else {
			tabs = append(tabs, lipgloss.NewStyle().
				Foreground(pal.TextMuted).
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
	maxMetaWidth := max(m.width-logoW-permW-8, 0)
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
	availableCenter := max(m.width-logoW-rightW-2, 0)
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
		Background(theme.Current().BgHeader).
		Render(row)
}

func (m Model) viewSep(width int) string {
	return lipgloss.NewStyle().
		Foreground(theme.Current().Rule).
		Render(strings.Repeat("─", width))
}

// planLabelFrameDivisor slows the MAX rainbow down. The frame counter ticks
// every 50 ms; advancing a hue per tick made the label strobe in the corner of
// the eye, which is the opposite of what a status label should do. One step
// per 150 ms keeps it visibly alive without pulling focus.
const planLabelFrameDivisor = 3

// renderPlanLabel renders a plan name with its tier color.
// "max" animates through rainbow colors using the given frame counter.
func renderPlanLabel(plan string, frame int) string {
	label := strings.ToUpper(plan)
	switch plan {
	case "free":
		return lipgloss.NewStyle().Bold(true).Foreground(theme.Current().TextSubtle).Render(label)
	case "lite":
		return lipgloss.NewStyle().Bold(true).Foreground(theme.Current().Text).Render(label)
	case "plus":
		return lipgloss.NewStyle().Bold(true).Foreground(theme.Current().PlanPlus).Render(label)
	case "pro":
		return lipgloss.NewStyle().Bold(true).Foreground(theme.Current().PlanPro).Render(label)
	case "max":
		rainbow := theme.Current().RampRainbow
		var out strings.Builder
		for i, ch := range label {
			c := rainbow[(i+frame/planLabelFrameDivisor)%len(rainbow)]
			out.WriteString(lipgloss.NewStyle().Bold(true).Foreground(c).Render(string(ch)))
		}
		return out.String()
	default:
		return lipgloss.NewStyle().Bold(true).Foreground(theme.Current().TextSubtle).Render(label)
	}
}

// dialogInnerWidth returns the usable content width for a dialog of dialogWidth,
// accounting for 2-char padding on each side.
func dialogInnerWidth(dialogWidth int) int {
	w := max(dialogWidth-4, 4)
	return w
}

// viewCmdOverlay renders the /command suggestions as a centered overlay in the
// content area so the layout (eyes, viewport, input, status) never shifts.
func (m Model) viewCmdOverlay(width, height int) string {
	mc := m.currentColor()

	dialogWidth := min(width-4, 68)
	if dialogWidth < 32 {
		dialogWidth = 32
	}
	innerW := dialogInnerWidth(dialogWidth)

	titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ commands")
	title := diagFillTitle(titleLabel, innerW)

	// Descriptions must fit on one line to prevent the dialog from growing taller
	// than the height passed to lipgloss.Place (which doesn't clip overflow).
	maxDescW := max(innerW-18, 8)

	var rows []string
	for i, cmd := range m.cmdItems {
		desc := truncateLabel(cmd.desc, maxDescW)
		if i == m.cmdCursor {
			label := fmt.Sprintf("%-16s  %s", cmd.name, desc)
			rows = append(rows, lipgloss.NewStyle().
				Background(theme.Current().BgSelection).
				Foreground(theme.Current().Text).
				Bold(true).
				Width(innerW).
				Render(label))
		} else {
			nameStyle := lipgloss.NewStyle().Foreground(theme.Current().Text)
			descStyle := lipgloss.NewStyle().Foreground(theme.Current().TextMuted)
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
		start = max(m.cmdCursor-maxRows/2, 0)
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

	// The dialog has floors of its own (four rows of list plus title, hint,
	// padding and border), so on a very short terminal Place is handed less
	// height than the dialog needs and does not clip. MaxHeight makes the
	// overlay honour the budget viewContent reserved for it instead of
	// pushing the input box off the bottom of the screen.
	return lipgloss.NewStyle().MaxHeight(height).Render(lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(theme.Current().Rule)),
	))
}

func (m Model) viewMentionPalette(width int) string {
	if len(m.mentionItems) == 0 {
		return ""
	}
	boxW := width - 4
	innerW := dialogInnerWidth(boxW)
	titleLabel := lipgloss.NewStyle().Foreground(theme.Current().TextMuted).Bold(true).Render("available files")
	title := diagFillTitle(titleLabel, innerW)
	var rows []string
	for i, item := range m.mentionItems {
		if i == m.mentionCursor {
			rows = append(rows, lipgloss.NewStyle().
				Background(theme.Current().BgSelection).
				Foreground(theme.Current().Text).
				Bold(true).
				Width(innerW).
				Render("› "+item))
		} else {
			rows = append(rows, lipgloss.NewStyle().Foreground(theme.Current().TextMuted).Render("  "+item))
		}
	}
	hint := styleMuted.Render("↑↓ navigate  enter inserts mention")
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(theme.Current().Border).
		Width(boxW + 2).
		PaddingLeft(2).PaddingRight(2).
		Render(title + "\n\n" + strings.Join(rows, "\n") + "\n\n" + hint)
}

// wrapPlainLines word-wraps unstyled text to width and returns one entry per
// terminal line, so callers can count and cut lines before styling them —
// measuring styled output is what lets a wrapped line escape the layout's
// height budget.
func wrapPlainLines(s string, width int) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	wrapped := lipgloss.NewStyle().Width(width).Render(s)
	lines := strings.Split(wrapped, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}

// clampTextLines keeps at most maxLines of text, marking the cut with an ellipsis
// so a long question reads as truncated rather than silently missing its tail.
func clampTextLines(lines []string, maxLines, width int) []string {
	if maxLines < 1 || len(lines) <= maxLines {
		return lines
	}
	lines = lines[:maxLines]
	last := len(lines) - 1
	lines[last] = truncateLabel(lines[last], max(width-2, 4)) + " …"
	return lines
}

// inputTextareaView is the textarea with the workflow keyword lit up. Every
// place the input is drawn goes through here so the effect cannot appear in
// one input state and vanish in another.
func (m Model) inputTextareaView() string {
	return highlightUltracode(m.ta.View(), m.eyeFrame)
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
			lines = append(lines, m.inputTextareaView())
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
		lines = append(lines, m.renderQuestionForm())
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
			lines = append(lines, m.inputTextareaView())
		} else {
			lines = append(lines, m.renderApprovalPicker(
				"allow this command?",
				shellApprovalOptions,
				m.approvalCursor,
				theme.Current().Warning,
			))
		}
	} else {
		if chips := m.renderAttachmentChips(mc); chips != "" {
			lines = append(lines, chips)
		}
		if m.showAttachPrompt {
			lines = append(lines, styleMuted.Render("  attach file: (esc cancels)"))
		}
		lines = append(lines, m.inputTextareaView())
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

// footerBudget is the total number of rows everything between the transcript
// and the input may occupy: workflow, swarm, delegations and todos combined.
//
// Each of those used to size itself independently, so a run with a workflow
// and a todo list could eat two thirds of a short terminal between them. They
// now share one allowance, scaled to the window, and spend it in priority
// order — what is happening right now first.
func footerBudget(height int) int {
	return min(max(height/4, 4), 12)
}

// renderParallelAgents draws everything that sits between the transcript and
// the input: the workflow summary, the Ultra swarm, ordinary delegations, and
// the todo list. Swarms and workflows get their own bordered blocks — a
// fan-out of twenty agents mixed into the plain delegation list was
// unreadable, and the two are different enough that sharing one flat list
// helped nobody.
//
// Whatever is here is an annotation on the conversation, never a replacement
// for it, so the whole region is bounded and each block takes only what the
// ones before it left.
func (m Model) renderParallelAgents() string {
	paneW := m.paneWidth()
	remaining := footerBudget(m.height)
	var blocks []string

	// A bordered block costs its lines plus the border.
	spend := func(block string) {
		if block == "" {
			return
		}
		blocks = append(blocks, block)
		remaining -= lipgloss.Height(block)
	}

	active := make([]parallelAgentEntry, 0, len(m.parallelAgents))
	for _, a := range m.parallelAgents {
		// Swarm members have their own block above; listing them here too
		// would double every row of a fan-out.
		if a.Status == "running" && a.Kind != "swarm" {
			active = append(active, a)
		}
	}
	// Delegations and todos are shown nowhere else, so a swarm must not be
	// able to push them off the screen entirely: one row each is held back,
	// which is what their one-line forms need.
	reserved := 0
	if len(active) > 0 {
		reserved++
	}
	if len(m.todos) > 0 {
		reserved++
	}
	if rows := remaining - reserved - 2; rows >= 2 {
		spend(m.renderWorkflowBlock(paneW, rows))
	}
	if rows := remaining - reserved - 2; rows >= 2 {
		spend(m.renderSwarmBlock(paneW, rows))
	}
	if remaining <= 0 || (len(active) == 0 && len(m.todos) == 0) {
		return strings.Join(blocks, "\n")
	}

	var lines []string
	todoReserve := 0
	if len(m.todos) > 0 {
		todoReserve = 1
	}
	if deleg := m.delegationLines(active, remaining-todoReserve); len(deleg) > 0 {
		lines = append(lines, deleg...)
		remaining -= len(deleg)
	}
	if len(m.todos) > 0 && remaining >= 1 {
		if len(lines) > 0 && remaining >= 2 {
			lines = append(lines, "")
			remaining--
		}
		lines = append(lines, m.todoLines(remaining)...)
	}
	if len(lines) > 0 {
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n")
}

// delegationLines lists the ordinary sub-agents inside the row budget it is
// given — the count of hidden workers included, since that line costs a row
// too and forgetting it is how the block used to overrun and shove the todo
// list off the screen.
func (m Model) delegationLines(active []parallelAgentEntry, rows int) []string {
	if rows < 1 || len(active) == 0 {
		return nil
	}
	header := lipgloss.NewStyle().Bold(true).Foreground(theme.Current().TextMuted).Render("  agents")
	// The tightest form still says the work exists and where to look.
	compact := []string{header + styleMuted.Render(fmt.Sprintf("  %d running · ctrl+b", len(active)))}
	if rows == 1 {
		return compact
	}
	avail := rows - 2 // header + orchestrator
	if len(active) > avail {
		avail-- // the "… N more" line
	}
	shown := max(min(len(active), avail), 0)
	lines := []string{
		header,
		"  " + renderGlare("orchestrator: "+m.mode, m.eyeFrame, m.currentColor()),
	}
	for _, a := range active[:shown] {
		lines = append(lines, m.delegationRow(a))
	}
	if hidden := len(active) - shown; hidden > 0 {
		lines = append(lines, styleMuted.Render(fmt.Sprintf("  … %d more · ctrl+b for all of them", hidden)))
	}
	// When the listed form does not fit, the count does — better one honest
	// line than a truncated list that reads as the whole story.
	if len(lines) > rows {
		return compact
	}
	return lines
}

// delegationRow renders one ordinary (non-swarm) sub-agent.
func (m Model) delegationRow(a parallelAgentEntry) string {
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
	pal := theme.Current()
	taskStyle := lipgloss.NewStyle().Foreground(pal.TextMuted)
	switch a.Status {
	case "running":
		return "  " + renderGlare(fmt.Sprintf("%-20s %s", label, task), m.eyeFrame, agentColor)
	case "done":
		return lipgloss.NewStyle().Foreground(agentColor).Render(fmt.Sprintf("  ● %-18s", label)) +
			taskStyle.Render(task)
	case "error", "failed":
		return lipgloss.NewStyle().Foreground(pal.Error).Render(fmt.Sprintf("  ✗ %-18s", label)) +
			taskStyle.Render(task)
	default:
		return lipgloss.NewStyle().Foreground(pal.TextMuted).Render(fmt.Sprintf("  ○ %-18s", label)) +
			taskStyle.Render(task)
	}
}

// todoLines renders the task list within a row budget, keeping what is live:
// in-progress and blocked tasks first, then whatever is still pending.
// Completed ones are counted rather than listed — a finished task is not news.
// With a single row to spend it collapses to a count plus the current task.
func (m Model) todoLines(rows int) []string {
	blockedIDs := session.BlockedIDs(m.todos)
	statusOf := func(td session.Todo) string {
		if _, gated := blockedIDs[td.ID]; gated && td.Status == "pending" {
			return "blocked"
		}
		return td.Status
	}
	rank := map[string]int{"in_progress": 0, "running": 0, "blocked": 1, "failed": 1, "cancelled": 1}
	var live, rest []session.Todo
	completed := 0
	for _, td := range m.todos {
		st := statusOf(td)
		if st == "completed" || st == "done" {
			completed++
			continue
		}
		if _, hot := rank[st]; hot {
			live = append(live, td)
		} else {
			rest = append(rest, td)
		}
	}
	ordered := append(live, rest...)
	if len(ordered) == 0 {
		return nil
	}
	header := lipgloss.NewStyle().Bold(true).Foreground(theme.Current().TextMuted).Render("  todos")
	if completed > 0 {
		header += styleMuted.Render(fmt.Sprintf("  %d/%d done", completed, completed+len(ordered)))
	}
	compact := []string{header + " " + styleMuted.Render(truncateLabel(ordered[0].Content, 56))}
	if rows < 2 {
		return compact
	}
	lines := []string{header}
	avail := rows - 1
	if len(ordered) > avail {
		avail-- // the "… N more" line
	}
	shown := min(len(ordered), max(avail, 1))
	for _, td := range ordered[:shown] {
		lines = append(lines, todoRow(td, statusOf(td), m.eyeFrame))
	}
	if hidden := len(ordered) - shown; hidden > 0 {
		lines = append(lines, styleMuted.Render(fmt.Sprintf("  … %d more", hidden)))
	}
	if len(lines) > rows {
		return compact
	}
	return lines
}

func todoRow(td session.Todo, status string, frame int) string {
	label := td.Content
	if len(label) > 56 {
		label = label[:53] + "..."
	}
	switch status {
	case "in_progress", "running":
		return "  " + renderGlare(label, frame, theme.Current().Warning)
	case "blocked", "failed", "cancelled":
		return lipgloss.NewStyle().Foreground(theme.Current().Error).Render("  ! ") + styleMuted.Render(label)
	default:
		return lipgloss.NewStyle().Foreground(theme.Current().TextMuted).Render("  ○ ") + styleMuted.Render(label)
	}
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
	if n := len(m.convHistory); n > 0 && n == m.autoCompactNoopLen {
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
	pal := theme.Current()
	// The gauge shows occupancy (how full the window is), not cumulative cost.
	used := m.contextTokens
	var ctxColor color.Color
	switch {
	case eval.IsError:
		ctxColor = pal.Error
	case eval.IsWarning:
		ctxColor = pal.Warning
	default:
		ctxColor = pal.TextMuted
	}
	ctxLabel := fmt.Sprintf("%s / %s ctx", formatTokenCount(used), formatTokenCount(eval.EffectiveWindow))
	if !m.cfg.AutoCompactEnabled {
		ctxLabel += " (auto off)"
	}
	right := lipgloss.NewStyle().Foreground(ctxColor).Render(ctxLabel)
	// Live prompt-cache cue: hit rate of the LAST request. A sudden drop means
	// the cached prefix broke — visible without running /stats.
	if label, healthy := m.cacheIndicator(); label != "" {
		cacheColor := pal.Warning
		if healthy {
			cacheColor = pal.Success
		}
		right = lipgloss.NewStyle().Foreground(cacheColor).Render(label) + "  " + right
	}
	if n := jobs.Default().RunningCount(); n > 0 {
		label := fmt.Sprintf("◉ %d bg job", n)
		if n > 1 {
			label += "s"
		}
		right = lipgloss.NewStyle().Foreground(pal.SuccessBright).Render(label) + "  " + right
	}
	if n := pty.Default().RunningCount(); n > 0 {
		label := fmt.Sprintf("▣ %d pty", n)
		right = lipgloss.NewStyle().Foreground(pal.SuccessBright).Render(label) + "  " + right
	}

	leftWidth := max(width-lipgloss.Width(right)-2, 0)
	leftPadded := lipgloss.NewStyle().Width(leftWidth).Render(left)

	bar := leftPadded + right + " "
	return lipgloss.NewStyle().
		Width(width).
		Background(pal.BgHeader).
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
	if l := m.activeLoop; l != nil {
		state := fmt.Sprintf("next in %s", max(time.Until(l.NextAt).Round(time.Second), 0))
		if m.thinking {
			state = "running"
		}
		return styleSuccess.Render(fmt.Sprintf("↻ %s · every %s · iter %d · %s",
			truncateLabel(l.Prompt, 40), l.Interval, l.Iteration, state))
	}
	// The in-flight run's elapsed/token readout lives above the input box now
	// (viewWorkingIndicator); the bar keeps banner, goal and loop on the left
	// and the ctx/cache/jobs cluster on the right.
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
