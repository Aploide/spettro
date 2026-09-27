package tui

import (
	"fmt"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/termtext"
	"spettro/internal/theme"
)

var (
	reCodeSpan = regexp.MustCompile("`([^`]+)`")
	reBold     = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalic1  = regexp.MustCompile(`\*([^*]+)\*`)
	reItalic2  = regexp.MustCompile(`_([^_]+)_`)
	reLink     = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)

func renderMarkdown(content string, width int) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}

	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	// Everything rendered here is text the TUI did not write: a model's
	// reply, or tool output in the side panel. An escape sequence or a
	// carriage return in it would reach the terminal raw and move the
	// cursor (a "\r" sends the rest of the row to column 0, over whatever
	// is drawn there), so every line is made plain before it is styled.
	for i, line := range lines {
		lines[i] = termtext.SanitizeLine(line)
	}

	out := make([]string, 0, len(lines))
	inCode := false
	var codeLines []string
	inTable := false
	var tableLines []string

	flushTable := func() {
		if len(tableLines) > 0 {
			out = append(out, renderTable(tableLines, width))
			tableLines = nil
		}
		inTable = false
	}

	for _, line := range lines {
		trim := strings.TrimSpace(line)

		if strings.HasPrefix(trim, "```") {
			if inTable {
				flushTable()
			}
			if inCode {
				out = append(out, renderCodeBlock(strings.Join(codeLines, "\n"), width))
				codeLines = nil
				inCode = false
			} else {
				inCode = true
				codeLines = nil
			}
			continue
		}

		if inCode {
			codeLines = append(codeLines, line)
			continue
		}

		if isTableRow(line) {
			inTable = true
			tableLines = append(tableLines, line)
			continue
		}

		if inTable {
			flushTable()
		}

		if trim == "" {
			out = append(out, "")
			continue
		}

		if level, title, ok := parseHeading(trim); ok {
			titleStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Current().Text)
			if level == 1 {
				titleStyle = titleStyle.Underline(true)
			}
			out = append(out, titleStyle.Render(renderInlineMarkdown(title)))
			continue
		}

		if bullet, item, ok := parseListItem(line); ok {
			out = append(out, styleText.Render("  "+bullet+" "+renderInlineMarkdown(item)))
			continue
		}

		if quote, ok := parseQuote(line); ok {
			q := lipgloss.NewStyle().Foreground(theme.Current().TextMuted).Italic(true).Render(renderInlineMarkdown(quote))
			out = append(out, styleMuted.Render("│ ")+q)
			continue
		}

		if isHorizontalRule(trim) {
			ruleW := width
			if ruleW < 8 {
				ruleW = 24
			}
			out = append(out, styleRule.Render(strings.Repeat("─", ruleW-2)))
			continue
		}

		out = append(out, styleText.Render(renderInlineMarkdown(trim)))
	}

	if inTable {
		flushTable()
	}

	if inCode {
		out = append(out, renderCodeBlock(strings.Join(codeLines, "\n"), width))
	}

	return strings.Join(out, "\n")
}

func renderInlineMarkdown(s string) string {
	if s == "" {
		return s
	}

	codePieces := map[string]string{}
	codeN := 0
	s = reCodeSpan.ReplaceAllStringFunc(s, func(m string) string {
		tok := fmt.Sprintf("\x00CODE%d\x00", codeN)
		codeN++
		parts := reCodeSpan.FindStringSubmatch(m)
		if len(parts) < 2 {
			codePieces[tok] = m
			return tok
		}
		codePieces[tok] = lipgloss.NewStyle().
			Foreground(theme.Current().CodeInlineFg).
			Background(theme.Current().BgCodeInline).
			Render(" " + parts[1] + " ")
		return tok
	})

	s = reBold.ReplaceAllStringFunc(s, func(m string) string {
		parts := reBold.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return styleBold.Render(parts[1])
	})

	s = reItalic1.ReplaceAllStringFunc(s, func(m string) string {
		parts := reItalic1.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return lipgloss.NewStyle().Italic(true).Render(parts[1])
	})

	s = reItalic2.ReplaceAllStringFunc(s, func(m string) string {
		parts := reItalic2.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return lipgloss.NewStyle().Italic(true).Render(parts[1])
	})

	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		parts := reLink.FindStringSubmatch(m)
		if len(parts) < 3 {
			return m
		}
		return styleText.Render(parts[1]) + " " + styleMuted.Render("("+parts[2]+")")
	})

	for tok, rendered := range codePieces {
		s = strings.ReplaceAll(s, tok, rendered)
	}

	return s
}

