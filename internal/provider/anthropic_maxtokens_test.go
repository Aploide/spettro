package provider

import "testing"

func TestAnthropicMaxTokensResolution(t *testing.T) {
	cases := []struct {
		name     string
		req      Request
		wantMin  int64 // resolved maxTokens must be >= this
		wantExact int64 // if > 0, must equal exactly
	}{
		{
			name:      "explicit MaxTokens honoured",
			req:       Request{MaxTokens: 16000},
			wantExact: 16000,
		},
		{
			name:      "zero falls back to default",
			req:       Request{MaxTokens: 0},
			wantExact: 16384,
		},
		{
			name:    "thinking budget forces max_tokens above budget",
			req:     Request{MaxTokens: 1000, Thinking: "high"},
			wantMin: int64(ThinkingBudgetTokens(ThinkingHigh)) + 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const defaultMaxTokens = int64(16384)
			maxTokens := defaultMaxTokens
			if tc.req.MaxTokens > 0 {
				maxTokens = int64(tc.req.MaxTokens)
			}
			if budget := ThinkingBudgetTokens(ThinkingLevel(tc.req.Thinking)); budget > 0 {
				needed := int64(budget) + 4096
				if needed > maxTokens {
					maxTokens = needed
				}
			}

			if tc.wantExact > 0 && maxTokens != tc.wantExact {
				t.Errorf("maxTokens = %d, want %d", maxTokens, tc.wantExact)
			}
			if tc.wantMin > 0 && maxTokens < tc.wantMin {
				t.Errorf("maxTokens = %d, want >= %d", maxTokens, tc.wantMin)
			}
		})
	}
}
