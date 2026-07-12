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
	// Memory is meant to stay small; oversized files are tail-trimmed on load
	// (newest entries win — files are append-only).
	maxFileBytes = 8 * 1024
	// maxFactLen caps a single saved fact.
	maxFactLen = 500
)

// Store holds the resolved paths of the two memory files. Zero-value fields
// mean "no file for that scope".
type Store struct {
	UserFile    string
	ProjectFile string
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
	return s
}

// Path returns the file backing the given scope ("" when unavailable).
func (s Store) Path(scope Scope) string {
	if scope == ScopeProject {
		return s.ProjectFile
	}
	return s.UserFile
}

// Save appends one fact as a bullet line to the scope's memory file, creating
// the file (with a header) and parent directory as needed. The file is
// append-only: existing lines are never rewritten or reordered, so earlier
// content stays stable.
func (s Store) Save(scope Scope, fact string) (string, error) {
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return "", fmt.Errorf("save-memory: empty fact")
	}
	if len(fact) > maxFactLen {
		return "", fmt.Errorf("save-memory: fact too long (%d chars, max %d) — keep memories short", len(fact), maxFactLen)
	}
	if strings.ContainsAny(fact, "\n\r") {
		return "", fmt.Errorf("save-memory: fact must be a single line")
	}
	path := s.Path(scope)
	if path == "" {
		return "", fmt.Errorf("save-memory: no %s memory file available", scope)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("save-memory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("save-memory: %w", err)
	}
	defer f.Close()
	entry := "- " + fact + "\n"
	if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
		header := userHeader
		if scope == ScopeProject {
			header = projectHeader
		}
		entry = header + "\n" + entry
	}
	if _, err := f.WriteString(entry); err != nil {
		return "", fmt.Errorf("save-memory: %w", err)
	}
	return path, nil
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

// readCapped returns the file content, tail-trimmed to maxFileBytes on a line
// boundary (append-only files keep the newest facts at the end).
func readCapped(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	if len(content) > maxFileBytes {
		content = content[len(content)-maxFileBytes:]
		if i := strings.IndexByte(content, '\n'); i >= 0 {
			content = content[i+1:]
		}
	}
	return content
}

// Load renders the combined memory as a system-context section. Returns ""
// when no memory is saved.
func (s Store) Load() string {
	user := readCapped(s.UserFile)
	project := readCapped(s.ProjectFile)
	if user == "" && project == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n# Memory\nFacts and preferences saved in earlier sessions. Honor them unless the user says otherwise.\n")
	if user != "" {
		sb.WriteString("\n")
		sb.WriteString(user)
		sb.WriteString("\n")
	}
	if project != "" {
		sb.WriteString("\n")
		sb.WriteString(project)
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
