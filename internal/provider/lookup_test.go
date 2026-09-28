package provider

// Guards for Manager.Lookup: per-request model metadata must not copy or
// allocate, and must answer what the linear scans it replaced answered.

import (
	"fmt"
	"testing"
)

// guardManager knows a large catalog-sized model list plus one local model.
func guardManager() *Manager {
	pm := NewManager()
	cat := make([]Model, 625)
	for i := range cat {
		cat[i] = Model{Provider: fmt.Sprint("p", i%15), Name: fmt.Sprint("model-", i), Context: 128000}
	}
	pm.mu.Lock()
	pm.catalog = cat
	pm.rebuildSnapshotLocked()
	pm.mu.Unlock()
	pm.AddLocalModels([]Model{{Provider: "http://127.0.0.1:1/some/long/endpoint/path", Name: "a-long-local-model-name", Local: true, Vision: true}})
	return pm
}

func TestLookupDoesNotAllocate(t *testing.T) {
	pm := guardManager()
	var ok bool
	n := testing.AllocsPerRun(100, func() {
		_, ok = pm.Lookup("http://127.0.0.1:1/some/long/endpoint/path", "a-long-local-model-name")
		_ = pm.ModelContext("p3", "model-3")
		_ = pm.SupportsVision("p3", "model-3")
		_ = pm.MaxOutputTokens("p3", "model-3")
		_ = pm.isLocalEndpoint("p3", "model-3")
	})
	if !ok || n != 0 {
		t.Fatalf("lookups allocated %v times (found=%v)", n, ok)
	}
}

// The index keeps the first match and the first positive output limit,
// as the scans it replaced did.
func TestLookupFirstMatchWins(t *testing.T) {
	pm := NewManager()
	pm.SetSpettro("http://s", []Model{{Provider: "dup", Name: "m", Context: 1}})
	pm.AddLocalModels([]Model{{Provider: "dup", Name: "m", Context: 2, MaxOutput: 99}})
	if got := pm.ModelContext("dup", "m"); got != 1 {
		t.Fatalf("context = %d, want the first match's 1", got)
	}
	if got := pm.MaxOutputTokens("dup", "m"); got != 99 {
		t.Fatalf("max output = %d, want the first positive 99", got)
	}
	pm.ClearSpettro()
	if got := pm.ModelContext("dup", "m"); got != 2 {
		t.Fatalf("after ClearSpettro context = %d, want 2", got)
	}
	pm.RemoveLocalModels("dup")
	if pm.HasModel("dup", "m") {
		t.Fatal("removed model still found")
	}
	if len(pm.Models()) != len(fallbackModels) {
		t.Fatalf("Models() = %d entries, want the %d fallback models", len(pm.Models()), len(fallbackModels))
	}
}
