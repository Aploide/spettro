package termtext

import (
	"unicode"
	"unicode/utf8"
)

// Look-alike letters.
//
// Some letters of other scripts are drawn exactly like Latin ones: with the
// Cyrillic small i (U+0456) in place of its "i", "g\u0456thub.com" names
// another host than "github.com" while every terminal and editor draws the
// two the same, and with the Cyrillic small ie (U+0435), "us\u0435r.isAdmin"
// is another identifier than "user.isAdmin". EscapeControls and EscapeExact
// write such a letter as a "\u0456"-style escape wherever it sits in a word
// that reads as Latin text (see latinContext), so a command, a path or a
// line of code that only looks like the one the user expects no longer
// looks like it. A word written entirely in another script (Russian prose,
// say) is left alone: its letters are not posing as anything.
//
// The tables below name every letter by code point, not by its glyph: a
// glyph here would look like the Latin letter it imitates, which is the
// whole problem.

// latinLookalike reports whether r is a letter or digit outside ASCII that
// is drawn like an ASCII letter or digit: the Cyrillic, Greek and Armenian
// letters Unicode lists as confusable with Latin ones, the Cherokee and Lisu
// letters (nearly all of which are), dotless i and the other Latin-script
// look-alikes, small capitals, fullwidth forms, mathematical alphanumerics,
// letterlike symbols such as the Kelvin sign and Roman numerals.
func latinLookalike(r rune) bool {
	if r < 0x80 {
		return false
	}
	if lookalikeRunes[r] {
		return true
	}
	for _, rg := range lookalikeRanges {
		if r >= rg[0] && r <= rg[1] {
			return unicode.IsLetter(r) || unicode.IsNumber(r)
		}
	}
	return false
}

// lookalikeRanges are blocks whose letters and digits all look like Latin
// ones, or so nearly all that the rest are not worth telling apart.
var lookalikeRanges = [][2]rune{
	{0x13a0, 0x13f5},   // Cherokee capitals (U+13AA looks like A, U+13B4 like V, ...)
	{0x1d00, 0x1d2b},   // small capitals (U+1D00 looks like a small A, ...)
	{0x2100, 0x214f},   // letterlike symbols: U+212A KELVIN SIGN (K), U+212B ANGSTROM SIGN, ...
	{0x2160, 0x2188},   // Roman numerals (U+2160 looks like I, U+217C like l, ...)
	{0xa4d0, 0xa4f7},   // Lisu (U+A4D0 looks like B, ...)
	{0xff10, 0xff19},   // fullwidth digits
	{0xff21, 0xff3a},   // fullwidth capitals
	{0xff41, 0xff5a},   // fullwidth small letters
	{0x1d400, 0x1d7ff}, // mathematical alphanumerics (bold, italic, script, ... letters and digits)
}

// lookalikeRunes are the single letters, from scripts that are otherwise
// told apart from Latin at a glance, that are drawn like a Latin letter.
// Each is listed with the Latin letter it passes for.
var lookalikeRunes = func() map[rune]bool {
	set := map[rune]bool{}
	for _, r := range []rune{
		// Cyrillic small: a e o p c y x s i j d h l q w y v.
		0x0430, 0x0435, 0x043e, 0x0440, 0x0441, 0x0443, 0x0445, 0x0455, 0x0456,
		0x0458, 0x0501, 0x04bb, 0x04cf, 0x051b, 0x051d, 0x04af, 0x0475,
		// Cyrillic capital: A B E K M H O P C T Y X S I J Y Q W I V.
		0x0410, 0x0412, 0x0415, 0x041a, 0x041c, 0x041d, 0x041e, 0x0420, 0x0421,
		0x0422, 0x0423, 0x0425, 0x0405, 0x0406, 0x0408, 0x04ae, 0x051a, 0x051c,
		0x04c0, 0x0474,
		// Greek small: a i k v o p u x c j.
		0x03b1, 0x03b9, 0x03ba, 0x03bd, 0x03bf, 0x03c1, 0x03c5, 0x03c7, 0x03f2,
		0x03f3,
		// Greek capital: A B E Z H I K M N O P T Y X C.
		0x0391, 0x0392, 0x0395, 0x0396, 0x0397, 0x0399, 0x039a, 0x039c, 0x039d,
		0x039f, 0x03a1, 0x03a4, 0x03a5, 0x03a7, 0x03f9,
		// Armenian: h n u o g, capital U O.
		0x0570, 0x0578, 0x057d, 0x0585, 0x0581, 0x054d, 0x0555,
		// Latin letters that pass for plainer ones: dotless i, dotless j,
		// Latin alpha (a), script g, iota (i), the dental click (l).
		0x0131, 0x0237, 0x0251, 0x0261, 0x0269, 0x01c0,
	} {
		set[r] = true
	}
	return set
}()

// latinContext reports whether word (text between blanks) reads as Latin
// text, which a look-alike letter in it would be posing as: it has an ASCII
// letter or digit, or the punctuation of a path, URL, address or assignment
// ("/", ":", "@", "=", "_", or a "." between two letters), which prose in
// another script does not use inside a word.
func latinContext(word string) bool {
	prevLetter := false
	for i := 0; i < len(word); {
		r, size := utf8.DecodeRuneInString(word[i:])
		switch {
		case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			return true
		case r == '/', r == ':', r == '@', r == '=', r == '_':
			return true
		case r == '.' && prevLetter:
			if next, _ := utf8.DecodeRuneInString(word[i+size:]); unicode.IsLetter(next) {
				return true
			}
		}
		prevLetter = unicode.IsLetter(r)
		i += size
	}
	return false
}

// posingLookalikes returns, for text s, the byte ranges of the words that
// hold a look-alike letter posing as Latin (latinLookalike in a word for
// which latinContext holds), in order; nil when there are none. Words are
// separated by ASCII blanks and newlines.
func posingLookalikes(s string) [][2]int {
	var spans [][2]int
	start, hasLookalike := 0, false
	flush := func(end int) {
		if hasLookalike && latinContext(s[start:end]) {
			spans = append(spans, [2]int{start, end})
		}
	}
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == ' ', r == '\t', r == '\n', r == '\r':
			flush(i)
			start, hasLookalike = i+size, false
		case latinLookalike(r):
			hasLookalike = true
		}
		i += size
	}
	flush(len(s))
	return spans
}

// inSpans reports whether byte offset i lies in one of spans, which are in
// order; *next is where the search starts and is advanced past the spans
// that end at or before i, so a caller walking s from the start pays for
// each span once.
func inSpans(spans [][2]int, next *int, i int) bool {
	for *next < len(spans) && spans[*next][1] <= i {
		*next++
	}
	return *next < len(spans) && spans[*next][0] <= i
}
