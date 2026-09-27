package tui

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/termtext"
	"spettro/internal/theme"
)

var (
	reCodeSpan = regexp.MustCompile("`([^`]+)`")
	// Emphasis must not start or end with a space, as in CommonMark, so
	// arithmetic like "2 * 3 * 4" stays literal.
	reBold    = regexp.MustCompile(`\*\*([^*\s]|[^*\s][^*]*[^*\s])\*\*`)
	reItalic1 = regexp.MustCompile(`\*([^*\s]|[^*\s][^*]*[^*\s])\*`)
	reLink    = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	// reInlinePlaceholder matches an inlineSpans placeholder.
	reInlinePlaceholder = regexp.MustCompile("\x00([0-9]+)\x00")
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
			lead := "  " + listNesting(line, width)
			first := lead + bullet + " "
			rest := strings.Repeat(" ", ansi.StringWidth(first))
			out = append(out, hangIndent(renderInlineMarkdown(item), styleText.Render(first), rest, width, styleText))
			continue
		}

		if quote, ok := parseQuote(line); ok {
			bar := styleMuted.Render("│ ")
			quoteStyle := lipgloss.NewStyle().Foreground(theme.Current().TextMuted).Italic(true)
			out = append(out, hangIndent(renderInlineMarkdown(quote), bar, bar, width, quoteStyle))
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

// hangIndent lays out a block item, a list item or a quote, whose first row
// starts with first and whose wrapped rows start with rest. The body is
// wrapped here, to the width left after the prefix, so a wrapped row lines
// up under the item's text (or keeps the quote bar) instead of being wrapped
// later by the transcript to column 0. Each row is styled on its own, so a
// row the viewport shows without the ones above it keeps its colour. When
// width is unknown (<= 0) or too narrow to be worth indenting, the body is
// left on one row for the caller's wrap.
func hangIndent(body, first, rest string, width int, style lipgloss.Style) string {
	textW := width - ansi.StringWidth(first)
	if width <= 0 || textW < minHangingTextWidth {
		return first + style.Render(body)
	}
	rows := termtext.Wrap(body, textW)
	var b strings.Builder
	for i, row := range rows {
		if i > 0 {
			b.WriteString("\n")
			b.WriteString(rest)
		} else {
			b.WriteString(first)
		}
		b.WriteString(style.Render(row))
	}
	return b.String()
}

// minHangingTextWidth is the narrowest text column hangIndent indents for;
// below it the indent would leave only a sliver per row.
const minHangingTextWidth = 8

// listNesting returns the indent of a nested list item: its leading
// whitespace (a tab counts four cells), capped at a third of width (when
// width is known) so a deeply indented item still leaves room for its text.
func listNesting(line string, width int) string {
	n := 0
	for _, r := range line {
		if r == ' ' {
			n++
		} else if r == '\t' {
			n += 4
		} else {
			break
		}
	}
	if width > 0 {
		n = min(n, width/3)
	}
	return strings.Repeat(" ", n)
}

// renderInlineMarkdown styles the inline spans of one line: `code`,
// [text](url), **bold**, *italic* and _italic_.
//
// Every styled span is parked behind a placeholder (see inlineSpans) as soon
// as it is rendered, and the placeholders are expanded only at the end. That
// keeps two things safe: markup inside a code span or a URL stays literal,
// and the escape sequences of an already styled span ("\x1b[1m" for bold)
// are never seen by a later pattern. Without it the link pattern matched from
// the "[" of the bold escape to the link's "]", and a line like
// "**bold** and a [link](url)" showed "1mbold ... [link" with the escape
// half eaten.
func renderInlineMarkdown(s string) string {
	if s == "" {
		return s
	}
	var spans inlineSpans
	s = reCodeSpan.ReplaceAllStringFunc(s, func(m string) string {
		code := m[1 : len(m)-1] // the pattern guarantees the two backticks
		return spans.hold(lipgloss.NewStyle().
			Foreground(theme.Current().CodeInlineFg).
			Background(theme.Current().BgCodeInline).
			Render(" " + code + " "))
	})
	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		parts := reLink.FindStringSubmatch(m)
		text := renderEmphasis(parts[1], &spans)
		return spans.hold(styleText.Render(text) + " " + styleMuted.Render("("+parts[2]+")"))
	})
	return spans.expand(renderEmphasis(s, &spans))
}

