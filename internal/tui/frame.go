package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// framePart is a rendered piece of the frame split into rows, each row's
// width in cells measured once.
type framePart struct {
	rows   []string
	widths []int
	maxW   int
}

// newFramePart splits s into rows as lipgloss's joins do (tabs become four
// spaces, CRLF becomes LF) and measures them.
func newFramePart(s string) framePart {
	if strings.Contains(s, "\t") {
		s = strings.ReplaceAll(s, "\t", "    ")
	}
	if strings.Contains(s, "\r\n") {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	rows := strings.Split(s, "\n")
	p := framePart{rows: rows, widths: make([]int, len(rows))}
	for i, r := range rows {
		p.widths[i] = ansi.StringWidth(r)
		p.maxW = max(p.maxW, p.widths[i])
	}
	return p
}

// fixedWidthPart is a part whose rows are all exactly w cells wide, such as
// the transcript viewport's, so nothing needs measuring.
func fixedWidthPart(s string, w int) framePart {
	if s == "" || w <= 0 {
		return newFramePart(s)
	}
	rows := strings.Split(s, "\n")
	widths := make([]int, len(rows))
	for i := range widths {
		widths[i] = w
	}
	return framePart{rows: rows, widths: widths, maxW: w}
}

// composeFrame lays the frame out exactly as
//
//	main := lipgloss.JoinVertical(lipgloss.Left, mainParts...)
//	body := lipgloss.JoinHorizontal(lipgloss.Top, main, " ", side) // with a side panel
//	lipgloss.JoinVertical(lipgloss.Left, header, body)
//
// did, byte for byte (TestComposeFrameMatchesLipgloss), from parts whose
// rows were measured once, most of them in an earlier frame (see
// frameMemo). The lipgloss joins measured every row of every part on every
// frame, three times over with the side panel open: about half of a
// frame's cost while a reply streams.
func composeFrame(header framePart, mainParts []framePart, side *framePart) string {
	mainW := 0
	mainRows := 0
	for _, p := range mainParts {
		mainW = max(mainW, p.maxW)
		mainRows += len(p.rows)
	}
	bodyW, bodyRows := mainW, mainRows
	if side != nil {
		bodyW = mainW + 1 + side.maxW
		bodyRows = max(mainRows, len(side.rows))
	}
	frameW := max(header.maxW, bodyW)

	var b strings.Builder
	b.Grow((len(header.rows) + bodyRows) * (frameW + 24))
	for i, row := range header.rows {
		b.WriteString(row)
		writeSpaces(&b, frameW-header.widths[i])
		b.WriteByte('\n')
	}
	// mainRow returns main pane row r and its width ("" past the end).
	part, offset := 0, 0
	mainRow := func(r int) (string, int) {
		for part < len(mainParts) && r-offset >= len(mainParts[part].rows) {
			offset += len(mainParts[part].rows)
			part++
		}
		if part >= len(mainParts) {
			return "", 0
		}
		return mainParts[part].rows[r-offset], mainParts[part].widths[r-offset]
	}
	for r := 0; r < bodyRows; r++ {
		row, w := mainRow(r)
		b.WriteString(row)
		writeSpaces(&b, mainW-w)
		if side != nil {
			b.WriteByte(' ')
			sw := 0
			if r < len(side.rows) {
				b.WriteString(side.rows[r])
				sw = side.widths[r]
			}
			writeSpaces(&b, side.maxW-sw)
		}
		writeSpaces(&b, frameW-bodyW)
		if r < bodyRows-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// spaces is a run of blanks writeSpaces slices from.
var spaces = strings.Repeat(" ", 256)

func writeSpaces(b *strings.Builder, n int) {
	for n > 0 {
		k := min(n, len(spaces))
		b.WriteString(spaces[:k])
		n -= k
	}
}

// frameMemo keeps the frame's chrome (header, input area, status bar, side
// panel) from one frame to the next.
//
//   - What: each part rendered and split into measured rows (framePart).
//   - Key: Model.chromeSeq, which Update advances for every message except
//     the ones that only touch the transcript (streamed text, see
//     transcriptOnly), plus the sizes the part was drawn at and the few
//     fields a caller outside Update commonly changes (mode, banner, input
//     text); see chromeKey.
//   - Invalidation: any key difference re-renders the part.
//   - Owner: the Bubble Tea event loop goroutine, which runs both Update and
//     View. Update creates the memo; a Model that never went through Update
//     (a test calling View directly) has none and renders every part.
//
// While a reply streams, the chrome does not change between tokens, and
// re-rendering it was most of the per-token cost left after the transcript
// cache: 0.55 ms for the side panel, 0.2 ms for the input box twice (layout
// and view), plus their allocations (BenchmarkComponents in the perf
// harness).
type frameMemo struct {
	header, input, status, side memoPart
}

// memoPart is one memoized chrome part.
type memoPart struct {
	valid bool
	key   chromeKey
	text  string
	part  framePart
}

// chromeKey identifies the state a chrome part was rendered in.
type chromeKey struct {
	seq           uint64
	width, height int
	partWidth     int
	eyeFrame      int
	mode          string
	banner        string
	input         string
}

func (m Model) chromeKey(partWidth int) chromeKey {
	return chromeKey{
		seq:       m.chromeSeq,
		width:     m.width,
		height:    m.height,
		partWidth: partWidth,
		eyeFrame:  m.eyeFrame,
		mode:      m.mode,
		banner:    m.banner,
		input:     m.ta.Value(),
	}
}

// memoized returns the part cached in slot for key, rendering it with
// render when the slot holds another key. With no memo (slot nil) it just
// renders.
func memoized(slot *memoPart, key chromeKey, render func() string) (string, framePart) {
	if slot != nil && slot.valid && slot.key == key {
		return slot.text, slot.part
	}
	text := render()
	part := newFramePart(text)
	if slot != nil {
		*slot = memoPart{valid: true, key: key, text: text, part: part}
	}
	return text, part
}

func (m Model) memoSlot(pick func(*frameMemo) *memoPart) *memoPart {
	if m.frameMemo == nil {
		return nil
	}
	return pick(m.frameMemo)
}

// cachedHeader is viewHeader through the frame memo.
func (m Model) cachedHeader() framePart {
	_, p := memoized(m.memoSlot(func(f *frameMemo) *memoPart { return &f.header }), m.chromeKey(m.width), m.viewHeader)
	return p
}

// cachedInput is viewInput(width) through the frame memo; recalcLayout
// measures it and View draws it, so one render serves both.
func (m Model) cachedInput(width int) (string, framePart) {
	return memoized(m.memoSlot(func(f *frameMemo) *memoPart { return &f.input }), m.chromeKey(width), func() string { return m.viewInput(width) })
}

// cachedStatusBar is viewStatusBar(width) through the frame memo.
func (m Model) cachedStatusBar(width int) framePart {
	_, p := memoized(m.memoSlot(func(f *frameMemo) *memoPart { return &f.status }), m.chromeKey(width), func() string { return m.viewStatusBar(width) })
	return p
}

// cachedSidePanel is viewSidePanel(width) through the frame memo.
func (m Model) cachedSidePanel(width int) framePart {
	_, p := memoized(m.memoSlot(func(f *frameMemo) *memoPart { return &f.side }), m.chromeKey(width), func() string { return m.viewSidePanel(width) })
	return p
}

// transcriptOnly reports whether msg can change nothing but the transcript,
// so the chrome drawn for the previous frame is still right (frameMemo).
func transcriptOnly(msg tea.Msg) bool {
	switch msg := msg.(type) {
	case streamChunkMsg, renderFillMsg:
		return true
	case runEventsMsg:
		for _, ev := range msg.events {
			if ev.trace != nil {
				return false // a tool trace feeds the side panel
			}
		}
		return true
	}
	return false
}
