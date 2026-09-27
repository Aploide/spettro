package tui

import (
	"testing"
	"time"

	"spettro/internal/provider"
)

// localModel is a model served by the local endpoint url.
func localModel(url, name string) provider.Model {
	return provider.Model{Provider: url, ProviderName: url, Name: name, Local: true}
}

// A model a background probe adds while /models is open shows up in the
// selector at once, and the cursor stays on the model it was on.
func TestModelsChangedRefreshesOpenSelector(t *testing.T) {
	m := footerModel(120, 40)
	m.providers.AddLocalModels([]provider.Model{localModel("http://a.test/v1", "alpha"), localModel("http://a.test/v1", "beta")})
	m = m.openSelector("")
	m.selCursor = indexOfModel(t, m.selItems, "beta")

	// A second endpoint answers with a favorite, which the selector lists
	// first, so every other row moves down one.
	m.favorites = map[string]bool{"http://b.test/v1:gamma": true}
	m.providers.AddLocalModels([]provider.Model{localModel("http://b.test/v1", "gamma")})
	updated, _ := m.Update(modelsChangedMsg{})
	m = updated.(Model)

	if i := indexOfModel(t, m.selItems, "gamma"); i != 0 {
		t.Fatalf("favorite at row %d, want 0", i)
	}
	if got := m.selItems[m.selCursor].Name; got != "beta" {
		t.Fatalf("cursor on %q after the refresh, want it to stay on %q", got, "beta")
	}
}

// With a host signal, the handler waits for the next change: exactly one
// wait is pending at any time.
func TestModelsChangedWaitsForTheNextSignal(t *testing.T) {
	updates := make(chan struct{}, 1)
	m := footerModel(120, 40)
	m.modelUpdates = updates

	_, cmd := m.handleModelsChanged()
	if cmd == nil {
		t.Fatal("handler did not wait for the next change")
	}
	got := make(chan any, 1)
	go func() { got <- cmd() }()
	select {
	case msg := <-got:
		t.Fatalf("wait returned %T before any signal", msg)
	case <-time.After(20 * time.Millisecond):
	}
	updates <- struct{}{}
	select {
	case msg := <-got:
		if _, ok := msg.(modelsChangedMsg); !ok {
			t.Fatalf("wait returned %T, want modelsChangedMsg", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return after the signal")
	}
}

// Without a host signal the handler starts no wait.
func TestModelsChangedWithoutSignalStartsNoWait(t *testing.T) {
	if _, cmd := footerModel(120, 40).handleModelsChanged(); cmd != nil {
		t.Fatal("handler started a wait with no signal to wait on")
	}
}

// A closed signal ends the waiting instead of spinning.
func TestModelsChangedWaitEndsOnClose(t *testing.T) {
	updates := make(chan struct{})
	close(updates)
	if msg := waitForModelsChanged(updates)(); msg != nil {
		t.Fatalf("wait on a closed signal returned %T, want nil", msg)
	}
}

func TestCursorOnSameModel(t *testing.T) {
	a, b, c := localModel("u", "a"), localModel("u", "b"), localModel("u", "c")
	cases := []struct {
		name      string
		old       []provider.Model
		cursor    int
		items     []provider.Model
		wantIndex int
	}{
		{"model moved", []provider.Model{a, b}, 1, []provider.Model{c, a, b}, 2},
		{"model gone, clamp", []provider.Model{a, b, c}, 2, []provider.Model{a}, 0},
		{"empty new list", []provider.Model{a}, 0, nil, 0},
		{"empty old list", nil, 0, []provider.Model{a, b}, 0},
	}
	for _, tc := range cases {
		if got := cursorOnSameModel(tc.old, tc.cursor, tc.items); got != tc.wantIndex {
			t.Errorf("%s: cursor = %d, want %d", tc.name, got, tc.wantIndex)
		}
	}
}

func indexOfModel(t *testing.T, items []provider.Model, name string) int {
	t.Helper()
	for i, mod := range items {
		if mod.Name == name {
			return i
		}
	}
	t.Fatalf("model %q not in the list", name)
	return -1
}
