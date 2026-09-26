package agent

import (
	"slices"
	"strings"
)

// indentUnit is one level of a text's indentation: a tab, or width spaces.
// The zero value means the style could not be determined.
type indentUnit struct {
	tab   bool
	width int
}

func (u indentUnit) known() bool { return u.tab || u.width > 0 }

// cols is how many columns one level spans; a tab level counts as 4.
func (u indentUnit) cols() int {
	if u.tab || u.width <= 0 {
		return 4
	}
	return u.width
}

func (u indentUnit) str() string {
	if u.tab {
		return "\t"
	}
	return strings.Repeat(" ", u.width)
}

// detectIndentUnit guesses the indentation style of lines: tabs when most
// indented lines start with a tab, otherwise spaces, with the width taken from
// the most common step between consecutive space-indented lines.
func detectIndentUnit(lines []string) indentUnit {
	tabs, spaces := 0, 0
	var steps [9]int
	prev := -1
	for _, l := range lines {
		t := strings.TrimSpace(l)
		// Block-comment continuation lines (" * foo") are aligned, not indented.
		if t == "" || strings.HasPrefix(t, "*") {
			continue
		}
		lead := leadingWS(l)
		if strings.Contains(lead, "\t") {
			if strings.HasPrefix(lead, "\t") {
				tabs++
			}
			prev = -1
			continue
		}
		w := len(lead)
		if w > 0 {
			spaces++
		}
		if prev >= 0 && w > prev && w-prev < len(steps) {
			steps[w-prev]++
		}
		prev = w
	}
	if tabs == 0 && spaces == 0 {
		return indentUnit{}
	}
	if tabs >= spaces {
		return indentUnit{tab: true}
	}
	best := 0
	for d := 1; d < len(steps); d++ {
		if steps[d] > steps[best] {
			best = d
		}
	}
	return indentUnit{width: best}
}

// indentCols measures lead in columns, a tab counting as tabCols.
func indentCols(lead string, tabCols int) int {
	n := 0
	for _, c := range lead {
		if c == '\t' {
			n += tabCols
		} else {
			n++
		}
	}
	return n
}

// reindentForFile re-indents new_string for a fuzzy match so the file keeps
// its own indentation. quoted and file are paired lines: old_string's lines
// and the file lines they matched. Two mappings are considered:
//
//   - shift: swap old_string's base indentation for the file's, keeping the
//     rest of each line's indentation as written;
//   - level: re-express each line's depth, measured in old_string's indent
//     unit, in the file's unit (e.g. 4 spaces per level -> one tab).
//
// The chosen mapping must reproduce the file's indentation from every quoted
// line, so a guess about units is only trusted when the match confirms it.
// Level wins when the quote and the file disagree on tabs vs spaces.
func reindentForFile(newStr string, quoted, file []string, fileUnit indentUnit) string {
	base := -1
	for i, l := range quoted {
		if i < len(file) && strings.TrimSpace(l) != "" && strings.TrimSpace(file[i]) != "" {
			base = i
			break
		}
	}
	if base < 0 {
		return newStr
	}
	oldBase, fileBase := leadingWS(quoted[base]), leadingWS(file[base])
	newLines := strings.Split(newStr, "\n")
	quoteUnit := detectIndentUnit(append(append([]string{}, quoted...), newLines...))

	shift := func(lead string) (string, bool) {
		if !strings.HasPrefix(lead, oldBase) {
			return lead, false
		}
		return fileBase + lead[len(oldBase):], true
	}
	level := func(lead string) (string, bool) {
		if !quoteUnit.known() || !fileUnit.known() {
			return lead, false
		}
		qc := quoteUnit.cols()
		w := indentCols(lead, qc) - indentCols(oldBase, qc)
		if w < 0 {
			// Shallower than old_string's base: drop as many file levels.
			out := fileBase
			for n := (-w + qc - 1) / qc; n > 0 && strings.HasSuffix(out, fileUnit.str()); n-- {
				out = strings.TrimSuffix(out, fileUnit.str())
			}
			return out, true
		}
		return fileBase + strings.Repeat(fileUnit.str(), w/qc) + strings.Repeat(" ", w%qc), true
	}
	fits := func(f func(string) (string, bool)) bool {
		for i, l := range quoted {
			if i >= len(file) || strings.TrimSpace(l) == "" || strings.TrimSpace(file[i]) == "" {
				continue
			}
			got, ok := f(leadingWS(l))
			if !ok || got != leadingWS(file[i]) {
				return false
			}
		}
		return true
	}
	styleDiffers := quoteUnit.known() && fileUnit.known() && quoteUnit.tab != fileUnit.tab
	mapLead := shift
	switch {
	case styleDiffers && fits(level):
		mapLead = level
	case fits(shift):
		if oldBase == fileBase {
			return newStr
		}
	case fits(level):
		mapLead = level
	default:
		// No single mapping reproduces the file's indentation: the quote's
		// relative indentation is wrong somewhere, and new_string most likely
		// repeats the mistake. Map line by line instead.
		if out, ok := reindentPerLine(newLines, quoted, file, quoteUnit, fileUnit); ok {
			return out
		}
	}
	for i, l := range newLines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		lead := leadingWS(l)
		if nl, ok := mapLead(lead); ok {
			newLines[i] = nl + l[len(lead):]
		} else if nl, ok := level(lead); ok {
			// shift cannot place a line shallower than old_string's base
			// (a dedented "def" or closing brace); measure it in levels.
			newLines[i] = nl + l[len(lead):]
		}
	}
	return strings.Join(newLines, "\n")
}

