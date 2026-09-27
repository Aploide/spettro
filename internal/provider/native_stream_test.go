package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"spettro/internal/models"
)

// Response-equivalence tests: a streamed reply must come out of the native
// client exactly as it comes out of fantasy (Response, streamed events and
// error behaviour), for every reply shape below.

// streamOutcomeOf is what a caller observes from one streamed Send.
type streamOutcomeOf struct {
	resp     Response
	err      error
	events   []StreamEvent
	requests int
}

// sendBothWires serves every request with handler and sends req once
// through each client. local selects a local endpoint (which may omit the
// finish reason) instead of a catalog provider.
func sendBothWires(t *testing.T, local bool, req Request, handler http.HandlerFunc) (native, fantasy streamOutcomeOf) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	run := func(mode WireMode) streamOutcomeOf {
		pm := NewManager()
		pm.SetWireMode(string(mode))
		providerName := "compat"
		if local {
			providerName = srv.URL
			pm.AddLocalModels([]Model{{Provider: srv.URL, Name: "m", Local: true, ToolCall: true}})
		} else {
			pm.SetAPIKeys(map[string]string{"compat": "k"})
			pm.SetCatalog(models.Catalog{Providers: map[string]models.CatalogProvider{
				"compat": {API: models.APIOpenAI, BaseURL: srv.URL + "/v1", Models: map[string]models.CatalogModel{"m": {ToolCall: true}}},
			}})
		}
		requests.Store(0)
		var out streamOutcomeOf
		r := req
		r.OnStream = func(ev StreamEvent) { out.events = append(out.events, ev) }
		out.resp, out.err = pm.Send(context.Background(), providerName, "m", r)
		out.requests = int(requests.Load())
		return out
	}
	return run(WireNative), run(WireFantasy)
}

// assertSameOutcome compares what callers can observe.
func assertSameOutcome(t *testing.T, native, fantasy streamOutcomeOf) {
	t.Helper()
	if (native.err == nil) != (fantasy.err == nil) {
		t.Fatalf("errors differ: native %v, fantasy %v", native.err, fantasy.err)
	}
	if native.err != nil {
		if native.err.Error() != fantasy.err.Error() {
			t.Errorf("error text: native %q, fantasy %q", native.err, fantasy.err)
		}
		if a, b := ClassifyRetry(native.err), ClassifyRetry(fantasy.err); a != b {
			t.Errorf("ClassifyRetry: native %v, fantasy %v", a, b)
		}
		if a, b := Classify(native.err), Classify(fantasy.err); a != b {
			t.Errorf("Classify: native %v, fantasy %v", a, b)
		}
		ns, _, nok := httpErrorDetails(native.err)
		fs, _, fok := httpErrorDetails(fantasy.err)
		if ns != fs || nok != fok {
			t.Errorf("status: native %d/%v, fantasy %d/%v", ns, nok, fs, fok)
		}
		na, nh := RetryAfterHint(native.err)
		fa, fh := RetryAfterHint(fantasy.err)
		if na != fa || nh != fh {
			t.Errorf("Retry-After: native %v/%v, fantasy %v/%v", na, nh, fa, fh)
		}
		if a, b := IsContextOverflow(native.err), IsContextOverflow(fantasy.err); a != b {
			t.Errorf("IsContextOverflow: native %v, fantasy %v", a, b)
		}
		if a, b := ContextLimitFromError(native.err), ContextLimitFromError(fantasy.err); a != b {
			t.Errorf("ContextLimitFromError: native %d, fantasy %d", a, b)
		}
		if a, b := shouldFallbackToLegacy(native.err), shouldFallbackToLegacy(fantasy.err); a != b {
			t.Errorf("shouldFallbackToLegacy: native %v, fantasy %v", a, b)
		}
	}
	if !reflect.DeepEqual(native.resp, fantasy.resp) {
		t.Errorf("responses differ\nnative:  %+v\nfantasy: %+v", native.resp, fantasy.resp)
	}
	if !reflect.DeepEqual(native.events, fantasy.events) {
		t.Errorf("streamed events differ\nnative:  %+v\nfantasy: %+v", native.events, fantasy.events)
	}
	if native.requests != fantasy.requests {
		t.Errorf("requests: native %d, fantasy %d", native.requests, fantasy.requests)
	}
}

// sseData renders one SSE event whose data is v (a string is sent as is).
func sseData(v any) string {
	if s, ok := v.(string); ok {
		return "data: " + s + "\n\n"
	}
	raw, _ := json.Marshal(v)
	return "data: " + string(raw) + "\n\n"
}

