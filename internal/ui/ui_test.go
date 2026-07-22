package ui

import (
	"strings"
	"testing"
)

func TestRendererKnownMode(t *testing.T) {
	r := NewRenderer()
	prompt := r.Prompt("coding", "anthropic", "claude-fable-5")
	if !strings.Contains(prompt, "coding") || !strings.Contains(prompt, "anthropic/claude-fable-5") {
		t.Errorf("Prompt = %q", prompt)
	}
	status := r.Status("planning", "ask")
	if !strings.Contains(status, "Planning Agent") || !strings.Contains(status, "PLANNING") || !strings.Contains(status, "ask") {
		t.Errorf("Status = %q", status)
	}
	if got := r.Stage("chat"); got != "chat (chat agent)" {
		t.Errorf("Stage = %q", got)
	}
}

func TestRendererUnknownModeFallsBack(t *testing.T) {
	r := NewRenderer()
	if got := r.Stage("nonexistent"); got != "unknown" {
		t.Errorf("Stage fallback = %q", got)
	}
	if status := r.Status("nonexistent", "yolo"); !strings.Contains(status, "Unknown") {
		t.Errorf("Status fallback = %q", status)
	}
}

func TestPanelAndInfo(t *testing.T) {
	r := NewRenderer()
	panel := r.Panel("coding", "Title", "body text")
	lines := strings.Split(panel, "\n")
	if len(lines) != 3 {
		t.Fatalf("panel lines = %d", len(lines))
	}
	if !strings.Contains(lines[0], "Title") || !strings.Contains(lines[1], "body text") {
		t.Errorf("panel = %q", panel)
	}
	if info := r.Info("note"); !strings.Contains(info, "note") {
		t.Errorf("Info = %q", info)
	}
	if !strings.Contains(r.Welcome(), "SPETTRO") {
		t.Error("Welcome missing banner")
	}
}
