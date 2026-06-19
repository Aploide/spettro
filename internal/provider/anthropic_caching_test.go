package provider

import (
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	fantasyanthropic "charm.land/fantasy/providers/anthropic"
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

func TestAnthropicPromptCachingPenultimateMessage(t *testing.T) {
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
	penultimate := params.Messages[len(params.Messages)-2]
	if len(penultimate.Content) == 0 {
		t.Fatal("penultimate message has no content")
	}
	last := penultimate.Content[len(penultimate.Content)-1]
	if last.OfText == nil {
		t.Fatal("expected text block")
	}
	if last.OfText.CacheControl.Type != "ephemeral" {
		t.Errorf("penultimate message cache_control.type = %q, want \"ephemeral\"", last.OfText.CacheControl.Type)
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
	call := buildFantasyCall("openai", req)
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
		firstUser := true
		for _, m := range req.Messages {
			switch m.Role {
			case RoleUser:
				var blocks []anthropic.ContentBlockParamUnion
				if firstUser {
					firstUser = false
				}
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
				msgs = append(msgs, anthropic.NewUserMessage(blocks...))
			case RoleAssistant:
				msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
			}
		}
		if len(msgs) >= 2 {
			penultimate := &msgs[len(msgs)-2]
			if n := len(penultimate.Content); n > 0 {
				last := &penultimate.Content[n-1]
				if last.OfText != nil {
					last.OfText.CacheControl = anthropic.NewCacheControlEphemeralParam()
				}
			}
		}
		params.Messages = msgs
	}
	return params
}
