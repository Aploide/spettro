package fswalk

import (
	"context"
	"errors"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// skipNames is the agent's skipDirs list, for tests.
var skipNames = map[string]bool{".git": true, ".spettro": true, "vendor": true, "node_modules": true, "dist": true, "build": true}

func testWalker(root string, symlinked bool) Walker {
	return Walker{Base: root, SkipDir: func(n string) bool { return skipNames[n] }, Ignores: NewIgnoreSet(), SymlinkedFiles: symlinked}
}

func writeTree(t testing.TB, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// sequentialWalk is the walker as it was before the parallel one (a
// filepath.WalkDir with the same filters), kept as the oracle for the
// parallel walker's order and filtering.
func sequentialWalk(w Walker, root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(w.Base, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if path == root {
				return nil
			}
			if w.skip(d.Name()) || w.Ignores.Ignored(path, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !w.keepFile(path, d) || w.Ignores.Ignored(path, false) {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	return out
}

// randomWorkspace writes a random tree with nested .gitignore files, skipDirs
// names, symlinks and names chosen to make walk order differ from plain
// string order ("a" < "a.go" < "a0", "a/b" before "a.go").
func randomWorkspace(t *testing.T, rng *rand.Rand) string {
	t.Helper()
	root := t.TempDir()
	names := []string{"a", "a.go", "a0", "b", "C", "build", "node_modules", "x.log", "keep.log", "z.txt", ".hidden", "sub"}
	files := map[string]string{}
	for range 150 {
		var parts []string
		for range 1 + rng.Intn(4) {
			parts = append(parts, names[rng.Intn(len(names))])
		}
		files[strings.Join(parts, "/")] = "match me\n"
	}
	// Drop files whose path is a directory of another file.
	for p := range files {
		for q := range files {
			if strings.HasPrefix(q, p+"/") {
				delete(files, p)
				break
			}
		}
	}
	writeTree(t, root, files)
	ignores := []string{"*.log\n!keep.log\n", "a0/\n", "/z.txt\n", "sub/a\n", "C\n"}
	writeTree(t, root, map[string]string{".gitignore": ignores[rng.Intn(len(ignores))]})
	for d := range files {
		if dir := filepath.Dir(filepath.FromSlash(d)); dir != "." && rng.Intn(6) == 0 {
			writeTree(t, root, map[string]string{filepath.ToSlash(filepath.Join(dir, ".gitignore")): ignores[rng.Intn(len(ignores))]})
		}
	}
	_ = os.Symlink("a.go", filepath.Join(root, "link.go"))
	_ = os.Symlink("sub", filepath.Join(root, "linkdir"))
	return root
}

// The parallel walker visits exactly what a sequential WalkDir walk with the
// same filters visits, in the same order.
func TestWalkMatchesSequentialOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := range 25 {
		root := randomWorkspace(t, rng)
		for _, symlinked := range []bool{false, true} {
			w := testWalker(root, symlinked)
			var got []string
			if err := w.Walk(context.Background(), root, func(e Entry) error {
				got = append(got, e.Rel)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			want := sequentialWalk(testWalker(root, symlinked), root)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("tree %d symlinked=%v:\nparallel   %v\nsequential %v", i, symlinked, got, want)
			}
		}
	}
}

func TestWalkStopsEarlyAndHonoursCancellation(t *testing.T) {
	root := randomWorkspace(t, rand.New(rand.NewSource(1)))
	w := testWalker(root, false)
	n := 0
	err := w.Walk(context.Background(), root, func(Entry) error {
		n++
		if n == 3 {
			return ErrStop
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("stop: err=%v visited=%d", err, n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Walk(ctx, root, func(Entry) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled walk returned %v", err)
	}
}

func TestCompareWalkOrder(t *testing.T) {
	ordered := []string{"B.go", "a/b.go", "a/c/d.go", "a.go", "a0.go", "b/x"}
	for i := range ordered {
		for j := range ordered {
			got := CompareWalkOrder(ordered[i], ordered[j])
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got != want {
				t.Errorf("CompareWalkOrder(%q, %q) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}
