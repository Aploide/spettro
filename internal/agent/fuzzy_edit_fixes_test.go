package agent

import (
	"strings"
	"testing"
)

// The block-anchor tier must not take a nested closing brace for the
// quote's top-level one: that edit deleted the admin guard and duplicated
// the function's tail.
func TestBlockAnchorLastLineKeepsRelativeIndentation(t *testing.T) {
	content := "func handle(w http.ResponseWriter, r *http.Request) {\n" +
		"\tuser := auth(r)\n" +
		"\tif user == nil {\n" +
		"\t\thttp.Error(w, \"unauthorized\", 401)\n" +
		"\t\treturn\n" +
		"\t}\n" +
		"\tif !user.Admin {\n" +
		"\t\treturn\n" +
		"\t}\n" +
		"\tlog.Println(\"ok\")\n" +
		"\trender(w, user)\n" +
		"}\n"
	old := "func handle(w http.ResponseWriter, r *http.Request) {\n" +
		"\tuser := auth(r)\n" +
		"\tif user == nil {\n" +
		"\t\thttp.Error(w, \"unauthorized\", 401)\n" +
		"\t\treturn\n" +
		"\t}\n" +
		"\tlog.Println(\"ok\")\n" +
		"\trender(w, user)\n" +
		"}"
	res, err := applyEdit(content, editRequest{Old: old, New: strings.Replace(old, "unauthorized", "forbidden", 1)})
	if err == nil {
		if !strings.Contains(res.Content, "user.Admin") || strings.Count(res.Content, "render(w, user)") != 1 {
			t.Fatalf("edit corrupted the function (tier %d):\n%s", res.Tier, res.Content)
		}
	}
}

// The context tier must not match a block whose last anchor is nested
// deeper than the quote's, nor one whose differing lines are unrelated.
func TestContextTierRejectsWrongBlocks(t *testing.T) {
	content := "func setup() {\n\tinit()\n\tif debug {\n\t}\n\tserve()\n}\n"
	if res, err := applyEdit(content, editRequest{Old: "func setup() {\n\tinit()\n\tserve()\n}", New: "func setup() {\n\tinit()\n\tserveTLS()\n}"}); err == nil {
		t.Fatalf("matched a truncated block (tier %d):\n%s", res.Tier, res.Content)
	}
	content = "func a() error {\n\tif err != nil {\n\t\tlog.Print(\"a\")\n\t\treturn err\n\t}\n}\n" +
		"func b() error {\n\tif err != nil {\n\t\tcleanup()\n\t\treturn wrap(err)\n\t}\n}\n"
	old := "\tif err != nil {\n\t\tcleanup(ctx)\n\t\treturn err\n\t}"
	if res, err := applyEdit(content, editRequest{Old: old, New: strings.Replace(old, "return err", "return nil", 1)}); err == nil && !strings.Contains(res.Content, "log.Print(\"a\")") {
		t.Fatalf("deleted a line the model never quoted (tier %d):\n%s", res.Tier, res.Content)
	}
}

