package agent

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Match tiers for file-edit/multi-edit old_string lookup, tried in order.
// The chain is modelled on OpenCode's edit replacers, ordered from the most to
// the least precise. Every tier must find exactly one location (unless
// replace_all is set), and the first tier that finds anything decides: a tier
// that finds several locations stops the chain with an ambiguity error instead
// of letting a looser tier guess which one was meant.
const (
	editTierExact           = iota + 1
	editTierLineExact       // identical lines; only line endings / a trailing newline differ
	editTierIndentFlexible  // identical after removing the block's common indentation
	editTierLineTrim        // every line equal after trimming surrounding whitespace
	editTierWhitespace      // equal after collapsing every whitespace run
	editTierUnicode         // equal after mapping smart quotes, dashes and odd spaces to ASCII
	editTierEscape          // equal after unescaping \n, \t, \" ... in old_string
	editTierTrimmedBoundary // equal after trimming old_string's leading/trailing whitespace
	editTierBlockAnchor     // first/last lines equal, middle lines similar
	editTierContextAware    // first/last lines equal, at least half the middle lines equal
)

var editTierLabels = map[int]string{
	editTierExact:           "exact",
	editTierLineExact:       "line-ending",
	editTierIndentFlexible:  "indentation-flexible",
	editTierLineTrim:        "per-line trimmed",
	editTierWhitespace:      "whitespace-normalized",
	editTierUnicode:         "quote/dash-normalized",
	editTierEscape:          "escape-normalized",
	editTierTrimmedBoundary: "boundary-trimmed",
	editTierBlockAnchor:     "first/last-line anchored",
	editTierContextAware:    "context",
}

// editTierNote is the note appended to a successful edit's result, telling the
// model its old_string was not byte-exact. Tiers that only absorb line-ending
// differences return "": the quote was faithful, so there is nothing to fix.
func editTierNote(tier int) string {
	switch tier {
	case editTierIndentFlexible:
		return "matched with different indentation; new_string was re-indented to the file"
	case editTierLineTrim:
		return "matched after per-line whitespace trim"
	case editTierWhitespace:
		return "matched after whitespace normalization"
	case editTierUnicode:
		return "matched after normalizing smart quotes/dashes to ASCII"
	case editTierEscape:
		return `matched after unescaping \n, \t and quotes in old_string (it was double-escaped)`
	case editTierTrimmedBoundary:
		return "matched after trimming old_string's leading/trailing whitespace"
	case editTierBlockAnchor:
		return "matched only by its first and last lines; the lines between differed, so check the diff"
	case editTierContextAware:
		return "matched only by surrounding context; some lines differed, so check the diff"
	default:
		return ""
	}
}

// editRequest is one old_string -> new_string replacement.
type editRequest struct {
	Old, New   string
	ReplaceAll bool
	// LineOffset is added to every line number an error reports, for callers
	// that pass a slice of the file (file-edit's start_line).
	LineOffset int
	// TrustLineNumbers lets line-number prefixes copied into old_string pick
	// between identical matches. Set it only when the content is unchanged
	// since the read those numbers came from; once lines have moved, the
	// number would silently pick the wrong occurrence.
	TrustLineNumbers bool
}

type editResult struct {
	Content string
	Count   int
	Tier    int
	// Notes explain any normalization that was needed to find old_string.
	Notes []string
}

const utf8BOM = "\uFEFF"

