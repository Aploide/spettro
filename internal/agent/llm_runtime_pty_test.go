package agent

import "testing"

func TestNormalizePtyOutput(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[1;32mhello\x1b[0m\r\nworld", "hello\nworld"},
		// readline redraws the input line with \r per keystroke; carriage
		// returns overwrite from column 0 so only the settled line survives.
		{">>> 2\r>>> 2*\r>>> 2**\r>>> 2**64\r\n18446744073709551616\r\n>>> ", ">>> 2**64\n18446744073709551616\n>>> "},
		{"abc\rxy", "xyc"},
		{"ab\bc", "ac"},
	}
	for _, c := range cases {
		if got := normalizePtyOutput(c.in); got != c.want {
			t.Fatalf("normalizePtyOutput(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDecodePtyInput(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{`2**64\r`, "2**64\r"},
		{`\x03`, "\x03"},
		{`\u0003`, "\x03"},
		{`\e[A`, "\x1b[A"},
		{`a\\r`, `a\r`},
		{`\n\t`, "\n\t"},
		{`\q`, `\q`},   // unknown escape passes through
		{`\x0`, `\x0`}, // truncated hex passes through
		{"real\rbytes", "real\rbytes"},
	}
	for _, c := range cases {
		if got := decodePtyInput(c.in); got != c.want {
			t.Fatalf("decodePtyInput(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClampWinDim(t *testing.T) {
	cases := map[int]uint16{-1: 0, 0: 0, 80: 80, 9999: 500}
	for in, want := range cases {
		if got := clampWinDim(in); got != want {
			t.Fatalf("clampWinDim(%d) = %d, want %d", in, got, want)
		}
	}
}
