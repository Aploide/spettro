package agent_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"spettro/internal/provider"
)

// scriptedMessage converts one scripted response into an OpenAI-compatible
// assistant message. Lines starting with TOOL_CALL become native tool_calls
// entries (the runtime only accepts structured tool calls); a FINAL prefix is
// stripped and the remaining text becomes plain content.
func scriptedMessage(t *testing.T, response string) (map[string]any, string) {
	t.Helper()
	var contentLines []string
	var toolCalls []map[string]any
	lines := strings.Split(response, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "TOOL_CALL") {
			contentLines = append(contentLines, lines[i])
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "TOOL_CALL"))
		// A TOOL_CALL marker may be followed by JSON on subsequent lines.
		for raw == "" || !json.Valid([]byte(raw)) {
			i++
			if i >= len(lines) {
				break
			}
			raw = strings.TrimSpace(raw + "\n" + lines[i])
		}
		var envelope struct {
			Tool      string          `json:"tool"`
			Name      string          `json:"name"`
			Args      json.RawMessage `json:"args"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			t.Fatalf("scripted TOOL_CALL is not valid JSON: %q: %v", raw, err)
		}
		name := envelope.Tool
		if name == "" {
			name = envelope.Name
		}
		args := envelope.Args
		if len(args) == 0 {
			args = envelope.Arguments
		}
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":   fmt.Sprintf("call_%d", len(toolCalls)+1),
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": string(args),
			},
		})
	}
	content := strings.TrimSpace(strings.Join(contentLines, "\n"))
	if after, ok := strings.CutPrefix(content, "FINAL"); ok {
		content = strings.TrimSpace(strings.TrimPrefix(after, ":"))
	}
	msg := map[string]any{"role": "assistant", "content": content}
	finish := "stop"
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		finish = "tool_calls"
	}
	return msg, finish
}

func scriptedHandler(t *testing.T, responses []string) http.HandlerFunc {
	t.Helper()
	var idx atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		i := int(idx.Add(1)) - 1
		if i >= len(responses) {
			t.Errorf("unexpected extra request #%d (only %d scripted)", i+1, len(responses))
			http.Error(w, "no more scripted responses", http.StatusInternalServerError)
			return
		}
		msg, finish := scriptedMessage(t, responses[i])
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":     "chatcmpl-test",
			"object": "chat.completion",
			"choices": []map[string]any{
				{"index": 0, "message": msg, "finish_reason": finish},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 20,
				"total_tokens":      30,
			},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}
}

// scriptedServer returns an httptest.Server that serves a fixed sequence of
// OpenAI-compatible chat completion responses, one per request.
func scriptedServer(t *testing.T, responses []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(scriptedHandler(t, responses))
	t.Cleanup(srv.Close)
	return srv
}

// testProvider returns a Manager and provider name pointing at srv.
// The Manager treats http:// URLs as OpenAI-compatible local servers.
func testProvider(srv *httptest.Server) (*provider.Manager, string) {
	return provider.NewManager(), srv.URL
}

// scriptedManager creates a provider.Manager wired to a local HTTP server
// that serves a scripted sequence of LLM responses in order.
// Returns (manager, providerName, modelName).
func scriptedManager(t *testing.T, responses []string) (*provider.Manager, string, string) {
	t.Helper()
	srv := httptest.NewServer(scriptedHandler(t, responses))
	t.Cleanup(srv.Close)

	pm := provider.NewManager()
	pm.AddLocalModels([]provider.Model{{Provider: srv.URL, Name: "test-model", Local: true, ToolCall: true}})
	return pm, srv.URL, "test-model"
}
