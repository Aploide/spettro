package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Transcript is one saved session rendered as plain text for mining.
type Transcript struct {
	SessionID string
	Text      string
}

// CompleteFunc is a single-shot LLM completion. The miner is deliberately
// decoupled from the provider package so it can be tested with a fake.
type CompleteFunc func(ctx context.Context, prompt string) (string, error)

// maxTranscriptChars caps how much of each transcript is fed to the miner —
// preferences show up in user turns, which are short; full tool output is
// noise and cost.
const maxTranscriptChars = 6000

// Mine scans saved session transcripts for recurring durable facts,
// preferences, and workflows, and returns them as candidate memory entries.
// Candidates are drafts only: the caller puts them in the review inbox and
// nothing becomes active memory without explicit user approval.
//
// existingMemory (the current Store.Load content) is shown to the model so it
// does not re-propose what is already saved; Inbox.Add deduplicates again as
// a backstop.
func Mine(ctx context.Context, transcripts []Transcript, existingMemory string, complete CompleteFunc) ([]Candidate, error) {
	if complete == nil {
		return nil, fmt.Errorf("memory miner: no completion function")
	}
	if len(transcripts) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString(`You review transcripts of a user's past coding-assistant sessions and extract facts worth remembering across future sessions.

Extract ONLY durable, recurring signals:
- stable user preferences (style, language, tone, workflow habits)
- project conventions the user stated or enforced (build/test commands, layout, naming)
- corrections the user made more than once

Do NOT extract: one-off task details, file contents, secrets or tokens, anything session-specific, or anything already covered by the "Already saved" list below.

Output a JSON array (and nothing else) of at most 8 entries:
[{"fact": "<one short sentence, max 300 chars>", "scope": "user"|"project"}]
Use scope "project" for facts tied to this repository, "user" for facts that apply everywhere. Output [] if nothing qualifies.
`)
	if strings.TrimSpace(existingMemory) != "" {
		sb.WriteString("\nAlready saved (do not repeat):\n")
		sb.WriteString(existingMemory)
		sb.WriteString("\n")
	}
	ids := make([]string, 0, len(transcripts))
	for _, tr := range transcripts {
		text := strings.TrimSpace(tr.Text)
		if text == "" {
			continue
		}
		if len(text) > maxTranscriptChars {
			text = text[:maxTranscriptChars] + "\n[truncated]"
		}
		ids = append(ids, tr.SessionID)
		sb.WriteString(fmt.Sprintf("\n--- session %s ---\n%s\n", tr.SessionID, text))
	}
	if len(ids) == 0 {
		return nil, nil
	}
	raw, err := complete(ctx, sb.String())
	if err != nil {
		return nil, fmt.Errorf("memory miner: %w", err)
	}
	facts, err := parseMinedFacts(raw)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(facts))
	for _, f := range facts {
		fact := strings.Join(strings.Fields(f.Fact), " ")
		if fact == "" || len(fact) > maxFactLen {
			continue
		}
		scope := ScopeUser
		if strings.EqualFold(strings.TrimSpace(f.Scope), string(ScopeProject)) {
			scope = ScopeProject
		}
		out = append(out, Candidate{
			ID:      candidateID(fact),
			Fact:    fact,
			Scope:   scope,
			Sources: ids,
		})
	}
	return out, nil
}

type minedFact struct {
	Fact  string `json:"fact"`
	Scope string `json:"scope"`
}

// parseMinedFacts extracts the first JSON array from the model output,
// tolerating code fences and surrounding prose.
func parseMinedFacts(raw string) ([]minedFact, error) {
	start := strings.Index(raw, "[")
	end := strings.LastIndex(raw, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("memory miner: no JSON array in model output: %s", truncateForErr(raw))
	}
	var facts []minedFact
	if err := json.Unmarshal([]byte(raw[start:end+1]), &facts); err != nil {
		return nil, fmt.Errorf("memory miner: parse model output: %w", err)
	}
	return facts, nil
}

func truncateForErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
