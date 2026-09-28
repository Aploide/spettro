package ignore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestMatcher(t *testing.T, gitignore string) *Matcher {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewMatcher(root)
}

func TestIgnored(t *testing.T) {
	m := newTestMatcher(t, `
# comment line
*.log
build/
/rooted.txt
node_modules
docs/**/*.tmp
!keep.log
`)
	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"app.log", false, true},
		{"nested/deep/app.log", false, true},
		{"app.log.bak", false, false},
		{"build", true, true},
		{"build", false, false}, // dir-only pattern must not match files
		{"rooted.txt", false, true},
		{"node_modules", true, true},
		{"vendor/node_modules", true, true},
		{"docs/a/b/c.tmp", false, true},
		{"docs/c.tmp", false, true}, // ** matches zero segments
		{"other/c.tmp", false, false},
		{"keep.log", false, false}, // negated
		{"README.md", false, false},
	}
	for _, c := range cases {
		if got := m.Ignored(c.path, c.isDir); got != c.want {
			t.Errorf("Ignored(%q, isDir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

func TestNegationOrderMatters(t *testing.T) {
	m := newTestMatcher(t, "!important.log\n*.log\n")
	// The later *.log rule re-ignores the file: last match wins.
	if !m.Ignored("important.log", false) {
		t.Error("later ignore rule must override earlier negation")
	}
}

// The last matching rule wins across the indexed and the scanned kinds.
func TestLastRuleWinsAcrossKinds(t *testing.T) {
	m := newTestMatcher(t, "*.log\n!a*.log\nabc.log\n!/abc.log\n")
	cases := map[string]bool{
		"x.log":       true,  // *.log
		"a1.log":      false, // !a*.log comes later
		"abc.log":     false, // !/abc.log is the last match
		"sub/abc.log": true,  // the anchored negation does not reach here
	}
	for p, want := range cases {
		if got := m.Ignored(p, false); got != want {
			t.Errorf("Ignored(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestParseRule(t *testing.T) {
	if _, ok := parseRule(""); ok {
		t.Error("empty line must not produce a rule")
	}
	if _, ok := parseRule("# comment"); ok {
		t.Error("comment must not produce a rule")
	}
	r, ok := parseRule("!logs/")
	if !ok || !r.negate || !r.dirOnly || r.kind != kindBaseLiteral {
		t.Errorf("!logs/ parsed wrong: %+v", r)
	}
	r, _ = parseRule("/src/gen")
	if r.kind != kindPathLiteral || r.pattern != "src/gen" {
		t.Errorf("/src/gen parsed wrong: %+v", r)
	}
	r, _ = parseRule(`\#literal`)
	if r.negate || !Wildmatch(r.pattern, "#literal") {
		t.Errorf("escaped hash parsed wrong: %+v", r)
	}
	r, _ = parseRule("*.go")
	if r.kind != kindBaseExt {
		t.Errorf("*.go parsed wrong: %+v", r)
	}
	r, _ = parseRule("*.tar.gz")
	if r.kind != kindBaseSuffix {
		t.Errorf("*.tar.gz parsed wrong: %+v", r)
	}
	r, _ = parseRule("a/b*/c")
	if r.kind != kindPathGlob || r.prefix != "a/" {
		t.Errorf("a/b*/c parsed wrong: %+v", r)
	}
	r, _ = parseRule("trailing   ")
	if r.pattern != "trailing" {
		t.Errorf("trailing spaces kept: %q", r.pattern)
	}
	r, _ = parseRule(`escaped\ `)
	if !Wildmatch(r.pattern, "escaped ") {
		t.Errorf("escaped trailing space dropped: %q", r.pattern)
	}
}

func TestMissingGitignore(t *testing.T) {
	m := NewMatcher(t.TempDir())
	if m.Ignored("anything.log", false) {
		t.Error("matcher with no .gitignore must ignore nothing")
	}
}

func TestLoadAndMatch(t *testing.T) {
	dir := t.TempDir()
	if Load(filepath.Join(dir, ".gitignore")) != nil {
		t.Fatal("missing file must load as nil")
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n!keep.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := Load(filepath.Join(dir, ".gitignore"))
	if ig, ok := m.Match("a.log", false); !ig || !ok {
		t.Fatalf("a.log: ignored=%v matched=%v", ig, ok)
	}
	if ig, ok := m.Match("keep.log", false); ig || !ok {
		t.Fatalf("keep.log: ignored=%v matched=%v", ig, ok)
	}
	if _, ok := m.Match("a.go", false); ok {
		t.Fatal("a.go must not match any rule")
	}
}

func TestParseSkipsBOMAndCRLF(t *testing.T) {
	m := Parse(strings.NewReader("\xef\xbb\xbf*.log\r\nbuild/\r\n"))
	if !m.Ignored("a.log", false) || !m.Ignored("build", true) {
		t.Fatalf("BOM/CRLF file not applied: %q", m.String())
	}
}

// A pattern with a slash is anchored to the .gitignore's directory.
func TestRootedLiteralPatternIsAnchored(t *testing.T) {
	m := newTestMatcher(t, "/secret.txt\ndocs/out\n")
	cases := map[string]bool{
		"secret.txt":      true,
		"deep/secret.txt": false,
		"docs/out":        true,
		"x/docs/out":      false,
	}
	for p, want := range cases {
		if got := m.Ignored(p, false); got != want {
			t.Errorf("Ignored(%q) = %v, want %v", p, got, want)
		}
	}
}

// Cases from git's t/t3070-wildmatch.sh (the pathname-matching column).
func TestWildmatch(t *testing.T) {
	cases := []struct {
		pattern, text string
		want          bool
	}{
		{"foo", "foo", true},
		{"bar", "foo", false},
		{"", "", true},
		{"???", "foo", true},
		{"??", "foo", false},
		{"*", "foo", true},
		{"f*", "foo", true},
		{"*f", "foo", false},
		{"*foo*", "foo", true},
		{"*ob*a*r*", "foobar", true},
		{"*ab", "aaaaaaabababab", true},
		{`foo\*`, "foo*", true},
		{`foo\*bar`, "foobar", false},
		{`f\\oo`, `f\oo`, true},
		{"*[al]?", "ball", true},
		{"[ten]", "ten", false},
		{"**[!te]", "ten", true},
		{"**[!ten]", "ten", false},
		{"t[a-g]n", "ten", true},
		{"t[!a-g]n", "ten", false},
		{"t[!a-g]n", "ton", true},
		{"t[^a-g]n", "ton", true},
		{"a[]]b", "a]b", true},
		{"a[]-]b", "a-b", true},
		{"a[]-]b", "a]b", true},
		{"a[]-]b", "aab", false},
		{"a[]a-]b", "aab", true},
		{"]", "]", true},
		{"foo*bar", "foo/baz/bar", false},
		{"foo**bar", "foo/baz/bar", false},
		{"foo**bar", "foobazbar", true},
		{"foo/**/bar", "foo/baz/bar", true},
		{"foo/**/**/bar", "foo/baz/bar", true},
		{"foo/**/bar", "foo/b/a/z/bar", true},
		{"foo/**/**/bar", "foo/b/a/z/bar", true},
		{"foo/**/bar", "foo/bar", true},
		{"foo/**/**/bar", "foo/bar", true},
		{"foo?bar", "foo/bar", false},
		{"foo[/]bar", "foo/bar", false},
		{"foo[^a-z]bar", "foo/bar", false},
		{"f[^eiu][^eiu][^eiu][^eiu][^eiu]r", "foo/bar", false},
		{"f[^eiu][^eiu][^eiu][^eiu][^eiu]r", "foo-bar", true},
		{"**/foo", "foo", true},
		{"**/foo", "XXX/foo", true},
		{"**/foo", "bar/baz/foo", true},
		{"*/foo", "bar/baz/foo", false},
		{"**/bar*", "foo/bar/baz", false},
		{"**/bar/*", "deep/foo/bar/baz", true},
		{"**/bar/*", "deep/foo/bar/baz/", false},
		{"**/bar/**", "deep/foo/bar/baz/", true},
		{"**/bar/*", "deep/foo/bar", false},
		{"**/bar/**", "deep/foo/bar/", true},
		{"**/bar**", "foo/bar/baz", false},
		{"*/bar/**", "foo/bar/baz/x", true},
		{"*/bar/**", "deep/foo/bar/baz/x", false},
		{"**/bar/*/*", "deep/foo/bar/baz/x", true},
		{"a[c-c]st", "acrt", false},
		{"a[c-c]rt", "acrt", true},
		{"[!]-]", "]", false},
		{"[!]-]", "a", true},
		{`\`, "", false},
		{`*/\`, `XXX/\`, false},
		{`*/\\`, `XXX/\`, true},
		{"foo", "foo", true},
		{"@foo", "@foo", true},
		{"@foo", "foo", false},
		{`\[ab]`, "[ab]", true},
		{"[[]ab]", "[ab]", true},
		{"[[:]ab]", "[ab]", true},
		{"[[::]ab]", "[ab]", false},
		{"[[:digit]ab]", "[ab]", true},
		{`[\[:]ab]`, "[ab]", true},
		{`\??\?b`, "?a?b", true},
		{`\a\b\c`, "abc", true},
		{"", "foo", false},
		{"**/t[o]", "foo/bar/baz/to", true},
		{"[[:alpha:]][[:digit:]][[:upper:]]", "a1B", true},
		{"[[:digit:][:upper:][:space:]]", "a", false},
		{"[[:digit:][:upper:][:space:]]", "A", true},
		{"[[:digit:][:upper:][:space:]]", "1", true},
		{"[[:digit:][:upper:][:spaci:]]", "1", false},
		{"[[:space:]]", " ", true},
		{"[[:xdigit:]]", "5", true},
		{"[[:xdigit:]]", "f", true},
		{"[[:xdigit:]]", "D", true},
		{"[a-c[:digit:]x-z]", "5", true},
		{"[a-c[:digit:]x-z]", "b", true},
		{"[a-c[:digit:]x-z]", "y", true},
		{"[a-c[:digit:]x-z]", "q", false},
		{`[\\-^]`, "]", true},
		{`[\\-^]`, "[", false},
		{`[\-_]`, "-", true},
		{`[\]]`, "]", true},
		{`[\]]`, `\]`, false},
		{`[\]]`, `\`, false},
		{"a[]b", "ab", false},
		{"ab[", "ab[", false},
		{"[!", "ab", false},
		{"[-", "ab", false},
		{"[-]", "-", true},
		{"[a-", "-", false},
		{"[!a-", "-", false},
		{"[--A]", "-", true},
		{"[--A]", "5", true},
		{"[ --]", " ", true},
		{"[ --]", "$", true},
		{"[ --]", "-", true},
		{"[ --]", "0", false},
		{"[---]", "-", true},
		{"[------]", "-", true},
		{"[a-e-n]", "j", false},
		{"[a-e-n]", "-", true},
		{"[!------]", "a", true},
		{"[]-a]", "[", false},
		{"[]-a]", "^", true},
		{"[!]-a]", "^", false},
		{"[!]-a]", "[", true},
		{"[a^bc]", "^", true},
		{"[a-]b]", "-b]", true},
		{`[\]`, `\`, false},
		{`[\\]`, `\`, true},
		{`[!\\]`, `\`, false},
		{`[A-\\]`, "G", true},
		{"b*a", "aaabbb", false},
		{"*ba*", "aabcaa", false},
		{"[,]", ",", true},
		{`[\\,]`, ",", true},
		{`[\\,]`, `\`, true},
		{"[,-.]", "-", true},
		{"[,-.]", "+", false},
		{"[,-.]", "-.]", false},
		{`[\1-\3]`, "2", true},
		{`[\1-\3]`, "3", true},
		{`[\1-\3]`, "4", false},
		{`[[-\]]`, `\`, true},
		{`[[-\]]`, "[", true},
		{`[[-\]]`, "]", true},
		{`[[-\]]`, "-", false},
		{"-*-*-*-*-*-*-12-*-*-*-m-*-*-*", "-adobe-courier-bold-o-normal--12-120-75-75-m-70-iso8859-1", true},
		{"-*-*-*-*-*-*-12-*-*-*-m-*-*-*", "-adobe-courier-bold-o-normal--12-120-75-75-X-70-iso8859-1", false},
		{"-*-*-*-*-*-*-12-*-*-*-m-*-*-*", "-adobe-courier-bold-o-normal--12-120-75-75-/-70-iso8859-1", false},
		{"XXX/*/*/*/*/*/*/12/*/*/*/m/*/*/*", "XXX/adobe/courier/bold/o/normal//12/120/75/75/m/70/iso8859/1", true},
		{"XXX/*/*/*/*/*/*/12/*/*/*/m/*/*/*", "XXX/adobe/courier/bold/o/normal//12/120/75/75/X/70/iso8859/1", false},
		{"**/*a*b*g*n*t", "abcd/abcdefg/abcdefghijk/abcdefghijklmnop.txt", true},
		{"**/*a*b*g*n*t", "abcd/abcdefg/abcdefghijk/abcdefghijklmnop.txtz", false},
		{"*/*/*", "foo", false},
		{"*/*/*", "foo/bar", false},
		{"*/*/*", "foo/bba/arr", true},
		{"*/*/*", "foo/bb/aa/rr", false},
		{"**/**/**", "foo/bb/aa/rr", true},
		{"*X*i", "abcXdefXghi", true},
		{"*X*i", "ab/cXd/efXg/hi", false},
		{"*/*X*/*/*i", "ab/cXd/efXg/hi", true},
		{"**/*X*/**/*i", "ab/cXd/efXg/hi", true},
	}
	for _, c := range cases {
		if got := Wildmatch(c.pattern, c.text); got != c.want {
			t.Errorf("Wildmatch(%q, %q) = %v, want %v", c.pattern, c.text, got, c.want)
		}
	}
}

// Match is on the walker's hot path (once per file and directory level): it
// must not allocate. Deterministic guard for the E2 matcher rework.
func TestMatchDoesNotAllocate(t *testing.T) {
	m := Parse(strings.NewReader(benchGitignore))
	paths := []string{"src/pkg/file.go", "a/b/c/d.log", "node_modules", "docs/_build", "x/testdata/fixtures/a.golden", "keep.env.example"}
	allocs := testing.AllocsPerRun(200, func() {
		for _, p := range paths {
			m.Match(p, false)
			m.Match(p, true)
		}
	})
	if allocs != 0 {
		t.Fatalf("Match allocates %.1f times per run, want 0", allocs)
	}
}
