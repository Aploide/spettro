package storage

// The artifact registry is the single authoritative inventory of everything
// Spettro writes to disk. Every feature that persists data registers its
// artifact class here with a class (cache|history|user|secret), a sizer and —
// only for reclaimable classes — a deleter. Cleanup (/storage clean, `spettro
// clean`) operates exclusively on registered reclaimable items; anything under
// ~/.spettro the registry does not claim is reported as unknown and never
// deleted. That contract is what keeps cleanup from ever deleting something
// load-bearing.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Class describes how an artifact may be treated by cleanup.
type Class string

const (
	// ClassCache is regenerable data: safe to delete, rebuilt lazily.
	ClassCache Class = "cache"
	// ClassHistory is user-visible history (checkpoints, sessions): safe to
	// delete but loses /rewind or /resume for the deleted entries.
	ClassHistory Class = "history"
	// ClassUser is user content (memory, skills, custom commands): never
	// offered for deletion, managed only through its own commands.
	ClassUser Class = "user"
	// ClassSecret is config and credentials: never deleted, never listed.
	ClassSecret Class = "secret"
)

// Reclaimable reports whether cleanup may ever offer this class for deletion.
func (c Class) Reclaimable() bool { return c == ClassCache || c == ClassHistory }

// CleanOptions tunes what the clean planner preselects and protects.
type CleanOptions struct {
	// SessionAgeDays: sessions not updated within this many days are
	// candidates. <=0 uses DefaultSessionAgeDays.
	SessionAgeDays int
	// KeepSessions: the most recent K sessions per project are never
	// candidates regardless of age. <=0 uses DefaultKeepSessions.
	KeepSessions int
	// ActiveSessionID is never a candidate (the live TUI session).
	ActiveSessionID string
}

const (
	DefaultSessionAgeDays = 30
	DefaultKeepSessions   = 5
)

func (o CleanOptions) withDefaults() CleanOptions {
	if o.SessionAgeDays <= 0 {
		o.SessionAgeDays = DefaultSessionAgeDays
	}
	if o.KeepSessions <= 0 {
		o.KeepSessions = DefaultKeepSessions
	}
	return o
}

// Item is one deletable unit within an artifact class: a single history dir,
// session dir, cache file or spool dir. Items carry their own validated
// deleter; cleanup never removes arbitrary paths.
type Item struct {
	ClassName string // registry entry name, e.g. "history", "sessions"
	Label     string // human-readable, e.g. "orphan: /old/project (312 MB)"
	Path      string
	Size      int64
	// Preselected marks the safe defaults (/storage clean checks them on
	// open; `spettro clean` deletes exactly these with --yes).
	Preselected bool
	// root and match implement belt-and-braces deletion: Delete refuses to
	// remove anything outside root or not matching the class pattern, even if
	// a bug corrupts the item's Path.
	root  string
	match func(path string) bool
}

// Delete removes the item after validating it is inside its class root and
// matches the class pattern.
func (it Item) Delete() error {
	path, err := filepath.Abs(it.Path)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(it.root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to delete %s: outside class root %s", it.Path, it.root)
	}
	if it.match != nil && !it.match(path) {
		return fmt.Errorf("refusing to delete %s: does not match class pattern", it.Path)
	}
	return os.RemoveAll(path)
}

// ClassReport is the per-class section of the /storage report.
type ClassReport struct {
	Name        string
	Description string
	Class       Class
	Size        int64
	Count       int    // entries (history dirs, sessions, files)
	Note        string // e.g. "3 orphaned"
	Items       []Item // deletable units; empty for user/secret classes
}

// Report is the full storage inventory.
type Report struct {
	Classes []ClassReport
	// Unknown lists top-level entries under ~/.spettro no registry entry
	// claims. They are reported so the user can investigate, never deleted.
	Unknown []string
	Total   int64
}

// TotalReclaimable sums the sizes of all preselected items.
func (r Report) TotalReclaimable() int64 {
	var total int64
	for _, c := range r.Classes {
		for _, it := range c.Items {
			if it.Preselected {
				total += it.Size
			}
		}
	}
	return total
}

// PreselectedItems returns the safe-default deletion plan across all classes.
func (r Report) PreselectedItems() []Item {
	var out []Item
	for _, c := range r.Classes {
		for _, it := range c.Items {
			if it.Preselected {
				out = append(out, it)
			}
		}
	}
	return out
}

