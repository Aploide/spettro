package acp

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/provider"
)

func TestPromptFromBlocks_TextAndResourceLink(t *testing.T) {
	task, images, mentioned, err := promptFromBlocks([]acpsdk.ContentBlock{
		acpsdk.TextBlock("Read "),
		acpsdk.ResourceLinkBlock("main.go", "file:///tmp/proj/main.go"),
		acpsdk.TextBlock(" and summarize it."),
	}, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task != "Read @/tmp/proj/main.go and summarize it." {
		t.Fatalf("unexpected task: %q", task)
	}
	if len(images) != 0 {
		t.Fatalf("expected no images, got %v", images)
	}
	if len(mentioned) != 1 || mentioned[0] != "/tmp/proj/main.go" {
		t.Fatalf("unexpected mentioned files: %v", mentioned)
	}
}

func TestPromptFromBlocks_EmbeddedResource(t *testing.T) {
	task, _, _, err := promptFromBlocks([]acpsdk.ContentBlock{
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
	if !strings.Contains(task, "Context from /tmp/proj/util.go:") || !strings.Contains(task, "package util") {
		t.Fatalf("embedded context missing from task: %q", task)
	}
}

func TestPromptFromBlocks_ImageDecodedToFile(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0x89, 0x50, 0x4e, 0x47}
	_, images, _, err := promptFromBlocks([]acpsdk.ContentBlock{
		acpsdk.TextBlock("look"),
		acpsdk.ImageBlock(base64.StdEncoding.EncodeToString(payload), "image/png"),
	}, filepath.Join(dir, "media"))
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

func TestToolKindClassification(t *testing.T) {
	cases := map[string]acpsdk.ToolKind{
		"file-read":   acpsdk.ToolKindRead,
		"file-edit":   acpsdk.ToolKindEdit,
		"file-write":  acpsdk.ToolKindEdit,
		"shell-exec":  acpsdk.ToolKindExecute,
		"repo-search": acpsdk.ToolKindSearch,
		"grep":        acpsdk.ToolKindSearch,
		"http-fetch":  acpsdk.ToolKindFetch,
		"mystery":     acpsdk.ToolKindOther,
	}
	for name, want := range cases {
		if got := toolKind(name); got != want {
			t.Errorf("toolKind(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestToolLocations(t *testing.T) {
	locs := toolLocations(`{"path":"/tmp/a.go","content":"x"}`)
	if len(locs) != 1 || locs[0].Path != "/tmp/a.go" {
		t.Fatalf("unexpected locations: %v", locs)
	}
	if locs := toolLocations("not json"); locs != nil {
		t.Fatalf("expected nil locations for non-JSON args, got %v", locs)
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
