package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/indexer"
)

func TestRepoSearchSymbolDefinitionsFirst(t *testing.T) {
	dir := t.TempDir()
	src := "package pkg\n\nfunc StartServer() {}\n"
	use := "package pkg\n\nfunc run() { StartServer() }\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte(use), 0o644); err != nil {
		t.Fatal(err)
	}
	s := RepoSearcher{Index: indexer.NewSymbolIndex(dir, "")}
	out, err := s.Search(context.Background(), dir, "StartServer")
	if err != nil {
		t.Fatal(err)
	}
	defIdx := strings.Index(out, "a.go:3  func StartServer")
	useIdx := strings.Index(out, "b.go:3")
	if defIdx < 0 {
		t.Fatalf("definition line missing in output:\n%s", out)
	}
	if useIdx < 0 {
		t.Fatalf("usage match missing in output:\n%s", out)
	}
	if defIdx > useIdx {
		t.Fatalf("definition not ranked before usage:\n%s", out)
	}
}

func TestRepoSearchNonIdentifierFallsBackToGrep(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package pkg // hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := RepoSearcher{Index: indexer.NewSymbolIndex(dir, "")}
	out, err := s.Search(context.Background(), dir, "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "definitions:") {
		t.Fatalf("phrase query should not hit the symbol index:\n%s", out)
	}
	if !strings.Contains(out, "a.go:1") {
		t.Fatalf("grep match missing:\n%s", out)
	}
}

func TestRepoSearchWithoutIndexUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := RepoSearcher{}.Search(context.Background(), dir, "pkg")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "definitions:") || !strings.Contains(out, "a.go:1") {
		t.Fatalf("zero-value searcher behavior changed:\n%s", out)
	}
}
