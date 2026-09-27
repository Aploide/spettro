package acp

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
	"spettro/internal/config"
)

// Every canonical built-in maps to a deliberate kind, and every retired name
// maps to its canonical tool's kind.
func TestToolKindClassification(t *testing.T) {
	cases := map[string]acpsdk.ToolKind{
		// canonical built-ins
		"file-read":       acpsdk.ToolKindRead,
		"file-edit":       acpsdk.ToolKindEdit,
		"file-write":      acpsdk.ToolKindEdit,
		"rename-symbol":   acpsdk.ToolKindEdit,
		"bash":            acpsdk.ToolKindExecute,
		"pty-start":       acpsdk.ToolKindExecute,
		"grep":            acpsdk.ToolKindSearch,
		"glob":            acpsdk.ToolKindSearch,
		"lsp":             acpsdk.ToolKindSearch,
		"web-fetch":       acpsdk.ToolKindFetch,
		"web-search":      acpsdk.ToolKindFetch,
		"view-image":      acpsdk.ToolKindRead,
		"todo-write":      acpsdk.ToolKindThink,
		"agent":           acpsdk.ToolKindThink,
		"workflow":        acpsdk.ToolKindThink,
		"skill":           acpsdk.ToolKindRead,
		"enter-plan-mode": acpsdk.ToolKindSwitchMode,
		"ask-user":        acpsdk.ToolKindOther,
		"save-memory":     acpsdk.ToolKindOther,
		"config":          acpsdk.ToolKindOther,
		// retired names follow their canonical tool
		"shell-exec":  acpsdk.ToolKindExecute,
		"bash-output": acpsdk.ToolKindExecute,
		"multi-edit":  acpsdk.ToolKindEdit,
		"repo-search": acpsdk.ToolKindSearch,
		"ls":          acpsdk.ToolKindSearch,
		"task-create": acpsdk.ToolKindThink,
		"task-list":   acpsdk.ToolKindThink,
		"skill-read":  acpsdk.ToolKindRead,
		"skill-list":  acpsdk.ToolKindRead,
		"diagnostics": acpsdk.ToolKindSearch,
		"lsp-restart": acpsdk.ToolKindSearch,
		// tools that are not built-ins are guessed from their name
		"http-fetch":       acpsdk.ToolKindFetch,
		"github_read_file": acpsdk.ToolKindRead,
		"db_write_row":     acpsdk.ToolKindEdit,
		"mystery":          acpsdk.ToolKindOther,
	}
	for name, want := range cases {
		if got := toolKind(name); got != want {
			t.Errorf("toolKind(%q) = %q, want %q", name, got, want)
		}
	}
}

// Every tool the default manifest ships has a kind decided on purpose: it is
// in builtinToolKinds, or it is one of the built-ins documented as "other".
// A new built-in fails this test until someone picks its kind.
func TestEveryDefaultToolHasADeliberateKind(t *testing.T) {
	other := map[string]bool{
		"ask-user": true, "config": true, "save-memory": true, "send-message": true,
		"task-stop": true, "mcp-auth": true, "enter-worktree": true, "exit-worktree": true,
		"comment": true,
	}
	for _, spec := range config.DefaultAgentManifest().Tools {
		if !spec.IsBuiltin() {
			continue
		}
		if _, ok := builtinToolKinds[spec.ID]; !ok && !other[spec.ID] {
			t.Errorf("built-in %q has no ACP kind in builtinToolKinds", spec.ID)
		}
		if _, ok := builtinToolTitles[spec.ID]; !ok && spec.ID != "agent" && spec.ID != "workflow" && spec.ID != "comment" {
			t.Errorf("built-in %q has no title in builtinToolTitles", spec.ID)
		}
	}
}

