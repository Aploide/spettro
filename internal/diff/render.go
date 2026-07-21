package diff

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// Palette matches internal/tui's styles.go so diffs read as part of the UI.
var (
	styleAdd     = lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981"))
	styleDel     = lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444"))
	styleHunk    = lipgloss.NewStyle().Foreground(lipgloss.Color("#60A5FA")).Italic(true)
	styleMeta    = lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280"))
	styleCtx     = lipgloss.NewStyle().Foreground(lipgloss.Color("#9CA3AF"))
	styleLineNo  = lipgloss.NewStyle().Foreground(lipgloss.Color("#4B5563"))
	styleDivider = lipgloss.NewStyle().Foreground(lipgloss.Color("#374151"))
	// Intra-line emphasis: the changed span within a modified line pair.
	styleAddHi = lipgloss.NewStyle().Foreground(lipgloss.Color("#6EE7B7")).Background(lipgloss.Color("#064E3B"))
	styleDelHi = lipgloss.NewStyle().Foreground(lipgloss.Color("#FCA5A5")).Background(lipgloss.Color("#7F1D1D"))
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
func parseUnified(diffText string) []parsedLine {
	var out []parsedLine
	oldNo, newNo := 0, 0
	inHunk := false
	for line := range strings.SplitSeq(strings.TrimRight(diffText, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			oldNo, newNo = parseHunkHeader(line)
			inHunk = oldNo > 0 || newNo > 0
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

func parseHunkHeader(line string) (oldStart, newStart int) {
	// "@@ -12,7 +12,9 @@ optional context"
	fields := strings.FieldsSeq(line)
	for f := range fields {
		if strings.HasPrefix(f, "-") {
			oldStart = leadingInt(f[1:])
		} else if strings.HasPrefix(f, "+") {
			newStart = leadingInt(f[1:])
		}
	}
	return oldStart, newStart
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
	parsed := parseUnified(diffText)

	avail := 0 // 0 = unlimited
	if opts.Width > 0 {
		avail = opts.Width - len(opts.Indent)
	}
	var rendered []string
	if opts.Width >= SideBySideMinWidth {
		rendered = renderSideBySide(parsed, avail)
	} else {
		rendered = renderUnifiedLines(parsed, avail)
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

// truncCells hard-caps s at max cells (rune-based) so a rendered line never
// wraps in the terminal, which would break height budgeting upstream. max <= 0
// means unlimited.
func truncCells(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// renderUnifiedLines renders one row per diff line, each capped at maxW cells
// (0 = unlimited).
func renderUnifiedLines(parsed []parsedLine, maxW int) []string {
	w := numWidth(parsed)
	textW := 0
	if maxW > 0 {
		textW = max(
			// line numbers + space + sign + space
			maxW-(2*w+1)-3, 8)
	}
	var out []string
	i := 0
	for i < len(parsed) {
		l := parsed[i]
		if l.meta {
			raw := truncCells(l.raw, maxW)
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
			text := truncCells(l.text, textW)
			nums := styleLineNo.Render(fmtNo(l.oldNo, w)+" "+fmtNo(l.newNo, w)) + " "
			out = append(out, nums+styleCtx.Render("  "+text))
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
			out = append(out, nums+renderBodyLine("- ", d.text, delSpans[r], textW, styleDel, styleDelHi))
		}
		for r, a := range adds {
			nums := styleLineNo.Render(strings.Repeat(" ", w)+" "+fmtNo(a.newNo, w)) + " "
			out = append(out, nums+renderBodyLine("+ ", a.text, addSpans[r], textW, styleAdd, styleAddHi))
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
func renderBodyLine(sign, text string, spans []span, textW int, base, hi lipgloss.Style) string {
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
		return renderUnifiedLines(parsed, width)
	}
	textW := col - w - 2

	divider := styleDivider.Render(" │ ")
	cellSpans := func(no int, text string, spans []span, style, hi lipgloss.Style) string {
		r := []rune(text)
		if len(r) > textW {
			r = append(r[:textW-1:textW-1], '…')
			spans = clipSpans(spans, textW-1)
		}
		pad := textW - len(r)
		return styleLineNo.Render(fmtNo(no, w)) + " " + renderSpans(string(r), spans, style, hi) + strings.Repeat(" ", pad)
	}
	cell := func(no int, text string, style lipgloss.Style) string {
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
