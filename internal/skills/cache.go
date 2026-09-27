package skills

import (
	"fmt"
	"path/filepath"
	"sync"
)

// Cache memoises Discover per working directory and lookup options, so every
// agent run, sub-agent and command menu in a session sees the same catalog
// without rescanning the disk. That stability is what keeps the skill list
// in the system prompt byte-identical from one run to the next, which the
// provider prompt cache depends on.
//
// The cache never expires on its own: a skill added or edited on disk
// appears after Invalidate, which /skill reload, install, uninstall, enable
// and disable call. A zero Cache is ready to use and safe for concurrent
// use.
type Cache struct {
	mu      sync.Mutex
	entries map[string]Catalog
}

// Shared is the process-wide cache the hosts (TUI, ACP, headless) and the
// agent runtime use.
var Shared = &Cache{}

// Get returns the catalog for cwd and opts, discovering it on first use. The
// result is a copy the caller may modify.
func (c *Cache) Get(cwd string, opts LookupOptions) Catalog {
	key := cacheKey(cwd, opts)
	c.mu.Lock()
	defer c.mu.Unlock()
	if cat, ok := c.entries[key]; ok {
		return cat.clone()
	}
	cat, _ := Discover(cwd, opts)
	if c.entries == nil {
		c.entries = map[string]Catalog{}
	}
	c.entries[key] = cat
	return cat.clone()
}

// Invalidate drops every cached catalog; the next Get rescans.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

// cacheKey identifies one Discover call. Options are part of the key so a
// change to the compat setting takes effect without an explicit reload.
func cacheKey(cwd string, opts LookupOptions) string {
	return fmt.Sprintf("%s|%t|%t|%t|%q", filepath.Clean(cwd), opts.IncludeProject, opts.IncludeUser, opts.IncludeCompat, opts.ExtraDirs)
}
