package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t testing.TB, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixtureRepo(t testing.TB) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "pkg/server.go", `package pkg

type Server struct{}

func NewServer() *Server { return &Server{} }

func (s *Server) Start() error { return nil }

const ServerVersion = "1.0"
`)
	writeFile(t, root, "app/main.py", `class ServerPool:
    def start(self):
        pass

def new_server():
    return ServerPool()
`)
	writeFile(t, root, "web/index.ts", `export function startServer(port: number) {}
export const serverName = "x"
interface ServerConfig { port: number }
`)
	writeFile(t, root, "ignored/gen.go", "package gen\n\nfunc Server() {}\n")
	writeFile(t, root, ".gitignore", "ignored/\n")
	return root
}

func TestLookupRanksDefinitionsFirst(t *testing.T) {
	x := NewSymbolIndex(fixtureRepo(t), "")
	syms := x.Lookup(context.Background(), "Server")
	if len(syms) == 0 {
		t.Fatal("no symbols found")
	}
	// Exact match first.
	if syms[0].Name != "Server" || syms[0].Kind != "type" || syms[0].Path != "pkg/server.go" {
		t.Fatalf("expected exact type Server first, got %+v", syms[0])
	}
	// Gitignored file must not appear.
	for _, s := range syms {
		if s.Path == "ignored/gen.go" {
			t.Fatalf("gitignored file was indexed: %+v", s)
		}
	}
	// Prefix matches (ServerVersion, ServerPool, ServerConfig, serverName)
	// come before pure substring matches (NewServer, startServer, new_server).
	seenSubstring := false
	for _, s := range syms[1:] {
		hasPrefix := len(s.Name) >= 6 && (s.Name[:6] == "Server" || s.Name[:6] == "server")
		if !hasPrefix {
			seenSubstring = true
		} else if seenSubstring {
			t.Fatalf("prefix match %q ranked after a substring match", s.Name)
		}
	}
}

func TestLookupAcrossLanguages(t *testing.T) {
	x := NewSymbolIndex(fixtureRepo(t), "")
	want := map[string]string{ // name -> expected kind
		"ServerPool":   "class",
		"new_server":   "func",
		"startServer":  "func",
		"ServerConfig": "type",
		"NewServer":    "func",
		"Start":        "method",
	}
	for name, kind := range want {
		syms := x.Lookup(context.Background(), name)
		if len(syms) == 0 {
			t.Errorf("no result for %q", name)
			continue
		}
		if syms[0].Name != name || syms[0].Kind != kind {
			t.Errorf("%q: got %s %s, want kind %s", name, syms[0].Kind, syms[0].Name, kind)
		}
	}
}

func TestInvalidationAfterEdit(t *testing.T) {
	root := fixtureRepo(t)
	x := NewSymbolIndex(root, "")
	if syms := x.Lookup(context.Background(), "NewServer"); len(syms) == 0 {
		t.Fatal("NewServer not indexed")
	}
	// Rewrite the file with a renamed function; force mtime-independent
	// invalidation the way the agent's write tools do.
	writeFile(t, root, "pkg/server.go", "package pkg\n\nfunc BuildServer() {}\n")
	x.Invalidate("pkg/server.go")
	if syms := x.Lookup(context.Background(), "NewServer"); len(syms) != 0 {
		t.Fatalf("stale NewServer still indexed: %+v", syms)
	}
	if syms := x.Lookup(context.Background(), "BuildServer"); len(syms) != 1 {
		t.Fatalf("BuildServer not found after edit: %+v", syms)
	}
}

func TestMtimeInvalidationWithoutExplicitCall(t *testing.T) {
	root := fixtureRepo(t)
	x := NewSymbolIndex(root, "")
	x.Lookup(context.Background(), "NewServer")
	path := filepath.Join(root, "pkg", "server.go")
	if err := os.WriteFile(path, []byte("package pkg\n\nfunc Other() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Push mtime clearly past the indexed one for coarse-mtime filesystems.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if syms := x.Lookup(context.Background(), "NewServer"); len(syms) != 0 {
		t.Fatalf("stale symbol survived mtime change: %+v", syms)
	}
}

func TestCachePersistsAcrossInstances(t *testing.T) {
	root := fixtureRepo(t)
	cache := filepath.Join(root, ".spettro", "cache", "symbols.json")
	x := NewSymbolIndex(root, cache)
	x.Lookup(context.Background(), "Server")
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	y := NewSymbolIndex(root, cache)
	if syms := y.Lookup(context.Background(), "NewServer"); len(syms) == 0 {
		t.Fatal("cached index returned nothing")
	}
}

// BenchmarkLookup1kFiles verifies the acceptance bound: sub-second queries on
// a ~1k-file repo (steady state, after the first lazy build).
func BenchmarkLookup1kFiles(b *testing.B) {
	root := b.TempDir()
	for i := range 1000 {
		writeFile(b, root, fmt.Sprintf("pkg%d/file%d.go", i%50, i),
			fmt.Sprintf("package pkg%d\n\nfunc Handler%d() {}\n\ntype Widget%d struct{}\n", i%50, i, i))
	}
	x := NewSymbolIndex(root, "")
	x.Lookup(context.Background(), "Handler1") // build once
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if syms := x.Lookup(context.Background(), "Handler500"); len(syms) == 0 {
			b.Fatal("no result")
		}
	}
	b.StopTimer()
	if avg := b.Elapsed() / time.Duration(b.N); avg > time.Second {
		b.Fatalf("query too slow: %v per lookup", avg)
	}
}
