package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CurateOp is one edit the curation model proposes over the memory store.
// Nothing is applied without explicit user approval; each accepted op is
// applied as its own atomic file rewrite.
type CurateOp struct {
	Action string   `json:"action"`         // "delete" | "rewrite" | "merge"
	IDs    []string `json:"ids"`            // fact ids the op touches
	Text   string   `json:"text,omitempty"` // replacement text (rewrite/merge)
	Reason string   `json:"reason,omitempty"`
}

// staleAfterDays: curation proposes deleting facts unused this long that
// also reference things which no longer exist.
const staleAfterDays = 90

// StaleHints returns human-readable notes about project facts that look
// stale: unused for >staleAfterDays and referencing paths that no longer
// exist in the working tree at cwd. The hints are fed to the curation model
// as evidence; they never delete anything on their own.
func StaleHints(facts []Fact, cwd string) []string {
	if strings.TrimSpace(cwd) == "" {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -staleAfterDays).Format(dateLayout)
	var hints []string
	for _, f := range facts {
		last := recencyKey(f)
		if last == "" || last >= cutoff {
			continue
		}
		for _, tok := range strings.Fields(f.Text) {
			tok = strings.Trim(tok, "`'\",;:()")
			if !strings.Contains(tok, "/") && !strings.Contains(tok, ".") {
				continue
			}
			if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
				continue
			}
			p := tok
			if !filepath.IsAbs(p) {
				p = filepath.Join(cwd, tok)
			}
			if _, err := os.Stat(p); os.IsNotExist(err) {
				hints = append(hints, fmt.Sprintf("fact %s: unused since %s and %q does not exist in the working tree", f.ID, last, tok))
				break
			}
		}
	}
	return hints
}

// Curate runs one LLM pass over a scope's facts and returns proposed edit
// operations. It never touches the file — the caller presents the ops for
// review and applies approved ones via ApplyOp.
func Curate(ctx context.Context, facts []Fact, hints []string, complete CompleteFunc) ([]CurateOp, error) {
	if complete == nil {
		return nil, fmt.Errorf("memory curate: no completion function")
	}
	if len(facts) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString(`You curate a coding assistant's long-term memory: a list of short facts, each with an id and added/used dates. Propose edits that keep the list small, current, and consistent.

Propose an operation ONLY when clearly warranted:
- "merge": two or more facts say overlapping things — combine into one better fact.
- "rewrite": a fact is vague, bloated, or outdated — replace its text.
- "delete": a fact is stale, contradicted by a newer fact, or references things that no longer exist.

Do not propose changes to facts that are fine as-is. Prefer keeping the newer/more specific fact when two contradict.

Output a JSON array (and nothing else):
[{"action": "delete"|"rewrite"|"merge", "ids": ["m-..."], "text": "<new text for rewrite/merge, else omit>", "reason": "<short why>"}]
Output [] if nothing needs changing.

Facts:
`)
	for i := range facts {
		f := facts[i]
		f.stamp()
		sb.WriteString(fmt.Sprintf("- id:%s added:%s used:%s | %s\n", f.ID, f.Added, f.Used, f.Text))
	}
	if len(hints) > 0 {
		sb.WriteString("\nStaleness evidence (verified against the working tree):\n")
		for _, h := range hints {
			sb.WriteString("- " + h + "\n")
		}
	}
	raw, err := complete(ctx, sb.String())
	if err != nil {
		return nil, fmt.Errorf("memory curate: %w", err)
	}
	start := strings.Index(raw, "[")
	end := strings.LastIndex(raw, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("memory curate: no JSON array in model output: %s", truncateForErr(raw))
	}
	var ops []CurateOp
	if err := json.Unmarshal([]byte(raw[start:end+1]), &ops); err != nil {
		return nil, fmt.Errorf("memory curate: parse model output: %w", err)
	}
	known := map[string]struct{}{}
	for i := range facts {
		f := facts[i]
		f.stamp()
		known[f.ID] = struct{}{}
	}
	out := ops[:0]
	for _, op := range ops {
		op.Action = strings.ToLower(strings.TrimSpace(op.Action))
		valid := len(op.IDs) > 0
		for _, id := range op.IDs {
			if _, ok := known[id]; !ok {
				valid = false
			}
		}
		switch op.Action {
		case "delete":
		case "rewrite", "merge":
			t := strings.Join(strings.Fields(op.Text), " ")
			if t == "" || len(t) > maxFactLen {
				valid = false
			}
			op.Text = t
		default:
			valid = false
		}
		if valid {
			out = append(out, op)
		}
	}
	return out, nil
}

// ApplyOp applies one approved curation op to the scope's memory file as a
// single atomic rewrite (temp+rename). Facts get stamped with metadata as a
// side effect. A rejected op is simply never passed here, leaving the file
// untouched.
func (s Store) ApplyOp(scope Scope, op CurateOp) error {
	facts := s.readFacts(scope)
	for i := range facts {
		facts[i].stamp()
	}
	targets := map[string]struct{}{}
	for _, id := range op.IDs {
		targets[id] = struct{}{}
	}
	switch strings.ToLower(op.Action) {
	case "delete":
		kept := facts[:0]
		for _, f := range facts {
			if _, hit := targets[f.ID]; !hit {
				kept = append(kept, f)
			}
		}
		return s.writeFacts(scope, kept)
	case "rewrite", "merge":
		text := strings.Join(strings.Fields(op.Text), " ")
		if text == "" || len(text) > maxFactLen {
			return fmt.Errorf("memory curate: invalid replacement text")
		}
		kept := facts[:0]
		added := ""
		for _, f := range facts {
			if _, hit := targets[f.ID]; hit {
				// The merged fact keeps the earliest added: date of its parts.
				if added == "" || (f.Added != "" && f.Added < added) {
					added = f.Added
				}
				continue
			}
			kept = append(kept, f)
		}
		nf := Fact{Text: text, Added: added, Used: today()}
		nf.stamp()
		return s.writeFacts(scope, append(kept, nf))
	default:
		return fmt.Errorf("memory curate: unknown action %q", op.Action)
	}
}