// chunkWith builds a chat.completion.chunk with one choice at index 0.
func chunkWith(delta map[string]any, finish any) map[string]any {
	return map[string]any{"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
}

func usageChunk(prompt, completion, cached int) map[string]any {
	u := map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion}
	if cached > 0 {
		u["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	return map[string]any{"id": "chatcmpl-1", "object": "chat.completion.chunk", "choices": []any{}, "usage": u}
}

func toolFrag(index int, id, name, args string) map[string]any {
	tc := map[string]any{"index": index, "function": map[string]any{}}
	fn := tc["function"].(map[string]any)
	if id != "" {
		tc["id"] = id
		tc["type"] = "function"
	}
	if name != "" {
		fn["name"] = name
	}
	if args != "" || name != "" {
		fn["arguments"] = args
	}
	return tc
}

func toolDeltas(frags ...map[string]any) map[string]any {
	calls := make([]any, len(frags))
	for i, f := range frags {
		calls[i] = f
	}
	return map[string]any{"tool_calls": calls}
}

// sseReply serves the given raw SSE text.
func sseReply(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join(events, ""))
	}
}

func fragmentArgs(args string, n int) []string {
	step := max(1, len(args)/n)
	var out []string
	for i := 0; i < len(args); i += step {
		out = append(out, args[i:min(i+step, len(args))])
	}
	return out
}

