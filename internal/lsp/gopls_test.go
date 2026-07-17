package lsp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGoplsDiagnosticsAndReferences drives a real gopls against a scratch
// module: a type error must surface via DiagnosticsForFile without running
// go build, and Lookup must resolve references. Skipped when gopls is not on
// PATH so CI without language servers stays green (the degrade-silently rule).
func TestGoplsDiagnosticsAndReferences(t *testing.T) {
	gopls, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls not installed")
	}
	root := t.TempDir()
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
