package provider

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"charm.land/fantasy"

	wire "spettro/internal/provider/wire/chatcompletions"
	"spettro/internal/version"
)

// WireMode selects the client that carries streamed requests to
// OpenAI-compatible chat-completions endpoints (every provider with an
// OpenAI-style API except the official OpenAI one, the Spettro
// Subscription and local model servers).
type WireMode string

const (
	// WireNative is Spettro's own chat-completions client (the default):
	// per-message request encoding cached across the steps of a run, and
	// streamed tool arguments accumulated in linear time.
	WireNative WireMode = "native"
	// WireFantasy routes those requests through the fantasy SDK, as every
	// request went before the native client existed. Anthropic, the
	// official OpenAI provider and non-streamed requests always use it.
	WireFantasy WireMode = "fantasy"
)

// wireEnv overrides the configured wire mode for one process (debugging).
const wireEnv = "SPETTRO_PROVIDER_WIRE"

// ParseWireMode parses a provider_wire setting. The empty string is the
// default, WireNative.
func ParseWireMode(s string) (WireMode, error) {
	switch WireMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", WireNative:
		return WireNative, nil
	case WireFantasy:
		return WireFantasy, nil
	}
	return WireNative, fmt.Errorf("unknown provider wire %q (want %q or %q)", s, WireNative, WireFantasy)
}

// SetWireMode selects the chat-completions client (config provider_wire).
// An unknown value keeps the default. SPETTRO_PROVIDER_WIRE, when set,
// overrides it.
func (m *Manager) SetWireMode(mode string) {
	parsed, _ := ParseWireMode(mode)
	m.mu.Lock()
	m.wire = parsed
	m.mu.Unlock()
}

