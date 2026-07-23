package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/memory"
	"spettro/internal/provider"
	"spettro/internal/session"
)

// memoryMineDoneMsg reports a finished background mining run.
type memoryMineDoneMsg struct {
	added   int
	scanned int
	err     error
}

// memoryMineSessionLimit caps how many recent sessions one /memory mine run
// scans; older sessions can be reached with an explicit count argument.
const memoryMineSessionLimit = 10

// runMemoryMine scans this project's saved session transcripts in the
// background and drafts candidate memories into the review inbox. It never
// touches the active memory files — candidates only become memory through
// explicit approval in /memory review. The run does not block the UI (no
// m.thinking): results arrive as a banner via memoryMineDoneMsg.
func (m Model) runMemoryMine(limit int) (tea.Model, tea.Cmd) {
	if limit <= 0 {
		limit = memoryMineSessionLimit
	}
	items, err := session.List(m.store.GlobalDir, m.cwd)
	if err != nil || len(items) == 0 {
		m.showBanner("no saved sessions to mine for this project", "info")
		return m, nil
	}
	if len(items) > limit {
		items = items[:limit]
	}
	globalDir := m.store.GlobalDir
	cwd := m.cwd
	pm := m.providers
	providerName := m.cfg.ActiveProvider
	modelName := m.cfg.ActiveModel
	m.showBanner(fmt.Sprintf("mining %d saved session(s) for memories in the background…", len(items)), "info")
	return m, func() tea.Msg {
		var transcripts []memory.Transcript
		for _, it := range items {
			state, err := session.Load(globalDir, it.ID)
			if err != nil || len(state.Messages) < 2 {
				continue
			}
			var sb strings.Builder
			for _, msg := range state.Messages {
				if msg.Role == string(RoleSystem) {
					continue
				}
				sb.WriteString(msg.Role)
				sb.WriteString(": ")
				sb.WriteString(msg.Content)
				sb.WriteString("\n")
			}
			transcripts = append(transcripts, memory.Transcript{SessionID: it.ID, Text: sb.String()})
		}
		if len(transcripts) == 0 {
			return memoryMineDoneMsg{}
		}
		store := memory.DefaultStore(cwd)
		existing := store.Load()
		cands, err := memory.Mine(context.Background(), transcripts, existing,
			func(ctx context.Context, prompt string) (string, error) {
				resp, err := pm.Send(ctx, providerName, modelName, provider.Request{Prompt: prompt})
				if err != nil {
					return "", err
				}
				return resp.Content, nil
			})
		if err != nil {
			return memoryMineDoneMsg{scanned: len(transcripts), err: err}
		}
		for i := range cands {
			cands[i].ProjectPath = cwd
		}
		added, err := memory.DefaultInbox().Add(cands, existing)
		return memoryMineDoneMsg{added: added, scanned: len(transcripts), err: err}
	}
}

// openMemoryReview opens the inbox review modal.
func (m Model) openMemoryReview() (tea.Model, tea.Cmd) {
	cands, err := memory.DefaultInbox().Load()
	if err != nil {
		m.showBanner("memory inbox load failed: "+err.Error(), "error")
		return m, nil
	}
	if len(cands) == 0 {
		m.showBanner("memory inbox is empty — run /memory mine to draft candidates", "info")
		return m, nil
	}
	m.showMemoryReview = true
	m.memoryReviewItems = cands
	m.memoryReviewCursor = 0
	return m, nil
}

func (m Model) updateMemoryReview(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.showMemoryReview = false
	case "up", "shift+tab":
		if m.memoryReviewCursor > 0 {
			m.memoryReviewCursor--
		}
	case "down", "tab", "ctrl+n":
		if m.memoryReviewCursor < len(m.memoryReviewItems)-1 {
			m.memoryReviewCursor++
		}
	case "a", "enter":
		m = m.resolveMemoryCandidate(true)
	case "d", "x":
		m = m.resolveMemoryCandidate(false)
	}
	return m, nil
}

