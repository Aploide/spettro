package compact

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"spettro/internal/provider"
)

// SendFunc issues one summarization request. Callers decide the routing
// (internal utility model, fallback chain, plain active model) so this
// package stays free of provider-selection policy.
type SendFunc func(ctx context.Context, req provider.Request) (provider.Response, error)

// EstimateHistoryTokens approximates the request tokens a conversation will
// occupy, counting message content plus tool calls and tool results — the
// same accounting the in-loop compaction uses, so pre-turn checks and in-loop
// checks agree.
func EstimateHistoryTokens(system string, msgs []provider.Message) int {
	return provider.EstimateRequestTokens(provider.Request{System: system, Messages: msgs})
}

// MeasureFunc returns the prompt tokens a request with this system prompt and
// history would occupy. The agent loop passes one that adds the tool schemas
// and is calibrated against the provider-reported prompt size of the previous
// step; nil means EstimateHistoryTokens (chars/4 over the history alone).
type MeasureFunc func(system string, msgs []provider.Message) int

// Params configures one compaction pass. Zero values select the defaults.
type Params struct {
	// System is the system prompt the history is sent with (it counts toward
	// the measured size).
	System string
	// Window is the model's context window in tokens (0 → 128k).
	Window int
	// Force compacts regardless of the trigger and always summarizes: an
	// explicit /compact, or recovery from an over-budget or overflowing
	// context.
	Force bool
	// Policy is the auto-compaction policy; the zero value means auto
	// compaction on at the default threshold.
	Policy Config
	// Failures is the caller's consecutive summarizer-failure count (the
	// automatic trigger pauses once it reaches Policy.MaxFailures).
	Failures int
	// Measure sizes a would-be request (nil → EstimateHistoryTokens).
	Measure MeasureFunc
	// KeepToolExchanges is how many of the most recent tool exchanges (an
	// assistant tool-call turn and its results turn) stay verbatim
	// (0 → 3, or 2 when forced).
	KeepToolExchanges int
	// KeepUserMessages is how many of the latest user messages from the
	// summarized span are carried over verbatim (0 → 3).
	KeepUserMessages int
	// Focus is an optional instruction for the summarizer ("/compact <focus>").
	Focus string
	// ExtractiveFallback lets a forced pass whose summarizer fails compact
	// anyway, with a summary extracted from the transcript without a model.
	// For recovering an overflowing context mid-run, where failing is worse
	// than a plainer summary; an explicit /compact reports the failure.
	ExtractiveFallback bool
}

// Result is the outcome of a compaction pass.
type Result struct {
	// Messages is the history to continue with (the input when nothing
	// changed).
	Messages []provider.Message
	// Pruned counts the old tool outputs (and oversized tool-call arguments)
	// replaced by stubs.
	Pruned int
	// Summarized is set when older turns were replaced by a summary.
	Summarized bool
	// Summary is the summary text (without SummaryHeader) when Summarized.
	Summary string
	// Fallback is set when the summarizer failed and the summary was
	// extracted from the transcript without a model (see
	// Params.ExtractiveFallback).
	Fallback bool
}

// Compacted reports whether the pass changed the history.
func (r Result) Compacted() bool { return r.Pruned > 0 || r.Summarized }

// CompactHistory compacts msgs when the estimated request size approaches
// the context window (or unconditionally when force is set, for explicit
// /compact). See Compact for what is kept. Returns the (possibly shortened)
// slice, whether it compacted, and any error.
//
// It applies the default auto-compaction policy (enabled, 85%). Callers that
// carry a user-configured policy should use CompactHistoryWithPolicy.
func CompactHistory(ctx context.Context, send SendFunc, system string, msgs []provider.Message, window int, force bool) ([]provider.Message, bool, error) {
	return CompactHistoryWithPolicy(ctx, send, system, msgs, window, force, Config{}, 0)
}