// wireMode is the mode in effect: the environment override, else the
// configured one.
func (m *Manager) wireMode() WireMode {
	if v := os.Getenv(wireEnv); v != "" {
		if mode, err := ParseWireMode(v); err == nil {
			return mode
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cmp.Or(m.wire, WireNative)
}

// chatEncoder returns the Manager's request encoder, creating it on first
// use.
func (m *Manager) chatEncoder() *chatEncoder {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.encoder == nil {
		m.encoder = &chatEncoder{}
	}
	return m.encoder
}

// nativeChatBaseURL reports whether a streamed request to providerName can
// take the native client, and the base URL it goes to. It is the set of
// requests fantasy sends through its OpenAI-compatible chat-completions
// provider: not Anthropic-protocol providers, not the official OpenAI
// provider (Responses API), and not "openai-compatible" without a base URL
// (fantasy sends that to OpenAI's Responses API too). A provider without a
// usable endpoint is left to fantasy, which reports the error.
func nativeChatBaseURL(providerName, apiKind, baseURL string) (string, bool) {
	if isAnthropicAPI(providerName, apiKind) || providerName == "openai" {
		return "", false
	}
	resolved, err := resolveOpenAICompatibleBaseURL(providerName, baseURL)
	if err != nil || resolved == "" {
		return "", false
	}
	return resolved, true
}

// errNativeEncode marks a request the native client could not encode; the
// manager then sends it through fantasy instead.
var errNativeEncode = errors.New("native chat-completions encoder failed")

// sendStream sends a streamed request through the native client when the
// wire mode and the provider allow it, and through fantasy otherwise or
// when the native encoder fails.
func (m *Manager) sendStream(ctx context.Context, providerName, apiKind, modelName, apiKey, baseURL string, req Request) (Response, error) {
	if m.wireMode() == WireNative {
		if base, ok := nativeChatBaseURL(providerName, apiKind, baseURL); ok {
			resp, err := sendNativeStream(ctx, m.chatEncoder(), providerName, modelName, apiKey, base, req)
			if !errors.Is(err, errNativeEncode) {
				return resp, err
			}
		}
	}
	return sendWithFantasyStream(ctx, providerName, apiKind, modelName, apiKey, baseURL, req)
}

// encodeChatRequest encodes req, turning a panic in the encoder into
// errNativeEncode so the request still goes out through fantasy.
func encodeChatRequest(enc *chatEncoder, providerName, modelName string, req Request) (body *wire.Body, err error) {
	defer func() {
		if r := recover(); r != nil {
			body, err = nil, fmt.Errorf("%w: %v", errNativeEncode, r)
		}
	}()
	return enc.encode(providerName, modelName, req), nil
}

// chatCompletionsURL resolves the endpoint the way the OpenAI SDK does: the
// base URL's path gains a trailing slash, then "chat/completions" is
// resolved against it.
func chatCompletionsURL(baseURL string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("requestoption: WithBaseURL failed to parse url %s", err)
	}
	if base.Path != "" && !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	u, err := base.Parse("chat/completions")
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// nativeUserAgent is the User-Agent of native chat-completions requests.
func nativeUserAgent() string { return "Spettro/" + version.App }

// openAIEnvHeaders maps the environment variables the OpenAI SDK reads on
// every client it builds (openai.DefaultClientOptions, which fantasy's
// OpenAI-compatible provider uses) to the headers it sends from them.
var openAIEnvHeaders = [...]struct{ env, header string }{
	{"OPENAI_ORG_ID", "OpenAI-Organization"},
	{"OPENAI_PROJECT_ID", "OpenAI-Project"},
}

// setOpenAIEnvHeaders sets the organization and project headers from the
// environment as the SDK does, so a user whose OPENAI_PROJECT_ID routes
// billing keeps that routing on the native client. Like the SDK it sets a
// header whenever the variable is present, even when it is empty.
func setOpenAIEnvHeaders(h http.Header) {
	for _, e := range openAIEnvHeaders {
		if v, ok := os.LookupEnv(e.env); ok {
			h.Set(e.header, v)
		}
	}
}

// sendNativeStream is sendWithFantasyStream on Spettro's own
// chat-completions client: the same request JSON, the same watchdog, and
// the same Response, errors included (see TestNativeStreamMatchesFantasy).
func sendNativeStream(ctx context.Context, enc *chatEncoder, providerName, modelName, apiKey, baseURL string, req Request) (Response, error) {
	endpoint, err := chatCompletionsURL(baseURL)
	if err != nil {
		return Response{}, err
	}
	body, err := encodeChatRequest(enc, providerName, modelName, req)
	if err != nil {
		return Response{}, err
	}
	if apiKey == "" {
		apiKey = "local"
	}

	firstTimeout, idleTimeout := streamTimeouts(providerName, req)
	streamCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := startStreamWatchdog(firstTimeout, idleTimeout, cancel)
	defer watchdog.stop()

	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, endpoint, body.Reader())
	if err != nil {
		return Response{}, err
	}
	httpReq.ContentLength = body.Len()
	httpReq.GetBody = func() (io.ReadCloser, error) { return body.Reader(), nil }
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", nativeUserAgent())
	setOpenAIEnvHeaders(httpReq.Header)

	collector := newStreamCollector(req.OnStream)
	streamErr := doNativeStream(httpReq, watchdog, collector)
	watchdog.stop()
	if err := streamOutcome(ctx, streamCtx, streamErr, collector, req.localEndpoint); err != nil {
		return Response{}, err
	}
	return collector.response(providerName, modelName, max(req.MaxTokens, 0)), nil
}

// doNativeStream sends httpReq and feeds the streamed reply to collector.
// It returns the error that ended the stream, or nil when it ended cleanly.
func doNativeStream(httpReq *http.Request, watchdog *streamWatchdog, collector *streamCollector) error {
	resp, err := activityHTTPClient{onRead: watchdog.touch}.Do(httpReq)
	if ctxErr := httpReq.Context().Err(); ctxErr != nil {
		// A request cancelled (by the user or the watchdog) before the
		// reply's headers were handled fails with the bare context error,
		// not the *url.Error wrapping it, as the SDK reports it.
		if resp != nil {
			resp.Body.Close()
		}
		return ctxErr
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nativeHTTPError(httpReq, resp)
	}
	state := newCompatStream(collector)
	events := wire.NewEventReader(resp.Body)
	defer events.Release()
	var chunk wire.Chunk
	for events.Next() {
		data := events.Data()
		if len(data) == 0 {
			continue
		}
		if bytes.HasPrefix(data, []byte("[DONE]")) {
			// Read to the end so the connection can be reused; nothing
			// after the terminator counts, a read error included.
			_, _ = io.Copy(io.Discard, resp.Body)
			state.end()
			return nil
		}
		if err := wire.DecodeChunk(data, &chunk); err != nil {
			return err
		}
		if chunk.Error != nil {
			return errors.New("received error while streaming: " + wire.ErrorMessage(chunk.Error))
		}
		if err := state.chunk(&chunk); err != nil {
			return err
		}
	}
	if err := events.Err(); err != nil {
		return err
	}
	state.end()
	return nil
}

// maxErrorBody bounds how much of an error response is read.
const maxErrorBody = 4 << 20

// openAIContextPattern is fantasy's pattern for OpenAI-style context
// overflow messages.
var openAIContextPattern = regexp.MustCompile(`maximum context length is (\d+) tokens.*?(?:resulted in|requested) (\d+) tokens`)

// nativeHTTPError converts an HTTP error response into the
// *fantasy.ProviderError fantasy returns for it, so retry classification,
// rate-limit waits (Retry-After) and context-overflow handling treat both
// clients alike. The message is the body's error.message, or the whole
// body when it has none.
func nativeHTTPError(httpReq *http.Request, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	message := string(body)
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	headers := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		if len(v) > 0 {
			headers[k] = v[len(v)-1]
		}
	}
	pe := &fantasy.ProviderError{
		Title:           cmp.Or(fantasy.ErrorTitleForStatusCode(resp.StatusCode), "provider request failed"),
		Message:         message,
		Cause:           fmt.Errorf("%s %q: %s", httpReq.Method, httpReq.URL, resp.Status),
		URL:             httpReq.URL.String(),
		StatusCode:      resp.StatusCode,
		ResponseHeaders: headers,
		ResponseBody:    body,
	}
	if m := openAIContextPattern.FindStringSubmatch(message); m != nil {
		pe.ContextTooLargeErr = true
		pe.ContextMaxTokens, _ = strconv.Atoi(m[1])
		pe.ContextUsedTokens, _ = strconv.Atoi(m[2])
	}
	return pe
}
