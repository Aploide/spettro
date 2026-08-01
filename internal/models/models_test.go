package models

import (
	"os"
	"path/filepath"
	"testing"
)

// Load must serve a valid disk cache without touching the network.
func TestLoadFromCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cache := filepath.Join(home, ".spettro", "catalog.json")
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"version":1,"updated":"2026-07-01","providers":{
		"anthropic":{"name":"Anthropic","api":"anthropic","base_url":"https://api.anthropic.com","env":"ANTHROPIC_API_KEY",
			"models":{"claude-fable-5":{"name":"Fable 5","reasoning":true,"tool_call":true,"vision":true,"context":200000}}}}}`
	if err := os.WriteFile(cache, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	cat, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cat.Providers["anthropic"]
	if !ok || p.API != APIAnthropic {
		t.Fatalf("provider missing or wrong: %+v", cat.Providers)
	}
	m, ok := p.Models["claude-fable-5"]
	if !ok || !m.Reasoning || m.Context != 200000 {
		t.Errorf("model entry wrong: %+v", m)
	}
}

func TestCacheFileUsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, err := cacheFile()
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(home, ".spettro", "catalog.json") {
		t.Errorf("cacheFile = %q", p)
	}
}
