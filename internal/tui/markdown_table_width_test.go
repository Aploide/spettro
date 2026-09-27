package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// terminalColumns returns the column of every rune of line as a terminal
// without grapheme clustering (xterm.js) places it: advancing rune by rune
// by each rune's wcwidth.
func terminalColumns(line string) map[int]rune {
	cols := map[int]rune{}
	col := 0
	plain := ansi.Strip(line)
	for i, r := range plain {
		cols[col] = r
		col += ansi.StringWidthWc(plain[i : i+utf8.RuneLen(r)])
	}
	return cols
}

// A table holding CJK, emoji and an emoji ZWJ sequence keeps every border in
// the same column on every row, measured the way the layout does (grapheme
// clusters), the way Bubble Tea's renderer does on most terminals (wcwidth)
// and the way xterm.js moves its cursor (rune by rune). The ZWJ sequence
// U+1F469 U+200D U+1F4BB used to push that row's right border two cells
// right in xterm.js.
func TestMarkdownTableBordersAlignOnEveryTerminal(t *testing.T) {
	md := strings.Join([]string{
		"| Name | Width | Notes |",
		"|---|---:|---|",
		"| ascii | 1 | plain |",
		"| CJK 漢字テスト | 2 | 全角文字は二列を占める |",
		"| emoji 🚀🔥✅ | 2 | wide glyphs 👩‍💻 |",
		"| symbols ⚠️ ❤️ 👍🏽 🇮🇹 1️⃣ | 3 | text-style emoji |",
		"| long | 99 | " + strings.Repeat("A", 90) + " |",
	}, "\n")
	for _, width := range []int{40, 60, 110} {
		lines := strings.Split(renderMarkdown(md, width), "\n")
		var borderCols []int
		for col, r := range terminalColumns(lines[0]) {
			if r == '┌' || r == '┬' || r == '┐' {
				borderCols = append(borderCols, col)
			}
		}
		if len(borderCols) != 4 {
			t.Fatalf("width %d: top rule has %d column marks, want 4: %q", width, len(borderCols), ansi.Strip(lines[0]))
		}
		for _, line := range lines {
			gw, wc := ansi.StringWidth(line), ansi.StringWidthWc(line)
			if gw != wc {
				t.Errorf("width %d: row measures %d cells as graphemes, %d as wcwidth: %q", width, gw, wc, ansi.Strip(line))
			}
			cols := terminalColumns(line)
			for _, c := range borderCols {
				if r := cols[c]; !strings.ContainsRune("┌┬┐│├┼┤└┴┘", r) {
					t.Errorf("width %d: no border at column %d (found %q): %q", width, c, r, ansi.Strip(line))
				}
			}
		}
	}
}
