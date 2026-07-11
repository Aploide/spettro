package provider

import "sync"

// Usage is the token accounting one provider request reported. Cache fields
// follow Anthropic semantics: InputTokens EXCLUDES cached tokens, CacheRead
// are prefix tokens served from the prompt cache, CacheWrite are prefix
// tokens written to it. OpenAI-style "cached_tokens" (a subset of prompt
// tokens) is normalized into the same shape by the adapter.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// TotalInput is everything the model actually consumed as prompt: fresh
// input plus cached reads and cache writes.
func (u Usage) TotalInput() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// CacheHitRate is the fraction of the prompt served from cache, or -1 when
// the request reported no input at all (usage unavailable).
func (u Usage) CacheHitRate() float64 {
	total := u.TotalInput()
	if total == 0 {
		return -1
	}
	return float64(u.CacheReadTokens) / float64(total)
}

func (u *Usage) add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
}

// UsageTotals accumulates Usage over many requests.
type UsageTotals struct {
	Usage
	Requests int `json:"requests"`
}

// SessionUsage is the per-session accounting snapshot: overall totals, a
// per-model breakdown, and the last request (feeding the live cache
// indicator). It serializes with the session so /resume restores counters.
type SessionUsage struct {
	Totals  UsageTotals            `json:"totals"`
	ByModel map[string]UsageTotals `json:"by_model,omitempty"`
	Last    Usage                  `json:"last"`
}

func (s SessionUsage) clone() SessionUsage {
	out := s
	if s.ByModel != nil {
		out.ByModel = make(map[string]UsageTotals, len(s.ByModel))
		for k, v := range s.ByModel {
			out.ByModel[k] = v
		}
	}
	return out
}

// usageRecorder is the Manager-embedded accumulator. Every successful
// Manager.Send lands here, so sub-agent and compaction traffic is counted
// alongside the main loop.
type usageRecorder struct {
	mu    sync.Mutex
	usage SessionUsage
}

func (r *usageRecorder) record(providerName, modelName string, u Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usage.Totals.add(u)
	r.usage.Totals.Requests++
	if r.usage.ByModel == nil {
		r.usage.ByModel = map[string]UsageTotals{}
	}
	key := providerName + ":" + modelName
	t := r.usage.ByModel[key]
	t.add(u)
	t.Requests++
	r.usage.ByModel[key] = t
	r.usage.Last = u
}

// UsageSnapshot returns a copy of the session's accumulated usage.
func (m *Manager) UsageSnapshot() SessionUsage {
	m.usageRec.mu.Lock()
	defer m.usageRec.mu.Unlock()
	return m.usageRec.usage.clone()
}

// ResetUsage zeroes the accumulated counters (e.g. on /clear).
func (m *Manager) ResetUsage() {
	m.usageRec.mu.Lock()
	m.usageRec.usage = SessionUsage{}
	m.usageRec.mu.Unlock()
}

// RestoreUsage replaces the counters with a previously saved snapshot so a
// resumed session continues from where it was persisted.
func (m *Manager) RestoreUsage(s SessionUsage) {
	m.usageRec.mu.Lock()
	m.usageRec.usage = s.clone()
	m.usageRec.mu.Unlock()
}
