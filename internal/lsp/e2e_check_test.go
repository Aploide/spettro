package lsp

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestZeroConfigClangd exercises the full zero-config path against a real
// clangd when one is on PATH: no lsp.json, workspace manager auto-created,
// diagnostics returned for a broken C file.
func TestZeroConfigClangd(t *testing.T) {
	if _, err := lookPath("clangd"); err != nil {
		t.Skip("clangd not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/bad.c", []byte("int main() { return x; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := ForWorkspace(dir)
	if m == nil {
		t.Fatal("expected auto-detected manager with zero config")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := m.DiagnosticsForFile(ctx, dir+"/bad.c")
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("expected a diagnostic for undeclared identifier x")
	}
	t.Log(out)
}
