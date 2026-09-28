package provider

import (
	"testing"

	"spettro/internal/models"
)

// max_tokens on the Anthropic path: an explicit (already resolved) cap is
// sent as is, and a thinking budget fits under it rather than raising it;
// only with no cap at all is max_tokens raised to hold the budget.
func TestAnthropicMaxTokensResolution(t *testing.T) {
	cases := []struct {
		name      string
		req       Request
		wantExact int
	}{
		{name: "explicit MaxTokens honoured", req: Request{Prompt: "hi", MaxTokens: 16000}, wantExact: 16000},
		{name: "thinking fits under an explicit cap", req: Request{Prompt: "hi", MaxTokens: 16000, Thinking: ThinkingHigh}, wantExact: 16000},
		{name: "no cap: raised to hold the budget", req: Request{Prompt: "hi", Thinking: ThinkingHigh},
			wantExact: ThinkingBudgetTokens(ThinkingHigh) + thinkingAnswerReserve},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := buildFantasyCall("anthropic", models.APIAnthropic, "claude-sonnet-4-5", tc.req)
			if got := sentMaxOutput(call); got != tc.wantExact {
				t.Errorf("max_tokens = %d, want %d", got, tc.wantExact)
			}
			if b := thinkingBudgetOf(t, call); b > 0 && int(b) >= sentMaxOutput(call) {
				t.Errorf("budget_tokens %d not below max_tokens %d", b, sentMaxOutput(call))
			}
		})
	}
}
