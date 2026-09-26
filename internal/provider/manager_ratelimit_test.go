package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
		wantHint     time.Duration
	}{
		{"spettro 429 with header", spettroProviderID, fantasy429, true, 3 * time.Second},
		{"spettro 429 without header has no hint", spettroProviderID, fantasy429NoHeader, true, 0},
		{"spettro 402 is not a rate limit", spettroProviderID, fantasy402, false, 0},
		{"non-spettro 429 is not auto-waited", "openai", fantasy429, false, 0},
		{"spettro legacy-adapter 429 with header", spettroProviderID, openai429, true, 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hint, ok := rateLimitRetryAfter(tc.providerName, tc.err)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && hint != tc.wantHint {
				t.Fatalf("hint = %v, want %v", hint, tc.wantHint)
			}
		})
	}
}

// stubRateLimitJitter fixes the jitter fraction for a test.
func stubRateLimitJitter(t *testing.T, f float64) {
	t.Helper()
	saved := rateLimitJitter
	rateLimitJitter = func() float64 { return f }
	t.Cleanup(func() { rateLimitJitter = saved })
}

// The waits double from 1s, capped at Retry-After (or 20s without one), and
// the jitter spreads each one over its upper half, so clients throttled
// together do not retry in lockstep even at the cap.
func TestRateLimitDelayBacksOffWithJitter(t *testing.T) {
	stubRateLimitJitter(t, 0.999999)
	for attempts, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 5: 16 * time.Second, 6: 20 * time.Second, 40: 20 * time.Second} {
		if got := rateLimitDelay(attempts, 0).Round(time.Millisecond); got != want {
			t.Errorf("attempt %d: max delay %v, want %v", attempts, got, want)
		}
	}
	if got := rateLimitDelay(4, 7*time.Second).Round(time.Millisecond); got != 7*time.Second {
		t.Errorf("Retry-After 7s caps the wait: got %v", got)
	}
	// A tiny Retry-After does not turn the backoff into a busy loop.
	if got := rateLimitDelay(4, time.Millisecond).Round(time.Millisecond); got != time.Second {
		t.Errorf("Retry-After 1ms: max delay %v, want the 1s floor", got)
	}
	stubRateLimitJitter(t, 0)
	if got := rateLimitDelay(1, 0); got != 500*time.Millisecond {
		t.Errorf("min first delay %v, want 500ms", got)
	}
	if got := rateLimitDelay(9, 7*time.Second); got != 3500*time.Millisecond {
		t.Errorf("min capped delay %v, want 3.5s", got)
	}
}

// Send keeps waiting for about rateLimitMaxWait whatever the Retry-After and
// the jitter: the bound is on time, so short early waits do not use it up.
func TestRateLimitGivesUpAfterMaxWait(t *testing.T) {
	for _, retryAfter := range []time.Duration{0, time.Second, 7 * time.Second, 30 * time.Second} {
		for _, jitter := range []float64{0, 0.5, 0.999999} {
			stubRateLimitJitter(t, jitter)
			var waited time.Duration
			attempts := 0
			for {
				attempts++
				d, ok := nextRateLimitWait(attempts, waited, retryAfter)
				if !ok {
					break
				}
				waited += d
				if attempts > 10000 {
					t.Fatalf("Retry-After %v, jitter %v: never gives up", retryAfter, jitter)
				}
			}
			if floor := rateLimitMaxWait - max(retryAfter, rateLimitMaxDelay); waited < floor || waited > rateLimitMaxWait {
				t.Errorf("Retry-After %v, jitter %v: gave up after waiting %v, want between %v and %v", retryAfter, jitter, waited, floor, rateLimitMaxWait)
			}
		}
	}
}

// shrinkRateLimitWaits makes the rate-limit backoff fast enough for a test.
func shrinkRateLimitWaits(t *testing.T, base, maxWait time.Duration) {
	t.Helper()
	savedBase, savedMax := rateLimitBaseDelay, rateLimitMaxWait
	rateLimitBaseDelay, rateLimitMaxWait = base, maxWait
	t.Cleanup(func() { rateLimitBaseDelay, rateLimitMaxWait = savedBase, savedMax })
}

// newRateLimitedSpettro serves the Spettro inference endpoint: the first
// limited requests get a 429 (with a 1ms Retry-After), the rest succeed.
func newRateLimitedSpettro(t *testing.T, limited int) (*Manager, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		w.Header().Set("Content-Type", "application/json")
		if n <= limited {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "slow down", "type": "rate_limit"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "object": "chat.completion",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(srv.Close)
	pm := NewManager()
	pm.SetAPIKeys(map[string]string{spettroProviderID: "k"})
	pm.SetSpettro(srv.URL, []Model{{Provider: spettroProviderID, Name: "m"}})
	return pm, &calls
}

// Send waits out a few 429s and then succeeds.
func TestSendWaitsOutRateLimit(t *testing.T) {
	shrinkRateLimitWaits(t, time.Millisecond, time.Minute)
	pm, calls := newRateLimitedSpettro(t, 3)
	var waits []time.Duration
	resp, err := pm.Send(context.Background(), spettroProviderID, "m", Request{
		Prompt:      "hi",
		OnRateLimit: func(d time.Duration) { waits = append(waits, d) },
	})
	if err != nil || resp.Content != "ok" {
		t.Fatalf("resp = %q, err = %v", resp.Content, err)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("requests = %d, want 4", got)
	}
	if len(waits) != 3 {
		t.Errorf("waits = %v, want 3", waits)
	}
}

// Send stops waiting once it has waited rateLimitMaxWait and returns the
// 429, which the agent loop then does not retry again (but may still fall
// back to another model on).
func TestSendGivesUpOnPersistentRateLimit(t *testing.T) {
	shrinkRateLimitWaits(t, time.Millisecond, 20*time.Millisecond)
	pm, calls := newRateLimitedSpettro(t, 100000)
	_, err := pm.Send(context.Background(), spettroProviderID, "m", Request{Prompt: "hi"})
	if !errors.Is(err, ErrRateLimitRetriesExhausted) {
		t.Fatalf("err = %v, want ErrRateLimitRetriesExhausted", err)
	}
	// Waits of 0.5-1ms fill 20ms in 20 to 40 retries.
	if got := calls.Load(); got < 20 || got > 42 {
		t.Errorf("requests = %d, want about 20-40", got)
	}
	if ClassifyRetry(err) != RetryNever {
		t.Errorf("ClassifyRetry = %v, want RetryNever", ClassifyRetry(err))
	}
	if Classify(err) != FailureQuota {
		t.Errorf("Classify = %v, want FailureQuota (the fallback chain still applies)", Classify(err))
	}
}
