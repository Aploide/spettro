package provider

import (
	"testing"

	fantasyanthropic "charm.land/fantasy/providers/anthropic"
	anthropic "github.com/anthropics/anthropic-sdk-go"

	"spettro/internal/models"
)

func TestAnthropicPromptCachingSystem(t *testing.T) {
	a := AnthropicAdapter{APIKey: "test"}
	req := Request{
		System: "You are a helpful assistant.",
		Messages: []Message{
			{Role: RoleUser, Content: "hello"},
		},
	}
	params := buildAnthropicParams(a, req)

	if len(params.System) == 0 {
		t.Fatal("expected system blocks")
	}
	cc := params.System[len(params.System)-1].CacheControl
	if cc.Type != "ephemeral" {
		t.Errorf("system block cache_control.type = %q, want \"ephemeral\"", cc.Type)
	}
}

// TestAnthropicPromptCachingFinalMessage pins the second breakpoint to the
// FINAL message: the next request in a loop or session extends this exact
// prefix, so the newest content (frequently the largest tool results) must be
// inside the cached span, not left after it.
func TestAnthropicPromptCachingFinalMessage(t *testing.T) {
	a := AnthropicAdapter{APIKey: "test"}
	req := Request{
		System: "system",
		Messages: []Message{
			{Role: RoleUser, Content: "turn1"},
			{Role: RoleAssistant, Content: "reply1"},
			{Role: RoleUser, Content: "turn2"},
		},
	}
	params := buildAnthropicParams(a, req)

	if len(params.Messages) < 2 {
		t.Fatalf("expected >=2 messages, got %d", len(params.Messages))
	}
	final := params.Messages[len(params.Messages)-1]
	if len(final.Content) == 0 {
		t.Fatal("final message has no content")
	}
	last := final.Content[len(final.Content)-1]
	if last.OfText == nil {
		t.Fatal("expected text block")
	}
	if last.OfText.CacheControl.Type != "ephemeral" {
		t.Errorf("final message cache_control.type = %q, want \"ephemeral\"", last.OfText.CacheControl.Type)
	}
	// Earlier messages carry no breakpoint of their own; the moving marker on
	// the final message is the incremental caching pattern.
	first := params.Messages[0]
	if fb := first.Content[len(first.Content)-1]; fb.OfText != nil && fb.OfText.CacheControl.Type == "ephemeral" {
		t.Error("non-final message should not carry a cache breakpoint")
	}
}

func TestFantasyAnthropicCacheControlPlacement(t *testing.T) {
	req := Request{
		System: "system",
		Messages: []Message{
			{Role: RoleUser, Content: "turn1"},
			{Role: RoleAssistant, Content: "reply1"},
			{Role: RoleUser, Content: "turn2"},
		},
	}
	call := buildFantasyCall("anthropic", models.APIAnthropic, "claude-sonnet-4-5", req)
	if len(call.Prompt) < 2 {
		t.Fatalf("expected >=2 prompt messages, got %d", len(call.Prompt))
	}
	if cc := fantasyanthropic.GetCacheControl(call.Prompt[0].ProviderOptions); cc == nil {
		t.Error("system message should carry cache_control")
	}
	if cc := fantasyanthropic.GetCacheControl(call.Prompt[len(call.Prompt)-1].ProviderOptions); cc == nil {
		t.Error("final message should carry cache_control")
	}
	for _, msg := range call.Prompt[1 : len(call.Prompt)-1] {
		if cc := fantasyanthropic.GetCacheControl(msg.ProviderOptions); cc != nil {
			t.Error("middle messages should not carry cache_control")
		}
	}
}

func TestNonAnthropicNoCache(t *testing.T) {
	req := Request{
		System: "system",
		Messages: []Message{
			{Role: RoleUser, Content: "turn1"},
			{Role: RoleAssistant, Content: "reply1"},
			{Role: RoleUser, Content: "turn2"},
		},
	}
	call := buildFantasyCall("openai", models.APIOpenAI, "gpt-4o", req)
	for _, msg := range call.Prompt {
		cc := fantasyanthropic.GetCacheControl(msg.ProviderOptions)
		if cc != nil {
			t.Error("non-Anthropic provider should not have cache_control on messages")
		}
	}
}

// buildAnthropicParams mirrors the params construction in AnthropicAdapter.Send
// without making a real API call, for unit-testing the request shape.
func buildAnthropicParams(a AnthropicAdapter, req Request) anthropic.MessageNewParams {
	const defaultMaxTokens = int64(16384)
	maxTokens := defaultMaxTokens
	if req.MaxTokens > 0 {
		maxTokens = int64(req.MaxTokens)
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model("claude-sonnet-4-5"),
		MaxTokens: maxTokens,
	}
	if len(req.Messages) > 0 {
		if req.System != "" {
			sysBlock := anthropic.TextBlockParam{Text: req.System}
			sysBlock.CacheControl = anthropic.NewCacheControlEphemeralParam()
			params.System = []anthropic.TextBlockParam{sysBlock}
		}
		var msgs []anthropic.MessageParam
		for _, m := range req.Messages {
			switch m.Role {
			case RoleUser:
				msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
			case RoleAssistant:
				msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
			}
		}
		if n := len(msgs); n > 0 {
			final := &msgs[n-1]
			if k := len(final.Content); k > 0 {
				last := &final.Content[k-1]
				if last.OfText != nil {
					last.OfText.CacheControl = anthropic.NewCacheControlEphemeralParam()
				}
			}
		}
		params.Messages = msgs
	}
	return params
}
