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
	// No row ends with a space when it can end elsewhere: there nobody
	// could see it. A row of nothing but spaces is the exception.
	if got := HardWrap("a  b", 2); fmt.Sprintf("%q", got) != `["a" "  " "b"]` {
		t.Fatalf("HardWrap kept the wrong spaces: %q", got)
	}
}

// Two commands that differ only by a space must never wrap to the same
// rows. Breaking a row just after (or just before) a space leaves the space
// at a row's edge, where it cannot be seen: "./build/aaa/ ~/" (which deletes
// the home directory) then reads exactly like "./build/aaa/~/". HardWrap
// breaks between two non-space characters instead, so every space sits
// inside a row between two visible characters.
func TestHardWrapNeverHidesASpaceAtABreak(t *testing.T) {
	a := strings.Repeat("a", 63)
	for _, width := range []int{8, 13, 20, 40, 72, 76} {
		for pad := range 12 {
			withSpace := "rm -rf ./build/" + strings.Repeat("x", pad) + a + "/ ~/"
			without := "rm -rf ./build/" + strings.Repeat("x", pad) + a + "/~/"
			for _, s := range []string{withSpace, without} {
				rows := HardWrap(s, width)
				if strings.Join(rows, "") != s {
					t.Fatalf("HardWrap(%q, %d) lost characters: %q", s, width, rows)
				}
				for i, row := range rows {
					if ansi.StringWidth(row) > width {
						t.Fatalf("HardWrap(%q, %d): row %q is too wide", s, width, row)
					}
					if i < len(rows)-1 && (strings.HasSuffix(row, " ") || strings.HasPrefix(rows[i+1], " ")) {
						t.Fatalf("HardWrap(%q, %d) put a space at a break: %q", s, width, rows)
					}
				}
			}
			// Drawn one row per line, the two must differ.
			if strings.Join(HardWrap(withSpace, width), "\n") == strings.Join(HardWrap(without, width), "\n") {
				t.Fatalf("width %d: the two commands wrap to the same rows", width)
			}
		}
	}
	// With no place between two non-space characters, a row ends before a
	// space rather than after it, so the space starts the next row.
	if got := HardWrap("a b c d e f", 4); fmt.Sprintf("%q", got) != `["a b" " c d" " e f"]` {
		t.Fatalf("HardWrap of one-letter words = %q", got)
	}
}

// An escape written by EscapeExact stays on one row when the row can hold
// it: "\u" at the end of a row and "200b" at the start of the next would
// read as two things.
func TestHardWrapKeepsEscapesWhole(t *testing.T) {
	s := EscapeExact("zero\u200bwidth\U000e0100 echo safe\rrm")
	for _, width := range []int{5, 6, 9, 10, 11, 17} {
		rows := HardWrap(s, width)
		if strings.Join(rows, "") != s {
			t.Fatalf("width %d lost characters: %q", width, rows)
		}
		for i, row := range rows {
			if ansi.StringWidth(row) > width {
				t.Fatalf("width %d: row %q too wide", width, row)
			}
			if i == len(rows)-1 {
				continue
			}
			for _, esc := range []string{`\u200b`, `\U000e0100`, "^M"} {
				joined := row + rows[i+1]
				at := strings.Index(joined, esc)
				if at >= 0 && at < len(row) && at+len(esc) > len(row) && len(esc) <= width {
					t.Fatalf("width %d: %s split across %q and %q", width, esc, row, rows[i+1])
				}
			}
		}
	}
}