// renderCodeBlock draws a fenced code block on its tinted background, one row
// per source line. Code is not wrapped (a wrapped line reads as two
// statements); a line wider than the block is cut with "…" instead, which,
// unlike a plain MaxWidth clip, shows that something was cut. Tabs and
// control characters are sanitized first so the measured width is the drawn
// one.
func renderCodeBlock(code string, width int) string {
	if strings.TrimSpace(code) == "" {
		return ""
	}
	style := lipgloss.NewStyle().
		Foreground(theme.Current().CodeFg).
		Background(theme.Current().BgCode).
		Padding(0, 1)
	lines := strings.Split(code, "\n")
	for i, line := range lines {
		line = termtext.SanitizeLine(line)
		if width > 12 {
			line = termtext.Fit(line, width-2) // the padding takes a cell each side
		}
		lines[i] = line
	}
	return style.Render(strings.Join(lines, "\n"))
}

func parseHeading(line string) (int, string, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	if len(line) <= level || line[level] != ' ' {
		return 0, "", false
	}
	return level, strings.TrimSpace(line[level+1:]), true
}

func parseListItem(line string) (string, string, bool) {
	trim := strings.TrimLeft(line, " \t")
	if trim == "" {
		return "", "", false
	}

	if len(trim) >= 2 {
		switch trim[0] {
		case '-', '*', '+':
			if trim[1] == ' ' {
				return "•", strings.TrimSpace(trim[2:]), true
			}
		}
	}

	i := 0
	for i < len(trim) && trim[i] >= '0' && trim[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(trim) && trim[i] == '.' && trim[i+1] == ' ' {
		return trim[:i] + ".", strings.TrimSpace(trim[i+2:]), true
	}

	return "", "", false
}

func parseQuote(line string) (string, bool) {
	trim := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trim, ">") {
		return "", false
	}
	q := strings.TrimSpace(strings.TrimPrefix(trim, ">"))
	return q, true
}

func isHorizontalRule(line string) bool {
	if len(line) < 3 {
		return false
	}
	clean := strings.ReplaceAll(strings.ReplaceAll(line, " ", ""), "\t", "")
	if len(clean) < 3 {
		return false
	}
	for _, ch := range clean {
		if ch != '-' && ch != '*' && ch != '_' {
			return false
		}
	}
	return true
}

func isTableRow(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "|")
}

func isTableSeparator(line string) bool {
	trim := strings.TrimSpace(line)
	if !strings.HasPrefix(trim, "|") {
		return false
	}
	inner := strings.TrimPrefix(strings.TrimSuffix(trim, "|"), "|")
	for cell := range strings.SplitSeq(inner, "|") {
		clean := strings.TrimSpace(cell)
		if clean == "" {
			continue
		}
		for _, ch := range clean {
			if ch != '-' && ch != ':' {
				return false
			}
		}
	}
	return true
}

