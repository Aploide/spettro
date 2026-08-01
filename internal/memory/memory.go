// Package memory implements persistent cross-session agent memory: short
// user-approved facts appended to a per-user file (~/.spettro/memory.md) and
// an optional per-project file (<root>/.spettro/memory.md). The combined
// content is injected into the agent system context once per process so the
// prompt-cache prefix stays byte-stable for the whole session; facts saved
// mid-session become visible at the next session start.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Scope selects which memory file an operation targets.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

const (
	userHeader    = "# Spettro memory (user)\n"
	projectHeader = "# Spettro memory (project)\n"
	// maxFileBytes caps how much of each memory file is loaded into context.
	// Facts are injected recently-used-first, so when the cap hits, the
	// stalest facts are the ones dropped.
	maxFileBytes = 8 * 1024
	// maxFactLen caps a single saved fact.
	maxFactLen = 500
)

// Store holds the resolved paths of the two memory files. Zero-value fields
// mean "no file for that scope". When Inbox is set, Save routes near-duplicate
// facts there as supersede candidates instead of appending blindly.
type Store struct {
	UserFile    string
	ProjectFile string
	Inbox       *Inbox
}

// DefaultStore returns the store for the standard locations: the per-user file
// under the home directory and the per-project file under <cwd>/.spettro.
func DefaultStore(cwd string) Store {
	s := Store{}
	if home, err := os.UserHomeDir(); err == nil {
		s.UserFile = filepath.Join(home, ".spettro", "memory.md")
	}
	if strings.TrimSpace(cwd) != "" {
		s.ProjectFile = filepath.Join(cwd, ".spettro", "memory.md")
	}
	if in := DefaultInbox(); in.Path != "" {
		s.Inbox = &in
	}
	return s
}

// Path returns the file backing the given scope ("" when unavailable).
func (s Store) Path(scope Scope) string {
	if scope == ScopeProject {
		return s.ProjectFile
	}
	return s.UserFile
}

// SaveOutcome reports what Save did with a fact.
type SaveOutcome int

const (
	// SavedNew: the fact was appended to the memory file.
	SavedNew SaveOutcome = iota
	// SavedDuplicate: an exact (normalized) match already existed; its
	// used: date was bumped instead of appending a duplicate.
	SavedDuplicate
	// SavedToInbox: a near-duplicate or likely contradiction exists; the
	// new fact was routed to the review inbox as a supersede candidate so
	// the user decides which version wins (/memory review).
	SavedToInbox
)

// SaveResult describes where a saved fact ended up.
type SaveResult struct {
	Path    string
	Outcome SaveOutcome
	// Near is the existing fact the new one collided with (set for
	// SavedDuplicate and SavedToInbox).
	Near string
}

// Save persists one fact into the scope's memory file. Exact duplicates bump
// the existing fact's used: date; near-duplicates/contradictions are routed
// to the review inbox (when one is configured) instead of piling up; new
// facts are appended with id/added/used metadata. Rewrites are atomic.
func (s Store) Save(scope Scope, fact string) (SaveResult, error) {
	return s.save(scope, fact, true)
}

// SaveApproved is Save without inbox routing, for facts the user already
// reviewed (inbox approval, supersede resolution): near-duplicates append
// anyway rather than bouncing back into the inbox.
func (s Store) SaveApproved(scope Scope, fact string) (SaveResult, error) {
	return s.save(scope, fact, false)
}

