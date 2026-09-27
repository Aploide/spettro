package testhome

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// pureTestPackages lists the test packages (by directory, relative to the
// module root) that may run without Main, each with the reason it cannot
// reach per-user files: no dependency on homedir, config, storage or any
// package that does, and no child processes. Keep the list short; when in
// doubt, add the two-line TestMain instead.
var pureTestPackages = map[string]string{
	"internal/conversation": "pure message-slice helpers",
	"internal/diff":         "pure diff rendering (termtext, theme)",
	"internal/fsperm":       "file-mode constants only",
	"internal/mcp":          "reads servers.json from the test's own workspace",
	"internal/theme":        "pure colour palettes",
	"internal/ui":           "pure rendering helpers (theme, version)",
	"internal/version":      "build version string",
	"tests/mcp":             "reads servers.json from the test's own workspace",
}

// TestEveryTestPackageIsolatesHome walks the module and fails for any
// package directory with tests that neither runs them under Main (and
// carries the Check guard) nor is listed in pureTestPackages. It is how a new
// test package that forgets the isolation is caught before it rewrites a
// developer's real ~/.spettro. Nested modules (their own go.mod) and hidden
// and testdata directories are skipped.
func TestEveryTestPackageIsolatesHome(t *testing.T) {
	root := moduleRoot(t)
	var missing []string
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root {
			if strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		hasTests, isolated, err := scanTestFiles(path)
		if err != nil || !hasTests || rel == "internal/testhome" {
			return err
		}
		seen[rel] = true
		if _, pure := pureTestPackages[rel]; !pure && !isolated {
			missing = append(missing, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(missing)
	for _, rel := range missing {
		t.Errorf("%s: its tests can run with the real HOME; add a _test.go file with\n"+
			"\tfunc TestMain(m *testing.M) { os.Exit(testhome.Main(m)) }\n"+
			"\tfunc TestHomeIsIsolated(t *testing.T) { testhome.Check(t) }\n"+
			"or, only if it can never reach per-user files, list it in pureTestPackages", rel)
	}
	for rel := range pureTestPackages {
		if !seen[rel] {
			t.Errorf("pureTestPackages lists %s, which has no tests any more; remove the entry", rel)
		}
	}
}

// scanTestFiles reports whether dir holds _test.go files and whether they
// both call testhome.Main and carry the testhome.Check guard.
func scanTestFiles(dir string) (hasTests, isolated bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, false, err
	}
	var main, check bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		hasTests = true
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return false, false, err
		}
		main = main || strings.Contains(string(src), "testhome.Main(")
		check = check || strings.Contains(string(src), "testhome.Check(")
	}
	return hasTests, main && check, nil
}

// moduleRoot returns the directory holding the go.mod above the package
// directory the test runs in.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's directory")
		}
		dir = parent
	}
}
