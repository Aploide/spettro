package provider

// Conditional updates for background model discovery.
//
// Model discovery (local endpoint probes, the Spettro Subscription model
// list) runs in the background while the user can already change the same
// lists: remove or re-probe an endpoint, sign out. A discovery result that
// arrives after such a change is older than the change and must not undo it.
// The Manager therefore counts the changes to each list (localGen,
// spettroGen); discovery reads the count before it starts and applies its
// result only if the count is unchanged, checked and applied under one lock
// so no change can slip in between.

// bumpLocalGenLocked records a change to providerID's local model list.
// m.mu must be held.
func (m *Manager) bumpLocalGenLocked(providerID string) {
	if m.localGen == nil {
		m.localGen = map[string]uint64{}
	}
	m.localGen[providerID]++
}

// LocalModelsGeneration returns how many times the model list of the local
// endpoint providerID (see LocalProviderID) has changed.
func (m *Manager) LocalModelsGeneration(providerID string) uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.localGen[providerID]
}

// AddLocalModelsIfUnchanged does what AddLocalModels does, but only when the
// endpoint's model list is still at generation gen (as read with
// LocalModelsGeneration). It returns the endpoint's generation after the call
// and whether the models were applied.
func (m *Manager) AddLocalModelsIfUnchanged(models []Model, gen uint64) (uint64, bool) {
	if len(models) == 0 {
		return gen, false
	}
	providerID := models[0].Provider
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.localGen[providerID] != gen {
		return m.localGen[providerID], false
	}
	m.addLocalModelsLocked(models)
	return m.localGen[providerID], true
}

// SpettroGeneration returns how many times the Spettro Subscription models
// have been set or cleared.
func (m *Manager) SpettroGeneration() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.spettroGen
}

// SetSpettroIfUnchanged does what SetSpettro does, but only when the
// subscription models are still at generation gen (as read with
// SpettroGeneration), so a list fetched before a sign-out or a newer fetch
// does not overwrite it. It reports whether the models were applied.
func (m *Manager) SetSpettroIfUnchanged(inferenceBaseURL string, models []Model, gen uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.spettroGen != gen {
		return false
	}
	m.setSpettroLocked(inferenceBaseURL, models)
	return true
}