// resolveMemoryCandidate approves (save to memory + remove from inbox) or
// discards (remove only) the candidate under the cursor.
func (m Model) resolveMemoryCandidate(approve bool) Model {
	if len(m.memoryReviewItems) == 0 {
		m.showMemoryReview = false
		return m
	}
	cand := m.memoryReviewItems[m.memoryReviewCursor]
	if approve {
		cwd := cand.ProjectPath
		if cwd == "" {
			cwd = m.cwd
		}
		store := memory.DefaultStore(cwd)
		var err error
		if cand.Supersedes != "" {
			_, err = store.Supersede(cand.Scope, cand.Supersedes, cand.Fact)
		} else {
			_, err = store.SaveApproved(cand.Scope, cand.Fact)
		}
		if err != nil {
			m.showBanner("memory save failed: "+err.Error(), "error")
			return m
		}
	}
	if _, _, err := memory.DefaultInbox().Remove(cand.ID); err != nil {
		m.showBanner("memory inbox update failed: "+err.Error(), "error")
		return m
	}
	m.memoryReviewItems = append(m.memoryReviewItems[:m.memoryReviewCursor], m.memoryReviewItems[m.memoryReviewCursor+1:]...)
	if m.memoryReviewCursor >= len(m.memoryReviewItems) && m.memoryReviewCursor > 0 {
		m.memoryReviewCursor--
	}
	if approve {
		m.showBanner("memory approved — active from the next session", "success")
	} else {
		m.showBanner("candidate discarded", "info")
	}
	if len(m.memoryReviewItems) == 0 {
		m.showMemoryReview = false
	}
	return m
}

func (m Model) viewMemoryReview() string {
	mc := m.currentColor()
	title := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ memory review inbox")
	dialogWidth := 76
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 30 {
		dialogWidth = 30
	}

	// contentW is the usable line width inside the dialog: the box renders at
	// Width(dialogWidth+2) with a border and Padding(1,2), leaving
	// dialogWidth-4 columns for content; keep two extra columns of breathing
	// room so no row ever touches the border.
	contentW := dialogWidth - 6

	// Window the list around the cursor so a large inbox fits the dialog.
	maxRows := max(4, m.height-14)
	start := 0
	if m.memoryReviewCursor >= maxRows {
		start = m.memoryReviewCursor - maxRows + 1
	}
	end := min(len(m.memoryReviewItems), start+maxRows)

	var rows []string
	for i := start; i < end; i++ {
		c := m.memoryReviewItems[i]
		isSelected := i == m.memoryReviewCursor
		scope := fmt.Sprintf("[%s]", c.Scope)
		if isSelected {
			// Selected candidate: header line, then the FULL fact word-wrapped
			// so the user can read exactly what they are approving.
			prefix := lipgloss.NewStyle().Foreground(mc).Bold(true).Render("› ")
			header := prefix + lipgloss.NewStyle().Foreground(colorText).Bold(true).Render(scope)
			rows = append(rows, header)
			fact := lipgloss.NewStyle().
				Foreground(colorText).
				Width(max(8, contentW-4)).
				Render(c.Fact)
			for line := range strings.SplitSeq(fact, "\n") {
				rows = append(rows, "    "+line)
			}
			if c.Supersedes != "" {
				rows = append(rows, styleMuted.Render("    replaces: "+truncateLabel(c.Supersedes, max(8, contentW-14))))
			}
			if len(c.Sources) > 0 {
				rows = append(rows, styleMuted.Render("    from "+truncateLabel(strings.Join(c.Sources, ", "), max(8, contentW-9))))
			}
			continue
		}
		prefix := "  "
		scopeStyled := lipgloss.NewStyle().Foreground(colorMuted).Render(scope)
		budget := max(8, contentW-len(prefix)-len(scope)-1)
		rows = append(rows, prefix+scopeStyled+" "+lipgloss.NewStyle().Foreground(colorDim).Render(truncateLabel(c.Fact, budget)))
	}

	hint := styleMuted.Render("↑↓ navigate  a/enter approve  d discard  esc close")
	note := styleMuted.Render("approved memories load into context from the next session")
	dialog := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(dialogWidth+2).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			strings.Join(rows, "\n"),
			"",
			note,
			hint,
		))

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
	)
}

// curateItem is one proposed curation op bound to its scope, pending review.
type curateItem struct {
	Scope memory.Scope
	Op    memory.CurateOp
}

// memoryCurateDoneMsg reports a finished background curation LLM pass.
type memoryCurateDoneMsg struct {
	items []curateItem
	err   error
}

