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
	call := buildFantasyCall("anthropic", models.APIAnthropic, req)
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
	call := buildFantasyCall("openai", models.APIOpenAI, req)
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
	call := buildFantasyCall("anthropic", models.APIAnthropic, Request{Prompt: "what is this", Images: []string{img}})
	if got := imagePartsOf(call.Prompt[0]); got != 1 {
		t.Fatalf("expected 1 image part, got %d", got)
	}
}
