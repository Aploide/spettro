package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The legacy adapters now use the OpenAI and Anthropic SDK forks fantasy
// already links (one copy of each SDK in the binary). These tests pin the
// request shapes they send.

// legacyServer answers chat completions for chat models, refuses them for
// "text-model" (as OpenAI does for completion-only models) and answers
// legacy completions, recording each request.
type legacyServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	paths  []string
	bodies []map[string]any
}

func newLegacyServer(t *testing.T) *legacyServer {
	t.Helper()
	ls := &legacyServer{}
	ls.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		ls.mu.Lock()
		ls.paths = append(ls.paths, r.URL.Path)
		ls.bodies = append(ls.bodies, body)
		ls.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions") && body["model"] == "text-model":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"This is not a chat model and thus not supported in the v1/chat/completions endpoint. Did you mean to use v1/completions?","type":"invalid_request_error"}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"chat ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9,"prompt_tokens_details":{"cached_tokens":3}}}`)
		case strings.HasSuffix(r.URL.Path, "/completions"):
			_, _ = io.WriteString(w, `{"id":"c","object":"text_completion","choices":[{"index":0,"text":"legacy ok","finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ls.srv.Close)
	return ls
}

// A completion-only model falls back to the legacy completions endpoint,
// on both clients.
func TestLegacyCompletionsFallback(t *testing.T) {
	for _, mode := range []WireMode{WireNative, WireFantasy} {
		t.Run(string(mode), func(t *testing.T) {
			ls := newLegacyServer(t)
			pm := NewManager()
			pm.SetStreamAll(true)
			pm.SetWireMode(string(mode))
			pm.AddLocalModels([]Model{{Provider: ls.srv.URL, Name: "text-model", Local: true}})
			resp, err := pm.Send(context.Background(), ls.srv.URL, "text-model", Request{Prompt: "Say hi"})
			if err != nil || resp.Content != "legacy ok" || resp.Usage.InputTokens != 4 {
				t.Fatalf("resp = %+v, err = %v", resp, err)
			}
			ls.mu.Lock()
			defer ls.mu.Unlock()
			last := ls.bodies[len(ls.bodies)-1]
			if ls.paths[len(ls.paths)-1] != "/v1/completions" || last["model"] != "text-model" || last["prompt"] != "Say hi" {
				t.Fatalf("legacy request %s %v", ls.paths[len(ls.paths)-1], last)
			}
		})
	}
}

// The legacy chat adapter sends images as data-URL parts ahead of the text
// and reports cached tokens split out of the prompt count.
func TestLegacyChatAdapterImageParts(t *testing.T) {
	ls := newLegacyServer(t)
	img := writeImage(t, t.TempDir(), "shot.png", 7)
	a := OpenAICompatibleAdapter{APIKey: "k", BaseURL: ls.srv.URL + "/v1"}
	resp, err := a.Send(context.Background(), "chat-model", Request{
		System:   "sys",
		Messages: []Message{{Role: RoleUser, Content: "what is this?", Images: []string{img}}},
	})
	if err != nil || resp.Content != "chat ok" || resp.Usage.InputTokens != 4 || resp.Usage.CacheReadTokens != 3 {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	msgs, _ := ls.bodies[0]["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	user, _ := msgs[1].(map[string]any)
	parts, _ := user["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("user content = %v", user["content"])
	}
	image, _ := parts[0].(map[string]any)
	text, _ := parts[1].(map[string]any)
	url, _ := image["image_url"].(map[string]any)["url"].(string)
	if image["type"] != "image_url" || !strings.HasPrefix(url, "data:image/png;base64,") || text["type"] != "text" || text["text"] != "what is this?" {
		t.Fatalf("parts = %v", parts)
	}
}
