package provider

import (
	"encoding/base64"
	"os"
	"testing"

	"charm.land/fantasy"

	"spettro/internal/models"
)

func toolResultConversation(images ...string) Request {
	return Request{
		Messages: []Message{
			{Role: RoleUser, Content: "look at the site"},
			{Role: RoleAssistant, ToolCalls: []NativeTool{{ID: "call-1", Name: "screenshot", Args: []byte(`{"url":"https://x.test"}`)}}},
			{Role: RoleUser, ToolResults: []ToolResult{{ID: "call-1", Name: "screenshot", Output: `{"file":"shot.png"}`, Images: images}}},
		},
	}
}

// Anthropic consumes tool-produced images as media INSIDE the tool_result
// block, keeping the image tied to the exact call that produced it.
func TestToolResultImagesAnthropicMedia(t *testing.T) {
	img := writeTestImage(t)
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}

	call := buildFantasyCall("anthropic", models.APIAnthropic, "claude-sonnet-4-5", toolResultConversation(img))
	if len(call.Prompt) != 3 {
		t.Fatalf("expected 3 prompt messages (user, assistant, tool), got %d", len(call.Prompt))
	}
	toolMsg := call.Prompt[2]
	if toolMsg.Role != fantasy.MessageRoleTool {
		t.Fatalf("expected tool message, got role %q", toolMsg.Role)
	}
	part, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](toolMsg.Content[0])
	if !ok {
		t.Fatal("expected a tool result part")
	}
	media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](part.Output)
	if !ok {
		t.Fatalf("expected media output, got %T", part.Output)
	}
	if media.MediaType != "image/png" {
		t.Fatalf("media type = %q", media.MediaType)
	}
	if media.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Fatal("media data does not round-trip the image file")
	}
	if media.Text != `{"file":"shot.png"}` {
		t.Fatalf("tool text output lost: %q", media.Text)
	}
}

// Providers whose tool results are text-only get the image as an immediately
// following user turn instead, so the model still sees it.
func TestToolResultImagesNonAnthropicSpillToUserTurn(t *testing.T) {
	img := writeTestImage(t)
	for _, providerName := range []string{"openai", "ollama"} {
		call := buildFantasyCall(providerName, models.APIOpenAI, "test-model", toolResultConversation(img))
		if len(call.Prompt) != 4 {
			t.Fatalf("%s: expected 4 prompt messages (spill user turn appended), got %d", providerName, len(call.Prompt))
		}
		part, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](call.Prompt[2].Content[0])
		if !ok {
			t.Fatalf("%s: expected tool result part", providerName)
		}
		if _, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](part.Output); !ok {
			t.Fatalf("%s: tool result should stay text, got %T", providerName, part.Output)
		}
		spill := call.Prompt[3]
		if spill.Role != fantasy.MessageRoleUser {
			t.Fatalf("%s: spill turn role = %q", providerName, spill.Role)
		}
		if got := imagePartsOf(spill); got != 1 {
			t.Fatalf("%s: expected 1 image part on the spill turn, got %d", providerName, got)
		}
	}
}

// A second image on the same result cannot ride the anthropic media output
// (one image per tool_result); it spills to the follow-up user turn.
func TestToolResultExtraImagesSpillOnAnthropic(t *testing.T) {
	img1 := writeTestImage(t)
	img2 := writeTestImage(t)
	call := buildFantasyCall("anthropic", models.APIAnthropic, "claude-sonnet-4-5", toolResultConversation(img1, img2))
	if len(call.Prompt) != 4 {
		t.Fatalf("expected 4 prompt messages, got %d", len(call.Prompt))
	}
	if got := imagePartsOf(call.Prompt[3]); got != 1 {
		t.Fatalf("expected 1 spilled image part, got %d", got)
	}
}

// A vanished image file degrades to a text-only tool result on every provider
// instead of failing the request or appending an empty image turn.
func TestToolResultImagesUnreadableFileDegradesToText(t *testing.T) {
	for _, providerName := range []string{"anthropic", "openai"} {
		apiKind := models.APIOpenAI
		if providerName == "anthropic" {
			apiKind = models.APIAnthropic
		}
		call := buildFantasyCall(providerName, apiKind, "test-model", toolResultConversation("/no/such/image.png"))
		if len(call.Prompt) != 3 {
			t.Fatalf("%s: expected 3 prompt messages (no spill turn), got %d", providerName, len(call.Prompt))
		}
		part, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](call.Prompt[2].Content[0])
		if !ok {
			t.Fatalf("%s: expected tool result part", providerName)
		}
		if _, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](part.Output); !ok {
			t.Fatalf("%s: expected text fallback, got %T", providerName, part.Output)
		}
	}
}