// CompactHistoryWithPolicy is CompactHistory with an explicit auto-compaction
// policy and the caller's consecutive-failure count. A zero-value cfg means
// "use defaults" (auto enabled at the default threshold); a non-zero cfg is
// honored as-is, so AutoEnabled=false disables the automatic trigger entirely
// (force still works). When failures has reached cfg.MaxFailures the
// automatic trigger pauses, matching Evaluate's semantics.
func CompactHistoryWithPolicy(ctx context.Context, send SendFunc, system string, msgs []provider.Message, window int, force bool, cfg Config, failures int) ([]provider.Message, bool, error) {
	return CompactHistoryMeasured(ctx, send, system, msgs, window, force, cfg, failures, nil)
}

// CompactHistoryMeasured is CompactHistoryWithPolicy with a caller-supplied
// measure for the trigger (see MeasureFunc).
func CompactHistoryMeasured(ctx context.Context, send SendFunc, system string, msgs []provider.Message, window int, force bool, cfg Config, failures int, measure MeasureFunc) ([]provider.Message, bool, error) {
	res, err := Compact(ctx, send, msgs, Params{System: system, Window: window, Force: force, Policy: cfg, Failures: failures, Measure: measure})
	if err != nil {
		return msgs, false, err
	}
	return res.Messages, res.Compacted(), nil
}

