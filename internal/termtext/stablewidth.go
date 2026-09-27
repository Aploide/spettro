package termtext

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// StableWidth returns s with every grapheme cluster whose width terminals
// disagree on replaced by a form they all measure alike.
//
// The layout measures text as grapheme clusters (lipgloss, ansi.StringWidth).
// Bubble Tea's renderer measures it the same way only on a terminal that
// reports Unicode mode 2027; everywhere else (xterm.js, Terminal.app, most
// terminals in use) it falls back to a per-cluster wcwidth, and the terminal
// itself advances the cursor rune by rune. An emoji ZWJ sequence such as
// "woman technologist" (U+1F469 U+200D U+1F4BB) is 2 cells to the layout, 2
// to the renderer and 4 to xterm.js, so every cell after it on the row lands
// two columns right of where the layout put it: a table's right border
// jumps out of line on exactly that row. A text-style symbol followed by
// the emoji variation selector (U+26A0 U+FE0F, "warning sign") is the
// opposite case, 2 cells to the layout and 1 to the renderer and the
// terminal.
//
// Only emoji sequences are rewritten: a cluster that holds a presentation
// modifier or a regional indicator, and whose three measures (its grapheme
// width, its wcwidth, and the sum of its runes' wcwidths) do not all agree.
// Every other cluster is kept byte for byte. Text in scripts that cluster a
// base letter with combining marks (Devanagari, Thai, Tamil, decomposed
// Hangul) also measures differently by grapheme and by rune, but it has no
// modifier that could be dropped without changing the words, so it is left
// exactly as the base renderer drew it.
// In a rewritten cluster the joiners and presentation modifiers are dropped (U+200D ZERO WIDTH JOINER, the U+FE0E
// and U+FE0F variation selectors, the U+1F3FB..U+1F3FF skin tones, the
// U+20E3 keycap and the U+E0020..U+E007F tag characters), which leaves the
// sequence's component glyphs, each of which every terminal measures the
// same; a regional-indicator flag becomes its two ASCII letters (U+1F1EE
// U+1F1F9 -> "IT"); an emoji component that is still ambiguous becomes
// U+FFFD, one cell everywhere.
// Pure ASCII, the common case, is returned without being looked at twice.
func StableWidth(s string) string {
	if isASCII(s) || !hasUnstableCluster(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for rest := s; rest != ""; {
		cluster, width := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		rest = rest[len(cluster):]
		if clusterIsStable(cluster, width) {
			b.WriteString(cluster)
			continue
		}
		writeStableCluster(&b, cluster)
	}
	return b.String()
}

// isASCII reports whether s holds only 7-bit bytes, which every width
// method measures as one cell each (controls are removed before this runs).
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// hasUnstableCluster reports whether any grapheme cluster of s would be
// rewritten, so a line with none is returned without being rebuilt.
func hasUnstableCluster(s string) bool {
	for rest := s; rest != ""; {
		cluster, width := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		if !clusterIsStable(cluster, width) {
			return true
		}
		rest = rest[len(cluster):]
	}
	return false
}

// clusterIsStable reports whether cluster is kept as it is: it is not an
// emoji sequence (see isEmojiSequence), or the grapheme width of cluster,
// its wcwidth, and the sum of its runes' wcwidths are all width.
func clusterIsStable(cluster string, width int) bool {
	if len(cluster) == 1 || !isEmojiSequence(cluster) {
		return true
	}
	return measuresAgree(cluster, width)
}

// isEmojiSequence reports whether cluster holds a rune that only exists to
// build an emoji sequence: a presentation modifier or a regional indicator.
// Only such clusters are ever rewritten.
func isEmojiSequence(cluster string) bool {
	for _, r := range cluster {
		if isPresentationModifier(r) || isRegionalIndicator(r) {
			return true
		}
	}
	return false
}

// isRegionalIndicator reports whether r is a regional indicator symbol
// letter A..Z, the runes a flag is made of.
func isRegionalIndicator(r rune) bool {
	return r >= 0x1F1E6 && r <= 0x1F1FF
}

// measuresAgree reports whether the grapheme width of cluster, its wcwidth,
// and the sum of its runes' wcwidths are all width.
func measuresAgree(cluster string, width int) bool {
	if len(cluster) == 1 {
		return true // one ASCII byte
	}
	if ansi.StringWidthWc(cluster) != width {
		return false
	}
	sum := 0
	for i, r := range cluster {
		sum += ansi.StringWidthWc(cluster[i : i+utf8.RuneLen(r)])
	}
	return sum == width
}

// writeStableCluster writes the width-stable replacement of one unstable
// cluster (see StableWidth).
func writeStableCluster(b *strings.Builder, cluster string) {
	var kept strings.Builder
	for _, r := range cluster {
		switch {
		case isPresentationModifier(r):
		case isRegionalIndicator(r):
			kept.WriteByte(byte('A' + (r - 0x1F1E6)))
		default:
			kept.WriteRune(r)
		}
	}
	for rest := kept.String(); rest != ""; {
		part, width := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		rest = rest[len(part):]
		if measuresAgree(part, width) {
			b.WriteString(part)
		} else {
			b.WriteRune(utf8.RuneError)
		}
	}
}

// isPresentationModifier reports whether r only joins or restyles the glyph
// before it: dropping it leaves every component glyph in place.
func isPresentationModifier(r rune) bool {
	switch {
	case r == 0x200D, r == 0xFE0E, r == 0xFE0F, r == 0x20E3:
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF:
		return true
	case r >= 0xE0020 && r <= 0xE007F:
		return true
	}
	return false
}
