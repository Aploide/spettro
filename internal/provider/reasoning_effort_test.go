package provider

import (
	"errors"
	"testing"

	fantasyopenai "charm.land/fantasy/providers/openai"
	fantasyopenaicompat "charm.land/fantasy/providers/openaicompat"

	"spettro/internal/models"
)

func TestReasoningEffortMapping(t *testing.T) {
	cases := []struct {
		level ThinkingLevel
		want  string
	}{
		{ThinkingOff, ""},
		{ThinkingLevel(""), ""},
		{ThinkingLow, "low"},
		{ThinkingMedium, "medium"},
		{ThinkingHigh, "high"},
		{ThinkingXHigh, "xhigh"},
		{ThinkingMax, "xhigh"},
	}
	for _, tc := range cases {
		if got := ReasoningEffort(tc.level); got != tc.want {
			t.Errorf("ReasoningEffort(%q) = %q, want %q", tc.level, got, tc.want)
		}
	}
}

func TestThinkingSetsEffortForOpenAIResponsesModel(t *testing.T) {
	call := buildFantasyCall("openai", models.APIOpenAI, "o3", Request{Prompt: "hi", Thinking: ThinkingMedium})
	opts, ok := call.ProviderOptions[fantasyopenai.Name].(*fantasyopenai.ResponsesProviderOptions)
	if !ok {
		t.Fatalf("expected *openai.ResponsesProviderOptions, got %T", call.ProviderOptions[fantasyopenai.Name])
	}
	if opts.ReasoningEffort == nil || *opts.ReasoningEffort != fantasyopenai.ReasoningEffortMedium {
		t.Errorf("reasoning effort = %v, want medium", opts.ReasoningEffort)
	}
}

func TestThinkingSetsEffortForOpenAIChatModel(t *testing.T) {
	// A model id fantasy doesn't route to the Responses API (unknown ids fall
	// back to the chat completions path).
	call := buildFantasyCall("openai", models.APIOpenAI, "gpt-future-chat", Request{Prompt: "hi", Thinking: ThinkingHigh})
	opts, ok := call.ProviderOptions[fantasyopenai.Name].(*fantasyopenai.ProviderOptions)
	if !ok {
		t.Fatalf("expected *openai.ProviderOptions, got %T", call.ProviderOptions[fantasyopenai.Name])
	}
	if opts.ReasoningEffort == nil || *opts.ReasoningEffort != fantasyopenai.ReasoningEffortHigh {
		t.Errorf("reasoning effort = %v, want high", opts.ReasoningEffort)
	}
}

func TestThinkingSetsEffortForOpenAICompatProviders(t *testing.T) {
	for _, providerName := range []string{"groq", "xai", "deepseek", "spettro", "http://localhost:11434"} {
		call := buildFantasyCall(providerName, models.APIOpenAI, "some-model", Request{Prompt: "hi", Thinking: ThinkingLow})
		opts, ok := call.ProviderOptions[fantasyopenaicompat.Name].(*fantasyopenaicompat.ProviderOptions)
		if !ok {
			t.Fatalf("%s: expected *openaicompat.ProviderOptions, got %T", providerName, call.ProviderOptions[fantasyopenaicompat.Name])
		}
		if opts.ReasoningEffort == nil || *opts.ReasoningEffort != fantasyopenai.ReasoningEffortLow {
			t.Errorf("%s: reasoning effort = %v, want low", providerName, opts.ReasoningEffort)
		}
	}
}

func TestDowngradedThinkingOnEffortError(t *testing.T) {
	m := NewManager()
	effortErr := errors.New(`400: invalid value for reasoning_effort: "xhigh"`)

	// Non-Anthropic providers skip levels that serialize to the same wire
	// value: max and x-high are both "xhigh", so max drops straight to high.
	next, ok := m.downgradedThinking("groq", ThinkingMax, effortErr)
	if !ok || next != ThinkingHigh {
		t.Errorf("groq max -> (%q, %v), want (high, true)", next, ok)
	}
	// Anthropic uses distinct token budgets per level, so it steps one down.
	next, ok = m.downgradedThinking("anthropic", ThinkingMax, errors.New("thinking.budget_tokens: 100000 exceeds max"))
	if !ok || next != ThinkingXHigh {
		t.Errorf("anthropic max -> (%q, %v), want (x-high, true)", next, ok)
	}
	// The bottom of the ladder retries with no thinking parameter at all.
	next, ok = m.downgradedThinking("groq", ThinkingLow, effortErr)
	if !ok || next != "" {
		t.Errorf("groq low -> (%q, %v), want (\"\", true)", next, ok)
	}
	// Once at "" there is nothing left to try: the error surfaces.
	if _, ok := m.downgradedThinking("groq", "", effortErr); ok {
		t.Error("expected no retry once thinking is already empty")
	}
	// Unrelated errors must surface unchanged.
	if _, ok := m.downgradedThinking("groq", ThinkingHigh, errors.New("401 unauthorized")); ok {
		t.Error("expected no retry for a non-thinking error")
	}
}

func TestThinkingOffSendsNoEffort(t *testing.T) {
	for _, level := range []ThinkingLevel{"", ThinkingOff} {
		call := buildFantasyCall("openai", models.APIOpenAI, "o3", Request{Prompt: "hi", Thinking: level})
		if len(call.ProviderOptions) != 0 {
			t.Errorf("Thinking=%q: expected no provider options, got %v", level, call.ProviderOptions)
		}
	}
}
