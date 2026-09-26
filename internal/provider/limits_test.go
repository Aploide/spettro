package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	fantasyanthropic "charm.land/fantasy/providers/anthropic"

	"spettro/internal/models"
)

func TestKnownOutputLimit(t *testing.T) {
	for id, want := range map[string]int{
		"claude-sonnet-4-5":                        64000,
		"claude-opus-4-5":                          64000,
		"claude-opus-4-6":                          64000,
		"claude-opus-4":                            32000,
		"claude-opus-4-1":                          32000,
		"claude-opus-4-1-20250805":                 32000,
		"claude-opus-4-20250514":                   32000,
		"us.anthropic.claude-opus-4-20250514-v1:0": 32000,
		"anthropic/claude-sonnet-4.5":              64000,
		"claude-3-5-haiku-latest":                  8192,
		"claude-3-7-sonnet-latest":                 64000,
		"claude-3-haiku-20240307":                  4096,
		"gpt-4o-mini":                              16384,
		"gpt-4.1":                                  32768,
		"gpt-5-codex":                              128000,
		"o3":                                       100000,
		"openai/o4-mini":                           100000,
		"deepseek-chat":                            8192,
		"qwen3-coder-30b":                          0,
		"llama-3.3-70b-versatile":                  0,
	} {
		if got := knownOutputLimit(id); got != want {
			t.Errorf("knownOutputLimit(%q) = %d, want %d", id, got, want)
		}
	}
}

func TestResolveMaxOutput(t *testing.T) {
	m := NewManager()
	m.SetCatalog(models.Catalog{Providers: map[string]models.CatalogProvider{
		"anthropic": {API: models.APIAnthropic, Models: map[string]models.CatalogModel{
			"claude-future": {Name: "Future", Output: 100000},
		}},
		"zai":  {API: models.APIAnthropic, BaseURL: "https://x", Models: map[string]models.CatalogModel{"glm-9": {}}},
		"groq": {API: models.APIOpenAI, BaseURL: "https://x", Models: map[string]models.CatalogModel{"llama-x": {}}},
	}})
	cases := []struct {
		name, provider, apiKind, model string
		requested, want                int
	}{
		{"catalog output wins", "anthropic", models.APIAnthropic, "claude-future", 0, 100000},
		{"known family", "anthropic", models.APIAnthropic, "claude-sonnet-4-5", 0, 64000},
		{"unknown anthropic-protocol model gets the default", "zai", models.APIAnthropic, "glm-9", 0, DefaultMaxOutputTokens},
		{"unknown openai-style model keeps the server default", "groq", models.APIOpenAI, "llama-x", 0, 0},
		{"explicit value is kept", "groq", models.APIOpenAI, "llama-x", 4000, 4000},
		{"explicit value is clamped to the known limit", "anthropic", models.APIAnthropic, "claude-3-5-haiku-latest", 50000, 8192},
	}
	for _, tc := range cases {
		if got := m.resolveMaxOutput(tc.provider, tc.apiKind, tc.model, tc.requested); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestProbeLocalServerReadsContextWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"vllm-model","max_model_len":32768},{"id":"gw-model","context_length":131072},{"id":"bare"}]}`))
	}))
	t.Cleanup(srv.Close)
	got, err := ProbeLocalServer(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"vllm-model": 32768, "gw-model": 131072, "bare": 0}
	for _, m := range got {
		if m.Context != want[m.Name] {
			t.Errorf("%s: context %d, want %d", m.Name, m.Context, want[m.Name])
		}
	}
}

func TestEstimateRequestTokensCountsEverything(t *testing.T) {
	base := EstimateRequestTokens(Request{System: "sys", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	full := EstimateRequestTokens(Request{
		System: "sys",
		Messages: []Message{
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant, Reasoning: []ReasoningBlock{{Text: string(make([]byte, 400))}}, ToolCalls: []NativeTool{{Name: "x", Args: json.RawMessage(`{"a":"` + string(make([]byte, 400)) + `"}`)}}},
			{Role: RoleUser, ToolResults: []ToolResult{{Output: string(make([]byte, 4000))}}},
		},
		Tools: []ToolSpec{{Name: "x", Description: string(make([]byte, 400)), Schema: json.RawMessage(`{}`)}},
	})
	if full-base < 1200 {
		t.Fatalf("estimate grew by %d; tool results, args, reasoning and schemas must all count", full-base)
	}
}

// Reasoning is replayed only to the model that produced it, in the form its
// wire protocol accepts.
func TestReplayReasoning(t *testing.T) {
	signed := []ReasoningBlock{{Text: "t", Signature: "sig", Provider: "anthropic", Model: "claude-sonnet-4-5"}}
	plain := []ReasoningBlock{{Text: "t", Provider: "deepseek", Model: "deepseek-reasoner"}}

	if parts := replayReasoning("anthropic", models.APIAnthropic, "claude-sonnet-4-5", Request{Thinking: ThinkingHigh}, signed); len(parts) != 1 {
		t.Fatalf("anthropic with thinking: got %d parts", len(parts))
	} else if meta := fantasyanthropic.GetReasoningMetadata(parts[0].Options()); meta == nil || meta.Signature != "sig" {
		t.Fatalf("signature not carried: %+v", parts[0])
	}
	if parts := replayReasoning("anthropic", models.APIAnthropic, "claude-sonnet-4-5", Request{}, signed); len(parts) != 0 {
		t.Fatal("thinking disabled: nothing must be replayed")
	}
	if parts := replayReasoning("anthropic", models.APIAnthropic, "claude-opus-4-5", Request{Thinking: ThinkingHigh}, signed); len(parts) != 0 {
		t.Fatal("a signature must never be replayed to another model")
	}
	unsigned := []ReasoningBlock{{Text: "t", Provider: "anthropic", Model: "claude-sonnet-4-5"}}
	if parts := replayReasoning("anthropic", models.APIAnthropic, "claude-sonnet-4-5", Request{Thinking: ThinkingHigh}, unsigned); len(parts) != 0 {
		t.Fatal("unsigned thinking cannot be replayed to Anthropic")
	}
	parts := replayReasoning("deepseek", models.APIOpenAI, "deepseek-reasoner", Request{}, plain)
	if len(parts) != 1 {
		t.Fatalf("openai-compatible: got %d parts", len(parts))
	}
	if rp, ok := parts[0].(fantasy.ReasoningPart); !ok || rp.Text != "t" {
		t.Fatalf("unexpected part %+v", parts[0])
	}
	if parts := replayReasoning("openai", models.APIOpenAI, "o3", Request{}, []ReasoningBlock{{Text: "t", Provider: "openai", Model: "o3"}}); len(parts) != 0 {
		t.Fatal("OpenAI Responses reasoning is not replayable inline")
	}
}
