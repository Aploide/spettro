package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// A whitespace-only old_string (collapsing blank lines) is a real edit in
// both forms of file-edit, as it is in multi-edit; an empty one in edits[] is
// an error, never a silently skipped item.
func TestFileEditWhitespaceOnlyOldString(t *testing.T) {
	r, dir := newEditTestRuntime(t)
	p := writeTestFile(t, dir, "p.txt", "a\n\n\n\nb\nc\n")
	args, _ := json.Marshal(map[string]any{"path": "p.txt", "edits": []map[string]any{
		{"old_string": "\n\n\n", "new_string": "\n"}, {"old_string": "c", "new_string": "d"},
	}})
	if _, err := r.runFileEdit(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, p); got != "a\n\nb\nd\n" {
		t.Fatalf("edits[] with a whitespace-only old_string: %q", got)
	}
	q := writeTestFile(t, dir, "q.txt", "a\n\n\n\nb\n")
	if _, err := r.runFileEdit(context.Background(), editArgs("q.txt", "\n\n\n", "\n")); err != nil {
		t.Fatalf("single whitespace-only old_string: %v", err)
	}
	if got := readTestFile(t, q); got != "a\n\nb\n" {
		t.Fatalf("single whitespace-only old_string: %q", got)
	}
	args, _ = json.Marshal(map[string]any{"path": "q.txt", "edits": []map[string]any{
		{"old_string": "b", "new_string": "B"}, {"old_string": "", "new_string": "prefix"},
	}})
	if _, err := r.runFileEdit(context.Background(), args); err == nil || !strings.Contains(err.Error(), "edit 2") {
		t.Fatalf("empty old_string in edits[] must fail the call, got %v", err)
	}
	if got := readTestFile(t, q); got != "a\n\nb\n" {
		t.Fatalf("a failed edits[] call wrote the file: %q", got)
	}
}

// approvalRuntime is an edit runtime whose writes need approval; the
// callback plays a user who changes the file in their editor while the diff
// prompt is open, then approves.
func approvalRuntime(t *testing.T, tool string, onPrompt func()) (*toolRuntime, string) {
	t.Helper()
	r, dir := newEditTestRuntime(t)
	r.permission = config.PermissionAskFirst
	r.toolPolicies = map[string]config.ToolSpec{tool: {RequiresApproval: true}}
	r.shellApproval = func(context.Context, ShellApprovalRequest) (ShellApprovalDecision, error) {
		onPrompt()
		return ShellApprovalAllowOnce, nil
	}
	return r, dir
}

func TestWritesRecheckTheFileAfterApproval(t *testing.T) {
	const orig = "a := 1\nb := 2\n"
	const userVersion = "a := 1\nb := 2\nUSER_CHANGE\n"
	cases := []struct {
		tool string
		args string
	}{
		{"file-edit", `{"path":"f.go","old_string":"a := 1","new_string":"a := 10"}`},
		{"multi-edit", `{"path":"f.go","edits":[{"old_string":"a := 1","new_string":"a := 10"}]}`},
		{"file-write", `{"path":"f.go","content":"new\n"}`},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			var path string
			r, dir := approvalRuntime(t, c.tool, func() { _ = os.WriteFile(path, []byte(userVersion), 0o644) })
			path = writeTestFile(t, dir, "f.go", orig)
			r.recordReadStamp("f.go", []byte(orig))
			_, err := r.execute(context.Background(), toolCall{Tool: c.tool, Args: []byte(c.args)}, map[string]struct{}{c.tool: {}})
			if err == nil || !strings.Contains(err.Error(), "modified") {
				t.Fatalf("err = %v, want a stale-read refusal", err)
			}
			if got := readTestFile(t, path); got != userVersion {
				t.Fatalf("the user's change was overwritten: %q", got)
			}
		})
	}
}