// knownGlobalEntries are the top-level names under ~/.spettro claimed by
// user/secret classes (or covered by a reclaimable class below). Anything
// else found there is reported as unknown.
var knownGlobalEntries = map[string]Class{
	"history":               ClassHistory,
	"sessions":              ClassHistory,
	"catalog.json":          ClassCache,
	"memory-inbox.json":     ClassCache,
	"skills":                ClassUser,
	"memory.md":             ClassUser,
	"commands":              ClassUser,
	"config.json":           ClassSecret,
	"keys.enc":              ClassSecret,
	"master.key":            ClassSecret,
	"trusted.json":          ClassSecret,
	"telegram.json":         ClassSecret,
	"allowed_commands.json": ClassSecret,
}

// Inventory walks Spettro's global and project storage and builds the full
// report. opts shapes which items are preselected as safe defaults.
func Inventory(globalDir, projectDir string, opts CleanOptions) Report {
	opts = opts.withDefaults()
	var r Report

	r.Classes = append(r.Classes, historyReport(globalDir))
	r.Classes = append(r.Classes, sessionsReport(globalDir, opts))
	r.Classes = append(r.Classes, fileCacheReport(globalDir, "catalog", "models.dev catalog cache (re-fetched on demand)", "catalog.json", true))
	r.Classes = append(r.Classes, fileCacheReport(globalDir, "memory-inbox", "mined-fact candidates awaiting /memory review", "memory-inbox.json", false))
	r.Classes = append(r.Classes, projectCacheReport(projectDir))
	r.Classes = append(r.Classes, spoolReport())
	r.Classes = append(r.Classes, protectedReport(globalDir, projectDir))

	for i := range r.Classes {
		r.Total += r.Classes[i].Size
	}
	r.Unknown = unknownEntries(globalDir)
	return r
}

// historyReport inventories ~/.spettro/history/<hash>/ shadow repos. An entry
// is orphaned when its recorded project path no longer exists; entries written
// before project_path was recorded show as "unknown project" and are never
// preselected.
func historyReport(globalDir string) ClassReport {
	root := filepath.Join(globalDir, "history")
	cr := ClassReport{
		Name:        "history",
		Description: "checkpoint shadow repos + conversation blobs (/rewind)",
		Class:       ClassHistory,
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return cr
	}
	match := func(path string) bool { return filepath.Dir(path) == root }
	orphans := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		size := DirSize(dir)
		cr.Count++
		cr.Size += size
		project := historyProjectPath(dir)
		label := project
		preselect := false
		switch {
		case project == "":
			label = "unknown project (predates path recording)"
		case !dirExists(project):
			label = "orphan: " + project
			preselect = true
			orphans++
		}
		cr.Items = append(cr.Items, Item{
			ClassName:   "history",
			Label:       label,
			Path:        dir,
			Size:        size,
			Preselected: preselect,
			root:        root,
			match:       match,
		})
	}
	if orphans > 0 {
		cr.Note = fmt.Sprintf("%d orphaned", orphans)
	}
	sortItems(cr.Items)
	return cr
}

