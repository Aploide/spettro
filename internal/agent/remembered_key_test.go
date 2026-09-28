package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/config"
)

// A remembered command is keyed the way a shell splits words: runs of
// unquoted, unescaped spaces and tabs fold to one space, and nothing else
// does. Folding more would make "always allow" on a harmless one-argument
// command remember the command that deletes the home directory.
func TestRememberedCommandKeyFoldsOnlyShellWordBreaks(t *testing.T) {
	if got := RememberedCommandKey("  go\t test   ./... "); got != "go test ./..." {
		t.Fatalf("ASCII blanks: %q", got)
	}
	for _, harmless := range []string{
		"rm -rf ./build/\u00a0~/", // no-break space
		"rm -rf ./build/\v~/",     // vertical tab
		"rm -rf ./build/\f~/",     // form feed
		"rm -rf ./build/\r~/",     // carriage return
	} {
		if got := RememberedCommandKey(harmless); got != harmless {
			t.Errorf("%q keyed as %q", harmless, got)
		}
	}
	// Commands a shell splits differently never share a key.
	for _, pair := range [][2]string{
		{`rm -rf ./x\ ~/`, `rm -rf ./x\  ~/`},         // escaped space, then a word break
		{`rm -rf "a  b"`, `rm -rf "a b"`},             // spaces inside quotes
		{`rm -rf 'a  b'`, `rm -rf 'a b'`},             // likewise
		{`echo "$(ls "a  b")"`, `echo "$(ls "a b")"`}, // nested quotes: kept verbatim
		{"rm -rf ./build/\v~/", "rm -rf ./build/ ~/"},
	} {
		if RememberedCommandKey(pair[0]) == RememberedCommandKey(pair[1]) {
			t.Errorf("%q and %q share the key %q", pair[0], pair[1], RememberedCommandKey(pair[0]))
		}
	}
	// Blanks between quoted words still fold.
	if got := RememberedCommandKey(`git  commit -m  "a  b"`); got != `git commit -m "a  b"` {
		t.Errorf("quoted argument: %q", got)
	}
}

// End to end: allowing a harmless command for always never lets a
// different command run unasked, in particular not one whose words differ
// only where the old key folded or trimmed a character the shell keeps.
func TestAllowAlwaysNeverCoversAnotherCommand(t *testing.T) {
	for _, pair := range [][2]string{
		{`rm -rf ./build/\ ~/Documents`, `rm -rf ./build/\  ~/Documents`},
		{"rm -rf ./build/\v~/Documents", "rm -rf ./build/ ~/Documents"},
		// A no-break space ending a segment was trimmed from its key.
		{"rm -rf ~/Documents\u00a0&& echo done", "rm -rf ~/Documents && echo done"},
	} {
		var asked []string
		cb := func(_ context.Context, req ShellApprovalRequest) (ShellApprovalDecision, error) {
			asked = append(asked, req.Command)
			return ShellApprovalAllowAlways, nil
		}
		rt := newPermRuntime(config.PermissionAskFirst, cb, nil)
		rt.cwd = t.TempDir()
		for _, cmd := range pair {
			if err := rt.authorizeShellCommand(context.Background(), "bash", cmd); err != nil {
				t.Fatal(err)
			}
		}
		if len(asked) != 2 {
			t.Errorf("%q ran unasked after %q was allowed for always", pair[1], pair[0])
		}
	}
}

// The directory a command runs in is part of what is approved ("make
// install" runs whichever Makefile is there), so the request shows it as a
// leading "cd"; what "Allow always" remembers is still the command itself.
func TestShellApprovalShowsTheWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"vendor/evil", "odd dir"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var got ShellApprovalRequest
	cb := func(_ context.Context, req ShellApprovalRequest) (ShellApprovalDecision, error) {
		got = req
		return ShellApprovalDeny, nil
	}
	rt := newPermRuntime(config.PermissionAskFirst, cb, nil)
	rt.cwd = root
	for _, tc := range []struct{ cwd, want string }{
		{"vendor/evil", "cd vendor/evil && make install"},
		{"odd dir", "cd 'odd dir' && make install"},
		{".", "make install"},
		{"", "make install"},
	} {
		args, _ := json.Marshal(map[string]string{"command": "make install", "cwd": tc.cwd})
		if _, err := rt.runShellTool(context.Background(), "bash", args, "bash"); err == nil {
			t.Fatal("a denied command ran")
		}
		if got.Command != tc.want || len(got.Segments) != 1 || got.Segments[0] != "make install" {
			t.Errorf("cwd %q: command %q, segments %q; want %q", tc.cwd, got.Command, got.Segments, tc.want)
		}
	}
}

// A write through a symlink names the file it lands in, wherever that is,
// and the structured change points there; a symlink changed while the
// prompt was up makes the write fail rather than land somewhere else.
func TestWriteApprovalNamesTheSymlinkTarget(t *testing.T) {
	r, dir := newEditTestRuntime(t)
	outside := t.TempDir()
	target := writeTestFile(t, outside, "zshrc", "export PATH=/usr/bin\n")
	link := filepath.Join(dir, "notes.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var got ShellApprovalRequest
	r.permission = config.PermissionAskFirst
	r.toolPolicies = map[string]config.ToolSpec{"file-edit": {RequiresApproval: true}}
	r.shellApproval = func(_ context.Context, req ShellApprovalRequest) (ShellApprovalDecision, error) {
		got = req
		return ShellApprovalAllowOnce, nil
	}
	args := []byte(`{"path":"notes.md","old_string":"export PATH=/usr/bin","new_string":"echo changed"}`)
	if _, err := r.runFileEdit(context.Background(), "file-edit", args); err != nil {
		t.Fatal(err)
	}
	realTarget := realTargetPath(target)
	if !strings.HasPrefix(got.Command, "file-edit notes.md (through a symlink: writes ") || !strings.Contains(got.Command, realTarget) {
		t.Errorf("command %q does not name %s", got.Command, realTarget)
	}
	if got.Change == nil || got.Change.Path != realTarget {
		t.Errorf("change path %v, want %s", got.Change, realTarget)
	}

	// A plain file keeps the plain command.
	writeTestFile(t, dir, "plain.txt", "a\n")
	if _, err := r.runFileEdit(context.Background(), "file-edit", []byte(`{"path":"plain.txt","old_string":"a","new_string":"b"}`)); err != nil {
		t.Fatal(err)
	}
	if got.Command != "file-edit plain.txt" {
		t.Errorf("plain file: command %q", got.Command)
	}

	// Retargeted while the prompt is up: nothing is written.
	other := writeTestFile(t, outside, "other", "echo changed\n")
	r.shellApproval = func(_ context.Context, req ShellApprovalRequest) (ShellApprovalDecision, error) {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, link); err != nil {
			t.Fatal(err)
		}
		return ShellApprovalAllowOnce, nil
	}
	_, err := r.runFileEdit(context.Background(), "file-edit", []byte(`{"path":"notes.md","old_string":"echo changed","new_string":"echo again"}`))
	if err == nil || !strings.Contains(err.Error(), "different file than the one approved") {
		t.Errorf("retargeted symlink: err %v", err)
	}
	if readTestFile(t, other) != "echo changed\n" {
		t.Error("the write landed in the file the symlink was pointed at after approval")
	}
}
