package agent

import (
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

// oracleMatchGlobPattern is the glob matcher globMatchSegs replaced
// (split both sides, recurse on slices), kept as the oracle of the
// differential tests below.
func oracleMatchGlobPattern(pattern, rel string) bool {
	return oracleGlobMatch(strings.Split(pattern, "/"), strings.Split(rel, "/"))
}

func oracleGlobMatch(patParts, pathParts []string) bool {
	if len(patParts) == 0 && len(pathParts) == 0 {
		return true
	}
	if len(patParts) == 0 {
		return false
	}
	if patParts[0] == "**" {
		restPat := patParts[1:]
		if oracleGlobMatch(restPat, pathParts) {
			return true
		}
		for i := 1; i <= len(pathParts); i++ {
			if oracleGlobMatch(restPat, pathParts[i:]) {
				return true
			}
		}
		return false
	}
	if len(pathParts) == 0 {
		return false
	}
	matched, err := filepath.Match(patParts[0], pathParts[0])
	if err != nil || !matched {
		return false
	}
	return oracleGlobMatch(patParts[1:], pathParts[1:])
}

// isWalkPath reports whether rel is a path a workspace walk can produce:
// non-empty slash-separated segments. Only those reach globMatchSegs.
func isWalkPath(rel string) bool {
	if rel == "" {
		return false
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "" {
			return false
		}
	}
	return true
}

func checkGlobAgainstOracle(t *testing.T, pattern, rel string) {
	t.Helper()
	got := compileGlobs([]string{pattern})[0].match(rel)
	if want := oracleMatchGlobPattern(pattern, rel); got != want {
		t.Fatalf("glob %q on %q: got %v, oracle %v", pattern, rel, got, want)
	}
}

var globSeedPatterns = []string{
	"*", "**", "**/*.go", "*.go", "a/**", "a/**/b", "**/b/**", "a/*/c", "a/**/**/c",
	"[ab]/*.go", "a?/b", `a\*b`, "[", "a/[", "**/", "/a", "a//b", "a/**/", "{x}", "**/*_test.go",
}

var globSeedPaths = []string{
	"a", "a.go", "a/b", "a/b/c", "a/x/c", "b/a.go", "ab/b", "a*b", "a/b/b/c", "x_test.go", "d/e/x_test.go", "[", "a/[",
}

// TestGlobMatchSegsMatchesOracle runs every seed pattern against every seed
// path plus a few thousand random pairs.
func TestGlobMatchSegsMatchesOracle(t *testing.T) {
	for _, p := range globSeedPatterns {
		for _, rel := range globSeedPaths {
			checkGlobAgainstOracle(t, p, rel)
		}
	}
	rng := rand.New(rand.NewSource(11))
	segs := []string{"a", "b", "c", "*", "**", "?", "a*", "*.go", "[ab]", "[!a]", "x.go", "b.go"}
	pathSegs := []string{"a", "b", "c", "x.go", "b.go", "ab", "a.go"}
	join := func(from []string, n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = from[rng.Intn(len(from))]
		}
		return strings.Join(parts, "/")
	}
	for range 5000 {
		checkGlobAgainstOracle(t, join(segs, 1+rng.Intn(5)), join(pathSegs, 1+rng.Intn(5)))
	}
}

// FuzzGlobMatchSegs compares the allocation-free matcher with the oracle on
// arbitrary patterns and walk paths:
//
//	go test ./internal/agent -run '^$' -fuzz FuzzGlobMatchSegs
func FuzzGlobMatchSegs(f *testing.F) {
	for _, p := range globSeedPatterns {
		for _, rel := range globSeedPaths {
			f.Add(p, rel)
		}
	}
	f.Fuzz(func(t *testing.T, pattern, rel string) {
		if !isWalkPath(rel) {
			t.Skip()
		}
		checkGlobAgainstOracle(t, pattern, rel)
	})
}
