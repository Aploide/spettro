package diff

import (
	"reflect"
	"strings"
	"testing"
)

func TestIntralineSpanChangeAtStart(t *testing.T) {
	oldSp, newSp := intralineSpans("foo bar baz", "qux bar baz")
	if want := []span{{0, 3}}; !reflect.DeepEqual(oldSp, want) {
		t.Fatalf("old spans = %v, want %v", oldSp, want)
	}
	if want := []span{{0, 3}}; !reflect.DeepEqual(newSp, want) {
		t.Fatalf("new spans = %v, want %v", newSp, want)
	}
}

func TestIntralineSpanChangeInMiddle(t *testing.T) {
	oldSp, newSp := intralineSpans("foo bar baz", "foo qux baz")
	if want := []span{{4, 7}}; !reflect.DeepEqual(oldSp, want) {
		t.Fatalf("old spans = %v, want %v", oldSp, want)
	}
	if want := []span{{4, 7}}; !reflect.DeepEqual(newSp, want) {
		t.Fatalf("new spans = %v, want %v", newSp, want)
	}
}

func TestIntralineSpanChangeAtEnd(t *testing.T) {
	oldSp, newSp := intralineSpans("foo bar baz", "foo bar quux")
	if want := []span{{8, 11}}; !reflect.DeepEqual(oldSp, want) {
		t.Fatalf("old spans = %v, want %v", oldSp, want)
	}
	if want := []span{{8, 12}}; !reflect.DeepEqual(newSp, want) {
		t.Fatalf("new spans = %v, want %v", newSp, want)
	}
}

func TestIntralineMultiSpan(t *testing.T) {
	oldSp, newSp := intralineSpans("alpha two gamma four", "one two three four")
	if want := []span{{0, 5}, {10, 15}}; !reflect.DeepEqual(oldSp, want) {
		t.Fatalf("old spans = %v, want %v", oldSp, want)
	}
	if want := []span{{0, 3}, {8, 13}}; !reflect.DeepEqual(newSp, want) {
		t.Fatalf("new spans = %v, want %v", newSp, want)
	}
}

func TestIntralineEqualLinesNoSpans(t *testing.T) {
	oldSp, newSp := intralineSpans("same line", "same line")
	if oldSp != nil || newSp != nil {
		t.Fatalf("equal lines produced spans: %v / %v", oldSp, newSp)
	}
}

func TestIntralineInsertionOnlyMarksNewSide(t *testing.T) {
	oldSp, newSp := intralineSpans("foo baz", "foo bar baz")
	if len(oldSp) != 0 {
		t.Fatalf("insertion marked old side: %v", oldSp)
	}
	// "bar " (with one adjoining separator) is the inserted region.
	if len(newSp) != 1 || newSp[0].start < 3 || newSp[0].end > 8 {
		t.Fatalf("new spans = %v, want one span within [3,8)", newSp)
	}
}

func TestPairSpansWholeLineChangeDropsSpans(t *testing.T) {
	oldSp, newSp := pairSpans("aaa", "bbb")
	if oldSp != nil || newSp != nil {
		t.Fatalf("whole-line change should drop spans, got %v / %v", oldSp, newSp)
	}
}

func TestRenderSpansSegments(t *testing.T) {
	base := tagStyle{"b"}
	hi := tagStyle{"h"}
	got := renderSpans("foo bar baz", []span{{4, 7}}, base, hi)
	if want := "<b>foo </b><h>bar</h><b> baz</b>"; got != want {
		t.Fatalf("renderSpans = %q, want %q", got, want)
	}
	got = renderSpans("abcdef", []span{{0, 2}, {4, 6}}, base, hi)
	if want := "<h>ab</h><b>cd</b><h>ef</h>"; got != want {
		t.Fatalf("renderSpans multi = %q, want %q", got, want)
	}
}

func TestClipSpans(t *testing.T) {
	spans := []span{{0, 3}, {5, 9}}
	got := clipSpans(spans, 6)
	if want := []span{{0, 3}, {5, 6}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("clipSpans = %v, want %v", got, want)
	}
	if got := clipSpans(spans, -1); !reflect.DeepEqual(got, spans) {
		t.Fatalf("clipSpans unlimited = %v, want %v", got, spans)
	}
}

func TestRenderUnifiedHighlightsIntralineChange(t *testing.T) {
	d := Unified("f.txt", "foo bar baz\n", "foo qux baz\n")
	out := Render(d, Options{})
	// The changed word must render through the intra-line style, which sets a
	// background color the plain add/del styles never use.
	if !strings.Contains(out, styleDelHi.Render("bar")) {
		t.Fatalf("deleted word not highlighted:\n%s", out)
	}
	if !strings.Contains(out, styleAddHi.Render("qux")) {
		t.Fatalf("added word not highlighted:\n%s", out)
	}
}

func TestRenderSideBySideHighlightsIntralineChange(t *testing.T) {
	d := Unified("f.txt", "foo bar baz\n", "foo qux baz\n")
	out := Render(d, Options{Width: SideBySideMinWidth})
	if !strings.Contains(out, styleDelHi.Render("bar")) {
		t.Fatalf("side-by-side deleted word not highlighted:\n%s", out)
	}
	if !strings.Contains(out, styleAddHi.Render("qux")) {
		t.Fatalf("side-by-side added word not highlighted:\n%s", out)
	}
}

// tagStyle wraps rendered text in tags so span boundaries are assertable.
type tagStyle struct{ tag string }

func (t tagStyle) Render(strs ...string) string {
	return "<" + t.tag + ">" + strings.Join(strs, " ") + "</" + t.tag + ">"
}
