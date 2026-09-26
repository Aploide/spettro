package provider

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
)

// RetryClass says what a caller should do about a failed model request.
type RetryClass int

const (
	// RetryNever: deterministic failure (bad request, auth, permissions, an
	// unknown model, user cancellation). Resending the same request fails the
	// same way, so it must surface immediately.
	RetryNever RetryClass = iota
	// RetryTransient: rate limit, overload (Anthropic 529), 5xx, network or
	// interrupted/stalled stream. Retry with exponential backoff, honoring
	// any Retry-After hint.
	RetryTransient
	// RetryUnknown: an error with no status and no recognizable shape. Worth
	// a couple of attempts, but not the full transient budget.
	RetryUnknown
	// RetryContextOverflow: the prompt no longer fits the model's context
	// window. Resending is pointless until the history is compacted.
	RetryContextOverflow
)

func (c RetryClass) String() string {
	switch c {
	case RetryTransient:
		return "transient"
	case RetryUnknown:
		return "unknown"
	case RetryContextOverflow:
		return "context-overflow"
	}
	return "fatal"
}

// transientPatterns match provider errors that carry no HTTP status (errors
// delivered mid-stream, SDK-level failures, proxies) but are known to be
// temporary.
var transientPatterns = []string{
	"overloaded", "rate limit", "rate_limit", "ratelimit", "too many requests",
	"service unavailable", "temporarily unavailable", "server_error", "internal server error",
	"internal_error", "api_error", "bad gateway", "gateway timeout", "upstream",
	"try again", "capacity", "resource_exhausted", "resource exhausted",
	"connection reset", "connection refused", "econnreset", "econnrefused", "broken pipe",
	"socket hang up", "unexpected eof", "stream error", "stream closed", "stream was reset",
	"timed out", "timeout", "tls handshake", "no such host", "eof",
	" 429", " 500", " 502", " 503", " 504", " 529",
}

// fatalPatterns match errors that no retry can fix.
var fatalPatterns = []string{
	"invalid api key", "invalid x-api-key", "incorrect api key", "authentication", "unauthorized",
	"permission denied", "forbidden", "not_found_error", "model not found", "does not exist",
	"invalid_request_error", "no api endpoint configured", "credit balance", "insufficient_quota",
	"billing",
}

// quotaPatterns match 4xx errors (429 included) caused by an exhausted
// quota or balance rather than a rate limit.
var quotaPatterns = []string{
	"insufficient_quota", "exceeded your current quota", "credit balance", "billing",
}

// overflowPatterns match "the prompt is too long for this model" errors
// across providers (Anthropic, OpenAI, vLLM, llama.cpp, Gemini, Bedrock, …).
var overflowPatterns = []string{
	"prompt is too long", "context_length_exceeded", "context length exceeded",
	"maximum context length", "exceeds the context window", "exceed context limit",
	"exceeds context limit", "context window", "input is too long", "input too long",
	"request_too_large", "too many input tokens", "prompt too long",
	"exceeds the maximum number of tokens", "reduce the length of the messages",
	"model's maximum context", "context size has been exceeded", "exceeds the available context",
}

// ClassifyRetry decides how a failed request should be handled. See
// RetryClass. It inspects typed errors first (context, stream idle, HTTP
// status from both SDK shapes, net.Error) and only then falls back to
// message patterns for status-less errors.
func ClassifyRetry(err error) RetryClass {
	if err == nil || errors.Is(err, context.Canceled) {
		return RetryNever
	}
	if IsContextOverflow(err) {
		return RetryContextOverflow
	}
	if errors.Is(err, ErrStreamIdle) || errors.Is(err, ErrStreamIncomplete) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return RetryTransient
	}
	// A status below 400 means the error arrived mid-stream on a response
	// that started fine (e.g. an overloaded_error event); only the message
	// can classify it.
	if status, _, ok := httpErrorDetails(err); ok && status >= 400 {
		// Quota and billing failures often arrive as a 429 (OpenAI's
		// insufficient_quota), which the SDK flags as retryable; waiting
		// never fixes them, so they are checked before the status.
		if status < 500 && containsAny(strings.ToLower(err.Error()), quotaPatterns...) {
			return RetryNever
		}
		if pe, ok := errors.AsType[*fantasy.ProviderError](err); ok && pe.IsRetryable() {
			return RetryTransient
		}
		switch {
		case status == http.StatusTooManyRequests, status == http.StatusRequestTimeout,
			status == http.StatusConflict, status == http.StatusTooEarly, status >= 500:
			return RetryTransient
		}
		// A 4xx whose body says "overloaded" (some gateways wrap upstream
		// overload in a 400) is still transient; every other 4xx is final.
		if containsAny(strings.ToLower(err.Error()), "overloaded", "rate limit", "try again") {
			return RetryTransient
		}
		return RetryNever
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr != nil {
		return RetryTransient
	}
	msg := strings.ToLower(err.Error())
	if containsAny(msg, fatalPatterns...) {
		return RetryNever
	}
	if containsAny(msg, transientPatterns...) {
		return RetryTransient
	}
	return RetryUnknown
}

