package lsp

import (
	"encoding/json"
	"testing"
)

func edit(sl, sc, el, ec int, text string) TextEdit {
	var e TextEdit
	e.Range.Start = Position{Line: sl, Character: sc}
	e.Range.End = Position{Line: el, Character: ec}
	e.NewText = text
	return e
}

func TestApplyTextEdits(t *testing.T) {
	content := "func greet() {}\n\nfunc main() {\n\tgreet()\n}\n"
	got := applyTextEdits(content, []TextEdit{
		edit(0, 5, 0, 10, "hello"),
		edit(3, 1, 3, 6, "hello"),
	})
	want := "func hello() {}\n\nfunc main() {\n\thello()\n}\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyTextEditsUnsortedAndMultiline(t *testing.T) {
	content := "aaa\nbbb\nccc\n"
	got := applyTextEdits(content, []TextEdit{
		edit(2, 0, 2, 3, "C"),
		edit(0, 0, 1, 3, "X"), // spans two lines
	})
	if got != "X\nC\n" {
		t.Fatalf("got %q", got)
	}
}

func TestOffsetOfPositionUTF16(t *testing.T) {
	// 𝕏 is outside the BMP: 2 UTF-16 units, 4 UTF-8 bytes.
	content := "a𝕏b\n"
	if off := offsetOfPosition(content, Position{Line: 0, Character: 3}); off != 5 {
		t.Fatalf("expected byte offset 5 for utf16 col 3, got %d", off)
	}
	if off := offsetOfPosition(content, Position{Line: 9, Character: 0}); off != len(content) {
		t.Fatalf("expected clamp to len, got %d", off)
	}
}

func TestFlattenHoverContents(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`"plain"`, "plain"},
		{`{"kind":"markdown","value":"func greet() string"}`, "func greet() string"},
		{`{"language":"go","value":"var x int"}`, "var x int"},
		{`[{"language":"go","value":"one"},"two"]`, "one\n\ntwo"},
		{``, ""},
		{`null`, ""},
	}
	for _, c := range cases {
		if got := flattenHoverContents(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("flatten(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}
