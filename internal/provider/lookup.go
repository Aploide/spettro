package provider

import "sync"

// modelKey identifies a model by provider and name. A struct key keeps
// Lookup allocation-free (a concatenated string key would allocate for
// long local-endpoint URLs).
type modelKey struct {
	provider, name string
}

// modelEntry is the index entry of one provider/model pair.
type modelEntry struct {
	// pos is the position of the pair's first occurrence in
	// modelSnapshot.models: the first match wins, as it always has for
	// the linear scans this index replaces.
	pos int
	// maxOutput is the first positive MaxOutput among the pair's
	// occurrences (0 when none has one), which MaxOutputTokens reports.
	maxOutput int
}

// modelSnapshot is an immutable view of every known model in display
// order (Spettro Subscription, then the catalog or the built-in fallback
// list, then local endpoints) with an index for per-request lookups.
//
// It is a cache of the three model lists on Manager. Key: provider and
// model name. Invalidation: rebuilt, under Manager.mu, by every method that
// changes one of the lists (SetCatalog, SetSpettro, ClearSpettro,
// AddLocalModels, RemoveLocalModels) and never modified afterwards, so a
// reader holding it needs no lock. Owner: whichever goroutine holds
// Manager.mu for writing builds it; any goroutine may read it.
type modelSnapshot struct {
	models []Model
	index  map[modelKey]modelEntry
}

// buildModelSnapshot builds the snapshot of the given lists. An empty
// catalog is replaced by the built-in fallback list, as Models always did.
func buildModelSnapshot(spettro, catalog, local []Model) *modelSnapshot {
	base := catalog
	if len(base) == 0 {
		base = fallbackModels
	}
	all := make([]Model, 0, len(spettro)+len(base)+len(local))
	all = append(all, spettro...)
	all = append(all, base...)
	all = append(all, local...)
	index := make(map[modelKey]modelEntry, len(all))
	for i, mod := range all {
		key := modelKey{mod.Provider, mod.Name}
		entry, seen := index[key]
		if !seen {
			entry = modelEntry{pos: i}
		}
		if entry.maxOutput == 0 && mod.MaxOutput > 0 {
			entry.maxOutput = mod.MaxOutput
		}
		index[key] = entry
	}
	return &modelSnapshot{models: all, index: index}
}

// rebuildSnapshotLocked refreshes the snapshot after a model list changed.
// The caller holds m.mu for writing.
func (m *Manager) rebuildSnapshotLocked() {
	m.snapshot = buildModelSnapshot(m.spettroModels, m.catalog, m.localModels)
}

// models returns the current snapshot.
func (m *Manager) models() *modelSnapshot {
	m.mu.RLock()
	s := m.snapshot
	m.mu.RUnlock()
	if s == nil {
		// A Manager built as a struct literal (tests) has never had a
		// list set: it knows the fallback list only.
		return emptySnapshot()
	}
	return s
}

// emptySnapshot is the snapshot of a Manager with no lists set.
var emptySnapshot = sync.OnceValue(func() *modelSnapshot { return buildModelSnapshot(nil, nil, nil) })

// Lookup returns the first known model named modelName under providerName.
// It does not allocate; per-request metadata checks (vision, tool calling,
// context window, output limit) all go through it rather than through
// Models, which copies the whole list.
func (m *Manager) Lookup(providerName, modelName string) (Model, bool) {
	s := m.models()
	entry, ok := s.index[modelKey{providerName, modelName}]
	if !ok {
		return Model{}, false
	}
	return s.models[entry.pos], true
}