// IsContextOverflow reports whether err says the prompt exceeds the model's
// context window.
func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	if pe, ok := errors.AsType[*fantasy.ProviderError](err); ok && pe.IsContextTooLarge() {
		return true
	}
	status, _, hasStatus := httpErrorDetails(err)
	if hasStatus && (status == http.StatusTooManyRequests || status >= 500) {
		// "too many tokens per minute" style rate limits are not overflow.
		return false
	}
	if hasStatus && status == http.StatusRequestEntityTooLarge {
		return true
	}
	return containsAny(strings.ToLower(err.Error()), overflowPatterns...)
}

// contextLimitPatterns are tried in order; the specific shapes come first so
// "exceed context limit: 188240 + 21333 > 200000" yields the limit, not the
// input size.
var contextLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`maximum context length is (\d+)`),
	regexp.MustCompile(`>\s*(\d+)\s*maximum`),
	regexp.MustCompile(`\d+\s*\+\s*\d+\s*>\s*(\d+)`),
	regexp.MustCompile(`context (?:window|length|size) (?:of |is )?(\d{4,})`),
}

// ContextLimitFromError extracts the model's context window from an overflow
// error when the provider states it ("maximum context length is 32768",
// "… > 200000 maximum"), or 0. Hosts use it to learn the real window of
// endpoints the catalog knows nothing about.
func ContextLimitFromError(err error) int {
	if err == nil {
		return 0
	}
	if pe, ok := errors.AsType[*fantasy.ProviderError](err); ok && pe.ContextMaxTokens > 0 {
		return pe.ContextMaxTokens
	}
	msg := strings.ToLower(err.Error())
	for _, re := range contextLimitPatterns {
		if m := re.FindStringSubmatch(msg); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// RetryPolicy bounds the retries of one model request.
type RetryPolicy struct {
	// MaxAttempts is the total number of sends (first try included) for
	// transient failures.
	MaxAttempts int
	// UnknownMaxAttempts is the total number of sends for unclassified
	// failures.
	UnknownMaxAttempts int
	// BaseDelay is the first backoff delay; each retry doubles it.
	BaseDelay time.Duration
	// MaxDelay caps a computed backoff delay (a server's Retry-After is
	// honored above it, up to MaxRetryAfter).
	MaxDelay time.Duration
	// MaxRetryAfter is the longest server-requested wait honored; a longer
	// Retry-After (e.g. a daily quota) ends the retries instead.
	MaxRetryAfter time.Duration
	// Jitter adds up to this fraction of the delay at random, so parallel
	// agents hitting the same limit don't retry in lockstep.
	Jitter float64
}

// DefaultRetryPolicy is used by the agent loop: 6 attempts with 2s, 4s, 8s,
// 16s, 32s backoff (+≤25% jitter, capped at 60s), Retry-After honored up to
// 5 minutes. It is a variable so tests can shrink the delays.
var DefaultRetryPolicy = RetryPolicy{
	MaxAttempts:        6,
	UnknownMaxAttempts: 3,
	BaseDelay:          2 * time.Second,
	MaxDelay:           60 * time.Second,
	MaxRetryAfter:      5 * time.Minute,
	Jitter:             0.25,
}

// NextDelay reports whether a request that has now failed `attempts` times
// (1 after the first failure) with err should be retried, and after how
// long.
func (p RetryPolicy) NextDelay(err error, attempts int) (time.Duration, bool) {
	var limit int
	switch ClassifyRetry(err) {
	case RetryTransient:
		limit = p.MaxAttempts
	case RetryUnknown:
		limit = p.UnknownMaxAttempts
	default:
		return 0, false
	}
	if attempts >= limit {
		return 0, false
	}
	if hint, ok := RetryAfterHint(err); ok {
		if p.MaxRetryAfter > 0 && hint > p.MaxRetryAfter {
			return 0, false
		}
		return hint, true
	}
	d := p.BaseDelay
	for i := 1; i < attempts && d < p.MaxDelay; i++ {
		d *= 2
	}
	if p.Jitter > 0 && d > 0 {
		d += time.Duration(rand.Float64() * p.Jitter * float64(d))
	}
	if p.MaxDelay > 0 && d > p.MaxDelay {
		d = p.MaxDelay
	}
	return d, true
}

// RetryAfterHint returns the server-requested wait carried by err's response
// headers (retry-after-ms, then Retry-After as seconds or an HTTP date), for
// any provider.
func RetryAfterHint(err error) (time.Duration, bool) {
	_, header, ok := httpErrorDetails(err)
	if !ok || header == nil {
		return 0, false
	}
	return parseRetryAfter(header)
}

func parseRetryAfter(header http.Header) (time.Duration, bool) {
	if v := strings.TrimSpace(header.Get("Retry-After-Ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
	}
	return 0, false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