// Compact shrinks a conversation that is approaching its context window, in
// two stages, cheapest first:
//
//  1. Pruning: old, large tool outputs are replaced by short stubs
//     ("[output elided: N chars, spool:K …]") naming the stored copy the
//     tool-output tool re-reads — spooled outputs first, sparing the newest
//     ones, then everything before the verbatim tail. No model call and no
//     turn removed; when that brings the prompt under the trigger it stops.
//  2. Summarization: the turns between the first message and the verbatim
//     tail are replaced by one structured summary (goal, decisions, files
//     modified, current test/error state, next steps, references) written by
//     the summarizer model from a transcript that keeps edits, commands and
//     error text rather than 200-character snippets.
//
// Whatever happens, the result keeps the first message (the original task,
// with its session snapshot) verbatim, the latest user messages of the
// summarized span verbatim, and the most recent tool exchanges verbatim with
// valid call/result pairing (checked with ValidatePairing, so providers never
// see an orphaned tool result or an unanswered call). A result that would
// not be smaller than the input is discarded.
func Compact(ctx context.Context, send SendFunc, msgs []provider.Message, p Params) (Result, error) {
	measure := p.Measure
	if measure == nil {
		measure = EstimateHistoryTokens
	}
	window := p.Window
	if window <= 0 {
		window = 128000 // sane default so compaction always has a threshold
	}
	cfg := p.Policy
	if cfg == (Config{}) {
		cfg = Config{AutoEnabled: true}
	}
	eval := func(ms []provider.Message) Evaluation {
		return Evaluate(window, cfg, State{TokensUsed: measure(p.System, ms), ConsecutiveFailures: p.Failures})
	}
	// fits: under both the auto trigger and the error threshold, so the next
	// step does not compact again straight away.
	fits := func(ms []provider.Message) bool {
		e := eval(ms)
		return !e.ShouldAutoCompact && !e.IsError
	}
	noop := Result{Messages: msgs}
	if !p.Force {
		if len(msgs) <= 5 {
			return noop, nil
		}
		// IsError acts as a backstop trigger only while auto compaction is on
		// and not paused after repeated failures; with the off switch set, the
		// run proceeds untouched (the budget validator's forced compaction
		// remains the last line of defense).
		e := eval(msgs)
		if !e.ShouldAutoCompact && !(e.AutoDisabledReason == "" && e.IsError) {
			return noop, nil
		}
	}
	if len(msgs) < 2 {
		return noop, nil
	}

	keepEx, minTail := 3, 4
	if p.Force {
		// A forced compact keeps a shorter tail so an explicit /compact (or
		// an overflow) frees space even in mid-sized conversations.
		keepEx, minTail = 2, 2
	}
	if p.KeepToolExchanges > 0 {
		keepEx = p.KeepToolExchanges
	}
	start := chooseTail(msgs, keepEx, minTail, EffectiveContextWindow(window)/2)

	// Stage 1 — pruning. An explicit /compact always goes on to summarize.
	pruned := noop
	if !p.Force {
		for _, tier := range []pruneTier{pruneSpooled, pruneAll} {
			next, n := pruneToolOutputs(pruned.Messages, start, tier, protectTokens(window))
			if n == 0 {
				continue
			}
			pruned = Result{Messages: next, Pruned: pruned.Pruned + n}
			if fits(next) {
				return pruned, nil
			}
		}
	}

	// Stage 2 — summarize msgs[1:start] from the unpruned history (the
	// transcript renderer bounds each item itself and keeps spool IDs).
	middle := msgs[1:start]
	if len(middle) == 0 {
		// Nothing older than the tail: only its own outputs can shrink.
		if out, n := shrinkTail(msgs); n > 0 {
			return Result{Messages: out, Pruned: n}, nil
		}
		return pruned, nil
	}
	if !p.Force && measure(p.System, msgs)-measure(p.System, append([]provider.Message{msgs[0]}, msgs[start:]...)) < minSummarySavings(window) {
		// The pressure comes from elsewhere (system prompt, tool schemas, the
		// kept tail): a summarizer call would free next to nothing and fire
		// again on every step.
		return pruned, nil
	}
	res := Result{Summarized: true}
	// The instructions go in the system field, so the request uses Messages
	// (adapters drop System on legacy Prompt-only requests).
	prompt := summaryPrompt(msgs[0], middle, p.Focus, transcriptBudget(window))
	resp, err := send(ctx, provider.Request{System: summarizerSystem, Messages: []provider.Message{{Role: provider.RoleUser, Content: prompt}}})
	if err != nil {
		err = fmt.Errorf("compaction summarizer: %w", err)
	} else if res.Summary = strings.TrimSpace(resp.Content); res.Summary == "" {
		err = fmt.Errorf("compaction: empty summary")
	}
	if err != nil {
		// A cancelled or expired caller context is the run stopping, not the
		// summarizer failing: report it, so no model-free summary replaces a
		// history the caller may still keep (the ACP bridge adopts the
		// result even when the turn was cancelled).
		if !p.Force || !p.ExtractiveFallback || ctx.Err() != nil {
			return noop, err
		}
		res.Summary, res.Fallback = fallbackSummary(middle), true
	}

	keepUsers := p.KeepUserMessages
	if keepUsers <= 0 {
		keepUsers = 3
	}
	lifted := liftUserMessages(middle, keepUsers, userBudget(window))

	first := msgs[0]
	first.FileStamps = mergeStamps(msgs[:start])
	out := make([]provider.Message, 0, 2+len(lifted)+len(msgs)-start)
	out = append(out, first)
	out = append(out, provider.Message{Role: provider.RoleUser, Content: summaryMessage(res, modifiedFiles(middle), len(lifted))})
	out = append(out, lifted...)
	out = append(out, msgs[start:]...)
	out = RepairPairing(out)
	if verr := ValidatePairing(out); verr != nil && ValidatePairing(msgs) == nil {
		return noop, fmt.Errorf("compaction produced an invalid history: %w", verr)
	}
	if !fits(out) {
		// The kept tail alone still crowds the window (huge recent outputs):
		// stub its older outputs too, sparing only the latest exchange.
		var n int
		out, n = shrinkTail(out)
		res.Pruned += n
	}
	if measure(p.System, out) >= measure(p.System, msgs) {
		// A short conversation of short turns: the summary would cost more
		// than the turns it replaces.
		return noop, nil
	}
	res.Messages = out
	return res, nil
}

