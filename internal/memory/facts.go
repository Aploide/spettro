package memory

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Fact is one memory bullet. Metadata rides in an HTML-comment tail that is
// invisible in rendered Markdown and stripped before prompt injection:
//
//   - prefers table-driven tests <!-- id:m-a1b2c3 added:2026-07-23 used:2026-07-23 -->
//
// Legacy bare bullets are valid facts with empty metadata; they get stamped
// on the first pass that rewrites the file (Save dedupe or /memory curate).
type Fact struct {
	Text  string
	ID    string
	Added string // YYYY-MM-DD, "" = unknown (legacy)
	Used  string // YYYY-MM-DD, "" = never bumped
}

const dateLayout = "2006-01-02"

var metaTailRe = regexp.MustCompile(`\s*<!--\s*([^>]*?)\s*-->\s*$`)

// today is swappable for tests.
var today = func() string { return time.Now().Format(dateLayout) }

// factID derives a stable short id from the normalized fact text.
func factID(text string) string {
	sum := sha256.Sum256([]byte(normalizeFact(text)))
	return fmt.Sprintf("m-%x", sum[:3])
}

// stamp fills any missing metadata fields in place.
func (f *Fact) stamp() {
	if f.ID == "" {
		f.ID = factID(f.Text)
	}
	if f.Added == "" {
		f.Added = today()
	}
	if f.Used == "" {
		f.Used = f.Added
	}
}

// render returns the bullet line including the metadata tail.
func (f Fact) render() string {
	meta := []string{}
	if f.ID != "" {
		meta = append(meta, "id:"+f.ID)
	}
	if f.Added != "" {
		meta = append(meta, "added:"+f.Added)
	}
	if f.Used != "" {
		meta = append(meta, "used:"+f.Used)
	}
	if len(meta) == 0 {
		return "- " + f.Text
	}
	return "- " + f.Text + " <!-- " + strings.Join(meta, " ") + " -->"
}

// parseFactLine parses one bullet (or bare) line into a Fact. Returns false
// for blank lines and Markdown headers. Malformed metadata comments are
// treated as part of no metadata: the comment is dropped, the text kept.
func parseFactLine(line string) (Fact, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return Fact{}, false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "-"))
	if line == "" {
		return Fact{}, false
	}
	f := Fact{}
	if m := metaTailRe.FindStringSubmatch(line); m != nil {
		for field := range strings.FieldsSeq(m[1]) {
			switch {
			case strings.HasPrefix(field, "id:"):
				f.ID = strings.TrimPrefix(field, "id:")
			case strings.HasPrefix(field, "added:"):
				if _, err := time.Parse(dateLayout, strings.TrimPrefix(field, "added:")); err == nil {
					f.Added = strings.TrimPrefix(field, "added:")
				}
			case strings.HasPrefix(field, "used:"):
				if _, err := time.Parse(dateLayout, strings.TrimPrefix(field, "used:")); err == nil {
					f.Used = strings.TrimPrefix(field, "used:")
				}
			}
		}
		line = strings.TrimSpace(line[:len(line)-len(m[0])])
	}
	if line == "" {
		return Fact{}, false
	}
	f.Text = line
	return f, true
}

// parseFacts extracts all facts from a memory file's content, in file order.
func parseFacts(content string) []Fact {
	var out []Fact
	for line := range strings.SplitSeq(content, "\n") {
		if f, ok := parseFactLine(line); ok {
			out = append(out, f)
		}
	}
	return out
}

// readFacts loads and parses the scope's memory file ("" path or missing
// file → nil).
func (s Store) readFacts(scope Scope) []Fact {
	path := s.Path(scope)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseFacts(string(data))
}

// Facts returns the parsed facts of a scope, in file order.
func (s Store) Facts(scope Scope) []Fact {
	return s.readFacts(scope)
}

// writeFacts rewrites the scope's memory file atomically (temp+rename) with
// the header and the given facts, stamping any missing metadata.
func (s Store) writeFacts(scope Scope, facts []Fact) error {
	path := s.Path(scope)
	if path == "" {
		return fmt.Errorf("memory: no %s memory file available", scope)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	header := userHeader
	if scope == ScopeProject {
		header = projectHeader
	}
	var sb strings.Builder
	sb.WriteString(header)
	sb.WriteString("\n")
	for i := range facts {
		facts[i].stamp()
		sb.WriteString(facts[i].render())
		sb.WriteString("\n")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".memory-*.md")
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(sb.String()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("memory: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("memory: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("memory: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("memory: %w", err)
	}
	return nil
}

// recencyKey orders facts for injection: recently-used first, then recently
// added; unstamped legacy facts sort last.
func recencyKey(f Fact) string {
	if f.Used != "" {
		return f.Used
	}
	return f.Added
}

// orderByRecency returns facts sorted most-recently-used first. Ties keep
// reverse file order (later in an append-only file = saved more recently).
func orderByRecency(facts []Fact) []Fact {
	out := make([]Fact, len(facts))
	for i, f := range facts {
		out[len(facts)-1-i] = f
	}
	sort.SliceStable(out, func(i, j int) bool {
		return recencyKey(out[i]) > recencyKey(out[j])
	})
	return out
}

// tokenOverlap returns the Jaccard similarity of the two facts' normalized
// token sets.
func tokenOverlap(a, b string) float64 {
	as := strings.Fields(normalizeFact(a))
	bs := strings.Fields(normalizeFact(b))
	if len(as) == 0 || len(bs) == 0 {
		return 0
	}
	set := map[string]struct{}{}
	for _, t := range as {
		set[t] = struct{}{}
	}
	inter := 0
	bset := map[string]struct{}{}
	for _, t := range bs {
		if _, dup := bset[t]; dup {
			continue
		}
		bset[t] = struct{}{}
		if _, ok := set[t]; ok {
			inter++
		}
	}
	union := len(set) + len(bset) - inter
	return float64(inter) / float64(union)
}

// nearDuplicate reports whether two facts are close enough that saving both
// would likely duplicate or contradict: high token overlap, or the same
// leading phrase (first three tokens) suggesting the same subject with a
// different tail ("prefers tabs" vs "prefers spaces").
func nearDuplicate(a, b string) bool {
	if tokenOverlap(a, b) >= 0.8 {
		return true
	}
	as := strings.Fields(normalizeFact(a))
	bs := strings.Fields(normalizeFact(b))
	if len(as) >= 3 && len(bs) >= 3 {
		return as[0] == bs[0] && as[1] == bs[1] && as[2] == bs[2]
	}
	return false
}