func (s Store) save(scope Scope, fact string, routeNearDupes bool) (SaveResult, error) {
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return SaveResult{}, fmt.Errorf("save-memory: empty fact")
	}
	if len(fact) > maxFactLen {
		return SaveResult{}, fmt.Errorf("save-memory: fact too long (%d chars, max %d) — keep memories short", len(fact), maxFactLen)
	}
	if strings.ContainsAny(fact, "\n\r") {
		return SaveResult{}, fmt.Errorf("save-memory: fact must be a single line")
	}
	path := s.Path(scope)
	if path == "" {
		return SaveResult{}, fmt.Errorf("save-memory: no %s memory file available", scope)
	}
	facts := s.readFacts(scope)
	norm := normalizeFact(fact)
	for i := range facts {
		if normalizeFact(facts[i].Text) == norm {
			facts[i].Used = today()
			if err := s.writeFacts(scope, facts); err != nil {
				return SaveResult{}, err
			}
			return SaveResult{Path: path, Outcome: SavedDuplicate, Near: facts[i].Text}, nil
		}
	}
	if routeNearDupes && s.Inbox != nil {
		for _, existing := range facts {
			if nearDuplicate(existing.Text, fact) {
				cand := Candidate{
					Fact:       fact,
					Scope:      scope,
					Supersedes: existing.Text,
				}
				if scope == ScopeProject {
					cand.ProjectPath = filepath.Dir(filepath.Dir(path))
				}
				if _, err := s.Inbox.Add([]Candidate{cand}, ""); err != nil {
					return SaveResult{}, err
				}
				return SaveResult{Path: s.Inbox.Path, Outcome: SavedToInbox, Near: existing.Text}, nil
			}
		}
	}
	nf := Fact{Text: fact}
	nf.stamp()
	if err := s.writeFacts(scope, append(facts, nf)); err != nil {
		return SaveResult{}, err
	}
	return SaveResult{Path: path, Outcome: SavedNew}, nil
}

// Supersede replaces oldText (matched on normalized form, if still present)
// with newText in one atomic rewrite. Used when the user approves a
// supersede candidate from the review inbox.
func (s Store) Supersede(scope Scope, oldText, newText string) (SaveResult, error) {
	facts := s.readFacts(scope)
	norm := normalizeFact(oldText)
	kept := facts[:0]
	for _, f := range facts {
		if normalizeFact(f.Text) != norm {
			kept = append(kept, f)
		}
	}
	sCopy := s
	sCopy.Inbox = nil // never bounce an approved replacement back to the inbox
	if err := sCopy.writeFacts(scope, kept); err != nil {
		return SaveResult{}, err
	}
	return sCopy.save(scope, newText, false)
}

// Clear truncates the scope's memory file. Missing files are not an error.
func (s Store) Clear(scope Scope) error {
	path := s.Path(scope)
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return os.WriteFile(path, nil, 0o600)
}

// renderCapped renders a scope's facts as metadata-stripped bullets,
// recently-used first, dropping whatever exceeds maxFileBytes — so the cap
// cuts the stalest facts, never the freshest.
func renderCapped(facts []Fact) string {
	var sb strings.Builder
	for _, f := range orderByRecency(facts) {
		line := "- " + f.Text + "\n"
		if sb.Len()+len(line) > maxFileBytes {
			break
		}
		sb.WriteString(line)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// Load renders the combined memory as a system-context section: project
// facts before user facts, recently-used first within each scope, metadata
// stripped. Returns "" when no memory is saved.
func (s Store) Load() string {
	project := renderCapped(s.readFacts(ScopeProject))
	user := renderCapped(s.readFacts(ScopeUser))
	if user == "" && project == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n# Memory\nFacts and preferences saved in earlier sessions. Honor them unless the user says otherwise.\n")
	if project != "" {
		sb.WriteString("\n")
		sb.WriteString(project)
		sb.WriteString("\n")
	}
	if user != "" {
		sb.WriteString("\n")
		sb.WriteString(user)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

var (
	sessionMu    sync.Mutex
	sessionCache = map[string]string{}
)

// SessionContext returns the memory section for cwd, loaded once per process
// and then frozen. Freezing matters: the system prompt must stay byte-stable
// across every turn of a session or the provider prompt cache misses, so facts
// saved mid-session only appear in the next session.
func SessionContext(cwd string) string {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	if v, ok := sessionCache[cwd]; ok {
		return v
	}
	v := DefaultStore(cwd).Load()
	sessionCache[cwd] = v
	return v
}

// ResetSessionCacheForTesting clears the per-process session snapshot.
func ResetSessionCacheForTesting() {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	sessionCache = map[string]string{}
}
