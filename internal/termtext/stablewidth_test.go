package termtext

import (
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// runeSumWidth is how a terminal without grapheme clustering (xterm.js)
// advances the cursor: rune by rune.
func runeSumWidth(s string) int {
	sum := 0
	for i, r := range s {
		sum += ansi.StringWidthWc(s[i : i+utf8.RuneLen(r)])
	}
	return sum
}

func TestStableWidthRewritesAmbiguousClusters(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"ascii untouched", "plain text", "plain text"},
		{"cjk untouched", "漢字テスト", "漢字テスト"},
		{"single emoji untouched", "🚀🔥✅", "🚀🔥✅"},
		{"combining accent untouched", "é", "é"},
		{"zwj sequence split into its glyphs", "👩‍💻", "👩💻"},
		{"variation selector dropped", "⚠️ warn", "⚠ warn"},
		{"skin tone dropped", "👍🏽", "👍"},
		{"keycap reduced to its digit", "1️⃣", "1"},
		{"flag becomes letters", "🇮🇹", "IT"},
	}
	for _, tc := range cases {
		if got := StableWidth(tc.in); got != tc.want {
			t.Errorf("%s: StableWidth(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Every width method must agree on the result, whatever went in: that is
// what keeps a row's right edge where the layout put it on every terminal.
func TestStableWidthAllMethodsAgree(t *testing.T) {
	inputs := []string{
		"wide glyphs 👩‍💻 and 👨‍👩‍👧 family",
		"❤️ ☺️ ℹ️ ✔️ ⚠️", "👍🏽👋🏿", "1️⃣2️⃣#️⃣", "🇮🇹🇯🇵",
		"🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F",
		"CJK 漢字テスト 全角文字は二列を占める", "mixed 🚀 á b",
	}
	for _, in := range inputs {
		out := StableWidth(in)
		gw, wc, sum := ansi.StringWidth(out), ansi.StringWidthWc(out), runeSumWidth(out)
		if gw != wc || gw != sum {
			t.Errorf("StableWidth(%q) = %q: grapheme %d, wcwidth %d, per-rune %d", in, out, gw, wc, sum)
		}
	}
}

func TestSanitizeLineAppliesStableWidth(t *testing.T) {
	if got := SanitizeLine("a 👩‍💻 b"); got != "a 👩💻 b" {
		t.Fatalf("SanitizeLine kept a ZWJ sequence: %q", got)
	}
	if got := SanitizeLine("x\t⚠️"); got != "x    ⚠" {
		t.Fatalf("SanitizeLine slow path kept a variation selector: %q", got)
	}
}

func TestStableWidthASCIIDoesNotAllocate(t *testing.T) {
	s := "plain ascii text of a typical transcript line"
	if n := testing.AllocsPerRun(100, func() { _ = StableWidth(s) }); n != 0 {
		t.Fatalf("StableWidth on ASCII allocated %v times", n)
	}
}
