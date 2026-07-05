package provider

import (
	"net/http"
	"testing"
	"time"

	"charm.land/fantasy"
	openai "github.com/openai/openai-go/v3"
)

// Guards the auto-wait behaviour for the Spettro Subscription overflow tier:
// a 429 from the "spettro" provider must be treated as a transient rate
// limit the CLI waits out, while the same status from any other provider (or
// any other status from Spettro, e.g. 402 once the real budget is exhausted)
// must still surface as a normal error.
func TestRateLimitRetryAfter(t *testing.T) {
	fantasy429 := &fantasy.ProviderError{
		StatusCode:      http.StatusTooManyRequests,
		ResponseHeaders: map[string]string{"Retry-After": "3"},
	}
	fantasy429NoHeader := &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests}
	fantasy402 := &fantasy.ProviderError{StatusCode: http.StatusPaymentRequired}
	openai429 := &openai.Error{
		StatusCode: http.StatusTooManyRequests,
		Response:   &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"5"}}},
	}

	cases := []struct {
		name         string
		providerName string
		err          error
		wantOK       bool
		wantDelay    time.Duration
	}{
		{"spettro 429 with header", spettroProviderID, fantasy429, true, 3 * time.Second},
		{"spettro 429 without header falls back to default", spettroProviderID, fantasy429NoHeader, true, defaultRateLimitRetryAfter},
		{"spettro 402 is not a rate limit", spettroProviderID, fantasy402, false, 0},
		{"non-spettro 429 is not auto-waited", "openai", fantasy429, false, 0},
		{"spettro legacy-adapter 429 with header", spettroProviderID, openai429, true, 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delay, ok := rateLimitRetryAfter(tc.providerName, tc.err)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && delay != tc.wantDelay {
				t.Fatalf("delay = %v, want %v", delay, tc.wantDelay)
			}
		})
	}
}
