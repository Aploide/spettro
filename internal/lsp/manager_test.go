package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestLoadConfig(t *testing.T) {
	root := t.TempDir()
	if _, ok := loadConfig(root); ok {
		t.Fatal("no config file should mean disabled")
	}
	dir := filepath.Join(root, ".spettro")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Servers: map[string]ServerConfig{"go": {Command: "gopls", Enabled: true}}}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "lsp.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := loadConfig(root)
	if !ok {
		t.Fatal("expected enabled config")
	}
	if fts := got.Servers["go"].Filetypes; len(fts) != 1 || fts[0] != ".go" {
		t.Fatalf("expected default .go filetypes, got %v", fts)
	}

	// disabled server → treated as no LSP
	cfg.Servers["go"] = ServerConfig{Command: "gopls", Enabled: false}
	raw, _ = json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "lsp.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadConfig(root); ok {
		t.Fatal("disabled server should mean disabled")
	}
}

func TestServerKeyFor(t *testing.T) {
	m := &Manager{cfg: Config{Servers: map[string]ServerConfig{
		"go":         {Command: "gopls", Enabled: true, Filetypes: []string{".go"}},
		"typescript": {Command: "tsserver", Enabled: true, Filetypes: []string{".ts", ".tsx"}},
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

func TestFormatDiagnostics(t *testing.T) {
	m := &Manager{root: "/w"}
	var d Diagnostic
	d.Range.Start = Position{Line: 4, Character: 2}
	d.Severity = 1
	d.Source = "compiler"
	d.Message = "undefined:\nfoo"
	out := m.formatDiagnostics("file:///w/pkg/a.go", []Diagnostic{d})
	want := "pkg/a.go:5:3 [error] undefined: foo (compiler)\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}
