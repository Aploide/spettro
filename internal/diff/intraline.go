package diff

import (
	"strings"
	"unicode"
)

// span is a half-open rune-offset range [start, end) within a line.
type span struct {
	start, end int
}

// maxIntralineCells caps the token LCS table so pathological line pairs stay
// cheap; beyond it we fall back to a single prefix/suffix span.
const maxIntralineCells = 10_000

// token is one word or separator of a line, with its rune offset.
type token struct {
	text  string
	start int
}

// tokenizeLine splits a line into word tokens (letter/digit/underscore runs)
// and single-rune separators, so intra-line diffs align on word boundaries.
func tokenizeLine(s string) []token {
	var out []token
	runes := []rune(s)
	i := 0
	isWord := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
	}
	for i < len(runes) {
		start := i
		if isWord(runes[i]) {
			for i < len(runes) && isWord(runes[i]) {
				i++
			}
		} else {
			i++
		}
		out = append(out, token{text: string(runes[start:i]), start: start})
	}
	return out
}

// intralineSpans returns the changed rune ranges of a deleted/added line pair:
// the parts of oldText not present in newText and vice versa. Both are empty
// when the lines are equal. Adjacent changed tokens merge into one span.
func intralineSpans(oldText, newText string) (oldSpans, newSpans []span) {
	if oldText == newText {
		return nil, nil
	}
	a := tokenizeLine(oldText)
	b := tokenizeLine(newText)
	if len(a)*len(b) > maxIntralineCells {
		return prefixSuffixSpans(oldText, newText)
	}

	// LCS over tokens; changed = tokens outside the common subsequence.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i].text == b[j].text {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i].text == b[j].text:
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			oldSpans = appendSpan(oldSpans, tokenSpan(a[i]))
			i++
		default:
			newSpans = appendSpan(newSpans, tokenSpan(b[j]))
			j++
		}
	}
	for ; i < len(a); i++ {
		oldSpans = appendSpan(oldSpans, tokenSpan(a[i]))
	}
	for ; j < len(b); j++ {
		newSpans = appendSpan(newSpans, tokenSpan(b[j]))
	}
	return oldSpans, newSpans
}

// prefixSuffixSpans is the cheap fallback: everything between the common
// prefix and common suffix is one changed span per side.
func prefixSuffixSpans(oldText, newText string) (oldSpans, newSpans []span) {
	a, b := []rune(oldText), []rune(newText)
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	if len(a)-s > p {
		oldSpans = []span{{p, len(a) - s}}
	}
	if len(b)-s > p {
		newSpans = []span{{p, len(b) - s}}
	}
	return oldSpans, newSpans
}

func tokenSpan(t token) span {
	return span{t.start, t.start + len([]rune(t.text))}
}

// appendSpan adds sp, merging it into the previous span when contiguous.
func appendSpan(spans []span, sp span) []span {
	if n := len(spans); n > 0 && spans[n-1].end == sp.start {
		spans[n-1].end = sp.end
		return spans
	}
	return append(spans, sp)
}

// clipSpans drops or trims spans that extend past maxRunes (the visible,
// possibly truncated, portion of the line). maxRunes < 0 means no limit.
func clipSpans(spans []span, maxRunes int) []span {
	if maxRunes < 0 {
		return spans
	}
	var out []span
	for _, sp := range spans {
		if sp.start >= maxRunes {
			break
		}
		if sp.end > maxRunes {
			sp.end = maxRunes
		}
		out = append(out, sp)
	}
	return out
}

// wholeLine reports whether the spans cover text entirely — highlighting adds
// no information then, so the renderer skips it.
func wholeLine(spans []span, text string) bool {
	return len(spans) == 1 && spans[0].start == 0 && spans[0].end == len([]rune(text))
}

// renderSpans styles text with base, switching to hi inside the given spans.
// Output is built with per-segment WriteString calls (no per-rune concat).
func renderSpans(text string, spans []span, base, hi styler) string {
	if len(spans) == 0 {
		return base.Render(text)
	}
	runes := []rune(text)
	var sb strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp.start > pos {
			sb.WriteString(base.Render(string(runes[pos:sp.start])))
		}
		if sp.end > sp.start {
			sb.WriteString(hi.Render(string(runes[sp.start:sp.end])))
		}
		pos = sp.end
	}
	if pos < len(runes) {
		sb.WriteString(base.Render(string(runes[pos:])))
	}
	return sb.String()
}

// styler is the minimal rendering interface renderSpans needs (satisfied by
// lipgloss.Style); it keeps span rendering testable without styling noise.
type styler interface {
	Render(...string) string
}
