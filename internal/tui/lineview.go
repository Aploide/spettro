//go:build !spettro_bubblesviewport

package tui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// lineView is the transcript viewport: a window of height rows onto a list
// of rendered blocks (one per chat message, plus the logo), drawn with one
// blank row between consecutive blocks.
//
// It replaces bubbles/viewport for the transcript. That component takes the
// whole transcript as one string, so every refresh joined every block into a
// string, split it back into lines and measured the width of every line
// (maxLineWidth), which at 1k messages cost ~28 ms and 3 MB per streamed
// token (BenchmarkComponents/vpSetContent in the perf harness). Here the
// blocks arrive as the line slices the render cache already holds, so a
// refresh only rebuilds the per-block start offsets (a few microseconds for
// a thousand blocks), and View touches only the rows on screen.
//
// Invariant: content is wrapped to the pane width before it gets here. A
// row wider than the view is still cut to it (never wrapped), so a bug
// upstream cannot push the frame past the terminal edge; only rows longer
// in bytes than the view is wide are measured for that.
//
// Scrolling semantics are those of bubbles/viewport, which callers and
// tests rely on: the offset is clamped to [0, TotalLineCount-height], and
// AtBottom reports an offset at or past that maximum. Building with the tag
// spettro_bubblesviewport swaps in an adapter over bubbles/viewport with the
// same methods (lineview_bubbles.go), kept for one release so the two can
// be compared.
//
// Like the rest of Model, a lineView is only touched from the Bubble Tea
// Update/View goroutine. Its slices are replaced, never written in place, so
// a copy of the Model taken earlier keeps a consistent (older) view.
type lineView struct {
	width, height int
	yOffset       int
	// blocks are the rendered blocks, top to bottom; each is non-empty.
	blocks [][]string
	// starts[i] is the index of the first row of blocks[i]; the blank
	// separator row before it is starts[i]-1. starts has one extra entry,
	// the total row count plus one, so block i spans
	// [starts[i], starts[i+1]-1).
	starts []int
	// blocksWidth is the width the blocks were rendered for, as the last
	// restoreAnchor was told (see viewAnchor).
	blocksWidth int
}

// newLineView returns an empty viewport of the given size.
func newLineView(width, height int) lineView {
	return lineView{width: width, height: height}
}

// Width is the number of cells a row is drawn in.
func (v lineView) Width() int { return v.width }

// Height is the number of rows drawn.
func (v lineView) Height() int { return v.height }

// SetWidth sets the number of cells a row is drawn in.
func (v *lineView) SetWidth(w int) { v.width = w }

// SetHeight sets the number of rows drawn. Like bubbles/viewport it does not
// move the offset; callers that follow the bottom call GotoBottom after it.
func (v *lineView) SetHeight(h int) { v.height = h }

// SetBlocks replaces the content. Empty blocks are drawn as one blank row,
// which is what joining them with "\n\n" used to produce. The offset is
// clamped to the new content, as bubbles/viewport.SetContent does.
func (v *lineView) SetBlocks(blocks [][]string) {
	normalized := blocks
	for i, b := range blocks {
		if len(b) == 0 {
			// Copy on first empty block only: the common case keeps the
			// caller's slice.
			normalized = make([][]string, len(blocks))
			copy(normalized, blocks)
			for j := i; j < len(normalized); j++ {
				if len(normalized[j]) == 0 {
					normalized[j] = []string{""}
				}
			}
			break
		}
	}
	if len(normalized) == 1 && len(normalized[0]) == 1 && ansi.StringWidth(normalized[0][0]) == 0 {
		// One blank row is no content, as in bubbles/viewport.
		normalized = nil
	}
	starts := make([]int, len(normalized)+1)
	row := 0
	for i, b := range normalized {
		starts[i] = row
		row += len(b) + 1 // the block and the blank row after it
	}
	starts[len(normalized)] = row
	v.blocks = normalized
	v.starts = starts
	if v.yOffset > v.maxYOffset() {
		v.GotoBottom()
	}
}

// SetContent replaces the content with a single string, split on "\n". It
// exists for callers that have one pre-joined string (tests, the empty
// transcript); the transcript itself uses SetBlocks.
func (v *lineView) SetContent(s string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	v.SetBlocks([][]string{strings.Split(s, "\n")})
}

// GetContent returns the whole content as one string, rows joined by "\n".
func (v lineView) GetContent() string {
	var b strings.Builder
	for i, block := range v.blocks {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.Join(block, "\n"))
	}
	return b.String()
}

// TotalLineCount is the number of content rows, separators included.
func (v lineView) TotalLineCount() int {
	if len(v.blocks) == 0 {
		return 0
	}
	return v.starts[len(v.blocks)] - 1 // no separator after the last block
}

func (v lineView) maxYOffset() int {
	return max(0, v.TotalLineCount()-v.height)
}

// YOffset is the index of the first row on screen.
func (v lineView) YOffset() int { return v.yOffset }

// SetYOffset scrolls so that row n is the first on screen, clamped.
func (v *lineView) SetYOffset(n int) {
	v.yOffset = min(max(n, 0), v.maxYOffset())
}

// AtTop reports whether the first row is on screen.
func (v lineView) AtTop() bool { return v.yOffset <= 0 }

// AtBottom reports whether the last row is on screen.
func (v lineView) AtBottom() bool { return v.yOffset >= v.maxYOffset() }

