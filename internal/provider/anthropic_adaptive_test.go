package provider

import (
	"context"
	"testing"

	fantasyanthropic "charm.land/fantasy/providers/anthropic"

	"spettro/internal/models"
)

func TestAnthropicAdaptiveThinkingModels(t *testing.T) {
	cases := map[string]bool{
		"claude-opus-4-7":                            true,
		"claude-opus-4-8":                            true,
		"claude-opus-5-5":                            true,
		"claude-sonnet-4-6":                          true,
		"claude-sonnet-5-5":                          true,
		"claude-haiku-5-5":                           true,
		"claude-fable-5-1":                           true,
		"claude-mythos-5-1":                          true,
		"claude-opus-4-7[1m]":                        true,
		"anthropic/claude-opus-4.7":                  true,
		"us.anthropic.claude-opus-4-8-v1:0":          true,
		"claude-opus-4-5@20251101":                   false,
		"claude-opus-4-5-20251101":                   false,
		"claude-sonnet-4-5":                          false,
		"claude-haiku-4-5-20251001":                  false,
		"claude-opus-4-20250514":                     false,
		"claude-opus-4-1-20250805":                   false,
		"claude-3-7-sonnet-20250219":                 false,
		"MiniMax-M2":                                 false,
		"glm-4.6":                                    false,
		"us.anthropic.claude-3-5-sonnet-20241022-v2": false,
	}
	for model, want := range cases {
		if got := anthropicAdaptiveThinking(model); got != want {
			t.Errorf("anthropicAdaptiveThinking(%q) = %v, want %v", model, got, want)
		}
	}
}

// Claude 4.6+ get thinking as an output_config effort (fantasy turns that
// into thinking.type "adaptive"); older Claude models and non-Claude models
// on the Anthropic protocol (MiniMax) keep thinking.budget_tokens.
func TestAnthropicThinkingConfigPerModel(t *testing.T) {
	cases := []struct {
		model      string
		level      ThinkingLevel
		wantEffort string
		wantBudget bool
	}{
		{"claude-opus-4-7", ThinkingHigh, "high", false},
		{"claude-opus-4-7", ThinkingXHigh, "xhigh", false},
		{"claude-opus-4-6", ThinkingXHigh, "high", false},
		{"claude-opus-5-5", ThinkingMax, "max", false},
		{"claude-opus-5-5", ThinkingOff, "", false},
		{"claude-sonnet-4-5", ThinkingHigh, "", true},
		{"MiniMax-M2", ThinkingHigh, "", true},
	}
	for _, tc := range cases {
		call := buildFantasyCall("anthropic", models.APIAnthropic, tc.model, Request{Prompt: "hi", MaxTokens: 32000, Thinking: tc.level})
		opts, _ := call.ProviderOptions[fantasyanthropic.Name].(*fantasyanthropic.ProviderOptions)
		var effort string
		if opts != nil && opts.Effort != nil {
			effort = string(*opts.Effort)
		}
		if effort != tc.wantEffort {
			t.Errorf("%s at %q: effort = %q, want %q", tc.model, tc.level, effort, tc.wantEffort)
		}
		if got := thinkingBudgetOf(t, call) > 0; got != tc.wantBudget {
			t.Errorf("%s at %q: budget sent = %v, want %v", tc.model, tc.level, got, tc.wantBudget)
		}
	}
}

// A model that always thinks returns signed blocks even with thinking off;
// they go back on the next request instead of being dropped.
func TestAdaptiveModelReplaysReasoningWhenOff(t *testing.T) {
	blocks := []ReasoningBlock{{Text: "t", Signature: "sig", Provider: "anthropic", Model: "claude-opus-5-5"}}
	if got := replayReasoning("anthropic", models.APIAnthropic, "claude-opus-5-5", Request{}, blocks); len(got) != 1 {
		t.Errorf("adaptive model: replayed %d blocks, want 1", len(got))
	}
	blocks[0].Model = "claude-sonnet-4-5"
	if got := replayReasoning("anthropic", models.APIAnthropic, "claude-sonnet-4-5", Request{}, blocks); len(got) != 0 {
		t.Errorf("budget model with thinking off: replayed %d blocks, want 0", len(got))
	}
}

// On the wire: thinking.type "adaptive" with output_config.effort, and no
// budget_tokens, which Claude 4.7+ reject with a 400.
func TestAnthropicAdaptiveThinkingWire(t *testing.T) {
	pm, bodies := newAnthropicServer(t, anthropicTextStream())
	// Models the catalog does not list still go out over the provider's
	// Anthropic protocol.
	for _, model := range []string{"claude-opus-4-7", "MiniMax-M2"} {
		if _, err := pm.Send(context.Background(), "anth-test", model, Request{Prompt: "hi", MaxTokens: 32000, Thinking: ThinkingHigh}); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
	}
	opus, minimax := (*bodies)[0], (*bodies)[1]
	thinking, _ := opus["thinking"].(map[string]any)
	outCfg, _ := opus["output_config"].(map[string]any)
	if thinking["type"] != "adaptive" || thinking["budget_tokens"] != nil {
		t.Errorf("claude-opus-4-7 thinking = %v, want adaptive without budget_tokens", thinking)
	}
	if outCfg["effort"] != "high" {
		t.Errorf("claude-opus-4-7 output_config = %v, want effort high", outCfg)
	}
	thinking, _ = minimax["thinking"].(map[string]any)
	if _, has := minimax["output_config"]; thinking["type"] != "enabled" || thinking["budget_tokens"] == nil || has {
		t.Errorf("MiniMax-M2 thinking = %v (output_config sent: %v), want enabled with budget_tokens", thinking, has)
	}
}

// anthropicTextStream is a plain one-line text reply.
func anthropicTextStream() []string {
	return []string{
		anthropicEvent("message_start", map[string]any{"message": map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "m",
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		}}),
		anthropicEvent("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}}),
		anthropicEvent("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": "ok"}}),
		anthropicEvent("content_block_stop", map[string]any{"index": 0}),
		anthropicEvent("message_delta", map[string]any{"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 1}}),
		anthropicEvent("message_stop", map[string]any{}),
	}
}
