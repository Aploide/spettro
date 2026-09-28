package diff

import (
	"strings"
	"testing"
)

// A body line inside a hunk is a body line whatever it starts with. Deleting
// the SQL comment "-- keep row level security on" gives "--- keep ...", and
// adding "++counter;" gives "+++counter;": neither may be taken for a file
// header, which would hide the change and shift every line number after it.
func TestRenderReadsBodyLinesThatLookLikeHeaders(t *testing.T) {
	old := "select 1;\n-- keep row level security on\nalter table t enable row level security;\nend;\n"
	updated := "select 1;\nalter table t enable row level security;\n++counter;\nend;\n"
	d := Unified("policy.sql", old, updated)
	for _, opts := range []Options{{Width: 100}, {Width: 100, Wrap: true, Exact: true}, {Width: 160}} {
		parsed := parseUnified(d, opts.Exact)
		var dels, adds []parsedLine
		for _, l := range parsed {
			switch {
			case l.meta:
			case l.kind == kindDel:
				dels = append(dels, l)
			case l.kind == kindAdd:
				adds = append(adds, l)
			}
		}
		if len(dels) != 1 || dels[0].text != "-- keep row level security on" || dels[0].oldNo != 2 {
			t.Fatalf("the deleted comment was not read as a deletion of line 2: %+v", dels)
		}
		if len(adds) != 1 || adds[0].text != "++counter;" || adds[0].newNo != 3 {
			t.Fatalf("the added line was not read as an addition of line 3: %+v", adds)
		}
		out := stripANSI(Render(d, opts))
		if opts.Width >= SideBySideMinWidth {
			// Side by side: the deletion sits alone on the left.
			if !strings.Contains(out, "2 -- keep row level security on") || !strings.Contains(out, "3 ++counter;") {
				t.Fatalf("side by side lost a line number:\n%s", out)
			}
			continue
		}
		if !strings.Contains(out, "- -- keep row level security on") || !strings.Contains(out, "+ ++counter;") {
			t.Fatalf("rendered without the +/- signs:\n%s", out)
		}
	}
	// Between hunks, and in a diff of several files, headers are still
	// headers.
	two := "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n--- a/y\n+++ b/y\n@@ -1,2 +1 @@\n-c\n d\n"
	var meta, body int
	for _, l := range parseUnified(two, false) {
		if l.meta {
			meta++
		} else {
			body++
		}
	}
	if meta != 6 || body != 4 {
		t.Fatalf("two-file diff: %d header lines and %d body lines, want 6 and 4", meta, body)
	}
}

// Exact draws a tab as the tab mark and keeps a carriage return ending a
// line, so a tab-indented heredoc terminator cannot pass for a
// space-indented one; the default keeps tabs as spaces for a compact
// preview.
func TestRenderExactShowsTabsAndCarriageReturns(t *testing.T) {
	d := "--- /dev/null\n+++ b/run.sh\n@@ -0,0 +1,3 @@\n+cat <<-EOF\n+\tEOF\n+    EOF\r\n"
	exact := stripANSI(Render(d, Options{Width: 80, Wrap: true, Exact: true}))
	if !strings.Contains(exact, "+ ⇥   EOF") || !strings.Contains(exact, "+     EOF^M") {
		t.Fatalf("exact render hides a tab or a carriage return:\n%s", exact)
	}
	plain := stripANSI(Render(d, Options{Width: 80}))
	if strings.Contains(plain, "⇥") || strings.Contains(plain, "^M") {
		t.Fatalf("the compact render changed:\n%s", plain)
	}
}
