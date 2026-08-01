package provider

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
"spettro/internal/models"
)

func writeTestImage(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(p, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func imagePartsOf(msg fantasy.Message) int {
	n := 0
	for _, part := range msg.Content {
		if _, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
			n++
		}
	}
	return n
}

// Message-level images must reach the provider on every request that carries
// that message — not just the first call of a run.
func TestFantasyCallCarriesMessageImages(t *testing.T) {
	img := writeTestImage(t)
	req := Request{
		Messages: []Message{
			{Role: RoleUser, Content: "look at this", Images: []string{img}},
			{Role: RoleAssistant, Content: "ok", ToolCalls: []NativeTool{{ID: "1", Name: "file-read", Args: []byte(`{}`)}}},
			{Role: RoleUser, ToolResults: []ToolResult{{ID: "1", Name: "file-read", Output: "data"}}},
		},
	}
	call := buildFantasyCall("anthropic", models.APIAnthropic, "claude-sonnet-4-5", req)
	if len(call.Prompt) == 0 {
		t.Fatal("empty prompt")
	}
	if got := imagePartsOf(call.Prompt[0]); got != 1 {
		t.Fatalf("expected 1 image part on the user turn, got %d", got)
	}
}

// Request-level images (legacy field) attach to the last plain user message.
func TestFantasyCallAttachesRequestImagesToLastUserTurn(t *testing.T) {
	img := writeTestImage(t)
	req := Request{
		Images: []string{img},
		Messages: []Message{
			{Role: RoleUser, Content: "earlier"},
			{Role: RoleAssistant, Content: "sure"},
			{Role: RoleUser, Content: "current turn"},
		},
	}
	call := buildFantasyCall("openai", models.APIOpenAI, "gpt-4o", req)
	last := call.Prompt[len(call.Prompt)-1]
	if got := imagePartsOf(last); got != 1 {
		t.Fatalf("expected 1 image part on last user turn, got %d", got)
	}
	if got := imagePartsOf(call.Prompt[0]); got != 0 {
		t.Fatalf("expected no image on earlier turn, got %d", got)
	}
}

// Prompt-only requests (Chatter.Reply) still carry images.
func TestFantasyCallPromptPathImages(t *testing.T) {
	img := writeTestImage(t)
	call := buildFantasyCall("anthropic", models.APIAnthropic, "claude-sonnet-4-5", Request{Prompt: "what is this", Images: []string{img}})
	if got := imagePartsOf(call.Prompt[0]); got != 1 {
		t.Fatalf("expected 1 image part, got %d", got)
	}
}

// A chat that accumulated images under a vision model must remain usable after
// switching to a non-vision model: images are stripped from the outgoing
// request and replaced by text placeholders, without mutating the original.
func TestStripImagesReplacesWithPlaceholders(t *testing.T) {
	img := writeTestImage(t)
	req := Request{
		Images: []string{img},
		Messages: []Message{
			{Role: RoleUser, Content: "look", Images: []string{img, img}},
			{Role: RoleAssistant, Content: "ok", ToolCalls: []NativeTool{{ID: "1", Name: "screenshot", Args: []byte(`{}`)}}},
			{Role: RoleUser, ToolResults: []ToolResult{{ID: "1", Name: "screenshot", Output: "took it", Images: []string{img}}}},
			{Role: RoleUser, Content: "current turn"},
		},
	}
	out := stripImages(req)

	if len(out.Images) != 0 {
		t.Fatal("request-level images not stripped")
	}
	for i, m := range out.Messages {
		if len(m.Images) != 0 {
			t.Fatalf("message %d still has images", i)
		}
		for _, tr := range m.ToolResults {
			if len(tr.Images) != 0 {
				t.Fatalf("message %d tool result still has images", i)
			}
		}
	}
	if got := out.Messages[0].Content; got != "look\n\n[2 images omitted: the current model does not support vision]" {
		t.Fatalf("unexpected message content: %q", got)
	}
	if got := out.Messages[2].ToolResults[0].Output; got != "took it\n\n"+imageOmittedNote {
		t.Fatalf("unexpected tool result output: %q", got)
	}
	if got := out.Messages[3].Content; got != "current turn\n\n"+imageOmittedNote {
		t.Fatalf("request-level image note missing from last user turn: %q", got)
	}

	// The original request (chat history) must be untouched.
	if len(req.Images) != 1 || len(req.Messages[0].Images) != 2 || len(req.Messages[2].ToolResults[0].Images) != 1 {
		t.Fatal("stripImages mutated the original request")
	}
	if req.Messages[0].Content != "look" {
		t.Fatal("stripImages mutated original message content")
	}
}

func TestStripImagesLegacyPrompt(t *testing.T) {
	img := writeTestImage(t)
	out := stripImages(Request{Prompt: "hi", Images: []string{img}})
	if len(out.Images) != 0 {
		t.Fatal("images not stripped")
	}
	if out.Prompt != "hi\n\n"+imageOmittedNote {
		t.Fatalf("unexpected prompt: %q", out.Prompt)
	}
}
