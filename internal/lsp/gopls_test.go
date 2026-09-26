package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGoplsDiagnosticsAndReferences drives a real gopls against a scratch
// module: a type error must surface via DiagnosticsForFile without running
// go build, and Lookup must resolve references. Skipped when gopls is not
// installed so CI without language servers stays green (the degrade-silently
// rule).
func TestGoplsDiagnosticsAndReferences(t *testing.T) {
	gopls := findGopls(t)
	root := t.TempDir()
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module scratch\n\ngo 1.22\n")
	write("main.go", "package main\n\nfunc greet() string { return \"hi\" }\n\nfunc main() {\n\tprintln(greet())\n}\n")
	raw, _ := json.Marshal(Config{Servers: map[string]ServerConfig{"go": {Command: gopls}}})
	write(".spettro/lsp.json", string(raw))

	m := ForWorkspace(root)
	if m == nil {
		t.Fatal("manager not created despite config")
	}
	// gopls holds the workspace open; on Windows TempDir cleanup fails if it
	// is still running.
	t.Cleanup(m.Shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mainGo := filepath.Join(root, "main.go")

	out, err := m.DiagnosticsForFile(ctx, mainGo)
	if err != nil {
		t.Fatalf("clean file diagnostics: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("expected no diagnostics on clean file, got: %s", out)
	}

	// introduce a type error the way file-edit would: rewrite on disk
	write("main.go", "package main\n\nfunc greet() string { return 42 }\n\nfunc main() {\n\tprintln(greet())\n}\n")
	out, err = m.DiagnosticsForFile(ctx, mainGo)
	if err != nil {
		t.Fatalf("diagnostics after type error: %v", err)
	}
	if !strings.Contains(out, "main.go:3:") || !strings.Contains(out, "[error]") {
		t.Fatalf("expected type error diagnostic, got: %s", out)
	}

	refs, err := m.Lookup(ctx, mainGo, "greet", "references", 0, 0)
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if !strings.Contains(refs, "main.go:3:6") || !strings.Contains(refs, "main.go:6:") {
		t.Fatalf("expected declaration and call site, got: %s", refs)
	}

	// restore a clean file, then exercise hover and a cross-file rename
	write("main.go", "package main\n\nfunc greet() string { return \"hi\" }\n\nfunc main() {\n\tprintln(greet())\n}\n")
	write("other.go", "package main\n\nvar msg = greet()\n")
	// open other.go so the server knows it (we don't send file-watch events;
	// in the real flow the post-edit diagnostics pass syncs every written file)
	if _, err := m.DiagnosticsForFile(ctx, filepath.Join(root, "other.go")); err != nil {
		t.Fatalf("sync other.go: %v", err)
	}

	hov, err := m.Hover(ctx, mainGo, "greet", 0, 0)
	if err != nil {
		t.Fatalf("hover: %v", err)
	}
	if !strings.Contains(hov, "greet") || !strings.Contains(hov, "string") {
		t.Fatalf("expected signature in hover output, got: %s", hov)
	}

	changes, err := m.RenameEdits(ctx, mainGo, "greet", 0, 0, "salute")
	if err != nil {
		t.Fatalf("rename edits: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected rename to touch 2 files, got %d: %+v", len(changes), changes)
	}
	for _, ch := range changes {
		if strings.Contains(ch.New, "greet") || !strings.Contains(ch.New, "salute") {
			t.Fatalf("rename not fully applied in %s:\n%s", ch.Rel, ch.New)
		}
		if err := os.WriteFile(ch.Path, []byte(ch.New), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err = m.DiagnosticsForFile(ctx, mainGo)
	if err != nil {
		t.Fatalf("diagnostics after rename: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("expected clean diagnostics after rename, got: %s", out)
	}

	// reintroduce a type error so the post-restart respawn check below has a
	// diagnostic to find
	write("main.go", "package main\n\nfunc salute() string { return 42 }\n\nfunc main() {\n\tprintln(salute())\n}\n")

	if msg := m.Restart(""); !strings.Contains(msg, "restarted") {
		t.Fatalf("restart: %s", msg)
	}
	// after restart the server must lazily respawn on next use
	out, err = m.DiagnosticsForFile(ctx, mainGo)
	if err != nil {
		t.Fatalf("diagnostics after restart: %v", err)
	}
	if !strings.Contains(out, "[error]") {
		t.Fatalf("expected diagnostic after restart, got: %s", out)
	}
}

// findGopls returns gopls the way zero-config detection finds it (PATH, then
// the Go install directories), skipping the test when it is not installed.
func findGopls(t *testing.T) string {
	t.Helper()
	gopls, ok := FindServerBinary("gopls", t.TempDir())
	if !ok {
		t.Skip("gopls not installed")
	}
	return gopls
}

// TestGoplsPostEditDiagnostics is the post-edit flow against a real gopls:
// the edit's own type error is listed; a signature change that breaks a
// caller in another package is counted there on the very same pass (gopls
// checks reverse dependencies in a second pass unless told not to delay it);
// undoing it clears the block; and after a rename, or a change made behind
// the server's back, the counts describe the disk rather than what the
// server last saw.
func TestGoplsPostEditDiagnostics(t *testing.T) {
	gopls := findGopls(t)
	root := realPath(t.TempDir())
	write := func(rel, content string) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const (
		libOK    = "package lib\n\nfunc Greet() string { return \"hi\" }\n"
		mainOK   = "package main\n\nimport \"scratch/lib\"\n\nfunc main() {\n\tprintln(lib.Greet())\n}\n"
		mainGone = "package main\n\nimport \"scratch/lib\"\n\nfunc main() {\n\tprintln(lib.Gone())\n}\n"
	)
	write("go.mod", "module scratch\n\ngo 1.22\n")
	lib := write("lib/lib.go", libOK)
	mainGo := write("main.go", mainOK)
	m := newManager(root, Config{Servers: map[string]ServerConfig{
		"go": {Command: gopls, Filetypes: []string{".go"}},
	}})
	t.Cleanup(m.Shutdown)

	// warm like a file-read would, then give the cold start its time once
	m.Warm(lib)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, _, err := m.clientFor(ctx, lib); err != nil {
		t.Fatal(err)
	}
	// the budget the agent gives a real edit
	pass := func(path string, also ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return m.PostEditDiagnostics(ctx, path, also...)
	}
	if out := pass(lib); out != "" {
		t.Fatalf("clean workspace should add nothing, got:\n%s", out)
	}

	write("lib/lib.go", "package lib\n\nfunc Greet() string { return 42 }\n")
	if out := pass(lib); !strings.HasPrefix(out, "Diagnostics (errors) in lib/lib.go:\nlib/lib.go:3:") {
		t.Fatalf("expected lib.go's own type error, got:\n%s", out)
	}

	write("lib/lib.go", "package lib\n\nfunc Greet(name string) string { return name }\n")
	if out := pass(lib); !strings.HasPrefix(out, "No errors in lib/lib.go; 1 error in 1 other file: main.go (1)") {
		t.Fatalf("expected the broken caller in package main to be counted, got:\n%s", out)
	}

	write("lib/lib.go", libOK)
	if out := pass(lib); out != "" {
		t.Fatalf("restoring the signature should clear everything, got:\n%s", out)
	}

	// main.go is opened by an edit, then renamed into along with lib.go
	if out := pass(mainGo); out != "" {
		t.Fatalf("clean main.go should add nothing, got:\n%s", out)
	}
	rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rcancel()
	changes, err := m.RenameEdits(rctx, lib, "Greet", 0, 0, "Hello")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	var written []string
	for _, ch := range changes {
		write(ch.Rel, ch.New)
		written = append(written, ch.Path)
	}
	if out := pass(lib, written...); out != "" {
		t.Fatalf("a complete rename should be clean, got:\n%s", out)
	}
	if out := pass(lib); out != "" {
		t.Fatalf("a later edit must not see main.go as it was before the rename, got:\n%s", out)
	}

	// main.go broken and then fixed behind the server's back (a shell edit)
	write("main.go", mainGone)
	if out := pass(lib); !strings.Contains(out, "1 error in 1 other file: main.go (1)") {
		t.Fatalf("expected main.go's out-of-band error, got:\n%s", out)
	}
	write("main.go", strings.ReplaceAll(mainOK, "Greet", "Hello"))
	if out := pass(lib); out != "" {
		t.Fatalf("main.go was fixed on disk, got:\n%s", out)
	}
}