// renderEmphasis styles **bold**, *italic* and _italic_ in s, parking each
// styled span in spans.
func renderEmphasis(s string, spans *inlineSpans) string {
	s = reBold.ReplaceAllStringFunc(s, func(m string) string {
		return spans.hold(styleBold.Render(m[2 : len(m)-2]))
	})
	italic := lipgloss.NewStyle().Italic(true)
	s = reItalic1.ReplaceAllStringFunc(s, func(m string) string {
		return spans.hold(italic.Render(m[1 : len(m)-1]))
	})
	return italicizeUnderscores(s, func(inner string) string {
		return spans.hold(italic.Render(inner))
	})
}

// inlineSpans holds rendered inline spans behind placeholders of the form
// NUL <index> NUL. Model text never contains NUL (termtext.SanitizeLine drops
// control characters before inline rendering), so a placeholder cannot
// collide with real text, and it holds none of the characters the inline
// patterns look for.
type inlineSpans struct {
	pieces []string
}

// hold parks rendered and returns its placeholder.
func (p *inlineSpans) hold(rendered string) string {
	p.pieces = append(p.pieces, rendered)
	return "\x00" + strconv.Itoa(len(p.pieces)-1) + "\x00"
}

// expand replaces every placeholder in s with its span. A span can itself
// hold placeholders (a code span inside bold text), so expansion recurses;
// a span only ever holds placeholders created before it, so it terminates.
func (p *inlineSpans) expand(s string) string {
	if len(p.pieces) == 0 || !strings.Contains(s, "\x00") {
		return s
	}
	return reInlinePlaceholder.ReplaceAllStringFunc(s, func(m string) string {
		i, err := strconv.Atoi(m[1 : len(m)-1])
		if err != nil || i >= len(p.pieces) {
			return m
		}
		return p.expand(p.pieces[i])
	})
}

// italicizeUnderscores styles _emphasis_ the way CommonMark does for
// underscores: an underscore touching a letter or digit on its outer side is
// literal, so snake_case names and paths like file_with_a_long_name.go keep
// their underscores instead of turning into "filewithalongname.go" with an
// italic middle. style renders one emphasized span.
func italicizeUnderscores(s string, style func(string) string) string {
	if !strings.Contains(s, "_") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prev := ' ' // the start of the line counts as a space
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '_' && !isWordRune(prev) {
			if end := underscoreCloser(s[i+1:]); end >= 0 {
				b.WriteString(style(s[i+1 : i+1+end]))
				i += end + 2 // opener, span, closer
				prev = '_'
				continue
			}
		}
		b.WriteString(s[i : i+size])
		prev = r
		i += size
	}
	return b.String()
}

// underscoreCloser returns the byte offset in s of the "_" that closes an
// emphasis opened just before s, or -1 when there is none. The span must not
// be empty or start or end with a space, and the closer must not be followed
// by a letter or digit (that underscore is inside a word, so the search goes
// on to the next one).
func underscoreCloser(s string) int {
	if s == "" {
		return -1
	}
	if first, _ := utf8.DecodeRuneInString(s); first == '_' || unicode.IsSpace(first) {
		return -1
	}
	for j := 1; j < len(s); j++ {
		if s[j] != '_' {
			continue
		}
		last, _ := utf8.DecodeLastRuneInString(s[:j])
		next, _ := utf8.DecodeRuneInString(s[j+1:]) // RuneError at the end: not a word rune
		if !unicode.IsSpace(last) && last != '_' && !isWordRune(next) {
			return j
		}
	}
	return -1
}

// isWordRune reports whether r belongs to a word for emphasis purposes.
func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
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
