package termtext

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestSanitizeLine(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain text is untouched", "go test ./...", "go test ./..."},
		{"unicode is untouched", "héllo 中文", "héllo 中文"},
		{"tabs become spaces", "a\tb", "a    b"},
		{"escape sequences are removed", "\x1b[31mred\x1b[0m text", "red text"},
		{"cursor movement is removed", "a\x1b[2Jb\x1b[10;10Hc", "abc"},
		{"progress meter keeps its final state", "50%\r75%\r100%", "100%"},
		{"crlf line ending is dropped", "line\r", "line"},
		{"other controls are dropped", "a\x00b\x07c\x7fd", "abcd"},
		{"c1 controls are dropped", "a\u0085b\u009bc", "abc"},
	}
	for _, tc := range cases {
		if got := SanitizeLine(tc.in); got != tc.want {
			t.Errorf("%s: SanitizeLine(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// A command waiting for approval must show every character that will run.
// SanitizeLine would let a carriage return hide the dangerous half.
func TestEscapeControlsHidesNothing(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"rm -rf ~ #\recho hi", "rm -rf ~ #^Mecho hi"},
		{"printf '\x1b[2J'", "printf '^[[2J'"},
		{"a\tb", "a    b"},
		{"del\x7f", "del^?"},
		{"c1\u009b", `c1\u009b`},
		{"crlf ending\r", "crlf ending"},
		{"plain", "plain"},
	}
	for _, tc := range cases {
		if got := EscapeControls(tc.in); got != tc.want {
			t.Errorf("EscapeControls(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSingleLineFoldsWhitespace(t *testing.T) {
	got := SingleLine("cat <<'EOF'\n\tone\n  two\nEOF")
	if want := "cat <<'EOF' one two EOF"; got != want {
		t.Fatalf("SingleLine = %q, want %q", got, want)
	}
}

func TestFitNeverExceedsWidth(t *testing.T) {
	inputs := []string{
		strings.Repeat("A", 10000),
		strings.Repeat("宽", 500),
		"\x1b[1m" + strings.Repeat("bold ", 100) + "\x1b[0m",
		"short",
	}
	for _, in := range inputs {
		for _, w := range []int{1, 2, 3, 10, 79} {
			got := Fit(in, w)
			if gw := ansi.StringWidth(got); gw > w {
				t.Fatalf("Fit(%.20q, %d) is %d cells wide", in, w, gw)
			}
			if ansi.StringWidth(in) > w && !strings.Contains(got, Ellipsis) {
				t.Fatalf("Fit(%.20q, %d) cut the text without a marker: %q", in, w, got)
			}
		}
	}
	if Fit("short", 10) != "short" {
		t.Fatal("Fit changed text that already fits")
	}
	if Fit("x", 0) != "" {
		t.Fatal("Fit with no room should be empty")
	}
}

func TestFitLeftKeepsTheEnd(t *testing.T) {
	path := strings.Repeat("deep/", 40) + "file.go"
	got := FitLeft(path, 20)
	if w := ansi.StringWidth(got); w != 20 {
		t.Fatalf("FitLeft is %d cells wide, want 20: %q", w, got)
	}
	if !strings.HasPrefix(got, Ellipsis) || !strings.HasSuffix(got, "/file.go") {
		t.Fatalf("FitLeft should keep the file name and mark the cut: %q", got)
	}
	if FitLeft("a.go", 20) != "a.go" {
		t.Fatal("FitLeft changed a path that already fits")
	}
	if w := ansi.StringWidth(FitLeft(strings.Repeat("宽", 50), 9)); w > 9 {
		t.Fatalf("FitLeft on wide text is %d cells wide", w)
	}
}

func TestWrapKeepsEveryCellAndFits(t *testing.T) {
	in := strings.Repeat("A", 250) + " tail words here " + strings.Repeat("宽", 60)
	lines := Wrap(in, 40)
	if len(lines) < 2 {
		t.Fatalf("expected the long line to wrap, got %d line(s)", len(lines))
	}
	var joined strings.Builder
	for _, line := range lines {
		if w := ansi.StringWidth(line); w > 40 {
			t.Fatalf("wrapped line is %d cells wide: %q", w, line)
		}
		joined.WriteString(strings.ReplaceAll(line, " ", ""))
	}
	if want := strings.ReplaceAll(in, " ", ""); joined.String() != want {
		t.Fatal("wrapping lost or reordered text")
	}
}
