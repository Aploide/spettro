package skills

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Cache memoises Discover per set of discovery roots (see cacheKey), so every
// agent run, sub-agent and command menu in a session sees the same catalog
// without re-reading every SKILL.md. That stability is what keeps the skill
// list in the system prompt byte-identical from one run to the next, which
// the provider prompt cache depends on.
//
// A cached catalog is used only while it is current: Get re-stats every
// path the catalog was built from (the roots, the folders looked into, each
// SKILL.md; see discover) and rescans when any of them was added, removed or
// modified. So a skill added, edited or deleted on disk shows up on the next
// Get in every host, including the long-lived ACP and headless processes,
// which have no /skill reload; and the prompt changes only when a skill
// really changed. Invalidate forces a rescan regardless (/skill reload,
// install, uninstall, enable call it).
//
// A zero Cache is ready to use and safe for concurrent use.
type Cache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

// cacheEntry is one cached Discover result and the stamps of the paths it
// was built from.
type cacheEntry struct {
	cat    Catalog
	stamps []pathStamp
}

// Shared is the process-wide cache the hosts (TUI, ACP, headless) and the
// agent runtime use.
var Shared = &Cache{}

// Get returns the catalog for cwd and opts, discovering it on first use and
// again whenever the skill folders changed on disk. The result is a copy the
// caller may modify.
func (c *Cache) Get(cwd string, opts LookupOptions) Catalog {
	key := cacheKey(SearchRoots(cwd, opts))
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && stampsCurrent(e.stamps) {
		return e.cat.clone()
	}
	cat, stamps := discover(cwd, opts)
	if c.entries == nil {
		c.entries = map[string]cacheEntry{}
	}
	c.entries[key] = cacheEntry{cat: cat, stamps: stamps}
	return cat.clone()
}

// Invalidate drops every cached catalog; the next Get rescans.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

// cacheKey identifies one Discover call by the roots it scans, which is
// exactly what determines its result. Keying on the roots rather than on
// cwd and options means a change to anything that moves them (the compat
// setting, HOME, CODEX_HOME, a new .git higher up) takes effect without an
// explicit reload.
func cacheKey(roots []Root) string {
	var b strings.Builder
	for _, r := range roots {
		fmt.Fprintf(&b, "%s|%s|%s\n", r.Path, r.Source, r.Scope)
	}
	return b.String()
}

// pathStamp records what a path looked like when a catalog was built from
// it. A directory's modification time changes when an entry is added,
// removed or renamed in it, and a file's when it is written, so comparing
// stamps catches every change that could alter the catalog: a new skill
// folder, a SKILL.md created, edited or deleted, a legacy disabled marker
// added or removed.
type pathStamp struct {
	path    string
	exists  bool
	modTime time.Time
	size    int64
}

// stampOf stats path (following symlinks, as discovery does).
func stampOf(path string) pathStamp {
	info, err := os.Stat(path)
	if err != nil {
		return pathStamp{path: path}
	}
	return pathStamp{path: path, exists: true, modTime: info.ModTime(), size: info.Size()}
}

// stampsCurrent reports whether every stamped path still looks the same.
func stampsCurrent(stamps []pathStamp) bool {
	for _, s := range stamps {
		now := stampOf(s.path)
		if now.exists != s.exists || !now.modTime.Equal(s.modTime) || now.size != s.size {
			return false
		}
	}
	return true
}
