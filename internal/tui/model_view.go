package tui

import (
	"fmt"
	"image/color"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/compact"
	"spettro/internal/jobs"
	"spettro/internal/pty"
	"spettro/internal/session"
	"spettro/internal/skills"
	"spettro/internal/termtext"
	"spettro/internal/theme"
	"spettro/internal/version"
)

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
		return clampFrame(h.view(m), m.width, m.height)
	}

	header := m.cachedHeader()
	paneW := m.paneWidth()
	inputArea, inputPart := m.cachedInput(paneW)
	statusBar := m.cachedStatusBar(paneW)
	sideW := m.sidePanelWidth()

	// The working indicator sits directly above the input box on every path
	// that draws one, so a run is visible whether or not the command overlay
	// is open. It is "" when idle and costs no row then.
	indicator := m.viewWorkingIndicator(paneW)

	var parts []framePart
	if len(m.cmdItems) > 0 {
		// The overlay takes the place of the separators, the transcript and
		// the footer: every row the header, the status bar, the working
		// indicator and the input area leave. The input area is measured,
		// as recalcLayout does, because its height varies (attachment
		// chips, a taller textarea).
		innerH := max(m.height-1-1-m.workingIndicatorHeight()-lipgloss.Height(inputArea), 1)
		overlay := m.viewCmdOverlay(m.vp.Width(), innerH)
		parts = []framePart{newFramePart(overlay)}
	} else {
		sep := fixedWidthPart(m.viewSep(paneW), paneW)
		parts = []framePart{sep, fixedWidthPart(m.vp.View(), m.vp.Width()), sep}
		if m.showsParallelFooter() {
			if pa, part := m.cachedParallelAgents(); pa != "" {
				parts = append(parts, part)
			}
		}
	}
	if indicator != "" {
		parts = append(parts, newFramePart(indicator))
	}
	parts = append(parts, inputPart, statusBar)

	if sideW <= 0 {
		return composeFrame(header, parts, nil)
	}
	// A blank gutter column between the panes: the panel draws its own
	// border. The gutter was once a one-row "│", which JoinHorizontal left
	// as a stray tick at the end of the transcript's top rule.
	side := m.cachedSidePanel(sideW)
	return composeFrame(header, parts, &side)
}

