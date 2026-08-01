package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanRepoFilesSkipsNodeModules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "node_modules", "pkg", "index.js"))
	writeFile(t, filepath.Join(root, "src", "main.go"))

	entries, err := scanRepoFiles(root)
	if err != nil {
		t.Fatalf("scanRepoFiles: %v", err)
	}
	if !slices.Contains(entries, "src/main.go") {
		t.Errorf("expected src/main.go in entries, got %v", entries)
	}
	for _, e := range entries {
		if e == "node_modules/" || strings.HasPrefix(e, "node_modules/") {
			t.Errorf("node_modules should be skipped, found %q", e)
		}
	}
}

func TestScanRepoFilesRespectsGitignore(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("dist/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "dist", "bundle.js"))
	writeFile(t, filepath.Join(root, "main.go"))

	entries, err := scanRepoFiles(root)
	if err != nil {
		t.Fatalf("scanRepoFiles: %v", err)
	}
	if !slices.Contains(entries, "main.go") {
		t.Errorf("expected main.go in entries, got %v", entries)
	}
	if slices.Contains(entries, "dist/") || slices.Contains(entries, "dist/bundle.js") {
		t.Errorf("gitignored dist should be excluded, got %v", entries)
	}
}

func TestScanRepoFilesCapsCollectedEntries(t *testing.T) {
	origEntries := scanMaxEntries
	defer func() { scanMaxEntries = origEntries }()
	scanMaxEntries = 5

	root := t.TempDir()
	for i := range 20 {
		writeFile(t, filepath.Join(root, fmt.Sprintf("file%02d.txt", i)))
	}

	entries, err := scanRepoFiles(root)
	if err != nil {
		t.Fatalf("scanRepoFiles: %v", err)
	}
	if len(entries) > scanMaxEntries {
		t.Errorf("expected at most %d entries, got %d", scanMaxEntries, len(entries))
	}
}

func TestRepoFilesScannedMsgUpdatesModel(t *testing.T) {
	m := NewModelForTesting()
	if m.repoFiles != nil {
		t.Fatalf("expected no repo files before scan, got %v", m.repoFiles)
	}

	updated, _ := m.Update(repoFilesScannedMsg{files: []string{"a.go", "src/", "src/b.go"}})
	nm := updated.(Model)
	if !slices.Equal(nm.repoFiles, []string{"a.go", "src/", "src/b.go"}) {
		t.Fatalf("repoFiles not set from msg: %v", nm.repoFiles)
	}
}

func TestScanRepoFilesCapsVisitedPaths(t *testing.T) {
	origVisited := scanMaxVisited
	defer func() { scanMaxVisited = origVisited }()
	scanMaxVisited = 5

	root := t.TempDir()
	for i := range 20 {
		writeFile(t, filepath.Join(root, fmt.Sprintf("file%02d.txt", i)))
	}

	entries, err := scanRepoFiles(root)
	if err != nil {
		t.Fatalf("scanRepoFiles: %v", err)
	}
	if len(entries) >= 20 {
		t.Errorf("visited cap should stop the walk early, got %d entries", len(entries))
	}
}