// new_string lines shallower than old_string's base indentation are moved
// with the block, not left at their written depth.
func TestReindentMapsShallowerNewLines(t *testing.T) {
	content := "class C:\n    def f(self):\n        a()\n        b()\n"
	res, err := applyEdit(content, editRequest{Old: "            a()\n            b()", New: "            a()\n\n        def h(self):\n            b()"})
	if err != nil {
		t.Fatal(err)
	}
	want := "class C:\n    def f(self):\n        a()\n\n    def h(self):\n        b()\n"
	if res.Content != want {
		t.Fatalf("python reindent:\n%q\nwant\n%q", res.Content, want)
	}
	content = "func f() {\n\tif x {\n\t\ta()\n\t}\n}\n"
	res, err = applyEdit(content, editRequest{Old: "\t\t\ta()", New: "\t\t\ta()\n\t\t}\n\t\tb()"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "func f() {\n\tif x {\n\t\ta()\n\t}\n\tb()\n\t}\n}\n"; res.Content != want {
		t.Fatalf("tab reindent:\n%q\nwant\n%q", res.Content, want)
	}
}

// When no single mapping reproduces the file's indentation (the quote's
// relative indentation is wrong), unchanged lines keep the file's own
// indentation instead of being moved out of their block.
func TestLineTrimTierKeepsFileIndentationOfUnchangedLines(t *testing.T) {
	content := "def f():\n    if a:\n        b()\n        c()\n    return 1\n"
	res, err := applyEdit(content, editRequest{Old: "    if a:\n        b()\n    c()\n", New: "    if a:\n        b2()\n    c()\n"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "def f():\n    if a:\n        b2()\n        c()\n    return 1\n"; res.Content != want {
		t.Fatalf("got\n%q\nwant\n%q", res.Content, want)
	}
}

// Trimming old_string's padding widens the match; replace_all must not then
// rewrite every bare token (inside escapes included).
func TestTrimmedBoundaryIgnoresReplaceAll(t *testing.T) {
	content := "func f(n int) { fmt.Printf(\"%d\\n\", n); x := n+1 }"
	res, err := applyEdit(content, editRequest{Old: " n ", New: " count ", ReplaceAll: true})
	if err == nil {
		t.Fatalf("padded old_string rewrote every n (tier %d): %s", res.Tier, res.Content)
	}
	if strings.Contains(err.Error(), "replace_all") {
		t.Fatalf("the error must not suggest replace_all, which this tier ignores: %v", err)
	}
	// An inline fragment never starts right after a backslash (an escape).
	if res, err := applyEdit("a := \"x\\ny\"\n", editRequest{Old: " ny ", New: " z "}); err == nil {
		t.Fatalf("matched inside an escape sequence: %q", res.Content)
	}
}

// Similarity tiers never honour replace_all, so their ambiguity errors must
// not tell the model to set it.
func TestSimilarityTierAmbiguityDoesNotSuggestReplaceAll(t *testing.T) {
	content := "func a() {\n\tstart()\n\tfoo(1)\n\tend()\n}\n\nfunc b() {\n\tstart()\n\tfoo(2)\n\tend()\n}\n"
	_, err := applyEdit(content, editRequest{Old: "\tstart()\n\tfoo(3)\n\tend()", New: "\tstart()\n\tfoo(4)\n\tend()", ReplaceAll: true})
	if err == nil {
		t.Fatal("ambiguous similarity match accepted")
	}
	if strings.Contains(err.Error(), "replace_all") {
		t.Fatalf("error suggests replace_all: %v", err)
	}
}

// new_string lines that merely start with a number are content, not
// prefixes, when new_string was not written in the prefixed form.
func TestPrefixStrippingLeavesUnprefixedNewString(t *testing.T) {
	content := "# Title\n\nSteps:\n1. Install\n2. Run\n\nEnd\n"
	res, err := applyEdit(content, editRequest{Old: "4. 1. Install\n5. 2. Run", New: "1. Install deps\n2. Run tests\n3. Deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "# Title\n\nSteps:\n1. Install deps\n2. Run tests\n3. Deploy\n\nEnd\n"; res.Content != want {
		t.Fatalf("markdown list:\n%q\nwant\n%q", res.Content, want)
	}
	for _, n := range res.Notes {
		if strings.Contains(n, "file-read output") {
			t.Fatalf("note names the wrong source: %q", n)
		}
	}
	res, err = applyEdit("id\tname\n1\tAlice\n2\tBob\n", editRequest{Old: "     2\t1\tAlice\n     3\t2\tBob", New: "1\tAlicia\n2\tBob"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "id\tname\n1\tAlicia\n2\tBob\n"; res.Content != want {
		t.Fatalf("tsv:\n%q\nwant\n%q", res.Content, want)
	}
}

// A match that ends inside a one-to-many expansion ("…" read as "...") must
// not cut before the character and leave it behind.
func TestUnicodeTierRejectsPartialExpansion(t *testing.T) {
	content := "msg := \"Loading" + ellip + "\"\n"
	res, err := applyEdit(content, editRequest{Old: "\"Loading.", New: "\"Loading done."})
	if err == nil && strings.Contains(res.Content, ellip) {
		t.Fatalf("partial ellipsis match duplicated punctuation: %q", res.Content)
	}
}