func TestToolCallTitles(t *testing.T) {
	cases := []struct {
		name, args, want string
	}{
		{"bash", `{"command":"go test ./..."}`, "Run go test ./..."},
		{"bash", `{"command":"make","run_in_background":true}`, "Run in background: make"},
		{"file-read", `{"path":"main.go"}`, "Read main.go"},
		{"file-edit", `{"path":"a.txt","old_string":"x","new_string":"y"}`, "Edit a.txt"},
		{"file-write", `{"path":"a.txt","content":"x","append":true}`, "Append to a.txt"},
		{"glob", `{"pattern":"**/*.go"}`, "Find **/*.go in ."},
		{"grep", `{"pattern":"TODO","path":"internal"}`, "Search TODO in internal"},
		{"grep", `{"symbol":"Run"}`, "Find symbol Run"},
		{"lsp", `{"op":"hover","path":"a.go"}`, "LSP hover a.go"},
		{"web-fetch", `{"url":"https://example.com"}`, "Fetch https://example.com"},
		{"todo-write", `{"todos":[]}`, "Update tasks"},
		{"skill", `{"name":"greet"}`, "Load skill greet"},
		// a retired name is titled as its canonical tool
		{"shell-exec", `{"command":"ls"}`, "Run ls"},
		{"skill-list", `{}`, "List skills"},
		// anything else: name and arguments
		{"mcp_tool", `{"q":1}`, `mcp_tool {"q":1}`},
		{"file-read", `not json`, "file-read not json"},
	}
	for _, tc := range cases {
		if got := toolCallTitle(agent.ToolTrace{Name: tc.name, Args: tc.args}); got != tc.want {
			t.Errorf("title(%s %s) = %q, want %q", tc.name, tc.args, got, tc.want)
		}
	}
	// Swarm members are attributed.
	if got := toolCallTitle(agent.ToolTrace{AgentID: "code#3", Name: "file-read", Args: `{"path":"x"}`}); got != "[code#3] Read x" {
		t.Errorf("swarm title = %q", got)
	}
}

// A title is one line, bounded in runes, and never cuts a rune in half.
func TestToolCallTitleIsBoundedAndValidUTF8(t *testing.T) {
	cmd := strings.Repeat("é", 500) + "\nsecond line"
	args, _ := json.Marshal(map[string]string{"command": cmd})
	title := toolCallTitle(agent.ToolTrace{Name: "bash", Args: string(args)})
	if n := utf8.RuneCountInString(title); n > maxTitleRunes {
		t.Errorf("title has %d runes, want at most %d", n, maxTitleRunes)
	}
	if !utf8.ValidString(title) || strings.Contains(title, "\n") {
		t.Errorf("title is not a valid single line: %q", title)
	}
}

func TestToolLocationsAreAbsolute(t *testing.T) {
	locs := toolLocations(`{"path":"src/a.go","start_line":12}`, "/proj")
	if len(locs) != 1 || locs[0].Path != filepath.Join("/proj", "src/a.go") || locs[0].Line == nil || *locs[0].Line != 12 {
		t.Fatalf("relative path: %s", jsonString(locs))
	}
	locs = toolLocations(`{"path":"/tmp/a.go","content":"x"}`, "/proj")
	if len(locs) != 1 || locs[0].Path != "/tmp/a.go" || locs[0].Line != nil {
		t.Fatalf("absolute path: %s", jsonString(locs))
	}
	if locs := toolLocations("not json", "/proj"); locs != nil {
		t.Fatalf("expected nil locations for non-JSON args, got %v", locs)
	}
}

// rawInput keeps small arguments as they are, clips long strings, redacts
// secrets, and falls back to a clipped string past the total cap.
func TestBoundedRawInput(t *testing.T) {
	small := boundedRawInput(`{"path":"a.txt","n":3}`)
	if jsonString(small) != `{"n":3,"path":"a.txt"}` {
		t.Errorf("small args changed: %s", jsonString(small))
	}
	big := strings.Repeat("x", 3*maxRawInputString)
	args, _ := json.Marshal(map[string]any{"path": "a.txt", "content": big, "token": "s3cret"})
	bounded, ok := boundedRawInput(string(args)).(map[string]any)
	if !ok {
		t.Fatalf("bounded input is not an object: %T", boundedRawInput(string(args)))
	}
	if content := bounded["content"].(string); len(content) > maxRawInputString+64 || !strings.Contains(content, "more bytes not shown") {
		t.Errorf("content not clipped: %d bytes", len(content))
	}
	if bounded["token"] != "[redacted]" {
		t.Errorf("token not redacted: %v", bounded["token"])
	}
	// Many medium strings: each under the per-string cap, the whole over
	// the total cap.
	many := map[string]string{}
	for i := 0; i < 20; i++ {
		many[strings.Repeat("k", i+1)] = strings.Repeat("v", maxRawInputString-10)
	}
	raw, _ := json.Marshal(many)
	if s, ok := boundedRawInput(string(raw)).(string); !ok || len(s) > maxRawInputBytes+64 {
		t.Errorf("oversized input must collapse to a clipped string, got %T", boundedRawInput(string(raw)))
	}
	if got := boundedRawInput("not json"); got != "not json" {
		t.Errorf("non-JSON args: %v", got)
	}
}

