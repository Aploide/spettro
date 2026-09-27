package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A required read given as an absolute path (ACP attaches files as
// file:// resource links) is met by reading that file: the loop must end
// with the model's answer instead of nudging "you must read" forever.
func TestRequiredReadAbsolutePathIsMetByFileRead(t *testing.T) {
	pm, url, ls := newLoopServer(t,
		loopReply{toolName: "file-read", toolArgs: `{"path":"a.txt"}`},
		loopReply{content: "done"},
		loopReply{content: "done again"},
		loopReply{content: "done again"},
	)
	cfg := loopCfg(t, pm, url)
	cfg.AllowedTools = []string{"file-read"}
	abs := filepath.Join(cfg.CWD, "a.txt")
	if err := os.WriteFile(abs, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.RequiredReads = []string{abs}
	res, err := runToolLoop(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.content != "done" {
		t.Fatalf("content = %q, want the first answer", res.content)
	}
	if n := len(ls.requests()); n != 2 {
		t.Fatalf("requests = %d, want 2 (read, answer)", n)
	}
}

// A required read the model can never satisfy (outside the workspace, or
// missing) is dropped up front instead of blocking every other tool.
func TestRequiredReadUnreadableIsDropped(t *testing.T) {
	pm, url, ls := newLoopServer(t,
		loopReply{content: "done"},
		loopReply{content: "done again"},
	)
	cfg := loopCfg(t, pm, url)
	cfg.AllowedTools = []string{"file-read"}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.RequiredReads = []string{outside, "missing.txt", filepath.Join(cfg.CWD, "gone.txt")}
	res, err := runToolLoop(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.content != "done" || len(ls.requests()) != 1 {
		t.Fatalf("content = %q after %d requests; unreadable required reads must not nudge", res.content, len(ls.requests()))
	}
}
