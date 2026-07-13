package provider

import "testing"

func TestResolveActivePicksConnectedProvider(t *testing.T) {
	m := NewManager()

	// No credentials at all: nothing is usable.
	if p, mod := m.ResolveActive("openai-compatible", "gpt-5-mini", nil); p != "" || mod != "" {
		t.Fatalf("expected empty resolution without credentials, got %s/%s", p, mod)
	}

	// An openai key connects the fallback-catalog openai models.
	keys := map[string]string{"openai": "sk-test"}
	p, mod := m.ResolveActive("openai-compatible", "gpt-5-mini", keys)
	if p != "openai" || mod == "" {
		t.Fatalf("expected an openai model, got %s/%s", p, mod)
	}

	// A configured model whose provider has a key is kept as-is.
	if p, mod := m.ResolveActive("openai", "o3", keys); p != "openai" || mod != "o3" {
		t.Fatalf("expected configured model kept, got %s/%s", p, mod)
	}

	// Local endpoint providers (URL ids) need no key.
	if p, mod := m.ResolveActive("http://localhost:11434", "llama3", nil); p != "http://localhost:11434" || mod != "llama3" {
		t.Fatalf("expected local model kept, got %s/%s", p, mod)
	}
}

func TestPreferredModelPrefersSpettro(t *testing.T) {
	m := NewManager()
	m.SetSpettro("https://inference.spettro.app/v1", []Model{
		{Provider: "spettro", Name: "fast-1", ToolCall: true},
		{Provider: "spettro", Name: "smart-1", ToolCall: true},
	})
	keys := map[string]string{"spettro": "ep_test", "anthropic": "sk-ant"}
	pref, ok := m.PreferredModel(keys)
	if !ok || pref.Provider != "spettro" || pref.Name != "fast-1" {
		t.Fatalf("expected first spettro model preferred, got %+v ok=%v", pref, ok)
	}
}
