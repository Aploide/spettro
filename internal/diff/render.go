package diff

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/termtext"
	"spettro/internal/theme"
)

// themedStyle builds a lipgloss style from the palette that is active at the
// moment text is rendered. The diff palette cannot be frozen in package-level
// vars the way it used to be: a /theme switch has to repaint diffs that were
// already produced, and this package is built before any theme is resolved.
type themedStyle func(theme.Palette) lipgloss.Style

// Render resolves the current theme, so every call site keeps reading like a
// plain lipgloss.Style. themedStyle also satisfies styler, which is what the
// span renderer takes.
func (s themedStyle) Render(strs ...string) string {
	return s(theme.Current()).Render(strs...)
}

// Palette roles mirror internal/tui's so diffs read as part of the UI.
var (
	styleAdd     = themedStyle(func(p theme.Palette) lipgloss.Style { return lipgloss.NewStyle().Foreground(p.Success) })
	styleDel     = themedStyle(func(p theme.Palette) lipgloss.Style { return lipgloss.NewStyle().Foreground(p.Error) })
	styleHunk    = themedStyle(func(p theme.Palette) lipgloss.Style { return lipgloss.NewStyle().Foreground(p.Info).Italic(true) })
	styleMeta    = themedStyle(func(p theme.Palette) lipgloss.Style { return lipgloss.NewStyle().Foreground(p.TextMuted) })
	styleCtx     = themedStyle(func(p theme.Palette) lipgloss.Style { return lipgloss.NewStyle().Foreground(p.TextSubtle) })
	styleLineNo  = themedStyle(func(p theme.Palette) lipgloss.Style { return lipgloss.NewStyle().Foreground(p.DiffLineNo) })
	styleDivider = themedStyle(func(p theme.Palette) lipgloss.Style { return lipgloss.NewStyle().Foreground(p.DiffDivider) })
	// Intra-line emphasis: the changed span within a modified line pair. These
	// paint a background, so the light theme flips them to a pale tint under
	// dark ink rather than reusing the dark theme's deep box.
	styleAddHi = themedStyle(func(p theme.Palette) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(p.DiffAddHiFg).Background(p.DiffAddHiBg)
	})
	styleDelHi = themedStyle(func(p theme.Palette) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(p.DiffDelHiFg).Background(p.DiffDelHiBg)
	})
)

// Options controls Render.
type Options struct {
	// Width is the available cell width. Side-by-side layout is used when it
	// is at least SideBySideMinWidth; 0 disables side-by-side.
	Width int
	// MaxLines caps rendered diff body lines (0 = no cap). Truncation adds a
	// "… N more lines" footer, optionally suffixed with ExpandHint.
	MaxLines int
	// ExpandHint, when non-empty, is appended to the truncation footer, e.g.
	// "(ctrl+o to expand)".
	ExpandHint string
	// Indent is prefixed to every rendered line.
	Indent string
	// Wrap breaks a line wider than Width over as many rows as it needs
	// instead of cutting it with "…", so every character of the diff is
	// drawn. Continuation rows leave the line-number gutter and the +/-
	// column blank. Wrap always uses the unified layout (a wrapped
	// side-by-side row pairs badly with its other half). It is for views
	// that must show a change whole, such as the approval review; with Width
	// 0 there is nothing to wrap at and it has no effect.
	Wrap bool
	// Exact escapes every line with termtext.EscapeExact instead of
	// termtext.EscapeControls: a tab is drawn as a tab mark rather than as
	// spaces, and a carriage return at the end of a line as "^M" rather than
	// dropped. It is for the same views as Wrap, where a tab-indented line
	// must not pass for a space-indented one.
	Exact bool
}

// SideBySideMinWidth is the minimum terminal width for side-by-side layout.
const SideBySideMinWidth = 120

// parsedLine is one body line of a unified diff with resolved line numbers.
type parsedLine struct {
	kind  lineKind
	oldNo int
	newNo int
	text  string
	meta  bool // file headers / hunk headers / anything non-body
	raw   string
}

