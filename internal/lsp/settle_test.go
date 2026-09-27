package lsp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestClient is a client with no server behind it: tests dispatch the
// server's notifications themselves.
func newTestClient() *Client {
	c := &Client{
		stdin:    discardWriteCloser{},
		diags:    map[string]publishedDiags{},
		diagGen:  map[string]int{},
		openDocs: map[string]*openDoc{},
		closed:   make(chan struct{}),
	}
	c.diagCond = sync.NewCond(&c.diagMu)
	return c
}

func publishTo(c *Client, file string, version int, msgs ...string) {
	ds := []map[string]any{}
	for _, m := range msgs {
		ds = append(ds, map[string]any{"severity": 1, "message": m})
	}
	p := map[string]any{"uri": fileURI(file), "diagnostics": ds}
	if version > 0 {
		p["version"] = version
	}
	params, _ := json.Marshal(p)
	c.dispatch(rpcMessage{Method: "textDocument/publishDiagnostics", Params: params})
}

// A publish carrying the version just sent is final: the wait does not sit
// out the quiet window (the E1 change that took a gopls edit from 344 ms).
func TestWaitSettledVersionedPublishIsFinal(t *testing.T) {
	c := newTestClient()
	file := filepath.Join(t.TempDir(), "a.go")
	d, err := c.syncFile(context.Background(), file, "go", "x")
	if err != nil {
		t.Fatal(err)
	}
	publishTo(c, file, d.version, "err")
	start := time.Now()
	ds, fresh := c.waitSettled(context.Background(), d, settlePolicy{quiet: 5 * time.Second})
	if !fresh || len(ds) != 1 {
		t.Fatalf("got %+v fresh=%v", ds, fresh)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("versioned publish waited %s", took)
	}
}

// The versioned publish ends the wait only once its batch is in: a file the
// same pass publishes right after it (a reverse dependency) is current when
// the other-files summary is taken.
func TestWaitSettledVersionedKeepsItsBatch(t *testing.T) {
	c := newTestClient()
	dir := t.TempDir()
	file, other := filepath.Join(dir, "lib.go"), filepath.Join(dir, "main.go")
	d, err := c.syncFile(context.Background(), file, "go", "x")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		publishTo(c, file, d.version)
		time.Sleep(time.Millisecond)
		publishTo(c, other, 0, "caller broken")
	}()
	if _, fresh := c.waitSettled(context.Background(), d, settlePolicy{quiet: 5 * time.Second}); !fresh {
		t.Fatal("not fresh")
	}
	if got := c.allDiagnostics(); len(got[realPath(other)]) != 1 && len(got[other]) != 1 {
		t.Fatalf("the batch's other publish was missed: %+v", got)
	}
}

// A staged server's wait ends at its last stage, not at a fixed window, and
// never later than the ceiling.
func TestWaitSettledStagedEndsAtLastStage(t *testing.T) {
	c := newTestClient()
	file := filepath.Join(t.TempDir(), "a.ts")
	d, err := c.syncFile(context.Background(), file, "typescript", "x")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		publishTo(c, file, 0)
		time.Sleep(30 * time.Millisecond)
		publishTo(c, file, 0, "semantic")
	}()
	start := time.Now()
	ds, fresh := c.waitSettled(context.Background(), d, settlePolicy{stages: 2, ceiling: 5 * time.Second})
	if !fresh || len(ds) != 1 || ds[0].Message != "semantic" {
		t.Fatalf("got %+v fresh=%v, want the semantic stage", ds, fresh)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("staged wait took %s after its last stage", took)
	}

	// Only the first stage ever comes: the ceiling ends the wait.
	d, _ = c.syncFile(context.Background(), file, "typescript", "y")
	publishTo(c, file, 0)
	start = time.Now()
	if _, fresh := c.waitSettled(context.Background(), d, settlePolicy{stages: 2, ceiling: 50 * time.Millisecond}); !fresh {
		t.Fatal("the first stage is a fresh answer")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("ceiling not honoured: %s", took)
	}
}

func TestSettlePolicyPerServerAndOverride(t *testing.T) {
	ms := 75
	m := newManager("/w", Config{Servers: map[string]ServerConfig{
		"go":         {Command: "gopls", SettleMs: &fastSettleMs},
		"typescript": {Command: "typescript-language-server"},
		"rust":       {Command: "rust-analyzer"},
		"python":     {Command: "pylsp", SettleMs: &ms},
	}})
	cases := map[string]settlePolicy{
		"go":         {quiet: 30 * time.Millisecond},
		"typescript": {quiet: defaultSettle, stages: 2, ceiling: time.Second},
		"rust":       {quiet: defaultSettle},
		"python":     {quiet: 75 * time.Millisecond},
	}
	for key, want := range cases {
		if got := m.settlePolicy(key); got != want {
			t.Errorf("settlePolicy(%s) = %+v, want %+v", key, got, want)
		}
	}
	// A toggle-only lsp.json entry keeps the detected command and can set
	// settle_ms.
	root := t.TempDir()
	writeLspJSON(t, root, Config{Servers: map[string]ServerConfig{"c": {SettleMs: &ms}}})
	stubLookPath(t, "clangd")
	cfg, _ := loadConfig(root)
	if got := cfg.Servers["c"].SettleMs; got == nil || *got != 75 {
		t.Fatalf("settle_ms override lost: %v", got)
	}
	if got := cfg.Servers["cpp"].SettleMs; got == nil || *got != fastSettleMs {
		t.Fatalf("built-in clangd window lost: %v", got)
	}
}
