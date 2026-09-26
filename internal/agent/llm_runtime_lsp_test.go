package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spettro/internal/lsp"
	"spettro/internal/lsp/lsptest"
)

// TestMain lets the test binary double as a scripted language server for the
// post-edit diagnostics tests.
func TestMain(m *testing.M) {
	lsptest.MaybeServe()
	os.Exit(m.Run())
}

// fakeLSPRuntime returns a runtime over a fresh workspace whose .spettro/lsp.json
// points ".fk" files at the scripted server.
func fakeLSPRuntime(t *testing.T, opts lsptest.Options) (*toolRuntime, string) {
	t.Helper()
	raw, _ := json.Marshal(opts)
	t.Setenv(lsptest.EnvVar, string(raw))
	dir := t.TempDir()
	cfg, _ := json.Marshal(lsp.Config{Servers: map[string]lsp.ServerConfig{
		"fake": {Command: os.Args[0], Args: lsptest.Args, Filetypes: []string{".fk"}},
	}})
	if err := os.MkdirAll(filepath.Join(dir, ".spettro"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".spettro", "lsp.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if m := lsp.ForWorkspace(dir); m != nil {
			m.Shutdown()
		}
	})
	return &toolRuntime{cwd: dir, readSet: map[string]struct{}{}}, dir
}

func toolArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Every mutating file tool appends the errors of the file it just wrote, so
// the model sees them in the same step.
func TestEditToolsAppendDiagnostics(t *testing.T) {
	rt, dir := fakeLSPRuntime(t, lsptest.Options{})
	ctx := context.Background()
	allowed := map[string]struct{}{"file-read": {}, "file-write": {}}

	out, err := rt.execute(ctx, toolCall{Tool: "file-write", Args: toolArgs(t, map[string]any{
		"path": "a.fk", "content": "fine\nERR from write\n",
	})}, allowed)
	if err != nil {
		t.Fatal(err)
	}
	want := "\n\nDiagnostics (errors) in a.fk:\na.fk:2:1: bad thing: ERR from write (fake)"
	if !strings.HasPrefix(out, "created a.fk") || !strings.HasSuffix(out, want) {
		t.Fatalf("file-write result:\n%s\nwant it to end with:%s", out, want)
	}

	out, err = rt.runFileEdit(ctx, toolArgs(t, map[string]any{
		"path": "a.fk", "old_string": "ERR from write", "new_string": "WARN only",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "edited a.fk (1 replacements)") || strings.Contains(out, "Diagnostics") || strings.Contains(out, "not checked") {
		t.Fatalf("a clean edit should add nothing, got:\n%s", out)
	}

	out, err = rt.runMultiEdit(ctx, toolArgs(t, map[string]any{
		"path": "a.fk",
		"edits": []map[string]any{
			{"old_string": "fine", "new_string": "ERR first"},
			{"old_string": "WARN only", "new_string": "ERR second"},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "Diagnostics (errors) in a.fk:\na.fk:1:1: bad thing: ERR first (fake)\na.fk:2:1: bad thing: ERR second (fake)") {
		t.Fatalf("multi-edit result:\n%s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.fk")); string(got) != "ERR first\nERR second\n" {
		t.Fatalf("file content %q", got)
	}
}

// Reading a file starts its server in the background, so the first edit does
// not pay for the start.
func TestFileReadWarmsServer(t *testing.T) {
	rt, dir := fakeLSPRuntime(t, lsptest.Options{InitDelay: 700 * time.Millisecond})
	if err := os.WriteFile(filepath.Join(dir, "a.fk"), []byte("fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	allowed := map[string]struct{}{"file-read": {}}
	start := time.Now()
	if _, err := rt.execute(ctx, toolCall{Tool: "file-read", Args: toolArgs(t, map[string]any{"path": "a.fk"})}, allowed); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("file-read waited %s for the server", took)
	}
	time.Sleep(time.Second) // the model thinks; the server finishes starting

	start = time.Now()
	out, err := rt.runFileEdit(ctx, toolArgs(t, map[string]any{
		"path": "a.fk", "old_string": "fine", "new_string": "ERR now",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.fk:1:1: bad thing: ERR now") {
		t.Fatalf("expected diagnostics on the first edit after a read, got:\n%s", out)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("first edit after warm-up took %s", took)
	}
}

// A server that never answers costs the edit at most the bounded wait, and
// the edit itself still lands.
func TestEditNeverFailsOnSilentServer(t *testing.T) {
	rt, dir := fakeLSPRuntime(t, lsptest.Options{Silent: true})
	path := filepath.Join(dir, "a.fk")
	if err := os.WriteFile(path, []byte("fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, err := rt.runFileEdit(context.Background(), toolArgs(t, map[string]any{
		"path": "a.fk", "old_string": "fine", "new_string": "ERR now",
	}))
	if err != nil {
		t.Fatalf("edit failed because of the server: %v", err)
	}
	if took := time.Since(start); took > lspDiagnosticsWait+time.Second {
		t.Fatalf("edit took %s against a silent server", took)
	}
	if !strings.HasPrefix(out, "edited a.fk (1 replacements)") {
		t.Fatalf("unexpected result:\n%s", out)
	}
	if got, _ := os.ReadFile(path); string(got) != "ERR now\n" {
		t.Fatalf("edit did not land: %q", got)
	}
}