// parseUnified walks unified-diff text (ours or git's) tracking hunk line
// numbers so the renderer can show them.
//
// Diff bodies are file contents, so they carry tabs (Go and Makefiles are
// tab-indented), carriage returns and occasionally escape sequences. Each
// line has its control characters made visible here (termtext.EscapeControls,
// or termtext.EscapeExact when exact is set), before intra-line spans are
// computed and before truncation measures it, so every later width
// calculation sees exactly the cells the terminal will draw. They are escaped
// rather than stripped because a diff is often shown for approval: a carriage
// return must not be able to hide part of a line.
//
// A hunk header says how many old and new lines its hunk holds, and every
// line inside those counts is a body line whatever it starts with: deleting
// the SQL comment "-- keep row level security on" gives the body line
// "--- keep row level security on", which must be drawn as a deletion, not
// taken for a file header (that would hide the deletion and shift every line
// number after it). File headers are only recognised between hunks. A hunk
// header without counts (not one any diff tool writes) falls back to reading
// each line by its prefix until the next header.
func parseUnified(diffText string, exact bool) []parsedLine {
	escape := termtext.EscapeControls
	if exact {
		escape = termtext.EscapeExact
	}
	var out []parsedLine
	oldNo, newNo := 0, 0
	// oldLeft and newLeft are the lines the current hunk still holds;
	// counted is false for a header without counts, where the hunk runs to
	// the next header line instead.
	oldLeft, newLeft := 0, 0
	counted, inHunk := true, false
	for rawLine := range strings.SplitSeq(strings.TrimRight(diffText, "\n"), "\n") {
		line := escape(rawLine)
		if inHunk && counted && oldLeft <= 0 && newLeft <= 0 {
			inHunk = false
		}
		if inHunk && counted && !strings.HasPrefix(line, "@@") && !strings.HasPrefix(line, "\\") {
			// A body line, whatever it looks like.
			switch {
			case strings.HasPrefix(line, "+"):
				out = append(out, parsedLine{kind: kindAdd, newNo: newNo, text: line[1:], raw: line})
				newNo++
				newLeft--
			case strings.HasPrefix(line, "-"):
				out = append(out, parsedLine{kind: kindDel, oldNo: oldNo, text: line[1:], raw: line})
				oldNo++
				oldLeft--
			default:
				text := strings.TrimPrefix(line, " ")
				out = append(out, parsedLine{kind: kindContext, oldNo: oldNo, newNo: newNo, text: text, raw: line})
				oldNo++
				newNo++
				oldLeft--
				newLeft--
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "@@"):
			var h hunkHeader
			h, counted = parseHunkHeader(line)
			oldNo, newNo = h.oldStart, h.newStart
			oldLeft, newLeft = h.oldCount, h.newCount
			inHunk = !counted && (oldNo > 0 || newNo > 0) || counted && (oldLeft > 0 || newLeft > 0)
			out = append(out, parsedLine{meta: true, raw: line})
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"),
			strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "index "),
			strings.HasPrefix(line, "new file"), strings.HasPrefix(line, "deleted file"),
			strings.HasPrefix(line, "\\ No newline"):
			out = append(out, parsedLine{meta: true, raw: line})
		case inHunk && strings.HasPrefix(line, "+"):
			out = append(out, parsedLine{kind: kindAdd, newNo: newNo, text: line[1:], raw: line})
			newNo++
		case inHunk && strings.HasPrefix(line, "-"):
			out = append(out, parsedLine{kind: kindDel, oldNo: oldNo, text: line[1:], raw: line})
			oldNo++
		case inHunk:
			text := strings.TrimPrefix(line, " ")
			out = append(out, parsedLine{kind: kindContext, oldNo: oldNo, newNo: newNo, text: text, raw: line})
			oldNo++
			newNo++
		default:
			out = append(out, parsedLine{meta: true, raw: line})
		}
	}
	return out
}

// hunkHeader is what a "@@ -12,7 +12,9 @@" line says: where the hunk starts
// in the old and new file and how many lines of each it holds.
type hunkHeader struct {
	oldStart, oldCount int
	newStart, newCount int
}

// parseHunkHeader reads a hunk header. A range without a count ("-12")
// holds one line, as in every unified diff. ok is false when the line does
// not have both ranges in that form.
func parseHunkHeader(line string) (h hunkHeader, ok bool) {
	// "@@ -12,7 +12,9 @@ optional context"
	fields := strings.Fields(line)
	if len(fields) < 4 || fields[0] != "@@" || fields[3] != "@@" {
		// Not a well-formed header: take what start lines it has.
		for _, f := range fields {
			if strings.HasPrefix(f, "-") {
				h.oldStart = leadingInt(f[1:])
			} else if strings.HasPrefix(f, "+") {
				h.newStart = leadingInt(f[1:])
			}
		}
		return h, false
	}
	var okOld, okNew bool
	h.oldStart, h.oldCount, okOld = parseHunkRange(fields[1], '-')
	h.newStart, h.newCount, okNew = parseHunkRange(fields[2], '+')
	return h, okOld && okNew
}

