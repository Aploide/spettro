package ignore

import (
	"os"
	"path/filepath"
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

func TestParsePattern(t *testing.T) {
	if _, ok := parsePattern(""); ok {
		t.Error("empty line must not produce a pattern")
	}
	if _, ok := parsePattern("# comment"); ok {
		t.Error("comment must not produce a pattern")
	}
	p, ok := parsePattern("!logs/")
	if !ok || !p.negate || !p.dirOnly {
		t.Errorf("!logs/ parsed wrong: %+v", p)
	}
	p, _ = parsePattern("/src/gen")
	if !p.rooted || p.glob != "src/gen" {
		t.Errorf("/src/gen parsed wrong: %+v", p)
	}
	p, _ = parsePattern(`\#literal`)
	if p.glob != "#literal" || p.negate {
		t.Errorf("escaped hash parsed wrong: %+v", p)
	}
}

func TestMissingGitignore(t *testing.T) {
	m := NewMatcher(t.TempDir())
	if m.Ignored("anything.log", false) {
		t.Error("matcher with no .gitignore must ignore nothing")
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"exact", "exact", true},
		{"exact", "dir/exact", true}, // basename suffix match
		{"exact", "notexact", false},
		{"*.go", "main.go", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/c", false},
		{"src/**", "src/deep/file", true},
		{"[", "anything", false}, // invalid pattern
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.path); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}
