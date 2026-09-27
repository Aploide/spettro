package tui

import "spettro/internal/config"

// Option configures a Model built by New. Options carry state the host has
// already computed, so New does not compute it again on the startup path.
type Option func(*newOptions)

// newOptions collects what the Options passed to New asked for.
type newOptions struct {
	// manifest is the project's agent manifest, when the host loaded it.
	manifest    config.AgentManifest
	hasManifest bool
	// modelUpdates is the host's model-change signal (WithModelUpdates).
	modelUpdates <-chan struct{}
}

// WithManifest hands New the project's agent manifest, as
// config.LoadAgentManifestForProject returned it for the same working
// directory, so New does not read and parse spettro.agents.toml a second
// time before the first frame (0.18 ms for the default manifest, 0.45 ms for
// this repository's, measured with a throwaway benchmark of
// LoadAgentManifestForProject). Without it New loads the manifest itself.
func WithManifest(m config.AgentManifest) Option {
	return func(o *newOptions) {
		o.manifest = m
		o.hasManifest = true
	}
}

// WithModelUpdates hands New a channel the host signals after a background
// source (the catalog refresh, a local endpoint probe) has changed the
// provider manager's model lists. The TUI waits on it and redraws what shows
// models (see modelsChangedMsg).
//
// The host must apply each change to the provider manager before it signals,
// and should signal without blocking into a channel with a buffer of one:
// a signal sent while one is still pending can be dropped, because the
// pending one already makes the TUI read the manager, which by then holds
// both changes.
func WithModelUpdates(updates <-chan struct{}) Option {
	return func(o *newOptions) { o.modelUpdates = updates }
}

// collectOptions applies opts in order; a later option overrides an earlier
// one.
func collectOptions(opts []Option) newOptions {
	var o newOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}