// clampFrame cuts a full-screen frame to the terminal: at most height rows,
// each at most width cells (cut with "…"). The full-screen modals (resume,
// model selector, connect, theme, rewind, memory review, ...) centre a
// dialog of their own design; each keeps itself inside the terminal at the
// sizes it was designed for, and this is the backstop that makes a dialog
// taller or wider than a very small window crop at the edge instead of
// scrolling the whole screen. A zero size (no WindowSizeMsg yet) leaves the
// frame alone.
func clampFrame(frame string, width, height int) string {
	if width <= 0 || height <= 0 {
		return frame
	}
	lines := strings.Split(frame, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for i, line := range lines {
		lines[i] = termtext.Fit(line, width)
	}
	return strings.Join(lines, "\n")
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
	if mod, ok := m.providers.Lookup(m.cfg.ActiveProvider, m.cfg.ActiveModel); ok {
		if mod.DisplayName != "" {
			modelLabel = mod.DisplayName
		}
		if mod.ProviderName != "" {
			provLabel = mod.ProviderName
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
	// The header is exactly one row. On a narrow terminal the logo, tabs and
	// model/permission tags do not all fit, and the Width style below would
	// wrap the overflow onto a second row that the layout never budgeted,
	// pushing the status bar off the bottom of the screen. Cut it instead.
	row = termtext.Fit(row, m.width)

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

// Command overlay chrome. The full dialog spends 8 rows around its list:
// border (2), vertical padding (2), the title, a blank row on each side of
// the list and the key hint. On a short terminal that leaves no room for
// the list, so the compact dialog keeps only the border and the title.
const (
	cmdOverlayFullChrome    = 8
	cmdOverlayCompactChrome = 3
)

// viewCmdOverlay renders the /command suggestions as a centered overlay in the
// content area so the layout (eyes, viewport, input, status) never shifts.
//
// The dialog fits inside height rows: the list is windowed around the cursor
// and, when even four rows of list do not fit with the full chrome, the
// compact form is used. Only a terminal too short for the compact form's
// border, title and one row gets a dialog cut by MaxHeight.
func (m Model) viewCmdOverlay(width, height int) string {
	mc := m.currentColor()

	dialogWidth := min(width-4, 68)
	if dialogWidth < 32 {
		dialogWidth = 32
	}
	innerW := dialogInnerWidth(dialogWidth)

	titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ commands")
	title := diagFillTitle(titleLabel, innerW)

	// Every row must fit on one line: a wrapped row makes the dialog taller
	// than the height passed to lipgloss.Place, which does not clip.
	var rows []string
	for i, cmd := range m.cmdItems {
		name, desc := cmdMenuColumns(cmd.name, termtext.SingleLine(cmd.desc), innerW)
		if i == m.cmdCursor {
			rows = append(rows, lipgloss.NewStyle().
				Background(theme.Current().BgSelection).
				Foreground(theme.Current().Text).
				Bold(true).
				Width(innerW).
				Render(name+"  "+desc))
		} else {
			nameStyle := lipgloss.NewStyle().Foreground(theme.Current().Text)
			descStyle := lipgloss.NewStyle().Foreground(theme.Current().TextMuted)
			rows = append(rows, nameStyle.Render(name)+"  "+descStyle.Render(desc))
		}
	}
	if len(m.cmdItems) == 0 {
		rows = append(rows, styleMuted.Render("  no matches"))
	}

	hint := styleMuted.Render("enter inserts  enter again runs")

	compact := height > 0 && height-cmdOverlayFullChrome < min(len(rows), 4)
	chrome := cmdOverlayFullChrome
	if compact {
		chrome = cmdOverlayCompactChrome
	}
	maxRows := len(rows)
	if height > 0 {
		maxRows = min(maxRows, max(height-chrome, 1))
	}
	if len(rows) > maxRows {
		start := max(m.cmdCursor-maxRows/2, 0)
		if start+maxRows > len(rows) {
			start = len(rows) - maxRows
		}
		rows = rows[start : start+maxRows]
	}

	style := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(dialogWidth + 2)
	var dialog string
	if compact {
		dialog = style.Padding(0, 2).Render(lipgloss.JoinVertical(lipgloss.Left,
			title,
			strings.Join(rows, "\n"),
		))
	} else {
		dialog = style.Padding(1, 2).Render(lipgloss.JoinVertical(lipgloss.Left,
			title,
			"",
			strings.Join(rows, "\n"),
			"",
			hint,
		))
	}

	// MaxHeight makes the overlay honour the budget viewContent reserved for
	// it even on a terminal too short for the compact dialog, instead of
	// pushing the input box off the bottom of the screen.
	return lipgloss.NewStyle().MaxHeight(height).Render(lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(theme.Current().Rule)),
	))
}

// cmdNameWidth is the width of the command-name column in the slash menu.
const cmdNameWidth = 16

// cmdMenuColumns fits one slash-menu row into innerW terminal cells: the
// name padded to cmdNameWidth (a longer name, such as a skill's, keeps up to
// half the row) and the description cut to the rest, two cells of gap
// between them. Widths are measured in cells, so wide characters cannot
// push a row past the dialog edge.
func cmdMenuColumns(name, desc string, innerW int) (string, string) {
	nameW := max(cmdNameWidth, min(ansi.StringWidth(name), innerW/2))
	name = ansi.Truncate(name, nameW, "…")
	name += strings.Repeat(" ", max(nameW-ansi.StringWidth(name), 0))
	desc = ansi.Truncate(desc, max(innerW-nameW-2, 0), "…")
	return name, desc
}

// Mention palette chrome: the full form has a border (2 rows), a title, a
// blank row on each side of the list and a key hint; the compact form, used
// when the terminal is too short for that, is the border around the list.
const (
	mentionPaletteFullChrome    = 6
	mentionPaletteCompactChrome = 2
)

// mentionPaletteMaxRows is how many rows the @file/$skill completion palette
// may take: what the terminal has left after the header, the separators, the
// status bar, the working indicator, the input box itself and one row of
// transcript. The todo/agent footer is not drawn while the palette is open
// (see showsParallelFooter), so it does not compete for these rows.
func (m Model) mentionPaletteMaxRows(width int) int {
	if m.height <= 0 {
		return math.MaxInt32 // no WindowSizeMsg yet: nothing to overflow
	}
	fixed := lipgloss.Height(m.viewHeader()) + 2 + 1 + m.workingIndicatorHeight() + 1 +
		lipgloss.Height(m.viewInputBox(width))
	return m.height - fixed
}

// viewMentionPalette renders the completions for an @file or $skill mention
// being typed, inside the rows mentionPaletteMaxRows allows. Each row is cut
// to one line of the box. When not every completion fits, the list is
// windowed around the cursor and the title says which one is selected; on a
// very short terminal the title and hint are dropped, and with no room at
// all the palette is not drawn (typing still completes).
func (m Model) viewMentionPalette(width int) string {
	if len(m.mentionItems) == 0 {
		return ""
	}
	maxRows := m.mentionPaletteMaxRows(width)
	compact := maxRows < mentionPaletteFullChrome+1
	chrome := mentionPaletteFullChrome
	if compact {
		chrome = mentionPaletteCompactChrome
	}
	shown := min(len(m.mentionItems), maxRows-chrome)
	if shown < 1 {
		return ""
	}
	cursor := clampOffset(m.mentionCursor, 0, len(m.mentionItems)-1)
	start := clampOffset(cursor-shown/2, 0, len(m.mentionItems)-shown)

	boxW := width - 4
	innerW := dialogInnerWidth(boxW)
	label := "available files"
	if m.mentionKind == mentionSkill {
		label = "skills"
	}
	if shown < len(m.mentionItems) {
		label += fmt.Sprintf(" %d/%d", cursor+1, len(m.mentionItems))
	}
	titleLabel := lipgloss.NewStyle().Foreground(theme.Current().TextMuted).Bold(true).Render(label)
	title := diagFillTitle(titleLabel, innerW)
	var cat skills.Catalog
	if m.mentionKind == mentionSkill {
		cat = m.skillCatalog()
	}
	var rows []string
	for i := start; i < start+shown; i++ {
		item := m.mentionItems[i]
		text := termtext.SingleLine(item)
		if m.mentionKind == mentionSkill {
			text = "$" + text
			if s, ok := cat.Find(item); ok {
				text += "  " + termtext.SingleLine(s.ListingDescription())
			}
		}
		// Two cells go to the cursor marker in front of the text.
		text = ansi.Truncate(text, max(innerW-2, 1), "…")
		if i == cursor {
			rows = append(rows, lipgloss.NewStyle().
				Background(theme.Current().BgSelection).
				Foreground(theme.Current().Text).
				Bold(true).
				Width(innerW).
				Render("› "+text))
		} else {
			rows = append(rows, lipgloss.NewStyle().Foreground(theme.Current().TextMuted).Render("  "+text))
		}
	}
	body := strings.Join(rows, "\n")
	if !compact {
		hint := styleMuted.Render(ansi.Truncate("↑↓ navigate  enter inserts mention", innerW, "…"))
		body = title + "\n\n" + body + "\n\n" + hint
	}
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(theme.Current().Border).
		Width(boxW + 2).
		PaddingLeft(2).PaddingRight(2).
		Render(body)
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

// wrapIndentedLines is wrapPlainLines for a block drawn indent cells in: the
// text is wrapped to the width left beside the indent and every row gets
// the indent, so wrapped rows line up under the first one. Wrapping the
// indented text as a whole (as the ask-user dialog once did) indents only
// the first row and sends the rest back to column 0.
func wrapIndentedLines(s, indent string, width int) []string {
	lines := wrapPlainLines(s, max(width-ansi.StringWidth(indent), 1))
	for i, line := range lines {
		lines[i] = indent + line
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
	// One marker either way: " …" after a line that has room for it, or the
	// cut's own "…" when the line has to shrink to make room.
	lines[last] = termtext.Fit(lines[last]+" …", max(width, 4))
	return lines
}

// inputTextareaView is the textarea with the workflow keyword lit up. Every
// place the input is drawn goes through here so the effect cannot appear in
// one input state and vanish in another.
func (m Model) inputTextareaView() string {
	return highlightUltracode(m.ta.View(), m.eyeFrame)
}

// boxContentWidth is the room inside the input box for a box width cells
// wide: the rounded border and one cell of padding on each side.
func boxContentWidth(width int) int {
	return max(width-4, 1)
}

// viewInput is the whole input area: the @mention palette, when one is open,
// stacked on the input box.
func (m Model) viewInput(width int) string {
	inputBox := m.viewInputBox(width)
	// cmd overlay is shown in content area; only @mention inline popup stays here
	mentionPalette := m.viewMentionPalette(width)
	if mentionPalette == "" {
		return inputBox
	}
	return lipgloss.JoinVertical(lipgloss.Left, mentionPalette, inputBox)
}

// viewInputBox is the bordered box at the bottom of the pane: the textarea,
// or in its place the plan/steer/approval picker or the question form.
func (m Model) viewInputBox(width int) string {
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
		lines = append(lines, styleMuted.Render("  "+truncateLabel(termtext.SingleLine(m.steerPending), 100)))
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
		// The dialog decides for itself whether the label row fits.
		lines = m.approvalDialogLines(label, boxContentWidth(width))
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

	// Every row is cut to the box's content width. The box would otherwise
	// wrap a long row (a steering message, a picker option on a narrow
	// terminal) onto rows that recalcLayout never reserved.
	contentW := boxContentWidth(width)
	var fitted []string
	for _, line := range lines {
		for row := range strings.SplitSeq(line, "\n") {
			fitted = append(fitted, termtext.Fit(row, contentW))
		}
	}
	return boxStyle.Render(strings.Join(fitted, "\n"))
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

// showsParallelFooter reports whether the block drawn by renderParallelAgents
// (workflow, swarm, delegations, todos) sits between the transcript and the
// input. The side panel carries the same information while it is open, and
// the @/$ completion palette takes the block's place while the user is
// picking a completion: it is short-lived, and on a small terminal it needs
// those rows more. Every place that draws or budgets for the block asks this.
func (m Model) showsParallelFooter() bool {
	return m.sidePanelWidth() <= 0 && len(m.mentionItems) == 0
}

// parallelFooterHeight is the number of rows the footer block occupies in
// the frame right now (0 when it is not drawn). Every layout budget that has
// to leave room for it asks this rather than rendering the block itself.
func (m Model) parallelFooterHeight() int {
	if !m.showsParallelFooter() {
		return 0
	}
	if pa, part := m.cachedParallelAgents(); pa != "" {
		return len(part.rows)
	}
	return 0
}

// dialogMinTranscriptRows is how much of the conversation stays visible
// above the input area when the terminal is short: three rows on a normal
// terminal, down to one on a very short one, where the input area (a dialog,
// a picker) needs every row it can get.
func dialogMinTranscriptRows(height int) int {
	return min(max(height/8, 1), 3)
}

// dialogMinInputRows is the smallest input area the open dialog can be drawn
// in with its essentials, or 0 when no size-adaptive dialog is open. For an
// approval: the box border, the summary row, the preview footer and the
// picker (or the "instead" field). For a question: the box border and agent
// label plus the rows renderQuestionForm cannot do without.
//
// The plan approval and steer pickers take precedence over both in
// viewInputBox, so while one of them is open this is 0 as well: the input
// box then holds that picker, which has a fixed height and is measured.
func (m Model) dialogMinInputRows() int {
	if m.showPlanApproval || m.showSteerChoice {
		return 0
	}
	switch {
	case m.pendingAuth != nil:
		return 2 + 1 + 1 + m.approvalLatchedControlRows()
	case m.pendingQuestion != nil:
		return 3 + questionMinBlockRows
	}
	return 0
}

// inputRowsForFooter is the height of the input area the footer has to
// leave room for. A size-adaptive dialog (approval, question) shrinks to
// fit whatever the footer leaves it, so only its minimum counts; everything
// else drawn in the input box (the textarea with its attachment chips, the
// plan approval or steer picker) has one height, which is measured.
//
// Measuring cannot recurse: viewInputBox only consults the footer's height
// for the size-adaptive dialogs, and those take the first branch.
func (m Model) inputRowsForFooter() int {
	if need := m.dialogMinInputRows(); need > 0 {
		return need
	}
	return lipgloss.Height(m.viewInputBox(m.paneWidth()))
}

// parallelFooterBudget is the row budget renderParallelAgents spends. It is
// footerBudget, capped so the footer never takes the rows the rest of the
// frame needs: the header, the separators, the status bar, the working
// indicator, the input area (inputRowsForFooter) and a minimum of transcript
// (dialogMinTranscriptRows). On a normal terminal the cap is above
// footerBudget and changes nothing; on a short one the footer shrinks, down
// to nothing, instead of pushing the frame past the bottom edge. That holds
// whatever the input area holds: the textarea during a run, a picker, or a
// dialog.
func (m Model) parallelFooterBudget() int {
	budget := footerBudget(m.height)
	if m.height <= 0 {
		return budget // no WindowSizeMsg yet: nothing to overflow
	}
	chrome := 1 + 2 + 1 + m.workingIndicatorHeight() // header, separators, status bar, indicator
	room := m.height - chrome - m.inputRowsForFooter() - dialogMinTranscriptRows(m.height)
	return min(budget, max(room, 0))
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
	remaining := m.parallelFooterBudget()
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
		// Delegation and todo rows use fixed label budgets sized for a
		// normal terminal; on a narrow one they are cut to the pane here so
		// a long task name cannot widen the frame past the terminal edge.
		for i, line := range lines {
			lines[i] = termtext.Fit(line, paneW)
		}
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
	compact := []string{header + styleMuted.Render(fmt.Sprintf("  %d running%s", len(active), m.panelKeyHint(" · ", "")))}
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
		lines = append(lines, styleMuted.Render(fmt.Sprintf("  … %d more%s", hidden, m.panelKeyHint(" · ", "all of them"))))
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
	// Rune-based and folded: a byte slice of the task could split a
	// multi-byte character, and a multi-line task would break the row.
	task := truncateLabel(termtext.SingleLine(a.Task), 50)
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
	compact := []string{header + " " + styleMuted.Render(truncateLabel(termtext.SingleLine(ordered[0].Content), 56))}
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
	// Rune-based and folded: a byte slice of the content could split a
	// multi-byte character, and a multi-line task would break the row.
	label := truncateLabel(termtext.SingleLine(td.Content), 56)
	switch status {
	case "in_progress", "running":
		return "  " + renderGlare(label, frame, theme.Current().Warning)
	case "blocked", "failed", "cancelled":
		return lipgloss.NewStyle().Foreground(theme.Current().Error).Render("  ! ") + styleMuted.Render(label)
	default:
		return lipgloss.NewStyle().Foreground(theme.Current().TextMuted).Render("  ○ ") + styleMuted.Render(label)
	}
}

// contextWindow is the active model's context window from the model
// metadata, 0 when the model is unknown. It runs on every status bar render
// (the context gauge), so it uses the manager's index (Lookup, no
// allocation) rather than copying the model list: 20 us and 80 KB per call
// with the embedded catalog before (BenchmarkContextWindow).
func (m Model) contextWindow() int {
	mod, _ := m.providers.Lookup(m.cfg.ActiveProvider, m.cfg.ActiveModel)
	return mod.Context
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

	// The bar is one row: one cell of left padding, the left text, the
	// right cluster and one trailing space. The right cluster is built from
	// independent indicators with no bound of its own, so on a narrow
	// terminal it alone can be wider than the bar; it is then cut from the
	// left, keeping the context gauge at its end, which matters most.
	right = termtext.FitLeft(right, max(width-2, 0))
	leftWidth := max(width-lipgloss.Width(right)-2, 0)
	// A banner can be anything, a provider's error message included; the
	// Width style would wrap it onto a second row the layout never reserved,
	// so it is cut to the one row the bar has, one cell short of the right
	// cluster so the two never run together.
	leftPadded := lipgloss.NewStyle().Width(leftWidth).Render(termtext.Fit(left, leftWidth-1))

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
	// Banners are one row; a multi-line error message is folded onto it.
	return style.Render(prefix + termtext.SingleLine(text))
}
