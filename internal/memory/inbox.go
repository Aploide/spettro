package memory

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Candidate is a drafted memory entry awaiting user review. Nothing in the
// inbox is ever loaded into agent context — a candidate only becomes active
// memory when the user approves it (which appends it via Store.Save) and it
// disappears when discarded.
type Candidate struct {
	ID          string    `json:"id"`
	Fact        string    `json:"fact"`
	Scope       Scope     `json:"scope"`
	ProjectPath string    `json:"project_path,omitempty"`
	Sources     []string  `json:"sources,omitempty"` // session IDs the fact was mined from
	CreatedAt   time.Time `json:"created_at"`
}

// Inbox is the JSON file holding candidates pending review.
type Inbox struct {
	Path string
}

// DefaultInbox returns the per-user inbox (~/.spettro/memory-inbox.json).
// Candidates from every project share one inbox; project-scope candidates
// carry their ProjectPath.
func DefaultInbox() Inbox {
	home, err := os.UserHomeDir()
	if err != nil {
		return Inbox{}
	}
	return Inbox{Path: filepath.Join(home, ".spettro", "memory-inbox.json")}
}

// Load returns the pending candidates. A missing file is an empty inbox.
func (in Inbox) Load() ([]Candidate, error) {
	if in.Path == "" {
		return nil, fmt.Errorf("memory inbox: no path configured")
	}
	data, err := os.ReadFile(in.Path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory inbox: %w", err)
	}
	var out []Candidate
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("memory inbox: parse %s: %w", in.Path, err)
	}
	return out, nil
}

func (in Inbox) save(cands []Candidate) error {
	if in.Path == "" {
		return fmt.Errorf("memory inbox: no path configured")
	}
	if err := os.MkdirAll(filepath.Dir(in.Path), 0o700); err != nil {
		return fmt.Errorf("memory inbox: %w", err)
	}
	raw, err := json.MarshalIndent(cands, "", "  ")
	if err != nil {
		return fmt.Errorf("memory inbox: %w", err)
	}
	return os.WriteFile(in.Path, raw, 0o600)
}

// Add appends new candidates, skipping any whose normalized fact already
// exists in the inbox or in existingMemory (the current Store.Load content),
// so re-mining the same sessions never duplicates entries. Returns how many
// were actually added.
func (in Inbox) Add(cands []Candidate, existingMemory string) (int, error) {
	current, err := in.Load()
	if err != nil {
		return 0, err
	}
	seen := map[string]struct{}{}
	for _, c := range current {
		seen[normalizeFact(c.Fact)] = struct{}{}
	}
	for line := range strings.SplitSeq(existingMemory, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		if line != "" {
			seen[normalizeFact(line)] = struct{}{}
		}
	}
	added := 0
	for _, c := range cands {
		key := normalizeFact(c.Fact)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if c.ID == "" {
			c.ID = candidateID(c.Fact)
		}
		if c.CreatedAt.IsZero() {
			c.CreatedAt = time.Now()
		}
		current = append(current, c)
		added++
	}
	if added == 0 {
		return 0, nil
	}
	return added, in.save(current)
}

// Remove deletes one candidate by ID and returns it.
func (in Inbox) Remove(id string) (Candidate, bool, error) {
	current, err := in.Load()
	if err != nil {
		return Candidate{}, false, err
	}
	for i, c := range current {
		if c.ID == id {
			out := append(current[:i:i], current[i+1:]...)
			return c, true, in.save(out)
		}
	}
	return Candidate{}, false, nil
}

func normalizeFact(fact string) string {
	return strings.Join(strings.Fields(strings.ToLower(fact)), " ")
}

func candidateID(fact string) string {
	sum := sha256.Sum256([]byte(normalizeFact(fact)))
	return fmt.Sprintf("mem-%x", sum[:6])
}