// parseHunkRange reads one range of a hunk header ("-12,7" or "+12"), whose
// first byte must be sign.
func parseHunkRange(f string, sign byte) (start, count int, ok bool) {
	if len(f) < 2 || f[0] != sign {
		return 0, 0, false
	}
	startText, countText, hasCount := strings.Cut(f[1:], ",")
	start, err := strconv.Atoi(startText)
	if err != nil {
		return 0, 0, false
	}
	count = 1
	if hasCount {
		if count, err = strconv.Atoi(countText); err != nil {
			return start, 0, false
		}
	}
	return start, count, true
}

func leadingInt(s string) int {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	n, _ := strconv.Atoi(s)
	return n
}

// Render colorizes unified-diff text with line numbers, switching to a
// side-by-side layout when opts.Width allows. It caps output at opts.MaxLines
// and never fails: unparseable lines pass through with muted styling.
func Render(diffText string, opts Options) string {
	if strings.TrimSpace(diffText) == "" {
		return ""
	}
	parsed := parseUnified(diffText, opts.Exact)

	avail := 0 // 0 = unlimited
	if opts.Width > 0 {
		avail = opts.Width - len(opts.Indent)
	}
	var rendered []string
	switch {
	case opts.Wrap && avail > 0:
		rendered = renderUnifiedLines(parsed, avail, true)
	case opts.Width >= SideBySideMinWidth:
		rendered = renderSideBySide(parsed, avail)
	default:
		rendered = renderUnifiedLines(parsed, avail, false)
	}

	shown := rendered
	truncated := 0
	if opts.MaxLines > 0 && len(rendered) > opts.MaxLines {
		shown = rendered[:opts.MaxLines]
		truncated = len(rendered) - opts.MaxLines
	}

	var sb strings.Builder
	for i, l := range shown {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(opts.Indent)
		sb.WriteString(l)
	}
	if truncated > 0 {
		footer := fmt.Sprintf("… %d more lines", truncated)
		if opts.ExpandHint != "" {
			footer += " " + opts.ExpandHint
		}
		footer = truncCells(footer, avail)
		sb.WriteString("\n")
		sb.WriteString(opts.Indent)
		sb.WriteString(styleMeta.Render(footer))
	}
	return sb.String()
}

// numWidth returns the digit width needed for the largest line number.
func numWidth(parsed []parsedLine) int {
	max := 1
	for _, l := range parsed {
		if l.oldNo > max {
			max = l.oldNo
		}
		if l.newNo > max {
			max = l.newNo
		}
	}
	return len(strconv.Itoa(max))
}

func fmtNo(n, width int) string {
	if n <= 0 {
		return strings.Repeat(" ", width)
	}
	return fmt.Sprintf("%*d", width, n)
}

// truncCells hard-caps plain text s at max display cells, ending a cut line
// with "…", so a rendered line never wraps in the terminal, which would break
// height budgeting upstream. Widths are cells, not runes: a line of CJK text
// is twice as wide as its rune count. max <= 0 means unlimited.
func truncCells(s string, max int) string {
	if max <= 0 {
		return s
	}
	return termtext.Fit(s, max)
}

// Overflows reports whether Render would cut any line of diffText with "…"
// at opts.Width in the unified layout, so a caller that must show a change
// whole knows it did not. It assumes the unified layout (opts.Width below
// SideBySideMinWidth), and is false when opts.Width is 0 (no limit) or
// opts.Wrap is set (nothing is cut then).
func Overflows(diffText string, opts Options) bool {
	if opts.Width <= 0 || opts.Wrap || strings.TrimSpace(diffText) == "" {
		return false
	}
	avail := opts.Width - len(opts.Indent)
	parsed := parseUnified(diffText, opts.Exact)
	textW := unifiedTextWidth(avail, numWidth(parsed))
	for _, l := range parsed {
		limit, text := textW, l.text
		if l.meta {
			limit, text = avail, l.raw
		}
		if ansi.StringWidth(text) > limit {
			return true
		}
	}
	return false
}

