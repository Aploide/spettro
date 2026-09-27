package termtext

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// HardWrap keeps every character, spaces included, and never makes a row
// wider than asked (a wide character wider than the row gets its own).
func TestHardWrap(t *testing.T) {
	cases := []string{
		"",
		"short",
		"rm  -rf   /tmp/x   # double  spaces  matter",
		strings.Repeat("宽", 30) + "ascii" + strings.Repeat("é", 10),
		strings.Repeat("a", 100),
	}
	for _, s := range cases {
		for _, width := range []int{1, 2, 7, 10, 33} {
			rows := HardWrap(s, width)
			if strings.Join(rows, "") != s {
				t.Fatalf("HardWrap(%q, %d) lost characters: %q", s, width, rows)
			}
			for _, row := range rows {
				if w := ansi.StringWidth(row); w > width && utf8.RuneCountInString(row) > 1 {
					t.Fatalf("HardWrap(%q, %d): row %q is %d cells", s, width, row, w)
				}
			}
		}
	}
	if got := HardWrap("abcdef", 4); fmt.Sprint(got) != "[abcd ef]" {
		t.Fatalf("HardWrap(abcdef, 4) = %q", got)
	}
	if got := HardWrap("a  b", 2); fmt.Sprintf("%q", got) != `["a " " b"]` {
		t.Fatalf("HardWrap kept the wrong spaces: %q", got)
	}
}
