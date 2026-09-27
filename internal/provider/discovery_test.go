package provider

import "testing"

func localModels(endpoint string, names ...string) []Model {
	out := make([]Model, 0, len(names))
	for _, n := range names {
		out = append(out, Model{Provider: endpoint, Name: n, Local: true})
	}
	return out
}

func localNames(m *Manager, endpoint string) []string {
	var names []string
	for _, mod := range m.Models() {
		if mod.Local && mod.Provider == endpoint {
			names = append(names, mod.Name)
		}
	}
	return names
}

// A discovery result must not bring back an endpoint the user removed, or
// replace a list the user re-probed, after discovery started.
func TestAddLocalModelsIfUnchangedYieldsToLaterChanges(t *testing.T) {
	const ep = "http://127.0.0.1:1"

	t.Run("removed", func(t *testing.T) {
		m := NewManager()
		gen := m.LocalModelsGeneration(ep)
		m.AddLocalModels(localModels(ep, "user"))
		m.RemoveLocalModels(ep)
		if _, ok := m.AddLocalModelsIfUnchanged(localModels(ep, "stale"), gen); ok {
			t.Fatal("stale result applied after a removal")
		}
		if got := localNames(m, ep); len(got) != 0 {
			t.Fatalf("removed endpoint came back: %v", got)
		}
	})
	t.Run("re-probed", func(t *testing.T) {
		m := NewManager()
		gen := m.LocalModelsGeneration(ep)
		m.AddLocalModels(localModels(ep, "fresh"))
		if _, ok := m.AddLocalModelsIfUnchanged(localModels(ep, "stale"), gen); ok {
			t.Fatal("stale result replaced a newer probe")
		}
		if got := localNames(m, ep); len(got) != 1 || got[0] != "fresh" {
			t.Fatalf("models = %v, want the fresh probe", got)
		}
	})
	t.Run("unchanged", func(t *testing.T) {
		m := NewManager()
		gen := m.LocalModelsGeneration(ep)
		next, ok := m.AddLocalModelsIfUnchanged(localModels(ep, "found"), gen)
		if !ok || next == gen {
			t.Fatalf("applied=%v gen %d -> %d", ok, gen, next)
		}
		if _, ok := m.AddLocalModelsIfUnchanged(localModels(ep, "found"), next); !ok {
			t.Fatal("re-applying at the returned generation failed")
		}
	})
}

// A subscription model list fetched before a sign-out is dropped.
func TestSetSpettroIfUnchangedYieldsToSignOut(t *testing.T) {
	m := NewManager()
	m.SetSpettro("https://inference.example", nil)
	gen := m.SpettroGeneration()
	m.ClearSpettro()
	if m.SetSpettroIfUnchanged("https://inference.example", []Model{{Provider: spettroProviderID, Name: "fast"}}, gen) {
		t.Fatal("stale subscription list applied after sign-out")
	}
	for _, mod := range m.Models() {
		if mod.Provider == spettroProviderID {
			t.Fatalf("subscription model %q is back after sign-out", mod.Name)
		}
	}
}

// The generation bookkeeping works on a zero Manager, which some tests use.
func TestZeroManagerRemoveLocalModels(t *testing.T) {
	var m Manager
	m.RemoveLocalModels("http://127.0.0.1:1")
	if m.LocalModelsGeneration("http://127.0.0.1:1") != 1 {
		t.Fatal("removal not counted")
	}
}