// applyEdit applies one replacement to content. Exact matching is tried first;
// when that fails, the fuzzy tiers above run in order. Line endings are
// normalized around the whole chain: a CRLF file is matched as LF and written
// back as CRLF, and a UTF-8 BOM is preserved.
func applyEdit(content string, req editRequest) (editResult, error) {
	if req.Old == "" {
		return editResult{}, errors.New("old_string is empty")
	}
	bom := ""
	if strings.HasPrefix(content, utf8BOM) {
		bom, content = utf8BOM, content[len(utf8BOM):]
	}
	crlf := isCRLF(content)
	if crlf {
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	oldStr, newStr := req.Old, req.New
	// A file with stray "\r"s (mixed endings) is matched as-is; otherwise the
	// model's own CRLFs are noise.
	if !strings.Contains(content, "\r") {
		oldStr = strings.ReplaceAll(oldStr, "\r\n", "\n")
		newStr = strings.ReplaceAll(newStr, "\r\n", "\n")
	}
	m := newEditMatcher(content, req.LineOffset)
	res, err := m.run(oldStr, newStr, req.ReplaceAll, req.TrustLineNumbers)
	if err != nil {
		return editResult{}, err
	}
	if crlf {
		res.Content = strings.ReplaceAll(res.Content, "\n", "\r\n")
	}
	res.Content = bom + res.Content
	return res, nil
}

// isCRLF reports whether every line break in s is "\r\n".
func isCRLF(s string) bool {
	n := strings.Count(s, "\n")
	return n > 0 && strings.Count(s, "\r\n") == n
}

type editMatcher struct {
	content string
	// lines holds content split on "\n" with any trailing "\r" removed;
	// starts holds the byte offset of each line.
	lines      []string
	starts     []int
	lineOffset int
	unit       *indentUnit
}

func newEditMatcher(content string, lineOffset int) *editMatcher {
	m := &editMatcher{content: content, lineOffset: lineOffset}
	start := 0
	for {
		m.starts = append(m.starts, start)
		i := strings.IndexByte(content[start:], '\n')
		if i < 0 {
			m.lines = append(m.lines, strings.TrimSuffix(content[start:], "\r"))
			return m
		}
		m.lines = append(m.lines, strings.TrimSuffix(content[start:start+i], "\r"))
		start += i + 1
	}
}

// lineOf returns the 0-based line holding byte offset off.
func (m *editMatcher) lineOf(off int) int {
	return sort.Search(len(m.starts), func(i int) bool { return m.starts[i] > off }) - 1
}

// lineNo converts a 0-based line index to the 1-based number the model sees.
func (m *editMatcher) lineNo(idx int) int { return idx + 1 + m.lineOffset }

func (m *editMatcher) fileUnit() indentUnit {
	if m.unit == nil {
		u := detectIndentUnit(m.lines)
		m.unit = &u
	}
	return *m.unit
}

// editSpan is one location to replace: a byte range of the content and the
// text that goes there (new_string, possibly re-indented for this spot).
type editSpan struct {
	start, end int
	repl       string
	detail     string // extra note, e.g. how similar a block-anchor match was
}

// editQuery is old_string/new_string prepared for line-based matching.
type editQuery struct {
	old, new string
	// lines is old_string split into lines, without a trailing newline.
	lines []string
	// trailingNL records that old_string ended in "\n", so line-based matches
	// consume the newline after their last line too.
	trailingNL bool
}

func newEditQuery(oldStr, newStr string) editQuery {
	q := editQuery{old: oldStr, new: newStr}
	body := oldStr
	if len(body) > 1 && strings.HasSuffix(body, "\n") {
		body = body[:len(body)-1]
		q.trailingNL = true
	}
	for l := range strings.SplitSeq(body, "\n") {
		q.lines = append(q.lines, strings.TrimSuffix(l, "\r"))
	}
	return q
}

// blank reports whether old_string has no visible characters; such a pattern
// would fuzzy-match every blank line, so line-based tiers refuse it.
func (q editQuery) blank() bool {
	for _, l := range q.lines {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}

type editStrategy struct {
	tier int
	find func(m *editMatcher, q editQuery) []editSpan
	// unique marks tiers that never honour replace_all and must find exactly
	// one location: the similarity tiers, and boundary trimming, which drops
	// the padding the model used to narrow the match (" n " must not become
	// every bare n in the file).
	unique bool
}

// editStrategies is the fallback chain after the exact match.
var editStrategies = []editStrategy{
	{tier: editTierExact, find: findExact},
	{tier: editTierLineExact, find: findLineExact},
	{tier: editTierIndentFlexible, find: findIndentFlexible},
	{tier: editTierLineTrim, find: findLineTrimmed},
	{tier: editTierWhitespace, find: findWhitespaceNormalized},
	{tier: editTierUnicode, find: findUnicodeNormalized},
	{tier: editTierEscape, find: findEscapeNormalized},
	{tier: editTierTrimmedBoundary, find: findTrimmedBoundary, unique: true},
	{tier: editTierBlockAnchor, find: findBlockAnchor, unique: true},
	{tier: editTierContextAware, find: findContextAware, unique: true},
}

// strippedMaxTier is the loosest tier a prefix-stripped old_string may match
// at. Copied read output is verbatim apart from its prefixes, so whole-line
// tiers are enough; the inline and similarity tiers would let text that
// really starts with numbers land on some other block.
const strippedMaxTier = editTierLineTrim

// run tries the chain on old_string as written, then, only if nothing
// matched, on old_string with line-number prefixes copied from a read
// stripped. Numbered content (a Markdown list, "1: 'one'" map entries) thus
// keeps every tier. trustLines lets those prefixes pick between identical
// matches; callers set it only when the numbers still describe the content.
func (m *editMatcher) run(oldStr, newStr string, replaceAll, trustLines bool) (editResult, error) {
	q := newEditQuery(oldStr, newStr)
	if res, ok, err := m.runChain(q, replaceAll, 0, editTierContextAware, nil); ok {
		return res, err
	}
	sq, first, ok := stripQueryPrefixes(oldStr, newStr)
	if !ok {
		return editResult{}, m.notFoundError(q)
	}
	hint := 0
	if trustLines {
		hint = first
	}
	notes := []string{"stripped the line-number prefixes old_string was copied with"}
	if res, ok, err := m.runChain(sq, replaceAll, hint, strippedMaxTier, notes); ok {
		return res, err
	}
	return editResult{}, m.notFoundError(q, sq)
}

// runChain runs the tiers up to maxTier; ok reports that one of them found
// old_string, in which case res or err is the outcome.
func (m *editMatcher) runChain(q editQuery, replaceAll bool, hint, maxTier int, notes []string) (res editResult, ok bool, err error) {
	for _, s := range editStrategies {
		if s.tier > maxTier {
			break
		}
		spans := s.find(m, q)
		if len(spans) == 0 {
			continue
		}
		res, err = m.apply(spans, s.tier, replaceAll && !s.unique, hint, notes)
		return res, true, err
	}
	return editResult{}, false, nil
}

// stripQueryPrefixes strips line-number prefixes from old_string (every line
// must carry one) and, in the same form, from whichever new_string lines carry
// one: an edit built from a read deletes or inserts lines, so new_string's
// numbering is rarely complete or consecutive. See stripPrefixesLike for what
// counts as the same form.
func stripQueryPrefixes(oldStr, newStr string) (editQuery, int, bool) {
	stripped, first, sep, ok := stripLineNumberPrefixes(oldStr)
	if !ok {
		return editQuery{}, 0, false
	}
	shape := prefixShapeOf(oldStr, first, sep)
	return newEditQuery(stripped, stripPrefixesLike(newStr, shape)), first, true
}

// apply writes spans (sorted, non-overlapping) into the content. hint is the
// first line number the model's line-number prefixes named, used to pick
// between several otherwise identical matches.
func (m *editMatcher) apply(spans []editSpan, tier int, replaceAll bool, hint int, notes []string) (editResult, error) {
	if len(spans) > 1 && !replaceAll && hint > 0 {
		for _, s := range spans {
			if m.lineNo(m.lineOf(s.start)) == hint {
				spans = []editSpan{s}
				notes = append(notes, fmt.Sprintf("picked the occurrence at line %d named by those prefixes", hint))
				break
			}
		}
	}
	if len(spans) > 1 && !replaceAll {
		return editResult{}, m.ambiguityError(spans, tier)
	}
	var b strings.Builder
	prev := 0
	for _, s := range spans {
		b.WriteString(m.content[prev:s.start])
		b.WriteString(s.repl)
		prev = s.end
	}
	b.WriteString(m.content[prev:])
	if note := editTierNote(tier); note != "" {
		if d := spans[0].detail; d != "" {
			note += " (" + d + ")"
		}
		notes = append(notes, note)
	}
	return editResult{Content: b.String(), Count: len(spans), Tier: tier, Notes: notes}, nil
}

// tierIsUnique reports whether tier never honours replace_all.
func tierIsUnique(tier int) bool {
	for _, s := range editStrategies {
		if s.tier == tier {
			return s.unique
		}
	}
	return false
}

func (m *editMatcher) ambiguityError(spans []editSpan, tier int) error {
	nums := make([]string, 0, len(spans))
	for i, s := range spans {
		if i == 10 {
			nums = append(nums, fmt.Sprintf("and %d more", len(spans)-i))
			break
		}
		nums = append(nums, strconv.Itoa(m.lineNo(m.lineOf(s.start))))
	}
	where := fmt.Sprintf("%d locations (lines %s)", len(spans), strings.Join(nums, ", "))
	if tier == editTierExact {
		return fmt.Errorf("old_string matches %s; add more surrounding context lines to make it unique, or set replace_all to change every occurrence", where)
	}
	if tierIsUnique(tier) {
		// replace_all is ignored at this tier, so suggesting it would only
		// send the model round the same failure again.
		return fmt.Errorf("old_string is not an exact match, and %s matching finds %s; quote the file verbatim and add more surrounding context lines to make it unique; a match this loose only ever changes one location", editTierLabels[tier], where)
	}
	return fmt.Errorf("old_string is not an exact match, and %s matching finds %s; quote the file verbatim and add more surrounding context lines to make it unique, or set replace_all", editTierLabels[tier], where)
}

// literalSpans returns the non-overlapping occurrences of needle, left to
// right (the same ones strings.ReplaceAll would replace).
func (m *editMatcher) literalSpans(needle, repl string) []editSpan {
	if needle == "" {
		return nil
	}
	var spans []editSpan
	for off := 0; ; {
		i := strings.Index(m.content[off:], needle)
		if i < 0 {
			return spans
		}
		s := off + i
		spans = append(spans, editSpan{start: s, end: s + len(needle), repl: repl})
		off = s + len(needle)
	}
}

// lineSpan covers lines [first, first+count) as a byte range. The range stops
// before the last line's terminator unless old_string ended in a newline.
func (m *editMatcher) lineSpan(q editQuery, first, count int, repl string) editSpan {
	last := first + count - 1
	start := m.starts[first]
	end := m.starts[last] + len(m.lines[last])
	if q.trailingNL {
		switch {
		case strings.HasPrefix(m.content[end:], "\r\n"):
			end += 2
		case strings.HasPrefix(m.content[end:], "\n"):
			end++
		default:
			// The block ends the file without a newline; don't add one.
			repl = strings.TrimSuffix(repl, "\n")
		}
	}
	return editSpan{start: start, end: end, repl: repl}
}

// blockSpans returns spans for the non-overlapping windows of len(q.lines)
// lines that same accepts. When reindent is set, each span's replacement is
// new_string re-indented from old_string's indentation to the window's.
func (m *editMatcher) blockSpans(q editQuery, same func(start int) bool, reindent bool) []editSpan {
	k := len(q.lines)
	var spans []editSpan
	for i := 0; i+k <= len(m.lines); i++ {
		if !same(i) {
			continue
		}
		repl := q.new
		if reindent {
			repl = reindentForFile(q.new, q.lines, m.lines[i:i+k], m.fileUnit())
		}
		spans = append(spans, m.lineSpan(q, i, k, repl))
		i += k - 1 // matches never overlap
	}
	return spans
}

// normalizedBlockSpans matches line by line after applying norm to both sides.
func (m *editMatcher) normalizedBlockSpans(q editQuery, norm func(string) string) []editSpan {
	if q.blank() {
		return nil
	}
	want := make([]string, len(q.lines))
	for i, l := range q.lines {
		want[i] = norm(l)
	}
	got := make([]string, len(m.lines))
	for i, l := range m.lines {
		got[i] = norm(l)
	}
	return m.blockSpans(q, func(i int) bool {
		return slices.Equal(got[i:i+len(want)], want)
	}, true)
}

func findExact(m *editMatcher, q editQuery) []editSpan {
	return m.literalSpans(q.old, q.new)
}

// findLineExact matches identical lines, absorbing a trailing newline in
// old_string at the end of the file and "\r\n" vs "\n" in mixed files.
func findLineExact(m *editMatcher, q editQuery) []editSpan {
	if q.blank() {
		return nil
	}
	return m.blockSpans(q, func(i int) bool {
		for j, l := range q.lines {
			if m.lines[i+j] != l {
				return false
			}
		}
		return true
	}, false)
}

// findIndentFlexible matches a block whose lines are identical once each
// side's common leading indentation is removed, so relative indentation must
// still agree (stricter than per-line trimming).
func findIndentFlexible(m *editMatcher, q editQuery) []editSpan {
	if q.blank() {
		return nil
	}
	want := dedentLines(q.lines)
	return m.blockSpans(q, func(i int) bool {
		window := m.lines[i : i+len(q.lines)]
		for j, l := range window {
			if strings.TrimSpace(l) != strings.TrimSpace(q.lines[j]) {
				return false
			}
		}
		return slices.Equal(dedentLines(window), want)
	}, true)
}

func findLineTrimmed(m *editMatcher, q editQuery) []editSpan {
	return m.normalizedBlockSpans(q, strings.TrimSpace)
}

// findWhitespaceNormalized matches whole lines after collapsing every run of
// whitespace to one space. A single-line old_string may also match inside a
// line, with any run of spaces/tabs standing in for its whitespace.
func findWhitespaceNormalized(m *editMatcher, q editQuery) []editSpan {
	if spans := m.normalizedBlockSpans(q, collapseWS); len(spans) > 0 {
		return spans
	}
	if len(q.lines) != 1 {
		return nil
	}
	fields := strings.Fields(q.lines[0])
	if len(fields) < 2 {
		return nil
	}
	for i, f := range fields {
		fields[i] = regexp.QuoteMeta(f)
	}
	re, err := regexp.Compile(strings.Join(fields, `[ \t]+`))
	if err != nil {
		return nil
	}
	repl := trimBoundaryLike(q.old, q.new)
	var spans []editSpan
	for _, loc := range re.FindAllStringIndex(m.content, -1) {
		if m.atWordBoundaries(loc[0], loc[1]) {
			spans = append(spans, editSpan{start: loc[0], end: loc[1], repl: repl})
		}
	}
	return spans
}

// atWordBoundaries reports whether content[start:end] neither starts nor ends
// in the middle of a word, so an inline match of "x := 1" can't land inside
// "max := 1". Bytes of multi-byte runes count as word characters.
func (m *editMatcher) atWordBoundaries(start, end int) bool {
	c := m.content
	// A fragment right after a backslash starts inside an escape sequence:
	// the n of "\n" is not a token.
	if start > 0 && c[start-1] == '\\' {
		return false
	}
	if start > 0 && start < len(c) && isWordByte(c[start-1]) && isWordByte(c[start]) {
		return false
	}
	return !(end > 0 && end < len(c) && isWordByte(c[end-1]) && isWordByte(c[end]))
}

func isWordByte(b byte) bool {
	return b == '_' || b >= 0x80 || ('0' <= b && b <= '9') || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// findUnicodeNormalized matches after mapping typographic quotes, dashes and
// unusual spaces to ASCII on both sides: first as a substring, then line by
// line with whitespace normalization on top.
func findUnicodeNormalized(m *editMatcher, q editQuery) []editSpan {
	if !hasTypographic(m.content) && !hasTypographic(q.old) {
		return nil
	}
	normContent, offs := normalizeTypographicMap(m.content)
	needle := normalizeTypographic(q.old)
	// A position inside a one-to-many expansion ("…" read as "...") maps back
	// to the start of the character; a match beginning or ending there would
	// cut the character off and leave it behind, so it is skipped.
	inside := func(p int) bool { return p > 0 && offs[p] == offs[p-1] }
	var spans []editSpan
	for off := 0; needle != ""; {
		i := strings.Index(normContent[off:], needle)
		if i < 0 {
			break
		}
		s := off + i
		e := s + len(needle)
		if inside(s) || inside(e) {
			off = s + 1
			continue
		}
		spans = append(spans, editSpan{start: offs[s], end: offs[e], repl: q.new})
		off = e
	}
	if len(spans) == 0 {
		spans = m.normalizedBlockSpans(q, func(l string) string { return collapseWS(normalizeTypographic(l)) })
	}
	// Where old_string had a smart quote the file spells in ASCII, new_string
	// almost certainly carries the same wrong character; other typographic
	// characters in new_string (an intended em-dash) are kept.
	for i, s := range spans {
		spans[i].repl = asciiLikeFile(s.repl, q.old, m.content[s.start:s.end])
	}
	return spans
}

// asciiLikeFile maps to ASCII each typographic character of repl that old
// used but the matched region of the file does not contain.
func asciiLikeFile(repl, old, region string) string {
	var b strings.Builder
	for _, r := range repl {
		if a, ok := typographicASCII[r]; ok && strings.ContainsRune(old, r) && !strings.ContainsRune(region, r) {
			b.WriteString(a)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// findEscapeNormalized handles a double-escaped old_string (literal `\n`
// where the file has a line break, `\"` for `"`): it unescapes old_string and
// retries the precise tiers. new_string is unescaped only when it looks
// escaped the same way, and then only for the escapes old_string used, so a
// real "\n" inside a string literal the model is adding survives.
func findEscapeNormalized(m *editMatcher, q editQuery) []editSpan {
	u := unescapeEditString(q.old)
	if u == q.old || u == "" {
		return nil
	}
	newStr, detail := q.new, "new_string kept as written"
	// An old_string whose line breaks were all escaped came from a writer
	// that escapes; a new_string with real line breaks did not.
	if strings.Contains(q.old, "\n") || !strings.Contains(q.new, "\n") {
		// A writer that escapes anything escapes backslashes too.
		if un := unescapeEditStringKinds(q.new, editEscapesUsed(q.old)+`\`); un != q.new {
			newStr, detail = un, "new_string unescaped the same way"
		}
	}
	uq := newEditQuery(u, newStr)
	for _, f := range []func(*editMatcher, editQuery) []editSpan{findExact, findLineExact, findIndentFlexible, findLineTrimmed} {
		if spans := f(m, uq); len(spans) > 0 {
			for i := range spans {
				spans[i].detail = detail
			}
			return spans
		}
	}
	return nil
}

// findTrimmedBoundary retries with old_string's leading and trailing
// whitespace (blank lines included) removed, e.g. a fragment in mid-line.
// The fragment must not start or end inside a word.
func findTrimmedBoundary(m *editMatcher, q editQuery) []editSpan {
	t := strings.TrimSpace(q.old)
	if t == q.old || t == "" {
		return nil
	}
	spans := m.literalSpans(t, trimBoundaryLike(q.old, q.new))
	return slices.DeleteFunc(spans, func(s editSpan) bool { return !m.atWordBoundaries(s.start, s.end) })
}

// blockAnchorMinSimilarity is how similar the lines between the anchors must
// be. It is stricter than OpenCode's 0.65: a looser block match rewrites code
// the model never quoted correctly.
const blockAnchorMinSimilarity = 0.7

// anchorsUsable reports whether old_string's first and last lines are
// distinctive enough to anchor a block (not blank, not both a lone brace).
func anchorsUsable(q editQuery) (first, last string, ok bool) {
	k := len(q.lines)
	if k < 3 {
		return "", "", false
	}
	first, last = strings.TrimSpace(q.lines[0]), strings.TrimSpace(q.lines[k-1])
	if first == "" || last == "" || (len(first) < 4 && len(last) < 4) {
		return "", "", false
	}
	return first, last, true
}

// anchorIndentAgrees reports whether a candidate block's last anchor sits at
// the same depth relative to its first anchor as old_string's does, counted
// in levels of each side's own indentation unit. Anchors are compared
// trimmed, so without this a lone "}" would match the first nested closing
// brace and the edit would end the block early.
func anchorIndentAgrees(q editQuery, qUnit, fUnit indentUnit, fFirst, fLast string) bool {
	qc, fc := qUnit.cols(), fUnit.cols()
	dq := indentCols(leadingWS(q.lines[len(q.lines)-1]), qc) - indentCols(leadingWS(q.lines[0]), qc)
	df := indentCols(leadingWS(fLast), fc) - indentCols(leadingWS(fFirst), fc)
	return dq*fc == df*qc
}

// findBlockAnchor matches a block (3+ lines) by its first and last lines,
// letting its size differ from old_string's by up to a quarter, and scores
// the lines between by similarity. Every block above the threshold counts,
// so two plausible blocks are reported as ambiguous.
//
// Common anchors ("if err != nil {" ... "}") can occur thousands of times,
// so each candidate is first scored with a cheap upper bound and only the
// ones that could pass are aligned for real, one alignment covering every
// candidate size. The work is capped by blockAnchorBudget; a file too big
// to finish within it skips this tier rather than stall the edit.
func findBlockAnchor(m *editMatcher, q editQuery) []editSpan {
	first, last, ok := anchorsUsable(q)
	if !ok {
		return nil
	}
	k := len(q.lines)
	tol := max(1, k/4)
	qUnit, fUnit := detectIndentUnit(q.lines), m.fileUnit()
	sc := newLineScorer(m, q.lines[1:k-1], blockAnchorBudget)
	var spans []editSpan
	var sims []float64
	for i := 0; i < len(m.lines); i++ {
		if strings.TrimSpace(m.lines[i]) != first {
			continue
		}
		if sc.budget < 0 {
			return nil
		}
		var sizes []int
		for size := max(3, k-tol); size <= k+tol && i+size-1 < len(m.lines); size++ {
			if strings.TrimSpace(m.lines[i+size-1]) == last && anchorIndentAgrees(q, qUnit, fUnit, m.lines[i], m.lines[i+size-1]) {
				sizes = append(sizes, size)
			}
		}
		if len(sizes) == 0 {
			continue
		}
		nb := sizes[len(sizes)-1] - 2
		// Score of old_string's middle against the first b lines after the
		// anchor, for every b; a size's middle is its first size-2 lines.
		best := func(scores []float64) (size int, sim float64) {
			sim = -1
			for _, s := range sizes {
				n := max(len(sc.want), s-2)
				v := 1.0
				if n > 0 {
					v = scores[s-2] / float64(n)
				}
				if v > sim {
					size, sim = s, v
				}
			}
			return size, sim
		}
		if _, ub := best(alignPrefixes(len(sc.want), nb, tol+1, func(a, b int) float64 {
			return sc.bound(a, i+1+b)
		})); ub < blockAnchorMinSimilarity {
			continue
		}
		bestSize, bestSim := best(alignPrefixes(len(sc.want), nb, tol+1, func(a, b int) float64 {
			return sc.similarity(a, i+1+b)
		}))
		if sc.budget < 0 {
			return nil
		}
		if bestSim < blockAnchorMinSimilarity {
			continue
		}
		j := i + bestSize - 1
		repl := reindentForFile(q.new, []string{q.lines[0], q.lines[k-1]}, []string{m.lines[i], m.lines[j]}, m.fileUnit())
		span := m.lineSpan(q, i, bestSize, repl)
		span.detail = fmt.Sprintf("lines %d-%d, %d%% similar", m.lineNo(i), m.lineNo(j), int(bestSim*100))
		// Overlapping candidates are one block with different boundaries
		// (an earlier anchor whose window swallows it); keep the closer fit.
		if n := len(spans); n > 0 && span.start < spans[n-1].end {
			if bestSim > sims[n-1] {
				spans[n-1], sims[n-1] = span, bestSim
			}
			continue
		}
		spans = append(spans, span)
		sims = append(sims, bestSim)
	}
	return spans
}

// blockAnchorBudget bounds findBlockAnchor's work, counted in Levenshtein
// cells plus rune-bag comparisons; roughly 150ms. A var so tests can lower it.
var blockAnchorBudget = 100_000_000

// scoredLine is a line prepared for similarity scoring: trimmed and clipped
// like lineSimilarity, with a sorted bag of its runes for the cheap bound.
type scoredLine struct {
	runes []rune
	bag   []runeCount
}

type runeCount struct {
	r rune
	n int
}

func newScoredLine(l string) scoredLine {
	rs := []rune(strings.TrimSpace(l))
	rs = rs[:min(len(rs), 200)]
	sorted := slices.Clone(rs)
	slices.Sort(sorted)
	var bag []runeCount
	for _, r := range sorted {
		if n := len(bag); n > 0 && bag[n-1].r == r {
			bag[n-1].n++
		} else {
			bag = append(bag, runeCount{r, 1})
		}
	}
	return scoredLine{runes: rs, bag: bag}
}

// lineScorer scores old_string's lines (want) against the file's lines. The
// windows of nearby anchors overlap, so every score is cached by (want line,
// file line); the work is charged to a shared budget.
type lineScorer struct {
	m      *editMatcher
	want   []scoredLine
	budget int
	lines  []*scoredLine
	// bounds and sims are indexed by file line, then want line; NaN means
	// not computed yet.
	bounds, sims [][]float32
	prev, cur    []int
}

func newLineScorer(m *editMatcher, want []string, budget int) *lineScorer {
	s := &lineScorer{m: m, budget: budget, want: make([]scoredLine, len(want)),
		lines: make([]*scoredLine, len(m.lines)), bounds: make([][]float32, len(m.lines)), sims: make([][]float32, len(m.lines))}
	for i, l := range want {
		s.want[i] = newScoredLine(l)
	}
	return s
}

func (s *lineScorer) line(i int) scoredLine {
	if s.lines[i] == nil {
		sl := newScoredLine(s.m.lines[i])
		s.lines[i] = &sl
	}
	return *s.lines[i]
}

// cached returns table's score for (want line a, file line i), computing it
// with f on a miss.
func (s *lineScorer) cached(table [][]float32, a, i int, f func(x, y scoredLine) float64) float64 {
	row := table[i]
	if row == nil {
		row = make([]float32, len(s.want))
		for j := range row {
			row[j] = float32(math.NaN())
		}
		table[i] = row
	}
	if v := row[a]; !math.IsNaN(float64(v)) {
		return float64(v)
	}
	v := f(s.want[a], s.line(i))
	row[a] = float32(v)
	return v
}

// bound is an upper bound on similarity(a, i): an edit script needs at least
// as many edits as runes one line has that the other lacks.
func (s *lineScorer) bound(a, i int) float64 {
	return s.cached(s.bounds, a, i, func(x, y scoredLine) float64 {
		n := max(len(x.runes), len(y.runes))
		if n == 0 {
			return 1
		}
		s.budget -= len(x.bag) + len(y.bag)
		onlyX, onlyY := 0, 0
		p, q := 0, 0
		for p < len(x.bag) || q < len(y.bag) {
			switch {
			case q == len(y.bag) || (p < len(x.bag) && x.bag[p].r < y.bag[q].r):
				onlyX += x.bag[p].n
				p++
			case p == len(x.bag) || y.bag[q].r < x.bag[p].r:
				onlyY += y.bag[q].n
				q++
			default:
				if d := x.bag[p].n - y.bag[q].n; d > 0 {
					onlyX += d
				} else {
					onlyY -= d
				}
				p++
				q++
			}
		}
		return 1 - float64(max(onlyX, onlyY))/float64(n)
	})
}

// similarity is lineSimilarity of want line a and file line i. Once the
// budget runs out it returns 0, so callers must check the budget before
// trusting a result.
func (s *lineScorer) similarity(a, i int) float64 {
	return s.cached(s.sims, a, i, func(x, y scoredLine) float64 {
		ra, rb := x.runes, y.runes
		if slices.Equal(ra, rb) {
			return 1
		}
		if s.budget -= len(ra) * len(rb); s.budget < 0 {
			return 0
		}
		n := max(len(ra), len(rb))
		if cap(s.prev) < len(rb)+1 {
			s.prev, s.cur = make([]int, len(rb)+1), make([]int, len(rb)+1)
		}
		prev, cur := s.prev[:len(rb)+1], s.cur[:len(rb)+1]
		for j := range prev {
			prev[j] = j
		}
		for p := 1; p <= len(ra); p++ {
			cur[0] = p
			for q := 1; q <= len(rb); q++ {
				cost := 1
				if ra[p-1] == rb[q-1] {
					cost = 0
				}
				cur[q] = min(prev[q]+1, cur[q-1]+1, prev[q-1]+cost)
			}
			prev, cur = cur, prev
		}
		return 1 - float64(prev[len(rb)])/float64(n)
	})
}

// alignPrefixes aligns na lines against nb lines (monotonic, each line used
// at most once, pairs scored by score) and returns, for every b in 0..nb, the
// best total for all na lines against the first b. Only pairs within band of
// the diagonal are scored; the others count as unrelated.
func alignPrefixes(na, nb, band int, score func(a, b int) float64) []float64 {
	prev := make([]float64, nb+1)
	cur := make([]float64, nb+1)
	for a := 1; a <= na; a++ {
		cur[0] = 0
		for b := 1; b <= nb; b++ {
			v := max(prev[b], cur[b-1])
			if d := a - b; d <= band && d >= -band {
				v = max(v, prev[b-1]+score(a-1, b-1))
			}
			cur[b] = v
		}
		prev, cur = cur, prev
	}
	return prev
}

// findContextAware matches a same-sized block whose first and last lines
// agree (at the quote's relative indentation) and at least half of whose
// non-blank middle lines are equal after trimming, for edits where a few lines
// are badly misquoted. Such a block is only trusted when no other block with
// the same anchors resembles old_string more closely: otherwise the model
// most likely meant that one, and both are reported as ambiguous rather than
// rewriting the wrong one.
func findContextAware(m *editMatcher, q editQuery) []editSpan {
	first, last, ok := anchorsUsable(q)
	if !ok {
		return nil
	}
	k := len(q.lines)
	qUnit, fUnit := detectIndentUnit(q.lines), m.fileUnit()
	sc := newLineScorer(m, q.lines[1:k-1], blockAnchorBudget)
	type candidate struct {
		span     editSpan
		accepted bool
		sim      float64
	}
	var cands []candidate
	best := -1
	for i := 0; i+k <= len(m.lines); i++ {
		if strings.TrimSpace(m.lines[i]) != first || strings.TrimSpace(m.lines[i+k-1]) != last ||
			!anchorIndentAgrees(q, qUnit, fUnit, m.lines[i], m.lines[i+k-1]) {
			continue
		}
		total, equal := 0, 0
		simSum := 0.0
		for j := 1; j < k-1; j++ {
			want := strings.TrimSpace(q.lines[j])
			if want == "" {
				continue
			}
			total++
			if strings.TrimSpace(m.lines[i+j]) == want {
				equal++
				simSum++
			} else {
				simSum += sc.similarity(j-1, i+j)
			}
		}
		if sc.budget < 0 {
			return nil
		}
		c := candidate{sim: 1, accepted: total > 0 && equal*2 >= total}
		if total > 0 {
			c.sim = simSum / float64(total)
		}
		repl := reindentForFile(q.new, q.lines, m.lines[i:i+k], m.fileUnit())
		c.span = m.lineSpan(q, i, k, repl)
		c.span.detail = fmt.Sprintf("lines %d-%d, %d of %d inner lines equal", m.lineNo(i), m.lineNo(i+k-1), equal, total)
		cands = append(cands, c)
		if best < 0 || c.sim > cands[best].sim {
			best = len(cands) - 1
		}
	}
	var spans []editSpan
	for _, c := range cands {
		if c.accepted {
			spans = append(spans, c.span)
		}
	}
	if len(spans) > 0 && !cands[best].accepted {
		// A closer block fails the equal-lines rule only because every
		// line of it was misquoted a little: report both.
		spans = append(spans, cands[best].span)
		slices.SortFunc(spans, func(a, b editSpan) int { return a.start - b.start })
	}
	return spans
}

// notFoundError explains a miss and shows the most similar block of the file
// with line numbers, so the model can fix old_string without another read.
// With several forms of old_string (as written, prefixes stripped), the
// closest block of any of them is shown.
func (m *editMatcher) notFoundError(qs ...editQuery) error {
	var start, count int
	var sim float64
	ok := false
	for _, q := range qs {
		if s, c, sm, found := m.closestBlock(q); found && (!ok || sm > sim) {
			start, count, sim, ok = s, c, sm, true
		}
	}
	if !ok {
		return errors.New("old_string not found in the file and nothing similar exists; file-read it again and copy old_string verbatim")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "old_string not found in the file. Closest match is lines %d-%d (%d%% similar):\n",
		m.lineNo(start), m.lineNo(start+count-1), int(sim*100))
	for i := start; i < start+count; i++ {
		fmt.Fprintf(&b, "%d. %s\n", m.lineNo(i), clipRunes(m.lines[i], 200))
	}
	b.WriteString("Copy old_string verbatim from the current file (without these line-number prefixes); if the file may have changed, file-read it again.")
	return errors.New(b.String())
}

// Bounds that keep the closest-match search cheap on big files.
const (
	closestMinSimilarity = 0.4
	// closestAnchorVote is the vote a single line must earn (one distinctive
	// equal line, or a line this similar) to justify showing its region.
	closestAnchorVote = 0.6
	closestMaxLines   = 30
	closestScanLines  = 5000
	closestProbeRunes = 80
)

// closestBlock finds the window of the file most similar to old_string. It
// proposes start lines cheaply, then scores the best few with a line
// alignment. Proposals come from file lines equal to one of old_string's after
// trimming (each votes for where the block would start; short lines like "}"
// vote weakly), or, when no line is equal, from the lines most similar to up
// to three of old_string's longest lines. A window is reported when it is
// similar enough as a whole or holds one strong anchor line, since pointing
// the model at the right region is what matters.
func (m *editMatcher) closestBlock(q editQuery) (start, count int, sim float64, ok bool) {
	if q.blank() {
		return 0, 0, 0, false
	}
	k := len(q.lines)
	want := map[string][]int{}
	var probes []int
	for j, l := range q.lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if _, seen := want[t]; !seen {
			probes = append(probes, j)
		}
		want[t] = append(want[t], j)
	}
	votes := map[int]float64{}
	for i, l := range m.lines {
		t := strings.TrimSpace(l)
		weight := 1.0
		if len(t) < 6 {
			weight = 0.25
		}
		for _, j := range want[t] {
			votes[i-j] += weight
		}
	}
	if len(votes) == 0 {
		sort.SliceStable(probes, func(a, b int) bool {
			return len(strings.TrimSpace(q.lines[probes[a]])) > len(strings.TrimSpace(q.lines[probes[b]]))
		})
		for _, j := range probes[:min(3, len(probes))] {
			m.voteSimilarLines(votes, strings.TrimSpace(q.lines[j]), j)
		}
	}
	type cand struct {
		start int
		score float64
	}
	cands := make([]cand, 0, len(votes))
	for s, v := range votes {
		cands = append(cands, cand{max(0, s), v})
	}
	sort.Slice(cands, func(a, b int) bool {
		if cands[a].score != cands[b].score {
			return cands[a].score > cands[b].score
		}
		return cands[a].start < cands[b].start
	})
	best, bestVote := -1.0, 0.0
	for i, c := range cands {
		if i == 5 {
			break
		}
		end := min(len(m.lines), c.start+k)
		if s := blockSimilarity(q.lines, m.lines[c.start:end]); s > best {
			best, bestVote, start, count = s, c.score, c.start, end-c.start
		}
	}
	if count == 0 || (best < closestMinSimilarity && bestVote < closestAnchorVote) {
		return 0, 0, 0, false
	}
	return start, min(count, closestMaxLines), best, true
}

// voteSimilarLines votes for block starts whose line j would be a line of the
// file similar to probe. Both sides are clipped, and lines whose length alone
// rules out a close match are skipped, so this stays cheap on large files.
func (m *editMatcher) voteSimilarLines(votes map[int]float64, probe string, j int) {
	probe = clipRunes(probe, closestProbeRunes)
	pn := utf8.RuneCountInString(probe)
	for i, l := range m.lines {
		if i >= closestScanLines {
			return
		}
		t := clipRunes(strings.TrimSpace(l), closestProbeRunes)
		tn := utf8.RuneCountInString(t)
		if tn == 0 || float64(min(pn, tn)) < closestAnchorVote*float64(max(pn, tn)) {
			continue
		}
		if s := lineSimilarity(probe, t); s >= closestAnchorVote && s > votes[i-j] {
			votes[i-j] = s
		}
	}
}

// blockSimilarity scores how alike two runs of lines are (0..1): the best
// alignment of their trimmed lines, summed by per-line similarity and divided
// by the longer run, so inserted or dropped lines cost only themselves.
func blockSimilarity(a, b []string) float64 {
	n := max(len(a), len(b))
	if n == 0 {
		return 1
	}
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	ta := make([]string, len(a))
	for i, l := range a {
		ta[i] = strings.TrimSpace(l)
	}
	tb := make([]string, len(b))
	for i, l := range b {
		tb[i] = strings.TrimSpace(l)
	}
	var sum float64
	if len(a)*len(b) > 2500 {
		// Too big to align; compare index by index.
		for i := range min(len(a), len(b)) {
			sum += lineSimilarity(ta[i], tb[i])
		}
		return sum / float64(n)
	}
	prev := make([]float64, len(b)+1)
	cur := make([]float64, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cur[j] = max(prev[j], cur[j-1], prev[j-1]+lineSimilarity(ta[i-1], tb[j-1]))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)] / float64(n)
}

// lineSimilarity is 1 - normalized Levenshtein distance over runes, with both
// sides clipped to 200 runes to bound the cost.
func lineSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	ra, rb = ra[:min(len(ra), 200)], rb[:min(len(rb), 200)]
	n := max(len(ra), len(rb))
	if n == 0 {
		return 1
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return 1 - float64(prev[len(rb)])/float64(n)
}

// lineNumberPrefixRE matches a line-number prefix the model may copy from a
// read: file-read's "N. ", "N: ", and the padded "   N\t" (cat -n) and
// "   N→". Only those last two may be indented, since an indented "1: " or
// "1. " is far more likely to be the file's own text (a map entry, a nested
// list). A bare "N." or "N:" is accepted only on an otherwise empty line.
// Group 1 or 3 is the number, group 2 or 4 the separator.
var lineNumberPrefixRE = regexp.MustCompile(`^(?: *(\d+)(\t|\x{2192})|(\d+)(\. |: |\.$|:$))`)

// matchLineNumberPrefix returns the number and separator ("\t", "→", "." or
// ":") of l's line-number prefix and the offset where the text starts.
func matchLineNumberPrefix(l string) (n int, sep string, end int, ok bool) {
	loc := lineNumberPrefixRE.FindStringSubmatchIndex(l)
	if loc == nil {
		return 0, "", 0, false
	}
	num, sepLoc := loc[2:4], loc[4:6]
	if num[0] < 0 {
		num, sepLoc = loc[6:8], loc[8:10]
	}
	n, err := strconv.Atoi(l[num[0]:num[1]])
	if err != nil {
		return 0, "", 0, false
	}
	return n, strings.TrimSpace(l[sepLoc[0]:sepLoc[1]]), loc[1], true
}

// stripLineNumberPrefixes removes line-number prefixes when every line of s
// carries one with the same separator and the numbers are consecutive;
// otherwise it returns s unchanged and ok=false. first is the number of the
// first line.
func stripLineNumberPrefixes(s string) (stripped string, first int, sep string, ok bool) {
	body, trail := s, ""
	if strings.HasSuffix(body, "\n") {
		body, trail = body[:len(body)-1], "\n"
	}
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		l = strings.TrimSuffix(l, "\r")
		n, lsep, end, ok := matchLineNumberPrefix(l)
		if !ok {
			return s, 0, "", false
		}
		if i == 0 {
			first, sep = n, lsep
		} else if n != first+i || lsep != sep {
			return s, 0, "", false
		}
		lines[i] = l[end:]
	}
	return strings.Join(lines, "\n") + trail, first, sep, true
}

// stripPrefixesWithSep removes a line-number prefix using separator sep from
// every line of s that has one, leaving the other lines as written.
// prefixShape describes the line-number prefixes old_string was copied with:
// the separator, the numbers' range, and whether they were padded (file-read's
// right-aligned "     7\t") and to which width.
type prefixShape struct {
	sep         string
	first, last int
	padded      bool
	numEnd      int // offset where a padded number ends
}

func prefixShapeOf(oldStr string, first int, sep string) prefixShape {
	sh := prefixShape{sep: sep, first: first, last: first}
	for i, l := range strings.Split(strings.TrimSuffix(oldStr, "\n"), "\n") {
		sh.last = first + i
		if i == 0 {
			sh.numEnd = prefixNumEnd(strings.TrimSuffix(l, "\r"))
			sh.padded = strings.HasPrefix(l, " ")
		}
	}
	return sh
}

// prefixNumEnd is the offset where l's line-number prefix's number ends.
func prefixNumEnd(l string) int {
	return len(l) - len(strings.TrimLeft(strings.TrimLeft(l, " "), "0123456789"))
}

// stripPrefixesLike strips from new_string the line-number prefixes that have
// old_string's form: the same separator and padding, and a number in or just
// past old_string's range (lines copied from the read keep their numbers; a
// few new ones may be numbered on). Anything else — "1. Install" in a
// Markdown list, "1\tAlice" in a TSV — is content the model wrote.
func stripPrefixesLike(s string, sh prefixShape) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		t := strings.TrimSuffix(l, "\r")
		n, lsep, end, ok := matchLineNumberPrefix(t)
		if !ok || lsep != sh.sep || n < sh.first || n > sh.last+len(lines) {
			continue
		}
		if padded := strings.HasPrefix(t, " "); padded != sh.padded || (padded && prefixNumEnd(t) != sh.numEnd) {
			continue
		}
		lines[i] = l[end:]
	}
	return strings.Join(lines, "\n")
}

// collapseWS trims a line and collapses every internal whitespace run to a
// single space.
func collapseWS(line string) string {
	return strings.Join(strings.Fields(line), " ")
}

func leadingWS(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// dedentLines strips the longest leading-whitespace prefix shared by the
// non-blank lines; blank lines become "".
func dedentLines(lines []string) []string {
	prefix, set := "", false
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		lead := leadingWS(l)
		if !set {
			prefix, set = lead, true
			continue
		}
		n := 0
		for n < len(prefix) && n < len(lead) && prefix[n] == lead[n] {
			n++
		}
		prefix = prefix[:n]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out[i] = l[len(prefix):]
	}
	return out
}

// trimBoundaryLike removes from newStr the leading/trailing whitespace that
// oldStr had, for tiers whose match excludes that whitespace.
func trimBoundaryLike(oldStr, newStr string) string {
	lead := oldStr[:len(oldStr)-len(strings.TrimLeftFunc(oldStr, unicode.IsSpace))]
	trail := oldStr[len(strings.TrimRightFunc(oldStr, unicode.IsSpace)):]
	if lead != "" {
		if strings.HasPrefix(newStr, lead) {
			newStr = newStr[len(lead):]
		} else if !strings.Contains(lead, "\n") {
			newStr = strings.TrimLeft(newStr, " \t")
		}
	}
	if trail != "" {
		if strings.HasSuffix(newStr, trail) {
			newStr = newStr[:len(newStr)-len(trail)]
		} else if !strings.Contains(trail, "\n") {
			newStr = strings.TrimRight(newStr, " \t")
		}
	}
	return newStr
}

// typographicASCII maps typographic characters models often emit (or files
// contain) to their ASCII stand-ins.
var typographicASCII = map[rune]string{
	'\u2018': "'", '\u2019': "'", '\u201A': "'", '\u201B': "'", '\u2032': "'",
	'\u201C': `"`, '\u201D': `"`, '\u201E': `"`, '\u201F': `"`, '\u2033': `"`,
	'\u2010': "-", '\u2011': "-", '\u2012': "-", '\u2013': "-", '\u2014': "-", '\u2015': "-", '\u2212': "-",
	'\u00A0': " ", '\u2007': " ", '\u2009': " ", '\u200A': " ", '\u202F': " ",
	'\u2026': "...",
}

func hasTypographic(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		_, ok := typographicASCII[r]
		return ok
	})
}

func normalizeTypographic(s string) string {
	out, _ := normalizeTypographicMap(s)
	return out
}

// normalizeTypographicMap returns s with typographic characters mapped to
// ASCII, plus offs mapping each byte offset of the result (and its length)
// back to an offset in s.
func normalizeTypographicMap(s string) (string, []int) {
	var b strings.Builder
	offs := make([]int, 0, len(s)+1)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		rep, ok := typographicASCII[r]
		if !ok {
			rep = s[i : i+size]
		}
		b.WriteString(rep)
		for range len(rep) {
			offs = append(offs, i)
		}
		i += size
	}
	offs = append(offs, len(s))
	return b.String(), offs
}

// editEscapeKinds are the characters after a backslash that
// unescapeEditString undoes.
const editEscapeKinds = "ntr'\"`\\$"

// unescapeEditString undoes one level of backslash escaping: \n, \t, \r,
// \', \", \`, \\ and \$. Other backslashes are kept as written.
func unescapeEditString(s string) string {
	return unescapeEditStringKinds(s, editEscapeKinds)
}

// editEscapesUsed returns the escape kinds (see editEscapeKinds) present in s.
func editEscapesUsed(s string) string {
	var kinds []byte
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		if n := s[i+1]; strings.IndexByte(editEscapeKinds, n) >= 0 && !slices.Contains(kinds, n) {
			kinds = append(kinds, n)
		}
		i++
	}
	return string(kinds)
}

// unescapeEditStringKinds is unescapeEditString limited to the escapes in
// kinds; any other backslash pair is copied through untouched.
func unescapeEditStringKinds(s, kinds string) string {
	if !strings.Contains(s, `\`) || kinds == "" {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 == len(s) {
			b.WriteByte(c)
			continue
		}
		if strings.IndexByte(kinds, s[i+1]) < 0 {
			b.WriteByte(c)
			if strings.IndexByte(editEscapeKinds, s[i+1]) >= 0 {
				// An escape old_string never used: keep the pair whole, so
				// "\\n" doesn't turn into a backslash plus a line break.
				b.WriteByte(s[i+1])
				i++
			}
			continue
		}
		switch n := s[i+1]; n {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '\'', '"', '`', '\\', '$':
			b.WriteByte(n)
		default:
			b.WriteByte(c)
			continue
		}
		i++
	}
	return b.String()
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "\u2026"
}
