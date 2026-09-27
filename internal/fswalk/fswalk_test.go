package fswalk

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
		if _, ok := w.keepFile(path, d); !ok || w.Ignores.Ignored(path, false) {
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

// TestWalkBoundsReadAhead checks the read-ahead budget: with a consumer
// that stalls, the workers stop listing once maxAhead entries wait for it
// (plus at most one directory's worth per worker that was already
// listing), instead of listing the whole tree into memory.
func TestWalkBoundsReadAhead(t *testing.T) {
	root := t.TempDir()
	const dirs, perDir = 40, 30
	files := map[string]string{}
	for d := range dirs {
		for f := range perDir {
			files[filepath.Join(fmt.Sprintf("d%02d", d), fmt.Sprintf("f%02d", f))] = ""
		}
	}
	writeTree(t, root, files)
	const budget = 64
	defer func(old int) { maxAhead = old }(maxAhead)
	maxAhead = budget
	w := testWalker(root, false)
	n := 0
	r, err := w.walk(context.Background(), root, func(Entry) error {
		if n++; n == 1 {
			time.Sleep(50 * time.Millisecond) // let the workers run ahead
		}
		return nil
	})
	if err != nil || n != dirs*perDir {
		t.Fatalf("walk: err=%v visited=%d", err, n)
	}
	if limit := budget + DefaultWorkers*(perDir+1); r.peak > limit {
		t.Fatalf("read-ahead peaked at %d entries, want <= %d", r.peak, limit)
	}
}

// TestWalkReleasesConsumedListings checks that a finished walk leaves no
// entries reachable from its listings (the consumer clears them).
func TestWalkReleasesConsumedListings(t *testing.T) {
	root := randomWorkspace(t, rand.New(rand.NewSource(3)))
	r, err := testWalker(root, true).walk(context.Background(), root, func(Entry) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if r.ahead != 0 {
		t.Fatalf("%d entries still counted as waiting after the walk", r.ahead)
	}
	for _, l := range r.queue {
		if l.entries != nil {
			t.Fatalf("listing %s kept its entries", l.rel)
		}
	}
}

// TestReachableNeedsTheExactName: on a case-insensitive filesystem Lstat
// finds "SECRET" for a directory named "secret", but a walk from there
// would report paths .gitignore rules do not recognise.
func TestReachableNeedsTheExactName(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{".gitignore": "secret/\n", "sub/secret/s.go": "", "sub/open/o.go": ""})
	w := testWalker(root, false)
	for _, tc := range []struct {
		start string
		want  bool
	}{
		{"sub/open", true},
		{"sub/secret", false}, // ignored
		{"sub/SECRET", false}, // wrong case: missing, or not the listed name
		{"SUB/open", false},
		{"sub/Open", false},
	} {
		if got := w.Reachable(root, filepath.Join(root, filepath.FromSlash(tc.start))); got != tc.want {
			t.Errorf("Reachable(%s) = %v, want %v", tc.start, got, tc.want)
		}
	}
}

// TestIgnoreSetFromStopsAtTop: a set made with NewIgnoreSetFrom ignores the
// .gitignore files above its top; NewIgnoreSet applies them.
func TestIgnoreSetFromStopsAtTop(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "proj")
	writeTree(t, parent, map[string]string{".gitignore": "*.go\n", "proj/a.go": "", "proj/sub/.gitignore": "b.go\n", "proj/sub/b.go": "", "proj/sub/c.go": ""})
	walkNames := func(set *IgnoreSet) []string {
		var got []string
		w := Walker{Base: root, Ignores: set}
		_ = w.Walk(context.Background(), root, func(e Entry) error {
			got = append(got, e.Rel)
			return nil
		})
		return got
	}
	if got := walkNames(NewIgnoreSet()); len(got) != 1 || got[0] != "sub/.gitignore" {
		t.Errorf("NewIgnoreSet walk = %v, want only sub/.gitignore (the parent's *.go applies)", got)
	}
	want := []string{"a.go", "sub/.gitignore", "sub/c.go"}
	if got := walkNames(NewIgnoreSetFrom(root)); !reflect.DeepEqual(got, want) {
		t.Errorf("NewIgnoreSetFrom walk = %v, want %v", got, want)
	}
	if NewIgnoreSetFrom(root).Ignored(filepath.Join(root, "a.go"), false) {
		t.Error("Ignored applied the parent's .gitignore")
	}
}

// TestStatReportsASymlinkTarget: with Stat and SymlinkedFiles, a symlinked
// file's Info is its target's, so a change to the target changes it.
func TestStatReportsASymlinkTarget(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"real/b.go": "package real // longer than a link\n"})
	if err := os.Symlink(filepath.Join("real", "b.go"), filepath.Join(root, "link.go")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	w := Walker{Base: root, SymlinkedFiles: true, Stat: true}
	sizes := map[string]int64{}
	_ = w.Walk(context.Background(), root, func(e Entry) error {
		sizes[e.Rel] = e.Info.Size()
		return nil
	})
	if sizes["link.go"] != sizes["real/b.go"] || sizes["link.go"] == 0 {
		t.Fatalf("sizes = %v, want link.go reporting its target's size", sizes)
	}
}

// FileName filters files by name before anything else; directories are
// still descended into.
func TestFileNameFilter(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "", "b.txt": "", "sub/c.go": "", "sub/d.md": ""})
	w := Walker{Base: root, FileName: func(name string) bool { return filepath.Ext(name) == ".go" }}
	var got []string
	_ = w.Walk(context.Background(), root, func(e Entry) error {
		got = append(got, e.Rel)
		return nil
	})
	if want := []string{"a.go", "sub/c.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	var root2 []string
	_ = w.Walk(context.Background(), filepath.Join(root, "b.txt"), func(e Entry) error {
		root2 = append(root2, e.Rel)
		return nil
	})
	if len(root2) != 0 {
		t.Fatalf("a root file the filter rejects was visited: %v", root2)
	}
}