// chooseTail picks where the verbatim tail starts: keepEx tool exchanges and
// minTail messages, fewer exchanges when the tail alone would take more than
// maxTokens (so a run of huge recent outputs still leaves room to work).
func chooseTail(msgs []provider.Message, keepEx, minTail, maxTokens int) int {
	for k := keepEx; ; k-- {
		start := tailStart(msgs, k, minTail)
		if k <= 1 || maxTokens <= 0 || EstimateHistoryTokens("", msgs[start:]) <= maxTokens {
			return start
		}
	}
}

// shrinkTail stubs every large tool output and argument before the latest
// tool exchange of msgs. It is the last resort after summarizing, when even
// the verbatim tail does not fit.
func shrinkTail(msgs []provider.Message) ([]provider.Message, int) {
	last := len(msgs)
	for i := len(msgs) - 1; i >= 1; i-- {
		if isToolCall(msgs[i]) {
			last = i
			break
		}
	}
	return pruneToolOutputs(msgs, last, pruneAll, 0)
}

// minSummarySavings is the smallest span (in tokens) worth an automatic
// summarizer call: a tenth of the effective window, at most 2k tokens.
func minSummarySavings(window int) int {
	return min(EffectiveContextWindow(window)/10, 2000)
}

// userBudget is how many characters of earlier user messages (beyond the
// latest, always kept) compaction carries over verbatim: about a tenth of
// the window, within bounds.
func userBudget(window int) int {
	return min(max(EffectiveContextWindow(window)*4/10, 2000), 80000)
}

// liftUserMessages returns, oldest first, up to keep of the latest user
// messages in middle, to be carried past the summary verbatim. The latest
// one is always included; older ones only while they fit budget characters.
func liftUserMessages(middle []provider.Message, keep, budget int) []provider.Message {
	var picked []provider.Message
	used := 0
	for i := len(middle) - 1; i >= 0 && len(picked) < keep; i-- {
		m := middle[i]
		if !isUserText(m) {
			continue
		}
		if len(picked) > 0 && used+len(m.Content) > budget {
			break
		}
		used += len(m.Content)
		m.FileStamps = nil
		m.SessionContext = ""
		m.LoadedTools = nil
		picked = append(picked, m)
	}
	slices.Reverse(picked)
	return picked
}

// mergeStamps folds the file stamps recorded on msgs into one record per
// file, replaying them in order as the runtime does (the latest Seen and
// Read win independently), so summarizing the turns that carried them does
// not forget which files the agent has read.
func mergeStamps(msgs []provider.Message) []provider.FileStamp {
	byPath := map[string]provider.FileStamp{}
	for _, m := range msgs {
		for _, fs := range m.FileStamps {
			cur := byPath[fs.Path]
			cur.Path = fs.Path
			if fs.Seen != "" {
				cur.Seen = fs.Seen
				// A record with Seen is the path's whole state, so its
				// Shell mark (false included) replaces the earlier one.
				cur.Shell = fs.Shell
			}
			if fs.Read != "" {
				cur.Read = fs.Read
			}
			byPath[fs.Path] = cur
		}
	}
	if len(byPath) == 0 {
		return nil
	}
	out := make([]provider.FileStamp, 0, len(byPath))
	for _, fs := range byPath {
		out = append(out, fs)
	}
	slices.SortFunc(out, func(a, b provider.FileStamp) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// summaryMessage renders the synthetic user turn that replaces the
// summarized span.
func summaryMessage(res Result, files []string, lifted int) string {
	var sb strings.Builder
	sb.WriteString(SummaryHeader)
	sb.WriteString("\nEarlier turns of this conversation were compacted to save context. The first message is the original task, verbatim; this is a summary of the work done since then.\n\n")
	sb.WriteString(res.Summary)
	if len(files) > 0 && !res.Fallback {
		sb.WriteString("\n\nFiles changed by edit tools in the compacted turns (from the tool log):\n")
		sb.WriteString(strings.Join(files, "\n"))
	}
	sb.WriteString("\n\nFile contents you saw before this point may be out of date: re-read a file before editing it.")
	if lifted > 0 {
		sb.WriteString(" The latest user messages from the compacted turns follow verbatim, then the most recent turns.")
	}
	return sb.String()
}