// GotoTop scrolls to the first row.
func (v *lineView) GotoTop() { v.yOffset = 0 }

// GotoBottom scrolls so the last row is the bottom row on screen.
func (v *lineView) GotoBottom() { v.yOffset = v.maxYOffset() }

// ScrollUp scrolls n rows towards the top.
func (v *lineView) ScrollUp(n int) {
	if v.AtTop() || n == 0 || len(v.blocks) == 0 {
		return
	}
	v.SetYOffset(v.yOffset - n)
}

// ScrollDown scrolls n rows towards the bottom.
func (v *lineView) ScrollDown(n int) {
	if v.AtBottom() || n == 0 || len(v.blocks) == 0 {
		return
	}
	v.SetYOffset(v.yOffset + n)
}

// PageUp scrolls one screen towards the top.
func (v *lineView) PageUp() {
	if !v.AtTop() {
		v.ScrollUp(v.height)
	}
}

// PageDown scrolls one screen towards the bottom.
func (v *lineView) PageDown() {
	if !v.AtBottom() {
		v.ScrollDown(v.height)
	}
}

// viewAnchor records what the top row on screen shows, as a place in the
// content rather than a row number: row within block, of rows in the
// block, when the blocks were rendered width cells wide. A refresh that
// re-renders the blocks above it (a resize or the side panel rewrapping the
// transcript, ctrl+o expanding tool output, a budgeted render filling in
// placeholders) changes how many rows lie above that place, so keeping the
// row number would show different content: restoreAnchor finds the place
// again instead. The transcript is rendered for the pane width, which can
// change before the viewport's own width does, so the render width is
// passed in rather than read from the view.
type viewAnchor struct {
	ok     bool
	blocks int // number of blocks when captured
	block  int
	row    int // row within block; len(block) is the separator after it
	rows   int // len(block) when captured
	width  int
}

// topAnchor captures the place shown in the top row. It is not ok for an
// empty view.
func (v lineView) topAnchor() viewAnchor {
	if len(v.blocks) == 0 {
		return viewAnchor{}
	}
	b := sort.Search(len(v.blocks), func(k int) bool { return v.starts[k+1] > v.yOffset })
	if b >= len(v.blocks) {
		return viewAnchor{}
	}
	return viewAnchor{
		ok:     true,
		blocks: len(v.blocks),
		block:  b,
		row:    v.yOffset - v.starts[b],
		rows:   len(v.blocks[b]),
		width:  v.blocksWidth,
	}
}

// restoreAnchor scrolls so the top row shows the place a captures. Blocks
// are matched by index, which holds while the transcript only grows (the
// logo, then one block per message); when it has fewer blocks than when a
// was captured the content was replaced (/clear, a rewind, compaction) and
// the offset is left as SetBlocks clamped it. When the width changed, the
// block was rewrapped and the row is scaled to its new height; otherwise
// the row is kept, so a block growing at its end (a streamed answer being
// read while it arrives) does not move the view. width is the width the
// current blocks were rendered for; it is recorded for the next anchor even
// when a is not ok.
func (v *lineView) restoreAnchor(a viewAnchor, width int) {
	v.blocksWidth = width
	if !a.ok || len(v.blocks) < a.blocks || a.block >= len(v.blocks) {
		return
	}
	rows := len(v.blocks[a.block])
	row := a.row
	switch {
	case a.row >= a.rows:
		row = rows // the separator after the block
	case a.width != v.blocksWidth && a.rows > 0:
		row = a.row * rows / a.rows
	default:
		row = min(row, rows)
	}
	v.SetYOffset(v.starts[a.block] + row)
}

// line returns content row i (0 <= i < TotalLineCount).
func (v lineView) line(i int) string {
	// The block holding row i is the last one starting at or before it.
	b := sort.Search(len(v.blocks), func(k int) bool { return v.starts[k+1] > i })
	offset := i - v.starts[b]
	if offset >= len(v.blocks[b]) {
		return "" // the separator after block b
	}
	return v.blocks[b][offset]
}

// View draws the rows on screen: height rows, each padded with spaces to
// width cells, or cut to it when wider.
func (v lineView) View() string {
	if v.width <= 0 || v.height <= 0 {
		return ""
	}
	total := v.TotalLineCount()
	var b strings.Builder
	b.Grow(v.height * (v.width + 16))
	for r := 0; r < v.height; r++ {
		if r > 0 {
			b.WriteByte('\n')
		}
		i := v.yOffset + r
		if i >= total {
			b.WriteString(strings.Repeat(" ", v.width))
			continue
		}
		writeFittedRow(&b, v.line(i), v.width)
	}
	return b.String()
}

// writeFittedRow writes row padded to exactly width cells.
func writeFittedRow(b *strings.Builder, row string, width int) {
	// A row's width in cells is never more than its length in bytes (no
	// character is wider than its UTF-8 encoding is long), so a short row
	// only needs measuring for its padding, and a plain ASCII row not even
	// that.
	w := len(row)
	if !isPlainASCII(row) {
		w = ansi.StringWidth(row)
	}
	if w > width {
		row = ansi.Truncate(row, width, "")
		w = ansi.StringWidth(row)
	}
	b.WriteString(row)
	if w < width {
		b.WriteString(strings.Repeat(" ", width-w))
	}
}

// isPlainASCII reports whether s is printable ASCII with no escape
// sequence, so its width in cells is its length.
func isPlainASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}