// historyProjectPath extracts the recorded project path from a history dir's
// checkpoints.json. Supports both the current object format
// ({"project_path": ..., "checkpoints": [...]}) and the legacy bare array
// (which carries no path → "").
func historyProjectPath(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "checkpoints.json"))
	if err != nil {
		return ""
	}
	var wrapped struct {
		ProjectPath string `json:"project_path"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return ""
	}
	return wrapped.ProjectPath
}

// sessionsReport inventories ~/.spettro/sessions/. Preselected: sessions older
// than SessionAgeDays that are neither the active session nor among the
// KeepSessions most recent of their project.
func sessionsReport(globalDir string, opts CleanOptions) ClassReport {
	root := filepath.Join(globalDir, "sessions")
	cr := ClassReport{
		Name:        "sessions",
		Description: "saved conversations, tasks and events (/resume)",
		Class:       ClassHistory,
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return cr
	}
	match := func(path string) bool {
		return filepath.Dir(path) == root && strings.HasPrefix(filepath.Base(path), "session-")
	}
	type sess struct {
		id      string
		dir     string
		size    int64
		project string
		updated time.Time
	}
	var sessions []sess
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "session-") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		s := sess{id: e.Name(), dir: dir, size: DirSize(dir)}
		var meta struct {
			ProjectPath string    `json:"project_path"`
			ProjectHash string    `json:"project_hash"`
			UpdatedAt   time.Time `json:"updated_at"`
		}
		if data, err := os.ReadFile(filepath.Join(dir, "session.json")); err == nil {
			_ = json.Unmarshal(data, &meta)
		}
		s.project = meta.ProjectHash
		if s.project == "" {
			s.project = meta.ProjectPath
		}
		s.updated = meta.UpdatedAt
		if s.updated.IsZero() {
			if st, err := os.Stat(dir); err == nil {
				s.updated = st.ModTime()
			}
		}
		sessions = append(sessions, s)
	}
	// Rank per project by recency so keep-K survives regardless of age.
	rank := map[string]int{}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].updated.After(sessions[j].updated) })
	horizon := time.Now().AddDate(0, 0, -opts.SessionAgeDays)
	for _, s := range sessions {
		cr.Count++
		cr.Size += s.size
		r := rank[s.project]
		rank[s.project] = r + 1
		preselect := s.id != opts.ActiveSessionID &&
			r >= opts.KeepSessions &&
			s.updated.Before(horizon)
		label := fmt.Sprintf("%s (updated %s)", s.id, s.updated.Format("2006-01-02"))
		if s.id == opts.ActiveSessionID {
			label += " — active"
		}
		cr.Items = append(cr.Items, Item{
			ClassName:   "sessions",
			Label:       label,
			Path:        s.dir,
			Size:        s.size,
			Preselected: preselect,
			root:        root,
			match:       match,
		})
	}
	// The active session must never be offered at all, not merely unchecked.
	if opts.ActiveSessionID != "" {
		kept := cr.Items[:0]
		for _, it := range cr.Items {
			if filepath.Base(it.Path) != opts.ActiveSessionID {
				kept = append(kept, it)
			}
		}
		cr.Items = kept
	}
	sortItems(cr.Items)
	return cr
}

// fileCacheReport handles single-file caches under ~/.spettro.
func fileCacheReport(globalDir, name, desc, filename string, preselect bool) ClassReport {
	cr := ClassReport{Name: name, Description: desc, Class: ClassCache}
	path := filepath.Join(globalDir, filename)
	st, err := os.Stat(path)
	if err != nil {
		return cr
	}
	cr.Count = 1
	cr.Size = st.Size()
	cr.Items = []Item{{
		ClassName:   name,
		Label:       filename,
		Path:        path,
		Size:        st.Size(),
		Preselected: preselect,
		root:        globalDir,
		match:       func(p string) bool { return filepath.Base(p) == filename },
	}}
	return cr
}

// projectCacheReport inventories <project>/.spettro/cache/ (symbol index and
// other regenerable caches, rebuilt lazily).
func projectCacheReport(projectDir string) ClassReport {
	root := filepath.Join(projectDir, "cache")
	cr := ClassReport{
		Name:        "project-cache",
		Description: "current project's regenerable caches (rebuilt lazily)",
		Class:       ClassCache,
	}
	if !dirExists(root) {
		return cr
	}
	size := DirSize(root)
	cr.Count = 1
	cr.Size = size
	cr.Items = []Item{{
		ClassName:   "project-cache",
		Label:       root,
		Path:        root,
		Size:        size,
		Preselected: true,
		root:        projectDir,
		match:       func(p string) bool { return filepath.Base(p) == "cache" },
	}}
	return cr
}

// spoolSessionActiveWindow guards live sessions' spool dirs: a spool dir is
// only considered dead when untouched for this long. Spools from the current
// process are excluded separately by the caller (LiveSpoolDir).
const spoolSessionActiveWindow = 48 * time.Hour

// LiveSpoolDir is set by the host process to its own spool directory so
// cleanup never touches the live session's spool. Empty means "no live spool".
var LiveSpoolDir string

// spoolRoot is where spool dirs live (the OS temp dir); a var so tests can
// point the scan at a sandbox instead of the real temp dir.
var spoolRoot = os.TempDir()

// spoolReport finds dead spool directories (spettro-spool-* under the OS temp
// dir) left behind by crashed sessions. Recently-touched dirs may belong to
// another running spettro process and are left alone.
func spoolReport() ClassReport {
	root := spoolRoot
	cr := ClassReport{
		Name:        "spool",
		Description: "oversized tool outputs from crashed sessions",
		Class:       ClassCache,
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return cr
	}
	match := func(path string) bool {
		return filepath.Dir(path) == root && strings.HasPrefix(filepath.Base(path), "spettro-spool-")
	}
	cutoff := time.Now().Add(-spoolSessionActiveWindow)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "spettro-spool-") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if dir == LiveSpoolDir {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		size := DirSize(dir)
		cr.Count++
		cr.Size += size
		cr.Items = append(cr.Items, Item{
			ClassName:   "spool",
			Label:       e.Name(),
			Path:        dir,
			Size:        size,
			Preselected: true,
			root:        root,
			match:       match,
		})
	}
	sortItems(cr.Items)
	return cr
}

// protectedReport sizes the user/secret classes so the report is complete, but
// never emits items: these classes are not deletable through cleanup at all.
func protectedReport(globalDir, projectDir string) ClassReport {
	cr := ClassReport{
		Name:        "protected",
		Description: "config, keys, memory, skills, custom commands (never cleaned)",
		Class:       ClassSecret,
	}
	for name, class := range knownGlobalEntries {
		if class != ClassUser && class != ClassSecret {
			continue
		}
		path := filepath.Join(globalDir, name)
		if st, err := os.Stat(path); err == nil {
			cr.Count++
			if st.IsDir() {
				cr.Size += DirSize(path)
			} else {
				cr.Size += st.Size()
			}
		}
	}
	for _, name := range []string{"memory.md", "commands", "allowed_commands.json"} {
		path := filepath.Join(projectDir, name)
		if st, err := os.Stat(path); err == nil {
			cr.Count++
			if st.IsDir() {
				cr.Size += DirSize(path)
			} else {
				cr.Size += st.Size()
			}
		}
	}
	return cr
}

// unknownEntries lists top-level names under ~/.spettro the registry does not
// claim. Reported for visibility; cleanup never touches them.
func unknownEntries(globalDir string) []string {
	entries, err := os.ReadDir(globalDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if _, ok := knownGlobalEntries[e.Name()]; ok {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// SelectableTotal sums the sizes of every deletable item (preselected or
// not) so the report can show what a full manual selection could reclaim.
func (r Report) SelectableTotal() (int64, int) {
	var total int64
	count := 0
	for _, c := range r.Classes {
		for _, it := range c.Items {
			total += it.Size
			count++
		}
	}
	return total, count
}

// RenderReport renders the storage report. The TUI (/storage) and the CLI
// (spettro clean) both print exactly this, so the two can never disagree
// about what is on disk or what the safe defaults would reclaim.
func RenderReport(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-14s %9s %6s  %s\n", "class", "size", "count", "description")
	for _, c := range r.Classes {
		note := c.Description
		if c.Note != "" {
			note += " — " + c.Note
		}
		fmt.Fprintf(&b, "%-14s %9s %6d  %s\n", c.Name, FormatBytes(c.Size), c.Count, note)
	}
	fmt.Fprintf(&b, "%-14s %9s\n", "total", FormatBytes(r.Total))
	for _, name := range r.Unknown {
		fmt.Fprintf(&b, "\nunknown entry (never deleted): ~/.spettro/%s", name)
	}
	safe := r.TotalReclaimable()
	all, count := r.SelectableTotal()
	if safe > 0 {
		fmt.Fprintf(&b, "\nsafe to reclaim with defaults: %s", FormatBytes(safe))
	} else {
		b.WriteString("\nnothing to reclaim with the safe defaults")
	}
	if other := all - safe; other > 0 {
		fmt.Fprintf(&b, "\nselectable beyond the defaults: %s across %d item(s) (recent sessions, non-orphan history)",
			FormatBytes(other), count-len(r.PreselectedItems()))
	}
	return b.String()
}

// Clean deletes the given items, returning the bytes freed and the first
// error encountered (deletion continues past individual failures).
func Clean(items []Item) (freed int64, firstErr error) {
	for _, it := range items {
		if err := it.Delete(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		freed += it.Size
	}
	return freed, firstErr
}

// DirSize returns the total size in bytes of all files under root. Shared by
// the storage report and the checkpoint package's /checkpoints sizes.
func DirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// sortItems orders items largest-first so the biggest wins surface on top.
func sortItems(items []Item) {
	sort.Slice(items, func(i, j int) bool { return items[i].Size > items[j].Size })
}

// FormatBytes renders a byte count for the report tables.
func FormatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
