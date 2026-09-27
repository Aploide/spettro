package tui

import (
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"spettro/internal/termtext"
	"spettro/internal/theme"
)

// The theme picker previews inside a panel rather than by installing the
// palette as the cursor moves.
//
// Installing it looks like the more honest preview, and it was the first
// attempt, but overlays here replace the whole screen instead of floating over
// it: there is no transcript visible behind the dialog for a live palette to
// repaint, so the only thing it changes is the dialog — and it changes it
// wrongly. The terminal's own background does not follow the palette, so
// previewing dark on a light terminal paints dark-theme greys onto a white
// screen and the unselected rows fade to nothing. The user is then choosing a
// theme through a dialog the preview has made unreadable.
//
// So the chrome stays in the palette actually in force, which is by definition
// legible on this terminal, and the candidate theme is confined to a panel
// that paints its own background. Nothing global moves until enter, which also
// means escape has nothing to undo.

// themePickerOrder is the order the options are offered in. Auto leads: it is
// the default and the right answer for most terminals, and putting it first
// means the common case is one keypress away.
var themePickerOrder = []theme.Kind{theme.AutoKind, theme.DarkKind, theme.LightKind}

var themePickerDesc = map[theme.Kind]string{
	theme.AutoKind:  "follow the terminal background",
	theme.DarkKind:  "palette tuned for a dark terminal",
	theme.LightKind: "palette tuned for a light terminal",
}

func (m Model) openThemePicker() Model {
	selected, _ := m.themeSelection()
	m.themeCursor = 0
	for i, k := range themePickerOrder {
		if k == selected {
			m.themeCursor = i
			break
		}
	}
	m.showThemePicker = true
	return m
}

// previewKind is the palette the panel shows: the option under the cursor,
// with "auto" resolved the same way choosing it would resolve.
func (m Model) previewKind() theme.Kind {
	return theme.Seed(themePickerOrder[m.themeCursor])
}

func (m Model) updateThemePicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.showThemePicker = false
		return m, nil
	case "up", "shift+tab", "k":
		if m.themeCursor > 0 {
			m.themeCursor--
		}
		return m, nil
	case "down", "ctrl+n", "tab", "j":
		if m.themeCursor < len(themePickerOrder)-1 {
			m.themeCursor++
		}
		return m, nil
	case "enter":
		m.showThemePicker = false
		return m.applyThemeSelection(themePickerOrder[m.themeCursor])
	}

	// Digits select directly, matching the trust and question dialogs.
	if n := msg.String(); len(n) == 1 && n[0] >= '1' && n[0] <= '9' {
		if i := int(n[0] - '1'); i < len(themePickerOrder) {
			m.themeCursor = i
			m.showThemePicker = false
			return m.applyThemeSelection(themePickerOrder[i])
		}
	}
	return m, nil
}

func (m Model) viewThemePicker() string {
	mc := m.currentColor()
	pal := theme.Current()

	dialogWidth := 66
	if m.width < dialogWidth+4 {
		dialogWidth = m.width - 4
	}
	if dialogWidth < 34 {
		dialogWidth = 34
	}
	innerW := dialogInnerWidth(dialogWidth)

	titleLabel := lipgloss.NewStyle().Bold(true).Foreground(mc).Render("◈ select theme")
	rows := []string{diagFillTitle(titleLabel, innerW), ""}

	for i, k := range themePickerOrder {
		label := k.String()
		desc := themePickerDesc[k]
		// Auto is the only option whose name does not say what you will get,
		// so it carries what choosing it would resolve to right now.
		if k == theme.AutoKind {
			desc += " (now: " + theme.Seed(theme.AutoKind).String() + ")"
		}

		// Rows are cut, not wrapped, on a narrow terminal: a wrapped option
		// reads as two options.
		num := string(rune('1' + i))
		if i == m.themeCursor {
			rows = append(rows, lipgloss.NewStyle().
				Background(pal.BgSelection).
				Foreground(pal.Text).
				Bold(true).
				Width(innerW).
				Render(termtext.Fit("› "+num+"  "+label+"   "+desc, innerW)))
		} else {
			rows = append(rows, termtext.Fit("  "+num+"  "+
				lipgloss.NewStyle().Foreground(pal.Text).Render(label)+
				"   "+lipgloss.NewStyle().Foreground(pal.TextMuted).Render(desc), innerW))
		}
	}

	// The painted preview is the first thing to go on a short terminal: the
	// options and the keys are what the dialog cannot do without, and the
	// chosen theme is visible behind the dialog once applied anyway.
	preview := m.previewKind()
	previewRows := themePreview(theme.For(preview), innerW)
	const chrome = 2 + 2 + 2 // border, vertical padding, the hint and the blank row above it
	if len(rows)+2+len(previewRows)+chrome <= m.height || m.height <= 0 {
		rows = append(rows, "",
			lipgloss.NewStyle().Foreground(pal.TextMuted).Render("  preview — "+preview.String()))
		rows = append(rows, previewRows...)
	}
	rows = append(rows, "",
		lipgloss.NewStyle().Foreground(pal.TextMuted).Render(termtext.Fit("↑↓ preview  enter apply  esc cancel", innerW)))

	dialog := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(mc).
		// Two columns wider than the rows it holds. The painted preview rows
		// are exactly innerW wide, and sizing the box to precisely that makes
		// lipgloss trim their trailing spaces — which strips the background
		// off the padding and leaves the bars ending mid-row.
		Width(dialogWidth+2).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, rows...))

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		dialog,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(pal.Rule)),
	)
}

// themePreview mocks the parts of the UI a palette is actually judged by: the
// header band, ordinary and muted prose, a selected row, a code block and a
// diff.
//
// Every row is painted to the full panel width on the candidate theme's own
// base background, including rows that carry no background of their own.
// Without that the panel would show a dark theme's inks sitting on whatever
// colour the terminal happens to be, which is the one thing the preview must
// not do — it is answering "what will this look like", and a dark theme on a
// white terminal is not the answer.
func themePreview(pal theme.Palette, innerW int) []string {
	// fill pads a composed row out to innerW with the theme's page colour, so
	// rows built from several differently-coloured spans still end flush.
	fill := func(s string) string {
		if w := lipgloss.Width(s); w < innerW {
			return s + lipgloss.NewStyle().Background(pal.BgBase).
				Render(strings.Repeat(" ", innerW-w))
		}
		return s
	}
	span := func(f, b color.Color, s string) string {
		return lipgloss.NewStyle().Foreground(f).Background(b).Render(s)
	}
	// row paints one uniform band across the whole width.
	row := func(f, b color.Color, s string) string {
		return lipgloss.NewStyle().Foreground(f).Background(b).Width(innerW).Render(s)
	}

	prose := span(pal.Text, pal.BgBase, "  ordinary transcript text") +
		span(pal.TextMuted, pal.BgBase, "   muted note")

	status := span(pal.Success, pal.BgBase, "  ✓ ok") +
		span(pal.Error, pal.BgBase, "  ✗ error") +
		span(pal.Warning, pal.BgBase, "  ⚠ warn") +
		span(pal.Info, pal.BgBase, "  ● info")

	return []string{
		row(pal.Text, pal.BgHeader, " ◆ spettro  coding                        thinking:high "),
		fill(""),
		fill(prose),
		row(pal.Text, pal.BgSelection, "  › a selected row"),
		row(pal.CodeFg, pal.BgCode, `  fmt.Println("code block")`),
		row(pal.DiffAddHiFg, pal.DiffAddHiBg, "  + added line"),
		row(pal.DiffDelHiFg, pal.DiffDelHiBg, "  - removed line"),
		fill(status),
	}
}
