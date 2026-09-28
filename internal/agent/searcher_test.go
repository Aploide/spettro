package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// When the index was cut short, the symbol search says so even when it
// found no definition: the definition may be in the files never indexed.
func TestRepoSearchReportsATruncatedIndexWithoutDefinitions(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go": "package pkg\n\nfunc Early() {}\n",
		"z.go": "package pkg\n\nfunc LateDefinition() {}\n\nvar _ = LateDefinition\n",
	})
	idx := indexer.NewSymbolIndex(dir, "")
	idx.SetLimits(1, time.Minute)
	out, err := RepoSearcher{Index: idx}.Search(context.Background(), dir, "LateDefinition")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, `(no definitions of "LateDefinition" in the symbol index, which stopped at 1 source files`) {
		t.Fatalf("no truncation notice:\n%s", out)
	}
	if !strings.Contains(out, "z.go:3:") {
		t.Fatalf("usages missing:\n%s", out)
	}
	out, _ = RepoSearcher{Index: idx}.Search(context.Background(), dir, "Early")
	if !strings.Contains(out, "(the symbol index stopped at 1 source files") {
		t.Fatalf("no truncation notice under the definitions:\n%s", out)
	}
}

// The files the usage grep matched are re-checked before the definitions
// are listed, so an edit made outside Spettro right after the index synced
// shows at once (no stale line numbers, no vanished definitions).
func TestRepoSearchSeesOutsideEditsOfMatchedFiles(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.go": "package pkg\n\nfunc StartServer() {}\n"})
	s := RepoSearcher{Index: indexer.NewSymbolIndex(dir, "")}
	if out, _ := s.Search(context.Background(), dir, "StartServer"); !strings.Contains(out, "a.go:3  func StartServer") {
		t.Fatalf("first search:\n%s", out)
	}
	writeTree(t, dir, map[string]string{
		"a.go": "package pkg\n\n// moved down\n\nfunc StartServer() {}\n",
		"b.go": "package pkg\n\nfunc StartServerLater() {}\n",
	})
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "a.go"), future, future); err != nil {
		t.Fatal(err)
	}
	out, err := s.Search(context.Background(), dir, "StartServer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 definitions:\na.go:5  func StartServer") || !strings.Contains(out, "b.go:3  func StartServerLater") {
		t.Fatalf("definitions not current after an outside edit:\n%s", out)
	}
}