// unifiedTextWidth is the room a unified row leaves for a line's text once
// the two line-number columns, the sign and the spaces between them are
// drawn, for a row maxW cells wide (0 = unlimited) and line numbers numW
// digits wide.
func unifiedTextWidth(maxW, numW int) int {
	if maxW <= 0 {
		return 0
	}
	// line numbers + space + sign + space
	return max(maxW-(2*numW+1)-3, 8)
}

// renderUnifiedLines renders the diff in the unified layout, each row at
// most maxW cells (0 = unlimited). A line too long for its row is cut with
// "…", or, with wrap, continued on further rows under a blank gutter.
func renderUnifiedLines(parsed []parsedLine, maxW int, wrap bool) []string {
	w := numWidth(parsed)
	textW := unifiedTextWidth(maxW, w)
	blankNums := styleLineNo.Render(strings.Repeat(" ", 2*w+1)) + " "
	var out []string
	i := 0
	for i < len(parsed) {
		l := parsed[i]
		if l.meta {
			style := styleMeta
			if strings.HasPrefix(l.raw, "@@") {
				style = styleHunk
			}
			if wrap {
				for _, row := range termtext.HardWrap(l.raw, maxW) {
					out = append(out, style.Render(row))
				}
			} else {
				out = append(out, style.Render(truncCells(l.raw, maxW)))
			}
			i++
			continue
		}
		if l.kind == kindContext {
			nums := styleLineNo.Render(fmtNo(l.oldNo, w)+" "+fmtNo(l.newNo, w)) + " "
			if !wrap {
				out = append(out, nums+styleCtx.Render("  "+truncCells(l.text, textW)))
				i++
				continue
			}
			for r, row := range termtext.HardWrap(l.text, textW) {
				if r > 0 {
					nums = blankNums
				}
				out = append(out, nums+styleCtx.Render("  "+row))
			}
			i++
			continue
		}
		// Contiguous del run then add run: paired rows get intra-line spans.
		var dels, adds []parsedLine
		for i < len(parsed) && !parsed[i].meta && parsed[i].kind == kindDel {
			dels = append(dels, parsed[i])
			i++
		}
		for i < len(parsed) && !parsed[i].meta && parsed[i].kind == kindAdd {
			adds = append(adds, parsed[i])
			i++
		}
		delSpans := make([][]span, len(dels))
		addSpans := make([][]span, len(adds))
		for r := 0; r < len(dels) && r < len(adds); r++ {
			delSpans[r], addSpans[r] = pairSpans(dels[r].text, adds[r].text)
		}
		for r, d := range dels {
			nums := styleLineNo.Render(fmtNo(d.oldNo, w)+" "+strings.Repeat(" ", w)) + " "
			if wrap {
				out = append(out, wrapBodyLine(nums, blankNums, "- ", d.text, delSpans[r], textW, styleDel, styleDelHi)...)
			} else {
				out = append(out, nums+renderBodyLine("- ", d.text, delSpans[r], textW, styleDel, styleDelHi))
			}
		}
		for r, a := range adds {
			nums := styleLineNo.Render(strings.Repeat(" ", w)+" "+fmtNo(a.newNo, w)) + " "
			if wrap {
				out = append(out, wrapBodyLine(nums, blankNums, "+ ", a.text, addSpans[r], textW, styleAdd, styleAddHi)...)
			} else {
				out = append(out, nums+renderBodyLine("+ ", a.text, addSpans[r], textW, styleAdd, styleAddHi))
			}
		}
	}
	return out
}

// wrapBodyLine is renderBodyLine for Options.Wrap: the whole of a +/- line
// over as many rows as it takes, the first under its line numbers and sign,
// the rest under a blank gutter, with the changed spans still emphasized on
// whichever row they fall.
func wrapBodyLine(nums, blankNums, sign, text string, spans []span, textW int, base, hi styler) []string {
	rows := termtext.HardWrap(text, textW)
	out := make([]string, 0, len(rows))
	offset := 0 // rune offset of the row in text
	for r, row := range rows {
		n := utf8.RuneCountInString(row)
		prefix := nums + base.Render(sign)
		if r > 0 {
			prefix = blankNums + base.Render(strings.Repeat(" ", len(sign)))
		}
		out = append(out, prefix+renderSpans(row, shiftSpans(spans, offset, n), base, hi))
		offset += n
	}
	return out
}

