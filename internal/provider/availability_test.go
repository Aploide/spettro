package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"charm.land/fantasy"
)

func httpErr(status int) error {
	return fmt.Errorf("wrapped: %w", &fantasy.ProviderError{StatusCode: status})
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FailureKind
	}{
		{"nil", nil, FailureNone},
		{"quota 429", httpErr(429), FailureQuota},
		{"server 500", httpErr(500), FailureServer},
		{"server 503", httpErr(503), FailureServer},
		{"auth 401", httpErr(401), FailureUser},
		{"bad request 400", httpErr(400), FailureUser},
		{"deadline", context.DeadlineExceeded, FailureTimeout},
		{"canceled", context.Canceled, FailureUser},
		{"plain error", errors.New("boom"), FailureUser},
	}
	for _, tc := range cases {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify = %q, want %q", tc.name, got, tc.want)
		}
	}
	if FailureQuota.Transient() != true || FailureUser.Transient() != false {
		t.Errorf("Transient classification wrong")
	}
}

func TestParseModelRef(t *testing.T) {
	ref, err := ParseModelRef("openrouter/meta/llama-3-8b")
	if err != nil || ref.Provider != "openrouter" || ref.Model != "meta/llama-3-8b" {
		t.Fatalf("ParseModelRef = %+v, %v", ref, err)
	}
	for _, bad := range []string{"", "noslash", "/model", "provider/"} {
		if _, err := ParseModelRef(bad); err == nil {
			t.Errorf("ParseModelRef(%q): expected error", bad)
		}
	}
}

func TestSendWithFallbackWalksChain(t *testing.T) {
	primary := ModelRef{"a", "big"}
	chain := []ModelRef{{"a", "big"}, {"b", "mid"}, {"c", "small"}}
	var calls []ModelRef
	send := func(_ context.Context, ref ModelRef, _ Request) (Response, error) {
		calls = append(calls, ref)
		if ref.Provider == "c" {
			return Response{Content: "ok"}, nil
		}
		return Response{}, httpErr(429)
	}
	var switches int
	resp, err := SendWithFallback(context.Background(), send, primary, chain, Request{}, func(from, to ModelRef, cause error) { switches++ })
	if err != nil || resp.Content != "ok" {
		t.Fatalf("SendWithFallback = %v, %v", resp, err)
	}
	want := []ModelRef{{"a", "big"}, {"b", "mid"}, {"c", "small"}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
	if switches != 2 {
		t.Errorf("onSwitch called %d times, want 2", switches)
	}
}

func TestSendWithFallbackStopsOnUserError(t *testing.T) {
	primary := ModelRef{"a", "big"}
	chain := []ModelRef{{"b", "mid"}, {"c", "small"}}
	var calls int
	send := func(_ context.Context, ref ModelRef, _ Request) (Response, error) {
		calls++
		if calls == 1 {
			return Response{}, httpErr(429) // transient → walk to chain
		}
		return Response{}, httpErr(401) // user error → stop
	}
	_, err := SendWithFallback(context.Background(), send, primary, chain, Request{}, nil)
	if err == nil || Classify(err) != FailureUser {
		t.Fatalf("expected user error, got %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (no third attempt after user error)", calls)
	}
}

func TestSendWithFallbackExhaustedReturnsLastError(t *testing.T) {
	primary := ModelRef{"a", "big"}
	chain := []ModelRef{{"b", "mid"}}
	send := func(_ context.Context, ref ModelRef, _ Request) (Response, error) {
		return Response{}, httpErr(503)
	}
	_, err := SendWithFallback(context.Background(), send, primary, chain, Request{}, nil)
	if err == nil || Classify(err) != FailureServer {
		t.Fatalf("expected server error after exhausted chain, got %v", err)
	}
}
