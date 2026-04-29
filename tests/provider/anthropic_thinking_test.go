package provider_test

import (
	"strings"
	"testing"

	"spettro/internal/provider"
)

// TestAnthropicAdapter_ThinkingHigh_BudgetMapping is a guardrail: high-level
// thinking has to map to a non-trivial budget so Claude actually engages
// extended thinking. Concretely, Anthropic requires budget_tokens >= 1024
// when thinking.type=enabled; we want a budget at least an order of
// magnitude above that for "high" so users see a real difference.
func TestAnthropicAdapter_ThinkingHigh_BudgetMapping(t *testing.T) {
	got := provider.ThinkingBudgetTokens(provider.ThinkingHigh)
	if got < 16000 {
		t.Fatalf("expected high budget >= 16000, got %d", got)
	}
	if got >= provider.ThinkingBudgetTokens(provider.ThinkingXHigh) {
		t.Fatalf("expected x-high budget > high budget, got high=%d x-high=%d", got, provider.ThinkingBudgetTokens(provider.ThinkingXHigh))
	}
}

// TestAnthropicRequest_ThinkingPlumbing is a quick smoke check that the
// Request struct round-trips a ThinkingLevel. This guards against
// accidentally renaming/dropping the field in struct refactors.
func TestAnthropicRequest_ThinkingPlumbing(t *testing.T) {
	req := provider.Request{Prompt: "p", Thinking: provider.ThinkingHigh}
	if req.Thinking != provider.ThinkingHigh {
		t.Fatalf("expected ThinkingHigh, got %q", req.Thinking)
	}
	if !strings.EqualFold(string(req.Thinking), "high") {
		t.Fatalf("expected level string 'high', got %q", req.Thinking)
	}
}