func TestClipBytesKeepsRunesWhole(t *testing.T) {
	s := strings.Repeat("日本", 100)
	out := clipBytes(s, 7)
	if !utf8.ValidString(out) || !strings.HasPrefix(out, "日本") {
		t.Errorf("clipBytes produced %q", out)
	}
	if clipBytes("short", 10) != "short" {
		t.Error("clipBytes changed a string that fits")
	}
}

func TestFileChangeContent(t *testing.T) {
	blocks := fileChangeContent([]agent.FileChange{
		{Path: "/p/a.txt", OldText: "old\n", NewText: "new\n"},
		{Path: "/p/new.txt", NewText: "hello\n", Created: true},
		{Path: "/p/huge.bin", TextOmitted: true},
	})
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks: %s", len(blocks), jsonString(blocks))
	}
	d := blocks[0].Diff
	if d == nil || d.Path != "/p/a.txt" || d.OldText == nil || *d.OldText != "old\n" || d.NewText != "new\n" {
		t.Errorf("edit diff: %s", jsonString(blocks[0]))
	}
	if created := blocks[1].Diff; created == nil || created.OldText != nil {
		t.Errorf("a created file's diff must have no oldText: %s", jsonString(blocks[1]))
	}
	if note := blocks[2].Content; note == nil || note.Content.Text == nil || !strings.Contains(note.Content.Text.Text, "/p/huge.bin") {
		t.Errorf("omitted change must be named in a note: %s", jsonString(blocks[2]))
	}
	// The per-update budget: the second large change no longer fits.
	half := strings.Repeat("x", maxDiffBytesPerUpdate/2+1)
	blocks = fileChangeContent([]agent.FileChange{
		{Path: "/p/1", NewText: half, Created: true},
		{Path: "/p/2", NewText: half, Created: true},
	})
	if len(blocks) != 2 || blocks[0].Diff == nil || blocks[1].Content == nil {
		t.Errorf("budget not applied: %s", jsonString(blocks))
	}
}

// Tool-attached images (screenshot, view-image) must reach ACP clients as
// image content blocks alongside the tool's text output; unreadable paths are
// skipped rather than failing the update.
func TestToolOutputContentWithImages(t *testing.T) {
	payload := []byte{0x89, 0x50, 0x4e, 0x47}
	img := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(img, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	blocks := toolOutputContent(`{"file":"shot.png"}`, []string{img, "/no/such/file.png"})
	if len(blocks) != 2 {
		t.Fatalf("expected text+image blocks, got %d", len(blocks))
	}
	if blocks[0].Content == nil || blocks[0].Content.Content.Text == nil {
		t.Fatal("first block should be the text output")
	}
	if blocks[1].Content == nil || blocks[1].Content.Content.Image == nil {
		t.Fatal("second block should be an image")
	}
	imgBlock := blocks[1].Content.Content.Image
	if imgBlock.MimeType != "image/png" {
		t.Fatalf("mime = %q", imgBlock.MimeType)
	}
	if imgBlock.Data != base64.StdEncoding.EncodeToString(payload) {
		t.Fatal("image data does not round-trip the file")
	}

	if blocks := toolOutputContent("", nil); blocks != nil {
		t.Fatalf("empty output should produce no blocks, got %v", blocks)
	}
}

// jsonString renders v compactly for failure messages.
func jsonString(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