func parseTableCells(line string) []string {
	trim := strings.TrimSpace(line)
	trim = strings.TrimPrefix(trim, "|")
	trim = strings.TrimSuffix(trim, "|")
	parts := strings.Split(trim, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

func renderTable(tableLines []string, width int) string {
	type tableRow struct {
		cells    []string
		isHeader bool
	}

	var rows []tableRow
	for _, line := range tableLines {
		if isTableSeparator(line) {
			if len(rows) > 0 {
				rows[len(rows)-1].isHeader = true
			}
			continue
		}
		rows = append(rows, tableRow{cells: parseTableCells(line)})
	}

	if len(rows) == 0 {
		return ""
	}

	ncols := 0
	for _, r := range rows {
		if len(r.cells) > ncols {
			ncols = len(r.cells)
		}
	}
	if ncols == 0 {
		return ""
	}

	// Cells are measured as drawn: after inline markdown (the "**" of bold
	// takes no cells) and in display cells, not bytes (a CJK character
	// takes two).
	for i := range rows {
		for j, cell := range rows[i].cells {
			rows[i].cells[j] = renderInlineMarkdown(termtext.SanitizeLine(cell))
		}
	}
	colWidths := make([]int, ncols)
	for _, r := range rows {
		for j := 0; j < ncols; j++ {
			if j < len(r.cells) {
				colWidths[j] = max(colWidths[j], ansi.StringWidth(r.cells[j]))
			}
		}
	}
	fitTableColumns(colWidths, width)

	border := styleMuted
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(theme.Current().Text)

	sepLine := func(l, m, r, f string) string {
		var b strings.Builder
		b.WriteString(border.Render(l))
		for j, w := range colWidths {
			b.WriteString(border.Render(strings.Repeat(f, w+2)))
			if j < ncols-1 {
				b.WriteString(border.Render(m))
			}
		}
		b.WriteString(border.Render(r))
		return b.String()
	}

	buildRow := func(r tableRow) string {
		var b strings.Builder
		b.WriteString(border.Render("│"))
		for j := 0; j < ncols; j++ {
			cell := ""
			if j < len(r.cells) {
				cell = r.cells[j]
			}
			rendered := termtext.Fit(cell, colWidths[j])
			padding := strings.Repeat(" ", max(colWidths[j]-ansi.StringWidth(rendered), 0))
			if r.isHeader {
				rendered = headerStyle.Render(rendered)
			} else {
				rendered = styleText.Render(rendered)
			}
			b.WriteString(" ")
			b.WriteString(rendered)
			b.WriteString(padding)
			b.WriteString(" ")
			b.WriteString(border.Render("│"))
		}
		return b.String()
	}

	var out []string
	out = append(out, sepLine("┌", "┬", "┐", "─"))
	for _, r := range rows {
		out = append(out, buildRow(r))
		if r.isHeader {
			out = append(out, sepLine("├", "┼", "┤", "─"))
		}
	}
	out = append(out, sepLine("└", "┴", "┘", "─"))
	// Only a table with more columns than the width can hold even at the
	// narrowest column width is still too wide here; cut it rather than let
	// the text block wrap its borders onto the next row.
	if width > 0 {
		for i, line := range out {
			out[i] = termtext.Fit(line, width)
		}
	}
	return strings.Join(out, "\n")
}

// minTableColumnWidth is the narrowest a table column is squeezed to: room
// for two characters and the "…" that marks the cut.
const minTableColumnWidth = 3

// fitTableColumns shrinks column widths, in place, until a table drawn with
// them fits width cells. A row costs one border cell plus, per column, its
// width, a cell of padding on each side and a border: 1 + sum(w + 3). The
// widest column gives up a cell at a time, so narrow columns (ids, flags,
// counts) stay whole while a long description column absorbs the cut.
// width <= 0 means unlimited.
func fitTableColumns(widths []int, width int) {
	if width <= 0 {
		return
	}
	total := 1
	for _, w := range widths {
		total += w + 3
	}
	for total > width {
		widest := 0
		for j := range widths {
			if widths[j] > widths[widest] {
				widest = j
			}
		}
		if widths[widest] <= minTableColumnWidth {
			return // as narrow as it gets; renderTable cuts the rows
		}
		widths[widest]--
		total--
	}
}

func prefixBlockWithBullet(bullet, block string) string {
	if strings.TrimSpace(block) == "" {
		return ""
	}
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		if i == 0 {
			lines[i] = bullet + " " + line
			continue
		}
		if strings.TrimSpace(line) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = "    " + line
	}
	return strings.Join(lines, "\n")
}