// runMemoryCurate runs one LLM pass per scope over the memory store and
// drafts edit operations (merge/rewrite/delete) for review. Explicit like
// /memory mine — no silent token spend, and nothing is applied without
// per-op approval in the review modal.
func (m Model) runMemoryCurate(scopeArg string) (tea.Model, tea.Cmd) {
	scopes := []memory.Scope{memory.ScopeUser, memory.ScopeProject}
	switch scopeArg {
	case "", "all":
	case "user":
		scopes = []memory.Scope{memory.ScopeUser}
	case "project":
		scopes = []memory.Scope{memory.ScopeProject}
	default:
		m.showBanner("usage: /memory curate [user|project|all]", "error")
		return m, nil
	}
	cwd := m.cwd
	pm := m.providers
	providerName := m.cfg.ActiveProvider
	modelName := m.cfg.ActiveModel
	m.showBanner("curating memory in the background…", "info")
	return m, func() tea.Msg {
		store := memory.DefaultStore(cwd)
		complete := func(ctx context.Context, prompt string) (string, error) {
			resp, err := pm.Send(ctx, providerName, modelName, provider.Request{Prompt: prompt})
			if err != nil {
				return "", err
			}
			return resp.Content, nil
		}
		var items []curateItem
		for _, sc := range scopes {
			facts := store.Facts(sc)
			if len(facts) == 0 {
				continue
			}
			var hints []string
			if sc == memory.ScopeProject {
				hints = memory.StaleHints(facts, cwd)
			}
			ops, err := memory.Curate(context.Background(), facts, hints, complete)
			if err != nil {
				return memoryCurateDoneMsg{err: err}
			}
			for _, op := range ops {
				items = append(items, curateItem{Scope: sc, Op: op})
			}
		}
		return memoryCurateDoneMsg{items: items}
	}
}

func (m Model) updateMemoryCurate(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.showMemoryCurate = false
	case "up", "shift+tab":
		if m.memoryCurateCursor > 0 {
			m.memoryCurateCursor--
		}
	case "down", "tab", "ctrl+n":
		if m.memoryCurateCursor < len(m.memoryCurateItems)-1 {
			m.memoryCurateCursor++
		}
	case "a", "enter":
		m = m.resolveCurateOp(true)
	case "d", "x":
		m = m.resolveCurateOp(false)
	}
	return m, nil
}

// resolveCurateOp applies (atomic rewrite) or skips the op under the cursor.
func (m Model) resolveCurateOp(apply bool) Model {
	if len(m.memoryCurateItems) == 0 {
		m.showMemoryCurate = false
		return m
	}
	it := m.memoryCurateItems[m.memoryCurateCursor]
	if apply {
		if err := memory.DefaultStore(m.cwd).ApplyOp(it.Scope, it.Op); err != nil {
			m.showBanner("memory curate failed: "+err.Error(), "error")
			return m
		}
		m.showBanner("memory updated — applies from the next session", "success")
	} else {
		m.showBanner("op skipped — memory unchanged", "info")
	}
	m.memoryCurateItems = append(m.memoryCurateItems[:m.memoryCurateCursor], m.memoryCurateItems[m.memoryCurateCursor+1:]...)
	if m.memoryCurateCursor >= len(m.memoryCurateItems) && m.memoryCurateCursor > 0 {
		m.memoryCurateCursor--
	}
	if len(m.memoryCurateItems) == 0 {
		m.showMemoryCurate = false
	}
	return m
}

func (m Model) viewMemoryCurate() string {
	mc := m.currentColor()
	title := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ memory curation")
	dialogWidth := 76
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 30 {
		dialogWidth = 30
	}
	contentW := dialogWidth - 6

	maxRows := max(4, m.height-14)
	start := 0
	if m.memoryCurateCursor >= maxRows {
		start = m.memoryCurateCursor - maxRows + 1
	}
	end := min(len(m.memoryCurateItems), start+maxRows)

	var rows []string
	for i := start; i < end; i++ {
		it := m.memoryCurateItems[i]
		label := fmt.Sprintf("[%s] %s %s", it.Scope, it.Op.Action, strings.Join(it.Op.IDs, ","))
		if i == m.memoryCurateCursor {
			prefix := lipgloss.NewStyle().Foreground(mc).Bold(true).Render("› ")
			rows = append(rows, prefix+lipgloss.NewStyle().Foreground(colorText).Bold(true).Render(label))
			if it.Op.Text != "" {
				text := lipgloss.NewStyle().Foreground(colorText).Width(max(8, contentW-4)).Render("→ " + it.Op.Text)
				for line := range strings.SplitSeq(text, "\n") {
					rows = append(rows, "    "+line)
				}
			}
			if it.Op.Reason != "" {
				rows = append(rows, styleMuted.Render("    why: "+truncateLabel(it.Op.Reason, max(8, contentW-9))))
			}
			continue
		}
		rows = append(rows, "  "+lipgloss.NewStyle().Foreground(colorDim).Render(truncateLabel(label, max(8, contentW-2))))
	}

	hint := styleMuted.Render("↑↓ navigate  a/enter apply  d skip  esc close")
	note := styleMuted.Render("each applied op rewrites the file atomically; skipped ops change nothing")
	dialog := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		Width(dialogWidth+2).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left,
			title, "",
			strings.Join(rows, "\n"),
			"",
			note,
			hint,
		))

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(colorDim)),
	)
}
