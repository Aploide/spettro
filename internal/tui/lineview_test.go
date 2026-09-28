//go:build !spettro_bubblesviewport

package tui

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
	"github.com/charmbracelet/x/ansi"
)

// oracleViewport is the transcript viewport lineView replaced: bubbles'
// viewport fed the blocks joined with a blank row, as refreshViewport did.
func oracleViewport(width, height int, blocks [][]string) viewport.Model {
	vp := viewport.New(viewport.WithWidth(width), viewport.WithHeight(height))
	parts := make([]string, len(blocks))
	for i, b := range blocks {
		parts[i] = strings.Join(b, "\n")
	}
	vp.SetContent(strings.Join(parts, "\n\n"))
	return vp
}

func randomBlocks(r *rand.Rand) [][]string {
	blocks := make([][]string, 1+r.Intn(12))
	for i := range blocks {
		n := r.Intn(6)
		if i == 0 {
			n++ // the logo is never empty; bubbles drops an all-empty content
		}
		for j := 0; j < n; j++ {
			switch r.Intn(5) {
			case 0:
				blocks[i] = append(blocks[i], "")
			case 1:
				blocks[i] = append(blocks[i], "\x1b[1mbold\x1b[m row "+strings.Repeat("x", r.Intn(50)))
			case 2:
				blocks[i] = append(blocks[i], "漢字 wide "+strings.Repeat("字", r.Intn(30)))
			default:
				blocks[i] = append(blocks[i], fmt.Sprintf("block %d row %d %s", i, j, strings.Repeat("a", r.Intn(60))))
			}
		}
	}
	return blocks
}

// lineView must scroll and draw exactly like the bubbles viewport it
// replaces, for any content and any sequence of scroll operations; the
// offsets, AtBottom and the visible text (ignoring trailing padding) are
// compared after every step.
func TestLineViewMatchesBubblesViewport(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for trial := 0; trial < 300; trial++ {
		width, height := 20+r.Intn(40), 1+r.Intn(15)
		blocks := randomBlocks(r)
		lv := newLineView(width, height)
		lv.SetBlocks(blocks)
		vp := oracleViewport(width, height, blocks)
		for step := 0; step < 25; step++ {
			switch op := r.Intn(8); op {
			case 0:
				n := r.Intn(10)
				lv.ScrollUp(n)
				vp.ScrollUp(n)
			case 1:
				n := r.Intn(10)
				lv.ScrollDown(n)
				vp.ScrollDown(n)
			case 2:
				lv.PageUp()
				vp.PageUp()
			case 3:
				lv.PageDown()
				vp.PageDown()
			case 4:
				lv.GotoBottom()
				vp.GotoBottom()
			case 5:
				lv.GotoTop()
				vp.GotoTop()
			case 6:
				n := r.Intn(40)
				lv.SetYOffset(n)
				vp.SetYOffset(n)
			case 7:
				blocks = randomBlocks(r)
				lv.SetBlocks(blocks)
				vp.SetContentLines(strings.Split(oracleViewport(width, height, blocks).GetContent(), "\n"))
			}
			if lv.YOffset() != vp.YOffset() || lv.AtBottom() != vp.AtBottom() || lv.TotalLineCount() != vp.TotalLineCount() {
				t.Fatalf("trial %d step %d: offset %d/%d atBottom %v/%v total %d/%d",
					trial, step, lv.YOffset(), vp.YOffset(), lv.AtBottom(), vp.AtBottom(), lv.TotalLineCount(), vp.TotalLineCount())
			}
			if got, want := trimRows(lv.View()), trimRows(vp.View()); got != want {
				t.Fatalf("trial %d step %d: view differs\n--- lineView ---\n%s\n--- bubbles ---\n%s", trial, step, got, want)
			}
		}
	}
}

// trimRows strips escape sequences and trailing spaces from every row: the
// two viewports pad rows with the same spaces but may place resets
// differently.
func trimRows(s string) string {
	rows := strings.Split(ansi.Strip(s), "\n")
	for i, r := range rows {
		rows[i] = strings.TrimRight(r, " ")
	}
	return strings.Join(rows, "\n")
}

// Every row lineView draws is exactly the view's width: shorter rows are
// padded, wider ones cut, so the frame never depends on what upstream
// rendered.
func TestLineViewRowsAreExactlyTheWidth(t *testing.T) {
	lv := newLineView(12, 4)
	lv.SetBlocks([][]string{{"short", strings.Repeat("w", 40)}, {"\x1b[31m" + strings.Repeat("字", 20) + "\x1b[m"}})
	for i, row := range strings.Split(lv.View(), "\n") {
		if w := ansi.StringWidth(row); w != 12 {
			t.Errorf("row %d is %d cells wide, want 12: %q", i, w, row)
		}
	}
}

// Blocks are drawn with one blank row between them, and an empty block as
// one blank row, as joining them with "\n\n" did.
func TestLineViewBlockLayout(t *testing.T) {
	lv := newLineView(10, 10)
	lv.SetBlocks([][]string{{"a", "b"}, {}, {"c"}})
	if got, want := lv.GetContent(), "a\nb\n\n\n\nc"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if got := lv.TotalLineCount(); got != 6 {
		t.Fatalf("TotalLineCount = %d, want 6", got)
	}
	for i, want := range []string{"a", "b", "", "", "", "c"} {
		if got := lv.line(i); got != want {
			t.Errorf("line(%d) = %q, want %q", i, got, want)
		}
	}
}

// Allocation guard: a refresh that re-renders nothing allocates only the
// block list and the viewport offsets, however long the transcript. (The
// bubbles/viewport build joins and splits the whole transcript instead.)
func TestWarmRefreshAllocations(t *testing.T) {
	m := cacheModel(t, 1000)
	if n := testing.AllocsPerRun(20, func() { m.refreshViewport() }); n > 3 {
		t.Fatalf("a warm refresh allocated %v times, want at most 3", n)
	}
}

// A refresh with the same blocks allocates only the offsets: no joining,
// splitting or measuring of the transcript.
func TestLineViewSetBlocksAllocations(t *testing.T) {
	blocks := make([][]string, 1000)
	for i := range blocks {
		blocks[i] = []string{"row one", "row two", "row three"}
	}
	lv := newLineView(80, 40)
	if n := testing.AllocsPerRun(20, func() { lv.SetBlocks(blocks) }); n > 1 {
		t.Fatalf("SetBlocks allocated %v times, want at most 1 (the offsets)", n)
	}
}
