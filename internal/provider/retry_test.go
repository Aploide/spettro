package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"charm.land/fantasy"
	openai "github.com/charmbracelet/openai-go"
)

func TestClassifyRetry(t *testing.T) {
	status := func(code int, msg string) error {
		return &fantasy.ProviderError{StatusCode: code, Message: msg}
	}
	cases := []struct {
		name string
		err  error
		want RetryClass
	}{
		{"nil", nil, RetryNever},
		{"canceled", context.Canceled, RetryNever},
		{"429", status(429, "rate limited"), RetryTransient},
		{"529 overloaded", status(529, "overloaded"), RetryTransient},
		{"500", status(500, "boom"), RetryTransient},
		{"503 openai-go", &openai.Error{StatusCode: 503, Response: &http.Response{StatusCode: 503, Header: http.Header{}}}, RetryTransient},
		{"401 auth", status(401, "invalid x-api-key"), RetryNever},
		{"400 bad request", status(400, "tools.0.input_schema: invalid"), RetryNever},
		{"404 model", status(404, "model not found"), RetryNever},
		{"400 wrapping upstream overload", status(400, "upstream overloaded, try again"), RetryTransient},
		{"anthropic prompt too long", status(400, "prompt is too long: 210000 tokens > 200000 maximum"), RetryContextOverflow},
		{"openai context length", status(400, "This model's maximum context length is 32768 tokens. However, you requested 40000 tokens"), RetryContextOverflow},
		{"413 request too large", status(413, "request_too_large"), RetryContextOverflow},
		{"fantasy context flag", &fantasy.ProviderError{StatusCode: 400, ContextTooLargeErr: true}, RetryContextOverflow},
		{"429 mentioning tokens is not overflow", status(429, "too many input tokens per minute"), RetryTransient},
		{"stream idle", fmt.Errorf("send: %w", ErrStreamIdle), RetryTransient},
		{"unexpected eof", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), RetryTransient},
		{"deadline", context.DeadlineExceeded, RetryTransient},
		{"net error", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, RetryTransient},
		{"mid-stream overloaded event", errors.New(`received error while streaming: {"type":"overloaded_error","message":"Overloaded"}`), RetryTransient},
		{"mid-stream event on a 200", status(200, `{"type":"error","error":{"type":"overloaded_error"}}`), RetryTransient},
		{"status-less auth message", errors.New("authentication failed: invalid api key"), RetryNever},
		{"unknown", errors.New("something odd happened"), RetryUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyRetry(tc.err); got != tc.want {
				t.Fatalf("ClassifyRetry = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContextLimitFromError(t *testing.T) {
	cases := map[string]struct {
		err  error
		want int
	}{
		"fantasy field":  {&fantasy.ProviderError{ContextMaxTokens: 131072}, 131072},
		"openai message": {errors.New("This model's maximum context length is 32768 tokens."), 32768},
		"anthropic":      {errors.New("prompt is too long: 210000 tokens > 200000 maximum"), 200000},
		"input+max":      {errors.New("input length and `max_tokens` exceed context limit: 188240 + 21333 > 200000"), 200000},
		"none":           {errors.New("prompt is too long"), 0},
	}
	for name, tc := range cases {
		if got := ContextLimitFromError(tc.err); got != tc.want {
			t.Errorf("%s: got %d, want %d", name, got, tc.want)
		}
	}
}

func TestRetryPolicyNextDelay(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 6, UnknownMaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 10 * time.Second, MaxRetryAfter: time.Minute}
	overloaded := &fantasy.ProviderError{StatusCode: 529}

	// Exponential backoff without jitter: 1s, 2s, 4s, 8s, then capped at 10s.
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second, 5: 10 * time.Second} {
		got, ok := p.NextDelay(overloaded, attempt)
		if !ok || got != want {
			t.Errorf("attempt %d: got %v/%v, want %v", attempt, got, ok, want)
		}
	}
	if _, ok := p.NextDelay(overloaded, 6); ok {
		t.Error("the 6th failure must end the retries")
	}

	// Jitter stays within +25% and under the cap.
	pj := p
	pj.Jitter = 0.25
	for range 50 {
		d, _ := pj.NextDelay(overloaded, 2)
		if d < 2*time.Second || d > 2500*time.Millisecond {
			t.Fatalf("jittered delay %v outside [2s, 2.5s]", d)
		}
	}

	// Retry-After is honored for every provider, above the backoff cap…
	limited := &fantasy.ProviderError{StatusCode: 429, ResponseHeaders: map[string]string{"Retry-After": "30"}}
	if d, ok := p.NextDelay(limited, 1); !ok || d != 30*time.Second {
		t.Errorf("Retry-After: got %v/%v, want 30s", d, ok)
	}
	// With jitter, never earlier than asked and not all at the same moment.
	for range 50 {
		if d, _ := pj.NextDelay(limited, 1); d < 30*time.Second || d > 37500*time.Millisecond {
			t.Fatalf("jittered Retry-After %v outside [30s, 37.5s]", d)
		}
	}
	ms := &fantasy.ProviderError{StatusCode: 429, ResponseHeaders: map[string]string{"retry-after-ms": "1500"}}
	if d, ok := p.NextDelay(ms, 1); !ok || d != 1500*time.Millisecond {
		t.Errorf("retry-after-ms: got %v/%v, want 1.5s", d, ok)
	}
	// …but a wait beyond MaxRetryAfter (a daily quota) ends the retries.
	quota := &fantasy.ProviderError{StatusCode: 429, ResponseHeaders: map[string]string{"Retry-After": "86400"}}
	if _, ok := p.NextDelay(quota, 1); ok {
		t.Error("an hours-long Retry-After must not be waited out")
	}

	// Deterministic failures are never retried; unknown ones get fewer tries.
	if _, ok := p.NextDelay(&fantasy.ProviderError{StatusCode: 401}, 1); ok {
		t.Error("401 must not be retried")
	}
	if _, ok := p.NextDelay(&fantasy.ProviderError{StatusCode: 400, Message: "prompt is too long"}, 1); ok {
		t.Error("context overflow must not be retried as-is")
	}
	odd := errors.New("something odd happened")
	if _, ok := p.NextDelay(odd, 2); !ok {
		t.Error("an unknown error gets a second retry")
	}
	if _, ok := p.NextDelay(odd, 3); ok {
		t.Error("an unknown error stops at UnknownMaxAttempts")
	}
}