// shiftSpans returns the part of spans that falls in the runes
// [offset, offset+n) of a line, relative to offset: the spans of one row of
// a wrapped line.
func shiftSpans(spans []span, offset, n int) []span {
	var out []span
	for _, sp := range spans {
		start, end := max(sp.start-offset, 0), min(sp.end-offset, n)
		if start < end {
			out = append(out, span{start, end})
		}
	}
	return out
}

// pairSpans computes intra-line spans for a del/add row pair, dropping them
// when they cover a whole side (no information gain over line coloring).
func pairSpans(oldText, newText string) (oldSpans, newSpans []span) {
	oldSpans, newSpans = intralineSpans(oldText, newText)
	if wholeLine(oldSpans, oldText) || wholeLine(newSpans, newText) {
		return nil, nil
	}
	return oldSpans, newSpans
}

// renderBodyLine truncates a +/- body line to textW cells then styles it,
// emphasizing the changed spans.
func renderBodyLine(sign, text string, spans []span, textW int, base, hi styler) string {
	shown := truncCells(text, textW)
	if shown != text {
		// Reserve the trailing "…" from highlighting.
		spans = clipSpans(spans, len([]rune(shown))-1)
	}
	return base.Render(sign) + renderSpans(shown, spans, base, hi)
}

// renderSideBySide lays out deletions on the left and additions on the right.
// Adjacent del/add runs within a hunk are paired row-by-row.
func renderSideBySide(parsed []parsedLine, width int) []string {
	w := numWidth(parsed)
	// column = (width - divider(3)) / 2; each column holds "NNN text".
	col := (width - 3) / 2
	if col < w+10 {
		// Too narrow after all; fall back to unified.
		return renderUnifiedLines(parsed, width, false)
	}
	textW := col - w - 2

	divider := styleDivider.Render(" │ ")
	cellSpans := func(no int, text string, spans []span, style, hi styler) string {
		shown := truncCells(text, textW)
		if shown != text {
			// Reserve the trailing "…" from highlighting.
			spans = clipSpans(spans, len([]rune(shown))-1)
		}
		// Pad by cells, not runes, so the divider stays in one column when
		// a line holds wide characters.
		pad := max(textW-ansi.StringWidth(shown), 0)
		return styleLineNo.Render(fmtNo(no, w)) + " " + renderSpans(shown, spans, style, hi) + strings.Repeat(" ", pad)
	}
	cell := func(no int, text string, style styler) string {
		return cellSpans(no, text, nil, style, style)
	}
	emptyCell := strings.Repeat(" ", col-1)

	var out []string
	i := 0
	for i < len(parsed) {
		l := parsed[i]
		if l.meta {
			raw := truncCells(l.raw, width)
			switch {
			case strings.HasPrefix(l.raw, "@@"):
				out = append(out, styleHunk.Render(raw))
			default:
				out = append(out, styleMeta.Render(raw))
			}
			i++
			continue
		}
		if l.kind == kindContext {
			out = append(out, cell(l.oldNo, l.text, styleCtx)+divider+cell(l.newNo, l.text, styleCtx))
			i++
			continue
		}
		// Collect the contiguous del run then the contiguous add run and zip.
		var dels, adds []parsedLine
		for i < len(parsed) && !parsed[i].meta && parsed[i].kind == kindDel {
			dels = append(dels, parsed[i])
			i++
		}
		for i < len(parsed) && !parsed[i].meta && parsed[i].kind == kindAdd {
			adds = append(adds, parsed[i])
			i++
		}
		rows := max(len(adds), len(dels))
		for r := 0; r < rows; r++ {
			left, right := emptyCell, emptyCell
			var delSp, addSp []span
			if r < len(dels) && r < len(adds) {
				delSp, addSp = pairSpans(dels[r].text, adds[r].text)
			}
			if r < len(dels) {
				left = cellSpans(dels[r].oldNo, dels[r].text, delSp, styleDel, styleDelHi)
			}
			if r < len(adds) {
				right = cellSpans(adds[r].newNo, adds[r].text, addSp, styleAdd, styleAddHi)
			}
			out = append(out, left+divider+right)
		}
	}
	return out
}
