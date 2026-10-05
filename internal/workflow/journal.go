package workflow

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Journal records every agent call a run makes, so a workflow that was killed
// (cancelled, crashed, or edited mid-flight) can be relaunched without paying
// for the agents that already answered.
//
// Entries are keyed by a hash of the call's prompt and options rather than by
// call order: parallel() and pipeline() interleave, so ordinal position is not
// reproducible, while the (prompt, opts) pair of a given call is. Identical
// calls are replayed first-come-first-served from a per-key queue, which makes
// a fan-out of N identical prompts resume correctly too.
type Journal struct {
	dir string

	mu     sync.Mutex
	file   *os.File
	cached map[string][]JournalEntry
	hits   int
}

// Journal entry kinds. An entry written before kinds existed has none and is
// an agent entry: agent calls were all a journal ever recorded.
const (
	JournalKindAgent      = "agent"
	JournalKindCheckpoint = "checkpoint"
)

// JournalEntry is one recorded agent call or answered checkpoint.
//
// For a checkpoint entry, Index is the checkpoint's ordinal, Instance its id
// ("cp-2"), Label the script's message, and Output the orchestrator's reply as
// JSON.
type JournalEntry struct {
	Kind      string `json:"kind,omitempty"`
	Key       string `json:"key"`
	Index     int    `json:"index"`
	Label     string `json:"label,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Instance  string `json:"instance,omitempty"`
	AgentType string `json:"agent_type,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	Tokens    int    `json:"tokens,omitempty"`
}

// OpenJournal creates the run directory and opens its journal for appending.
// A nil Journal is valid everywhere and simply disables persistence.
func OpenJournal(dir string) (*Journal, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create workflow run dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open workflow journal: %w", err)
	}
	return &Journal{dir: dir, file: f}, nil
}

// Dir is the run directory holding script.js, meta.json and journal.jsonl.
func (j *Journal) Dir() string {
	if j == nil {
		return ""
	}
	return j.dir
}

// LoadCache reads a previous run's journal into this one, so matching calls
// resolve from it instead of executing. Missing files are not an error: a run
// id that no longer exists just means nothing is cached.
func (j *Journal) LoadCache(dir string) error {
	if j == nil {
		return nil
	}
	f, err := os.Open(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read workflow journal: %w", err)
	}
	defer f.Close()
	cached := map[string][]JournalEntry{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e JournalEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.Key == "" {
			continue
		}
		// Failures are not cached: the point of resuming is to retry them.
		if e.Error != "" {
			continue
		}
		cached[e.Key] = append(cached[e.Key], e)
	}
	j.mu.Lock()
	j.cached = cached
	j.mu.Unlock()
	return sc.Err()
}

func (e JournalEntry) kind() string {
	if e.Kind == "" {
		return JournalKindAgent
	}
	return e.Kind
}

// Take consumes a cached agent result for the given key, if one is left.
func (j *Journal) Take(key string) (JournalEntry, bool) {
	return j.take(key, JournalKindAgent)
}

// TakeCheckpoint consumes a cached checkpoint answer for the given key, if one
// is left. Unlike Take it does not count toward Hits, which reports replayed
// agents — the number a user reads as "work I did not pay for again".
func (j *Journal) TakeCheckpoint(key string) (JournalEntry, bool) {
	return j.take(key, JournalKindCheckpoint)
}

func (j *Journal) take(key, kind string) (JournalEntry, bool) {
	if j == nil {
		return JournalEntry{}, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	queue := j.cached[key]
	// The two kinds hash disjoint inputs, so a key never holds both; matching
	// the kind anyway keeps a corrupted or hand-edited journal from feeding an
	// orchestrator's reply to an agent() call as its answer.
	for i, e := range queue {
		if e.kind() != kind {
			continue
		}
		j.cached[key] = append(queue[:i:i], queue[i+1:]...)
		if kind == JournalKindAgent {
			j.hits++
		}
		return e, true
	}
	return JournalEntry{}, false
}

// Hits is how many agent calls this run replayed from a previous journal.
func (j *Journal) Hits() int {
	if j == nil {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.hits
}

// Append records a completed call. Write errors are returned but callers
// treat them as non-fatal: losing resumability must not fail a run that is
// otherwise succeeding.
func (j *Journal) Append(e JournalEntry) error {
	if j == nil || j.file == nil {
		return nil
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	_, err = j.file.Write(append(line, '\n'))
	return err
}

// WriteFile stores a companion artifact (script.js, meta.json, result.json)
// next to the journal.
func (j *Journal) WriteFile(name, content string) error {
	if j == nil {
		return nil
	}
	return os.WriteFile(filepath.Join(j.dir, name), []byte(content), 0o644)
}

// Close flushes and closes the journal file.
func (j *Journal) Close() error {
	if j == nil || j.file == nil {
		return nil
	}
	return j.file.Close()
}

// callKey hashes everything that determines a call's result. Two calls with
// the same key are interchangeable for resume purposes; changing a prompt,
// schema, model or agent type invalidates the cache entry, which is exactly
// the "longest unchanged prefix" behaviour an edited script needs.
func callKey(req Request) string {
	h := sha256.New()
	for _, part := range []string{
		req.Prompt, req.AgentType, req.Model, req.Effort, req.Isolation, string(req.Schema),
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// checkpointKey hashes what identifies a checkpoint for replay: its message
// and the canonical JSON of its data. Identical checkpoints (a loop asking the
// same question) replay first-come-first-served like identical agent calls.
// The leading kind tag keeps the key space disjoint from callKey's.
func checkpointKey(message, dataJSON string) string {
	h := sha256.New()
	for _, part := range []string{JournalKindCheckpoint, message, dataJSON} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