// Stamps are keyed by the real file, so reading or editing through a symlink
// and through the real path are the same file to the stale-read guard.
func TestFileStampsFollowSymlinks(t *testing.T) {
	r, dir := newEditTestRuntime(t)
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "real/a.txt", "hello\nworld\n")
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	for _, p := range []string{"real/a.txt", "link/a.txt"} {
		if _, err := r.runFileRead(context.Background(), []byte(`{"path":"`+p+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.runFileEdit(context.Background(), editArgs("link/a.txt", "hello", "HELLO")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.runFileEdit(context.Background(), editArgs("real/a.txt", "world", "WORLD")); err != nil {
		t.Fatalf("the agent's own edit through the link blocked an edit through the real path: %v", err)
	}
}

// Appending does not replace anything, so it needs no prior read; it must
// not grant a later overwrite either.
func TestFileWriteAppendNeedsNoRead(t *testing.T) {
	r, dir := newEditTestRuntime(t)
	r.permission = config.PermissionYOLO
	p := writeTestFile(t, dir, "CHANGELOG.md", "old\n")
	if _, err := r.execute(context.Background(), toolCall{Tool: "file-write", Args: []byte(`{"path":"CHANGELOG.md","content":"new\n","append":true}`)}, map[string]struct{}{"file-write": {}}); err != nil {
		t.Fatalf("append to an unread file: %v", err)
	}
	if got := readTestFile(t, p); got != "old\nnew\n" {
		t.Fatalf("content = %q", got)
	}
	if _, err := r.execute(context.Background(), toolCall{Tool: "file-write", Args: []byte(`{"path":"CHANGELOG.md","content":"x\n"}`)}, map[string]struct{}{"file-write": {}}); err == nil {
		t.Fatal("an append must not license overwriting a file never read")
	}
}

// file-read streams the file: it honours cancellation (a FIFO never blocks the
// step past its deadline) and refuses non-regular files outright.
func TestFileReadRefusesFIFOAndHonoursContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs")
	}
	r := newReadRuntime(t, nil)
	if err := syscall.Mkfifo(filepath.Join(r.cwd, "pipe"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.runFileRead(context.Background(), []byte(`{"path":"pipe","offset":1,"limit":5}`))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO read: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("file-read blocked on a FIFO")
	}
	writeTree(t, r.cwd, map[string]string{"big.txt": strings.Repeat("line\n", 100000)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.runFileRead(ctx, []byte(`{"path":"big.txt","offset":99990,"limit":5}`)); err == nil {
		t.Fatal("cancelled read succeeded")
	}
	out, err := r.runFileRead(context.Background(), []byte(`{"path":"big.txt","offset":99999,"limit":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != " 99999\tline\n100000\tline\n" {
		t.Fatalf("paged read = %q", out)
	}
}

// The stale-read guard spans the conversation, not one run: each user turn
// is a new run carrying the previous messages.
func TestFileStampsCarryAcrossTurns(t *testing.T) {
	pm, url, _ := newLoopServer(t,
		loopReply{toolName: "file-read", toolArgs: `{"path":"foo.go"}`},
		loopReply{content: "read it"},
		loopReply{toolName: "file-edit", toolArgs: `{"path":"foo.go","old_string":"x := 1","new_string":"x := 2"}`},
		loopReply{content: "edited"},
		loopReply{toolName: "file-write", toolArgs: `{"path":"bar.go","content":"package bar\n"}`},
		loopReply{content: "wrote"},
	)
	cfg := loopCfg(t, pm, url)
	cfg.AllowedTools = []string{"file-read", "file-edit", "file-write"}
	foo := filepath.Join(cfg.CWD, "foo.go")
	bar := filepath.Join(cfg.CWD, "bar.go")
	if err := os.WriteFile(foo, []byte("package foo\nx := 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bar, []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res1, err := runToolLoop(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The user edits foo.go between turns.
	if err := os.WriteFile(foo, []byte("package foo\n// USER EDIT\nx := 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Messages = res1.messages
	cfg.UserTask = "now edit it"
	res2, err := runToolLoop(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(foo); !strings.Contains(string(got), "x := 1") {
		t.Fatalf("a stale edit went through across turns: %q", got)
	}
	if !toolResultContains(res2.messages, "modified on disk") {
		t.Fatal("the stale edit was not refused")
	}
	// A file read in an earlier turn counts as read: bar.go was never read,
	// so writing it is refused; foo.go (read in turn 1) is covered above.
	cfg.Messages = res2.messages
	cfg.UserTask = "write bar"
	res3, err := runToolLoop(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !toolResultContains(res3.messages, `read "bar.go" first`) {
		t.Fatal("overwriting an unread file was not refused")
	}
}

// A file-read in an earlier turn licenses overwriting it in a later one.
func TestFileReadInEarlierTurnLicensesWrite(t *testing.T) {
	pm, url, _ := newLoopServer(t,
		loopReply{toolName: "file-read", toolArgs: `{"path":"foo.go"}`},
		loopReply{content: "read it"},
		loopReply{toolName: "file-write", toolArgs: `{"path":"foo.go","content":"package foo2\n"}`},
		loopReply{content: "wrote"},
	)
	cfg := loopCfg(t, pm, url)
	foo := filepath.Join(cfg.CWD, "foo.go")
	if err := os.WriteFile(foo, []byte("package foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res1, err := runToolLoop(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Messages = res1.messages
	cfg.UserTask = "rewrite it"
	if _, err := runToolLoop(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(foo); string(got) != "package foo2\n" {
		t.Fatalf("write after a read in an earlier turn: %q", got)
	}
}

func toolResultContains(msgs []provider.Message, s string) bool {
	for _, m := range msgs {
		for _, tr := range m.ToolResults {
			if strings.Contains(tr.Output, s) {
				return true
			}
		}
	}
	return false
}
