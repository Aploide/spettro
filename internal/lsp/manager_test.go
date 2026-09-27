package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPositionOfSymbol(t *testing.T) {
	content := "package main\n\nfunc Foobar() {}\n\nvar x = Foobar\n"
	pos, ok := positionOfSymbol(content, "Foobar")
	if !ok || pos.Line != 2 || pos.Character != 5 {
		t.Fatalf("got %+v ok=%v, want line 2 char 5", pos, ok)
	}
	// substring occurrences must not win over whole-identifier matches
	content = "notFoo := 1\nFoo := 2\n"
	pos, ok = positionOfSymbol(content, "Foo")
	if !ok || pos.Line != 1 || pos.Character != 0 {
		t.Fatalf("got %+v ok=%v, want line 1 char 0", pos, ok)
	}
	if _, ok := positionOfSymbol(content, "Missing"); ok {
		t.Fatal("expected miss")
	}
}

// stubLookPath makes only the named commands "installed" for the test.
func stubLookPath(t *testing.T, found ...string) {
	t.Helper()
	orig := lookPath
	lookPath = func(cmd string) (string, error) {
		if slices.Contains(found, cmd) {
			return "/usr/bin/" + cmd, nil
		}
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { lookPath = orig })
}

func writeLspJSON(t *testing.T, root string, cfg Config) {
	t.Helper()
	dir := filepath.Join(root, ".spettro")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "lsp.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigZeroConfig(t *testing.T) {
	root := t.TempDir()

	// nothing on PATH, no config file → disabled
	stubLookPath(t)
	if _, ok := loadConfig(root); ok {
		t.Fatal("no servers on PATH should mean disabled")
	}

	// gopls on PATH → enabled with no config file at all
	stubLookPath(t, "gopls")
	got, ok := loadConfig(root)
	if !ok {
		t.Fatal("gopls on PATH should enable lsp with zero config")
	}
	sc := got.Servers["go"]
	if sc.Command != "gopls" || !sc.enabled() || len(sc.Filetypes) != 1 || sc.Filetypes[0] != ".go" {
		t.Fatalf("unexpected auto-detected go server: %+v", sc)
	}

	// candidate fallback: pylsp is picked when pyright is absent
	stubLookPath(t, "pylsp")
	got, ok = loadConfig(root)
	if !ok || got.Servers["python"].Command != "pylsp" {
		t.Fatalf("expected pylsp fallback, got %+v ok=%v", got.Servers["python"], ok)
	}
}

func TestLoadConfigOverride(t *testing.T) {
	root := t.TempDir()
	stubLookPath(t, "gopls")

	// override command for a key not on PATH
	writeLspJSON(t, root, Config{Servers: map[string]ServerConfig{
		"zig": {Command: "zls", Filetypes: []string{".zig"}},
	}})
	got, ok := loadConfig(root)
	if !ok {
		t.Fatal("expected enabled config")
	}
	if got.Servers["go"].Command != "gopls" || got.Servers["zig"].Command != "zls" {
		t.Fatalf("expected detected go plus user zig, got %+v", got.Servers)
	}

	// commandless entry with enabled:false disables the detected server
	writeLspJSON(t, root, Config{Servers: map[string]ServerConfig{
		"go": {Enabled: new(false)},
	}})
	if _, ok := loadConfig(root); ok {
		t.Fatal("disabling the only detected server should mean disabled")
	}

	// explicit command override still gets default filetypes for known keys
	writeLspJSON(t, root, Config{Servers: map[string]ServerConfig{
		"go": {Command: "custom-gopls"},
	}})
	got, ok = loadConfig(root)
	if !ok || got.Servers["go"].Command != "custom-gopls" {
		t.Fatalf("expected command override, got %+v ok=%v", got.Servers["go"], ok)
	}
	if fts := got.Servers["go"].Filetypes; len(fts) != 1 || fts[0] != ".go" {
		t.Fatalf("expected default .go filetypes, got %v", fts)
	}
}

// A project entry that only tunes a server (settle_ms) keeps the user's
// enabled:false from ~/.spettro/lsp.json: an unset field inherits.
func TestLoadConfigPartialOverrideKeepsEnabled(t *testing.T) {
	root := t.TempDir()
	stubLookPath(t, "gopls")
	home := t.TempDir() // a home of its own: other tests read ~/.spettro/lsp.json too
	t.Setenv("HOME", home)
	writeLspJSON(t, home, Config{Servers: map[string]ServerConfig{"go": {Enabled: new(false)}}})
	writeLspJSON(t, root, Config{Servers: map[string]ServerConfig{"go": {SettleMs: new(200)}}})
	got, ok := loadConfig(root)
	if ok || got.Servers["go"].enabled() {
		t.Fatalf("settle_ms-only project entry re-enabled the server: %+v ok=%v", got.Servers["go"], ok)
	}
	if ms := got.Servers["go"].SettleMs; ms == nil || *ms != 200 {
		t.Fatalf("settle_ms not applied: %v", ms)
	}
	// An explicit enabled:true in the project still wins.
	writeLspJSON(t, root, Config{Servers: map[string]ServerConfig{"go": {Enabled: new(true)}}})
	if _, ok := loadConfig(root); !ok {
		t.Fatal("the project's enabled:true did not override the user's enabled:false")
	}
}

func TestServerKeyFor(t *testing.T) {
	m := &Manager{cfg: Config{Servers: map[string]ServerConfig{
		"go":         {Command: "gopls", Filetypes: []string{".go"}},
		"typescript": {Command: "tsserver", Filetypes: []string{".ts", ".tsx"}},
	}}, clients: map[string]*Client{}, broken: map[string]string{}}
	if key, ok := m.serverKeyFor("/x/main.go"); !ok || key != "go" {
		t.Fatalf("got %q ok=%v", key, ok)
	}
	if key, ok := m.serverKeyFor("/x/app.TSX"); !ok || key != "typescript" {
		t.Fatalf("case-insensitive ext match failed: %q ok=%v", key, ok)
	}
	if _, ok := m.serverKeyFor("/x/readme.md"); ok {
		t.Fatal("unexpected server for .md")
	}
}

// Servers answer with their own spelling of a path; output stays
// workspace-relative as long as the two spellings resolve to the same file.
func TestRelPathAcceptsOtherSpellings(t *testing.T) {
	root := realPath(t.TempDir())
	m := &Manager{root: root}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.relPath(filepath.Join(root, "pkg", "a.go")); got != "pkg/a.go" {
		t.Fatalf("relPath under root = %q, want pkg/a.go", got)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skip("symlinks unavailable:", err) // Windows without developer mode
	}
	if got := m.relPath(filepath.Join(link, "a.go")); got != "a.go" {
		t.Fatalf("relPath through a symlinked root = %q, want a.go", got)
	}
}

func TestFormatDiagnostics(t *testing.T) {
	m := &Manager{root: filepath.FromSlash("/w")}
	var d Diagnostic
	d.Range.Start = Position{Line: 4, Character: 2}
	d.Severity = 1
	d.Source = "compiler"
	d.Message = "undefined:\nfoo"
	out := m.formatDiagnostics(filepath.FromSlash("/w/pkg/a.go"), []Diagnostic{d})
	want := "pkg/a.go:5:3 [error] undefined: foo (compiler)\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}