// reindentPerLine re-indents new_string when no uniform mapping fits: a
// new_string line that repeats a quoted line (compared trimmed, aligned in
// order) takes the indentation of the file line that quoted line matched, so
// unchanged lines keep the file's own indentation; any other line keeps its
// depth relative to the nearest aligned line. ok is false when nothing aligns
// or the block is too large to align.
func reindentPerLine(newLines, quoted, file []string, quoteUnit, fileUnit indentUnit) (string, bool) {
	var nIdx, qIdx []int
	for i, l := range newLines {
		if strings.TrimSpace(l) != "" {
			nIdx = append(nIdx, i)
		}
	}
	for j, l := range quoted {
		if j < len(file) && strings.TrimSpace(l) != "" && strings.TrimSpace(file[j]) != "" {
			qIdx = append(qIdx, j)
		}
	}
	a, b := len(nIdx), len(qIdx)
	if a == 0 || b == 0 || a*b > 4_000_000 {
		return "", false
	}
	// Longest common subsequence of the trimmed lines.
	dp := make([][]int32, a+1)
	for i := range dp {
		dp[i] = make([]int32, b+1)
	}
	for i := a - 1; i >= 0; i-- {
		for j := b - 1; j >= 0; j-- {
			if strings.TrimSpace(newLines[nIdx[i]]) == strings.TrimSpace(quoted[qIdx[j]]) {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	match := map[int]int{} // new line -> file line
	for i, j := 0, 0; i < a && j < b; {
		switch {
		case strings.TrimSpace(newLines[nIdx[i]]) == strings.TrimSpace(quoted[qIdx[j]]):
			match[nIdx[i]] = qIdx[j]
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	if len(match) == 0 {
		return "", false
	}
	unit := fileUnit
	if !unit.known() {
		unit = quoteUnit
	}
	qc := quoteUnit.cols()
	out := slices.Clone(newLines)
	for _, i := range nIdx {
		l := newLines[i]
		lead := leadingWS(l)
		if j, ok := match[i]; ok {
			out[i] = leadingWS(file[j]) + l[len(lead):]
			continue
		}
		anchor := -1
		for p := i - 1; p >= 0 && anchor < 0; p-- {
			if _, ok := match[p]; ok {
				anchor = p
			}
		}
		for p := i + 1; p < len(newLines) && anchor < 0; p++ {
			if _, ok := match[p]; ok {
				anchor = p
			}
		}
		delta := indentCols(lead, qc) - indentCols(leadingWS(newLines[anchor]), qc)
		out[i] = shiftIndent(leadingWS(file[match[anchor]]), delta, qc, unit) + l[len(lead):]
	}
	return strings.Join(out, "\n"), true
}

// shiftIndent moves lead by delta columns (qc columns to a level) in unit.
func shiftIndent(lead string, delta, qc int, unit indentUnit) string {
	if delta >= 0 {
		if !unit.known() {
			return lead + strings.Repeat(" ", delta)
		}
		return lead + strings.Repeat(unit.str(), delta/qc) + strings.Repeat(" ", delta%qc)
	}
	us := unit.str()
	if !unit.known() {
		us = " "
		qc = 1
	}
	for n := (-delta + qc - 1) / qc; n > 0 && strings.HasSuffix(lead, us); n-- {
		lead = strings.TrimSuffix(lead, us)
	}
	return lead
}
