package acp

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
	"spettro/internal/provider"
	"spettro/internal/session"
)

func TestReadPromptContent_TextAndResourceLink(t *testing.T) {
	p, err := readPromptContent([]acpsdk.ContentBlock{
		acpsdk.TextBlock("Read "),
		acpsdk.ResourceLinkBlock("main.go", "file:///tmp/proj/main.go"),
		acpsdk.TextBlock(" and summarize it."),
	}, t.TempDir())
	task, images, mentioned := p.task(), p.images, p.mentioned
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task != "Read @"+filepath.FromSlash("/tmp/proj/main.go")+" and summarize it." {
		t.Fatalf("unexpected task: %q", task)
	}
	if len(images) != 0 {
		t.Fatalf("expected no images, got %v", images)
	}
	if len(mentioned) != 1 || mentioned[0] != filepath.FromSlash("/tmp/proj/main.go") {
		t.Fatalf("unexpected mentioned files: %v", mentioned)
	}
}

// Resource links are URIs: a percent-encoded space or a "localhost"
// authority must still name the file on disk, and a link that is not a
// file:// URI names no file to read first.
func TestReadPromptContent_ResourceLinkURIForms(t *testing.T) {
	p, err := readPromptContent([]acpsdk.ContentBlock{
		acpsdk.ResourceLinkBlock("a.txt", "file:///tmp/My%20Proj/a.txt"),
		acpsdk.ResourceLinkBlock("b.txt", "file://localhost/tmp/b.txt"),
		acpsdk.ResourceLinkBlock("docs", "https://example.com/docs"),
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.FromSlash("/tmp/My Proj/a.txt"), filepath.FromSlash("/tmp/b.txt")}
	if strings.Join(p.mentioned, "|") != strings.Join(want, "|") {
		t.Fatalf("mentioned = %q, want %q", p.mentioned, want)
	}
	if !strings.Contains(p.typed, "@https://example.com/docs") {
		t.Fatalf("a non-file link should stay in the text: %q", p.typed)
	}
}

func TestReadPromptContent_EmbeddedResource(t *testing.T) {
	p, err := readPromptContent([]acpsdk.ContentBlock{
		acpsdk.TextBlock("Explain this."),
		acpsdk.ResourceBlock(acpsdk.EmbeddedResourceResource{
			TextResourceContents: &acpsdk.TextResourceContents{
				Uri:  "file:///tmp/proj/util.go",
				Text: "package util",
			},
		}),
	}, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	task := p.task()
	if !strings.Contains(task, "Context from /tmp/proj/util.go:") || !strings.Contains(task, "package util") {
		t.Fatalf("embedded context missing from task: %q", task)
	}
	// The typed text is kept apart from the attached file, so a skill
	// command can be parsed from it alone.
	if p.typed != "Explain this." {
		t.Fatalf("typed text = %q", p.typed)
	}
}

func TestReadPromptContent_ImageDecodedToFile(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0x89, 0x50, 0x4e, 0x47}
	p, err := readPromptContent([]acpsdk.ContentBlock{
		acpsdk.TextBlock("look"),
		acpsdk.ImageBlock(base64.StdEncoding.EncodeToString(payload), "image/png"),
	}, filepath.Join(dir, "media"))
	images := p.images
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected one image, got %v", images)
	}
	raw, err := os.ReadFile(images[0])
	if err != nil {
		t.Fatalf("read decoded image: %v", err)
	}
	if string(raw) != string(payload) {
		t.Fatalf("decoded image content mismatch")
	}
}

// TestSessionHistoryIsStructured pins the cross-turn contract: the session
// stores the run's structured conversation verbatim (no flattening, no head
// eviction), because any mutation of carried turns would change the provider
// request prefix and defeat prompt caching.
func TestSessionHistoryIsStructured(t *testing.T) {
	s := &acpSession{}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "Task:\ndo the thing"},
		{Role: provider.RoleAssistant, Content: "done: " + strings.Repeat("x", 1024)},
	}
	s.history = msgs
	if len(s.history) != 2 {
		t.Fatalf("expected 2 carried messages, got %d", len(s.history))
	}
	if s.history[0].Content != msgs[0].Content || s.history[1].Content != msgs[1].Content {
		t.Fatalf("carried history must be stored verbatim")
	}
}

func TestPlanEntriesFromTodos(t *testing.T) {
	todos := []session.Todo{
		{ID: "c", Content: "ship", Status: "pending", Dependencies: []string{"b"}},
		{ID: "a", Content: "design", Status: "completed", Priority: "high"},
		{ID: "b", Content: "build", Status: "in_progress", Priority: "low", Dependencies: []string{"a"}},
	}
	entries := planEntriesFromTodos(todos)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	// Dependency order: a, b, c.
	if entries[0].Content != "design" || entries[0].Status != acpsdk.PlanEntryStatusCompleted || entries[0].Priority != acpsdk.PlanEntryPriorityHigh {
		t.Fatalf("unexpected first entry: %#v", entries[0])
	}
	if entries[1].Content != "build" || entries[1].Status != acpsdk.PlanEntryStatusInProgress || entries[1].Priority != acpsdk.PlanEntryPriorityLow {
		t.Fatalf("unexpected second entry: %#v", entries[1])
	}
	if entries[2].Content != "ship (blocked)" || entries[2].Status != acpsdk.PlanEntryStatusPending || entries[2].Priority != acpsdk.PlanEntryPriorityMedium {
		t.Fatalf("unexpected third entry: %#v", entries[2])
	}
}

// Only the session agent's own words reach the chat: a sub-agent's
// narration, comment-tool messages and steering notices stay on its cards.
func TestCommentChatText(t *testing.T) {
	turn := newSilentTurn()
	turn.agentID = "coding"
	cases := []struct {
		what string
		tr   agent.ToolTrace
		want string
	}{
		{"own narration", agent.ToolTrace{AgentID: "coding", Name: "comment", Status: "success", Output: "Looking.", Narration: true}, "Looking."},
		{"sub-agent narration", agent.ToolTrace{AgentID: "explore", Name: "comment", Status: "success", Output: "Looking.", Narration: true}, ""},
		{"own comment tool", agent.ToolTrace{AgentID: "coding", Name: "comment", Status: "running", Args: `{"message":"halfway"}`}, "halfway"},
		{"sub-agent comment tool", agent.ToolTrace{AgentID: "code#2", Name: "comment", Status: "running", Args: `{"message":"halfway"}`}, ""},
		{"own steering", agent.ToolTrace{AgentID: "coding", Name: "comment", Status: "success", Output: "steering delivered: use sqlite"}, "✔ steering delivered: use sqlite"},
		// A sub-agent's private steering queue carries only the runtime's
		// time-limit wrap-up notice; the user sent nothing.
		{"sub-agent steering", agent.ToolTrace{AgentID: "code#2", Name: "comment", Status: "success", Output: "steering delivered: wrap up now"}, ""},
		{"runtime note", agent.ToolTrace{AgentID: "coding", Name: "comment", Status: "success", Output: "Starting bash (ls)"}, ""},
	}
	for _, tc := range cases {
		if got := turn.commentChatText(tc.tr); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.what, got, tc.want)
		}
	}
}
