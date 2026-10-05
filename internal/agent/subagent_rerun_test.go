package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"charm.land/fantasy"

	"spettro/internal/provider"
)

// A workflow re-runs a sub-agent after a transient provider failure,
// but not after a rate limit the provider manager already waited out: that
// would restart the work from scratch only to queue on the same bucket.
func TestRerunSubagentAfter(t *testing.T) {
	limited := &fantasy.ProviderError{StatusCode: 429, Message: "slow down"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"rate limit", fmt.Errorf("agent call failed: %w", limited), true},
		{"server error", fmt.Errorf("agent call failed: %w", &fantasy.ProviderError{StatusCode: 503}), true},
		{"rate limit waited out", fmt.Errorf("agent call failed: %w", fmt.Errorf("%w (30 attempts over 3m0s): %w", provider.ErrRateLimitRetriesExhausted, limited)), false},
		{"bad request", fmt.Errorf("agent call failed: %w", &fantasy.ProviderError{StatusCode: 400}), false},
		{"cancelled", context.Canceled, false},
		{"plain", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := rerunSubagentAfter(tc.err); got != tc.want {
			t.Errorf("%s: rerunSubagentAfter = %v, want %v", tc.name, got, tc.want)
		}
	}
}
