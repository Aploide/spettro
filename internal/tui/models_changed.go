package tui

import (
	tea "charm.land/bubbletea/v2"

	"spettro/internal/provider"
)

// modelsChangedMsg reports that a background source (the catalog refresh,
// a local endpoint probe) changed the provider manager's model lists. The
// lists themselves are already in the manager; the message only makes the
// TUI draw them: the idle TUI runs no timer (see tick), and the frame memo
// keeps the header and status bar until a message arrives, so without it a
// model that arrived in the background would appear only after the next
// keystroke.
type modelsChangedMsg struct{}

// waitForModelsChanged returns a command that waits for the next signal on
// updates and reports it as a modelsChangedMsg. The handler starts the next
// wait, so exactly one wait is pending at any time.
func waitForModelsChanged(updates <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		if _, ok := <-updates; !ok {
			return nil // the host closed the channel: no more updates
		}
		return modelsChangedMsg{}
	}
}

// handleModelsChanged refreshes the open model lists (the /models selector
// and the onboarding picker) from the provider manager, keeping the cursor
// on the model it was on, and waits for the next change. Everything else
// that shows model data (the header's model label, the context gauge) is
// read from the manager at render time, and this message, like any
// non-transcript message, re-renders the chrome (see frameMemo).
func (m Model) handleModelsChanged() (Model, tea.Cmd) {
	if m.showSelector {
		items := m.filterModels(m.selFilter)
		m.selCursor = cursorOnSameModel(m.selItems, m.selCursor, items)
		m.selItems = items
	}
	if m.showOnboarding && m.onboarding.step == 0 {
		items := m.allOnboardingModels(m.onboarding.filter)
		m.onboarding.cursor = cursorOnSameModel(m.onboarding.items, m.onboarding.cursor, items)
		m.onboarding.items = items
	}
	if m.modelUpdates == nil {
		return m, nil
	}
	return m, waitForModelsChanged(m.modelUpdates)
}

// cursorOnSameModel returns the index in items of the model at cursor in
// old, so a list refreshed under the cursor keeps the user's place. When that
// model is gone the cursor stays where it was, clamped to the new list.
func cursorOnSameModel(old []provider.Model, cursor int, items []provider.Model) int {
	if cursor >= 0 && cursor < len(old) {
		want := old[cursor]
		for i, mod := range items {
			if mod.Provider == want.Provider && mod.Name == want.Name {
				return i
			}
		}
	}
	return max(0, min(cursor, len(items)-1))
}
