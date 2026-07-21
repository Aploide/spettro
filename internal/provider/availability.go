package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// FailureKind classifies why a model request failed, so callers can decide
// whether routing to a fallback model can help. Quota, server, timeout and
// network failures are model/provider availability problems (transient);
// everything else is a user/request error that a different model would fail
// on too.
type FailureKind string

const (
	FailureNone    FailureKind = "none"
	FailureQuota   FailureKind = "quota"   // 429 / rate or budget exhausted
	FailureServer  FailureKind = "server"  // 5xx from the provider
	FailureTimeout FailureKind = "timeout" // request deadline exceeded
	FailureNetwork FailureKind = "network" // connection-level failure
	FailureUser    FailureKind = "user"    // bad request, auth, cancel, etc.
)

// Transient reports whether the failure is an availability problem that a
// retry against a different model/provider could resolve.
func (k FailureKind) Transient() bool {
	switch k {
	case FailureQuota, FailureServer, FailureTimeout, FailureNetwork:
		return true
	}
	return false
}

// Classify inspects a request error and returns its FailureKind. It unwraps
// the HTTP error shapes produced by both provider paths (fantasy SDK and
// openai-go) plus standard context/net errors.
func Classify(err error) FailureKind {
	if err == nil {
		return FailureNone
	}
	if errors.Is(err, context.Canceled) {
		return FailureUser
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureTimeout
	}
	if status, _, ok := httpErrorDetails(err); ok {
		switch {
		case status == http.StatusTooManyRequests:
			return FailureQuota
		case status >= 500:
			return FailureServer
		case status == http.StatusRequestTimeout:
			return FailureTimeout
		default:
			return FailureUser
		}
	}
	if netErr, ok := errors.AsType[net.Error](err); ok {
		if netErr.Timeout() {
			return FailureTimeout
		}
		return FailureNetwork
	}
	return FailureUser
}

// ModelRef identifies one model as "provider/model".
type ModelRef struct {
	Provider string
	Model    string
}

func (r ModelRef) String() string { return r.Provider + "/" + r.Model }

func (r ModelRef) IsZero() bool { return r.Provider == "" && r.Model == "" }

// ParseModelRef parses a "provider/model" string. The model part may itself
// contain slashes (e.g. openrouter model ids).
func ParseModelRef(s string) (ModelRef, error) {
	s = strings.TrimSpace(s)
	i := strings.Index(s, "/")
	if i <= 0 || i == len(s)-1 {
		return ModelRef{}, fmt.Errorf("model ref %q must be provider/model", s)
	}
	return ModelRef{Provider: s[:i], Model: s[i+1:]}, nil
}

// ParseModelRefs parses a list of "provider/model" strings, skipping blanks.
func ParseModelRefs(refs []string) ([]ModelRef, error) {
	out := make([]ModelRef, 0, len(refs))
	for _, s := range refs {
		if strings.TrimSpace(s) == "" {
			continue
		}
		ref, err := ParseModelRef(s)
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, nil
}

// SendFunc dispatches one request to one specific model.
type SendFunc func(context.Context, ModelRef, Request) (Response, error)

// SendWithFallback sends req to primary and, on a transient availability
// failure, silently walks chain until a model answers. Non-transient errors
// abort immediately (a different model won't fix a bad request). onSwitch,
// when non-nil, is notified before each fallback attempt. Intended for
// internal utility calls (compaction, titling); the main conversation must
// instead get user consent before switching, since a model swap invalidates
// the provider prompt cache.
func SendWithFallback(ctx context.Context, send SendFunc, primary ModelRef, chain []ModelRef, req Request, onSwitch func(from, to ModelRef, cause error)) (Response, error) {
	resp, err := send(ctx, primary, req)
	if err == nil {
		return resp, nil
	}
	if !Classify(err).Transient() {
		return Response{}, err
	}
	lastErr := err
	cur := primary
	for _, next := range chain {
		if next == cur || next == primary {
			continue
		}
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		if onSwitch != nil {
			onSwitch(cur, next, lastErr)
		}
		resp, ferr := send(ctx, next, req)
		if ferr == nil {
			return resp, nil
		}
		if !Classify(ferr).Transient() {
			return Response{}, ferr
		}
		lastErr = ferr
		cur = next
	}
	return Response{}, lastErr
}
