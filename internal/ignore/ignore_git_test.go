package ignore

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestMatchAgainstGit is the differential test for the compiled matcher:
// random rule sets (a root and a nested .gitignore) over a random tree are
// matched by this package, layered the way the workspace walker layers them
// (deepest file first, an ignored parent directory ignores everything below
// it), and by `git check-ignore`. It runs when git is on PATH.
func TestMatchAgainstGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	rounds := 12
	if testing.Short() {
		rounds = 3
	}
	rng := rand.New(rand.NewSource(20260927))
	for round := range rounds {
		t.Run(fmt.Sprint(round), func(t *testing.T) { diffAgainstGit(t, rng) })
	}
}

var (
	gitNames = []string{"a", "b", "ab", "abc", "x.go", "y.log", "z.tmp", ".env", ".env.local", "build", "out", "docs", "keep.log", "t[1]", "q?", "x.tar.gz", "a b"}
	gitAtoms = []string{"a", "b", "ab", "abc", "x", "y", "build", "out", "docs", "log", "go", "env", "tmp", ".", "-", "_"}
)

// randomRule builds one .gitignore line from wildmatch syntax pieces.
func randomRule(rng *rand.Rand) string {
	var segs []string
	for n := 1 + rng.Intn(3); n > 0; n-- {
		var seg strings.Builder
		for k := 1 + rng.Intn(3); k > 0; k-- {
			switch rng.Intn(10) {
			case 0:
				seg.WriteString("*")
			case 1:
				seg.WriteString("?")
			case 2:
				seg.WriteString([]string{"[ab]", "[!a]", "[a-c]", "[[:alpha:]]", "[^x]"}[rng.Intn(5)])
			case 3:
				if k == 1 && seg.Len() == 0 {
					seg.WriteString("**")
					continue
				}
				seg.WriteString(".")
			default:
				seg.WriteString(gitAtoms[rng.Intn(len(gitAtoms))])
			}
		}
		segs = append(segs, seg.String())
	}
	line := strings.Join(segs, "/")
	if rng.Intn(5) == 0 {
		line = "/" + line
	}
	if rng.Intn(4) == 0 {
		line += "/"
	}
	if rng.Intn(4) == 0 {
		line = "!" + line
	}
	return line
}

// treeNames is gitNames minus the names the host cannot create: Windows
// reserves '?' in file names, so "q?" is only exercised elsewhere. Rules
// still use every atom, so wildmatch coverage of '?' is unchanged.
func treeNames() []string {
	if runtime.GOOS != "windows" {
		return gitNames
	}
	return slices.DeleteFunc(slices.Clone(gitNames), func(n string) bool {
		return strings.ContainsAny(n, `<>:"|?*`)
	})
}

// randomTree returns slash paths of a small random tree: files and the
// directories that hold them.
func randomTree(rng *rand.Rand) (files, dirs []string) {
	seenDir := map[string]bool{}
	for range 60 {
		depth := 1 + rng.Intn(4)
		var parts []string
		names := treeNames()
		for range depth {
			parts = append(parts, names[rng.Intn(len(names))])
		}
		p := strings.Join(parts, "/")
		if slices.Contains(dirs, p) || slices.Contains(files, p) {
			continue
		}
		clash := false
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if slices.Contains(files, d) {
				clash = true
			}
		}
		for _, f := range files {
			if strings.HasPrefix(f, p+"/") {
				clash = true
			}
		}
		if clash {
			continue
		}
		files = append(files, p)
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if !seenDir[d] {
				seenDir[d] = true
				dirs = append(dirs, d)
			}
		}
	}
	return files, dirs
}

func diffAgainstGit(t *testing.T, rng *rand.Rand) {
	root := t.TempDir()
	files, dirs := randomTree(rng)
	for _, f := range files {
		abs := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A root .gitignore plus one in a random directory.
	ignoreFiles := map[string]string{"": ""}
	if len(dirs) > 0 {
		ignoreFiles[dirs[rng.Intn(len(dirs))]] = ""
	}
	matchers := map[string]*Matcher{}
	for dir := range ignoreFiles {
		var lines []string
		for range 4 + rng.Intn(8) {
			lines = append(lines, randomRule(rng))
		}
		src := strings.Join(lines, "\n") + "\n"
		ignoreFiles[dir] = src
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(dir), ".gitignore"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		matchers[dir] = Parse(strings.NewReader(src))
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")

	all := append(append([]string{}, dirs...), files...)
	isDir := map[string]bool{}
	for _, d := range dirs {
		isDir[d] = true
	}
	gitIgnored := checkIgnoreGit(t, root, all)
	for _, p := range all {
		want := gitIgnored[p]
		got := layeredIgnored(matchers, p, isDir[p])
		if got != want {
			t.Errorf("%q (dir=%v): matcher says ignored=%v, git says %v\nignore files: %q", p, isDir[p], got, want, ignoreFiles)
		}
	}
}

// layeredIgnored applies the per-directory matchers the way the workspace
// walker does: an ignored ancestor directory ignores p, and otherwise the
// deepest .gitignore with a matching rule decides.
func layeredIgnored(matchers map[string]*Matcher, p string, dir bool) bool {
	var ancestors []string
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		ancestors = append([]string{d}, ancestors...)
	}
	for _, a := range ancestors {
		if levelIgnored(matchers, a, true) {
			return true
		}
	}
	return levelIgnored(matchers, p, dir)
}

func levelIgnored(matchers map[string]*Matcher, p string, dir bool) bool {
	for d := path.Dir(p); ; d = path.Dir(d) {
		key := d
		if d == "." {
			key = ""
		}
		if m, ok := matchers[key]; ok {
			rel := p
			if key != "" {
				rel = strings.TrimPrefix(p, key+"/")
			}
			if ig, matched := m.Match(rel, dir); matched {
				return ig
			}
		}
		if d == "." {
			return false
		}
	}
}

// checkIgnoreGit asks git which paths are ignored. --no-index ignores the
// (empty) index, and core.ignorecase=false keeps git case-sensitive like the
// matcher on every filesystem.
func checkIgnoreGit(t *testing.T, root string, paths []string) map[string]bool {
	t.Helper()
	cmd := exec.Command("git", "-c", "core.ignorecase=false", "check-ignore", "--no-index", "-v", "-n", "-z", "--stdin")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("git check-ignore: %v\n%s", err, stderr.String())
		}
	}
	// -z -v -n output: source NUL linenum NUL pattern NUL path NUL, per path.
	fields := strings.Split(strings.TrimSuffix(stdout.String(), "\x00"), "\x00")
	out := map[string]bool{}
	for i := 0; i+3 < len(fields); i += 4 {
		pattern, p := fields[i+2], fields[i+3]
		out[p] = pattern != "" && !strings.HasPrefix(pattern, "!")
	}
	if len(out) != len(paths) {
		t.Fatalf("git check-ignore answered %d of %d paths", len(out), len(paths))
	}
	return out
}
