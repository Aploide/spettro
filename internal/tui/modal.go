package tui

import (
	tea "charm.land/bubbletea/v2"
)

// modal identifies the full-screen overlay that owns the UI. There must be a
// single source of truth for which overlay is active so the three consumers —
// key routing (update), the non-key passthrough guard (update), and rendering
// (View) — can never drift in order or membership.
type modal int

const (
	modalNone modal = iota
	modalTrust
	modalLogin
	modalOnboarding
	modalQuestion
	modalResume
	modalMemoryReview
	modalMemoryCurate
	modalRewind
	modalStorageClean
	modalConnect
	modalSelector
	modalSetup
)

// activeModal returns the highest-precedence active overlay. The precedence is
// the canonical dispatch order consulted by both update() and View(). Trust is
// the startup gate so it wins; setup is last (legacy, currently never set).
func (m Model) activeModal() modal {
	switch {
	case m.showTrust:
		return modalTrust
	case m.showLogin:
		return modalLogin
	case m.showOnboarding:
		return modalOnboarding
	case m.pendingQuestion != nil:
		// A question is raised mid-run and blocks a tool call, so it outranks
		// every incidental picker the user may have left open. It stays below
		// the startup gates, which decide whether the run may happen at all.
		return modalQuestion
	case m.showResume:
		return modalResume
	case m.showMemoryReview:
		return modalMemoryReview
	case m.showMemoryCurate:
		return modalMemoryCurate
	case m.showRewind:
		return modalRewind
	case m.showStorageClean:
		return modalStorageClean
	case m.showConnect:
		return modalConnect
	case m.showSelector:
		return modalSelector
	case m.showSetup:
		return modalSetup
	default:
		return modalNone
	}
}

// modalHandler bundles the key handler and view renderer for one overlay.
// Adding a dialog means adding a per-dialog file with its update/view methods
// and registering them here — update() and viewContent() dispatch through this
// table instead of hand-maintained switches.
type modalHandler struct {
	update func(Model, tea.KeyPressMsg) (tea.Model, tea.Cmd)
	// view may be nil for modals with no dedicated overlay (modalSetup,
	// legacy); rendering then falls through to the main pane.
	view func(Model) string
}

var modalHandlers = map[modal]modalHandler{
	modalTrust:      {update: Model.updateTrust, view: Model.viewTrust},
	modalLogin:      {update: Model.updateLogin, view: Model.viewLogin},
	modalOnboarding: {update: Model.updateOnboarding, view: Model.viewOnboarding},
	// The question form renders inside the input box (see viewInput), so it
	// registers no view of its own: the conversation it is asking about has to
	// stay on screen behind it.
	modalQuestion:     {update: Model.updateQuestion},
	modalResume:       {update: Model.updateResume, view: Model.viewResume},
	modalMemoryReview: {update: Model.updateMemoryReview, view: Model.viewMemoryReview},
	modalMemoryCurate: {update: Model.updateMemoryCurate, view: Model.viewMemoryCurate},
	modalRewind:       {update: Model.updateRewind, view: Model.viewRewind},
	modalStorageClean: {update: Model.updateStorageClean, view: Model.viewStorageClean},
	modalConnect:      {update: Model.updateConnect, view: Model.viewConnect},
	modalSelector:     {update: Model.updateSelector, view: Model.viewSelector},
	modalSetup:        {update: Model.updateSetup},
}