func TestNativeStreamMatchesFantasy(t *testing.T) {
	bigArgs := `{"path":"big.go","content":"` + strings.Repeat(`fmt.Println(\"x\")\n`, 400) + `"}`
	var bigFrags []string
	bigFrags = append(bigFrags, sseData(chunkWith(toolDeltas(toolFrag(0, "call_big", "file-write", "")), nil)))
	for _, f := range fragmentArgs(bigArgs, 300) {
		bigFrags = append(bigFrags, sseData(chunkWith(toolDeltas(toolFrag(0, "", "", f)), nil)))
	}
	bigFrags = append(bigFrags, sseData(chunkWith(map[string]any{}, "tool_calls")), sseData(usageChunk(100, 50, 0)), "data: [DONE]\n\n")

	cases := []struct {
		name    string
		local   bool
		handler http.HandlerFunc
	}{
		{"text", false, sseReply(
			sseData(chunkWith(map[string]any{"role": "assistant", "content": ""}, nil)),
			sseData(chunkWith(map[string]any{"content": "Hel"}, nil)),
			sseData(chunkWith(map[string]any{"content": "lo"}, nil)),
			sseData(chunkWith(map[string]any{}, "stop")),
			sseData(usageChunk(10, 2, 4)), "data: [DONE]\n\n")},
		{"reasoning then text", false, sseReply(
			sseData(chunkWith(map[string]any{"reasoning_content": "think "}, nil)),
			sseData(chunkWith(map[string]any{"reasoning_content": "more"}, nil)),
			sseData(chunkWith(map[string]any{"content": "answer", "reasoning_content": "late"}, nil)),
			sseData(chunkWith(map[string]any{}, "stop")), sseData(usageChunk(3, 3, 0)))},
		{"fragmented tool call", false, sseReply(
			sseData(chunkWith(map[string]any{"content": "Reading."}, nil)),
			sseData(chunkWith(toolDeltas(toolFrag(0, "call_1", "file-read", "")), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(0, "", "", `{"pa`)), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(0, "", "", `th":"a.go"`)), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(0, "", "", `}`)), nil)),
			sseData(chunkWith(map[string]any{}, "tool_calls")), sseData(usageChunk(9, 9, 0)), "data: [DONE]\n\n")},
		{"big tool call", false, sseReply(bigFrags...)},
		{"parallel tool calls", false, sseReply(
			sseData(chunkWith(toolDeltas(toolFrag(0, "a", "grep", `{"q":`), toolFrag(1, "b", "glob", `{"p":`)), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(1, "", "", `"*.go"}`)), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(0, "", "", `"x"}`)), nil)),
			sseData(chunkWith(map[string]any{}, "tool_calls")))},
		{"ollama empty fragments and missing ids", true, sseReply(
			sseData(chunkWith(toolDeltas(map[string]any{"index": 0, "function": map[string]any{}}), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(0, "", "shell", `{"cmd":"ls"}`)), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(-1, "", "grep", `{"q":"y"}`)), nil)),
			sseData(chunkWith(map[string]any{}, "stop")))},
		{"only empty tool fragments", false, sseReply(
			sseData(chunkWith(toolDeltas(map[string]any{"index": 0, "function": map[string]any{}}), nil)),
			sseData(chunkWith(map[string]any{"content": "hi"}, "stop")))},
		{"no arguments", false, sseReply(
			sseData(chunkWith(toolDeltas(toolFrag(0, "c", "todo-read", "")), nil)),
			sseData(chunkWith(map[string]any{}, "tool_calls")))},
		{"arguments after a complete value", false, sseReply(
			sseData(chunkWith(toolDeltas(toolFrag(0, "c", "grep", `{}`)), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(0, "", "", `{"q":"late"}`)), nil)),
			sseData(chunkWith(map[string]any{}, "tool_calls")))},
		{"scalar and garbage arguments", false, sseReply(
			sseData(chunkWith(toolDeltas(toolFrag(0, "n", "a", `12`)), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(0, "", "", `3`)), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(1, "g", "b", `{"x":1]`)), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(1, "", "", `}`)), nil)),
			sseData(chunkWith(toolDeltas(toolFrag(2, "s", "c", ` "{\"a\":1}"`)), nil)),
			sseData(chunkWith(map[string]any{}, "tool_calls")))},
		{"truncated at the output limit", false, sseReply(
			sseData(chunkWith(toolDeltas(toolFrag(0, "w", "file-write", `{"path":"a.go","content":"pack`)), nil)),
			sseData(chunkWith(map[string]any{}, "length")), sseData(usageChunk(10, 50, 0)))},
		{"malformed arguments", false, sseReply(
			sseData(chunkWith(toolDeltas(toolFrag(0, "w", "file-write", "{\"path\":\"a.go\",\"content\":\"l1\nl2\",}")), nil)),
			sseData(chunkWith(map[string]any{}, "tool_calls")))},
		{"no finish reason", false, sseReply(sseData(chunkWith(map[string]any{"content": "half"}, nil)))},
		{"no finish reason, local", true, sseReply(sseData(chunkWith(map[string]any{"content": "half"}, nil)))},
		{"tool call without finish reason", false, sseReply(sseData(chunkWith(toolDeltas(toolFrag(0, "c", "grep", `{"q":`)), nil)))},
		{"unknown finish reason", false, sseReply(sseData(chunkWith(map[string]any{"content": "x"}, "eos")))},
		{"content filter", false, sseReply(sseData(chunkWith(map[string]any{"content": "x"}, "content_filter")))},
		{"usage not in the last chunk", false, sseReply(
			sseData(usageChunk(5, 5, 0)),
			sseData(chunkWith(map[string]any{"content": "x"}, "stop")))},
		{"chunk id changes", false, sseReply(
			sseData(chunkWith(map[string]any{"content": "x"}, nil)),
			sseData(map[string]any{"id": "other", "choices": []any{map[string]any{"index": 0, "delta": toolDeltas(toolFrag(0, "c", "grep", `{}`))}}}),
			sseData(chunkWith(map[string]any{}, "stop")))},
		// (fantasy repeats choice 0's reasoning for every further choice;
		// the native client keeps each choice's own, so only text is
		// compared here. Spettro never asks for more than one choice.)
		{"two choices", false, sseReply(
			sseData(map[string]any{"id": "c", "choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": "a"}},
				map[string]any{"index": 1, "delta": map[string]any{"content": "b"}, "finish_reason": "stop"},
			}}))},
		{"not a function", false, sseReply(sseData(chunkWith(toolDeltas(map[string]any{"index": 0, "id": "x", "type": "custom", "function": map[string]any{"name": "n", "arguments": "{}"}}), nil)))},
		{"mid-stream error object", false, sseReply(
			sseData(chunkWith(map[string]any{"content": "par"}, nil)),
			sseData(`{"error":{"message":"upstream overloaded","code":529}}`))},
		{"mid-stream error string", false, sseReply(sseData(`{"error":"rate limit exceeded"}`))},
		{"mid-stream error null", false, sseReply(sseData(`{"error":null}`))},
		{"invalid json", false, sseReply(sseData(chunkWith(map[string]any{"content": "a"}, nil)), sseData(`{"id":`))},
		{"keep-alives, crlf and multi-line data", false, sseReply(
			": keep-alive\r\n\r\n",
			"event: message\r\ndata: {\"id\":\"c\",\"choices\":[{\"index\":0,\r\ndata: \"delta\":{\"content\":\"x\"}}]}\r\n\r\n",
			"data:{\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")},
		{"unterminated last event", false, sseReply(
			sseData(chunkWith(map[string]any{"content": "x"}, nil)),
			"data: "+`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)},
		{"done then garbage", false, sseReply(
			sseData(chunkWith(map[string]any{"content": "x"}, "stop")), "data: [DONE]\n\n", "data: not json\n\n")},
		{"lenient field types", false, sseReply(
			sseData(`{"id":"c","choices":[{"index":0,"delta":{"content":"x","tool_calls":null},"finish_reason":null,"logprobs":{"bad":true}}],"usage":null}`),
			sseData(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))},
		{"json response to a stream request", false, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{Messages: []Message{{Role: RoleUser, Content: "go"}}, MaxTokens: 50}
			native, fantasy := sendBothWires(t, tc.local, req, tc.handler)
			assertSameOutcome(t, native, fantasy)
		})
	}
}

// A deliberate difference from fantasy: a delta carrying both text
// and a tool-call fragment keeps both (fantasy drops the fragment, which
// then corrupts the call's arguments).
func TestNativeStreamKeepsToolFragmentsSharingATextDelta(t *testing.T) {
	delta := toolDeltas(toolFrag(0, "c1", "grep", `{"q":`))
	delta["content"] = "Searching."
	handler := sseReply(
		sseData(chunkWith(delta, nil)),
		sseData(chunkWith(toolDeltas(toolFrag(0, "", "", `"x"}`)), nil)),
		sseData(chunkWith(map[string]any{}, "tool_calls")))
	native, _ := sendBothWires(t, false, Request{Messages: []Message{{Role: RoleUser, Content: "go"}}}, handler)
	if native.err != nil || native.resp.Content != "Searching." || len(native.resp.ToolCalls) != 1 {
		t.Fatalf("resp = %+v, err = %v", native.resp, native.err)
	}
	if tc := native.resp.ToolCalls[0]; tc.ArgsError != "" || string(tc.Args) != `{"q":"x"}` {
		t.Fatalf("tool call = %+v", tc)
	}
}

// A deliberate difference from fantasy (docs/configuration.md lists it): a
// final choice without a "delta" member ends the reply instead of failing
// the request.
func TestNativeStreamAcceptsAChoiceWithoutDelta(t *testing.T) {
	handler := sseReply(
		sseData(chunkWith(map[string]any{"content": "hi"}, nil)),
		sseData(`{"id":"c","choices":[{"index":0,"finish_reason":"stop"}]}`))
	native, fantasy := sendBothWires(t, false, Request{Messages: []Message{{Role: RoleUser, Content: "go"}}}, handler)
	if native.err != nil || native.resp.Content != "hi" {
		t.Fatalf("native: resp = %+v, err = %v", native.resp, native.err)
	}
	if fantasy.err == nil {
		t.Fatal("fantasy accepted the reply: the difference is gone, update docs/configuration.md")
	}
}

// HTTP error responses become the same *fantasy.ProviderError on both
// clients, so retries, Retry-After waits and overflow handling agree.
func TestNativeHTTPErrorsMatchFantasy(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
	}{
		{"bad request", 400, nil, `{"error":{"message":"bad thing","type":"invalid_request_error"}}`},
		{"auth", 401, nil, `{"error":{"message":"Incorrect API key provided"}}`},
		{"rate limit", 429, map[string]string{"Retry-After": "3"}, `{"error":{"message":"slow down"}}`},
		{"rate limit ms", 429, map[string]string{"Retry-After-Ms": "1500"}, ``},
		{"quota", 429, nil, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota"}}`},
		{"bad gateway html", 502, nil, `<html>Bad Gateway</html>`},
		{"overloaded", 529, nil, `{"error":{"message":"Overloaded"}}`},
		{"no error member", 400, nil, `{"message":"no error key"}`},
		{"string error member", 400, nil, `{"error":"string error"}`},
		{"context overflow", 400, nil, `{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens."}}`},
		{"should retry header", 400, map[string]string{"X-Should-Retry": "true"}, `{"error":{"message":"retry me"}}`},
		{"legacy completions model", 404, nil, `{"error":{"message":"This is not a chat model and thus not supported in the v1/chat/completions endpoint. Did you mean to use v1/completions?"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}
			req := Request{Messages: []Message{{Role: RoleUser, Content: "go"}}}
			native, fantasy := sendBothWires(t, false, req, handler)
			if native.err == nil {
				t.Fatal("expected an error")
			}
			assertSameOutcome(t, native, fantasy)
		})
	}
}

// The Spettro Subscription's 429 is waited out on the native client exactly
// as on fantasy: Retry-After honoured, then the request succeeds.
func TestNativeStreamWaitsOutSpettroRateLimit(t *testing.T) {
	shrinkRateLimitWaits(t, time.Millisecond, time.Minute)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, okSSE)
	}))
	t.Cleanup(srv.Close)
	pm := NewManager()
	pm.SetStreamAll(true)
	pm.SetAPIKeys(map[string]string{spettroProviderID: "k"})
	pm.SetSpettro(srv.URL, []Model{{Provider: spettroProviderID, Name: "m"}})
	var waits []time.Duration
	resp, err := pm.Send(context.Background(), spettroProviderID, "m", Request{
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
		OnRateLimit: func(d time.Duration) { waits = append(waits, d) },
	})
	if err != nil || resp.Content != "ok" {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
	if calls.Load() != 3 || len(waits) != 2 {
		t.Fatalf("requests = %d, waits = %v; want 3 requests and 2 waits", calls.Load(), waits)
	}
}

// A stream that stalls mid-reply fails with ErrStreamIdle on the native
// client too.
func TestNativeStreamIdleTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseData(chunkWith(map[string]any{"content": "hel"}, nil)))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	pm := NewManager()
	pm.AddLocalModels([]Model{{Provider: srv.URL, Name: "m", Local: true}})
	req := streamReq()
	req.StreamIdleTimeout = 150 * time.Millisecond
	_, err := pm.Send(context.Background(), srv.URL, "m", req)
	if !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("err = %v, want ErrStreamIdle", err)
	}
}

// A request cancelled before the reply's headers arrive fails with the
// bare context error on both clients (the SDK returns ctx.Err(), not the
// *url.Error around it).
func TestNativeCancelBeforeHeadersMatchesFantasy(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices a closed connection only once the body is
		// read; release covers a client that keeps it open.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs before srv.Close
	errs := map[WireMode]error{}
	for _, mode := range []WireMode{WireNative, WireFantasy} {
		pm := NewManager()
		pm.SetWireMode(string(mode))
		pm.AddLocalModels([]Model{{Provider: srv.URL, Name: "m", Local: true}})
		ctx, cancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(100*time.Millisecond, cancel)
		_, errs[mode] = pm.Send(ctx, srv.URL, "m", streamReq())
		timer.Stop()
		cancel()
	}
	native, fantasy := errs[WireNative], errs[WireFantasy]
	if !errors.Is(native, context.Canceled) || !errors.Is(fantasy, context.Canceled) {
		t.Fatalf("errors: native %v, fantasy %v; want context.Canceled", native, fantasy)
	}
	if native.Error() != fantasy.Error() {
		t.Fatalf("error text: native %q, fantasy %q", native, fantasy)
	}
}

// The environment override selects the client for one process.
func TestWireModeEnvironmentOverride(t *testing.T) {
	pm := NewManager()
	if pm.wireMode() != WireNative {
		t.Fatalf("default wire = %q, want native", pm.wireMode())
	}
	pm.SetWireMode("fantasy")
	if pm.wireMode() != WireFantasy {
		t.Fatalf("configured wire = %q, want fantasy", pm.wireMode())
	}
	t.Setenv(wireEnv, "native")
	if pm.wireMode() != WireNative {
		t.Fatalf("env override ignored: %q", pm.wireMode())
	}
	if _, err := ParseWireMode("grpc"); err == nil {
		t.Fatal("unknown wire mode accepted")
	}
}

// An encoder failure falls back to fantasy instead of failing the request.
func TestNativeEncoderFailureFallsBackToFantasy(t *testing.T) {
	rec := newBodyRecorder(t)
	pm := wireManager(rec, WireNative)
	// A nil encoder panics inside encode; the send must still go out.
	pm.encoder = nil
	enc := pm.chatEncoder()
	enc.lanes = []*encoderLane{nil}
	resp, err := pm.Send(context.Background(), rec.srv.URL, "m", Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil || resp.Content != "ok" {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
	rec.mu.Lock()
	ua := rec.headers[len(rec.headers)-1].Get("User-Agent")
	rec.mu.Unlock()
	if ua != fantasyUserAgent() {
		t.Fatalf("request did not fall back to fantasy (User-Agent %q)", ua)
	}
}
