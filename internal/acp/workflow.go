package acp

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
)

// Workflow runs are surfaced to editors as one long-lived tool call whose
// content is rewritten as the run progresses, rather than as a stream of
// separate calls. An editor then shows a single "workflow review-changes"
// entry that grows a phase tree in place — the ACP analogue of the TUI panel —
// while each sub-agent still gets its own tool call, so "follow the agent"
// navigation keeps working.
//
// A run can pause at a checkpoint and wait for the orchestrating model, which
// may continue it in the same turn or a later one. The card's state therefore
// lives on the session (acpWorkflowCards), keyed by run_id, not on the turn:
// the turn that continues the run picks up the phases, members and log built
// so far and re-announces the card next to the conversation driving it.

type acpWorkflowAgent struct {
	Instance string
	Label    string
	Phase    string
	Status   string
	Cached   bool
}

// acpWorkflowPhase is what a phase() call said about a phase beyond its
// title: its detail line, and whether the script added it at runtime instead
// of declaring it in meta.phases.
type acpWorkflowPhase struct {
	detail  string
	dynamic bool
}

// maxWorkflowLogLines bounds the log tail a card keeps. A dynamic run that
// loops until dry logs every round, and the card's whole content is re-sent
// on every update, so an unbounded tail would grow each notification for as
// long as the run lasts.
const maxWorkflowLogLines = 40

type acpWorkflow struct {
	callID      acpsdk.ToolCallId
	runID       string
	name        string
	description string
	phases      []string
	phaseInfo   map[string]acpWorkflowPhase
	agents      []acpWorkflowAgent
	logs        []string
	// dropped counts the log lines trimmed off the front of logs.
	dropped int

	// size, sizeAgents and budget come from the start payload: the run's
	// size tier, that tier's agent guideline (0 = none) and its token
	// budget (0 = none).
	size       string
	sizeAgents int
	budget     int

	// status is the run's lifecycle status as the last workflow trace gave
	// it ("running", "paused", ...). checkpointID and waiting describe the
	// checkpoint a paused run waits at.
	status       string
	checkpointID string
	waiting      string
	// stopReason is why a stopped run was stopped (by the orchestrator,
	// the idle reaper, the session ending), shown on its closed card.
	stopReason string

	// turn is the prompt turn that announced callID. A run paused at a
	// checkpoint outlives its turn; when a later turn continues it, the card
	// is announced again there (attach counts the announcements, so each
	// gets its own ID).
	turn   *turnState
	attach int
	// continuedFrom is the ID of the card the previous turn closed when this
	// one took the run over, "" for a run's first card.
	continuedFrom string
}

func (w *acpWorkflow) addPhase(title string) {
	if title == "" {
		return
	}
	for _, p := range w.phases {
		if p == title {
			return
		}
	}
	w.phases = append(w.phases, title)
}

// notePhase records a phase's detail and whether it was added at runtime.
// The flag is overwritten, not or-ed: a phase() call reports dynamic=false
// for a declared phase, and that is the last word on it.
func (w *acpWorkflow) notePhase(title, detail string, dynamic bool) {
	if title == "" {
		return
	}
	if w.phaseInfo == nil {
		w.phaseInfo = map[string]acpWorkflowPhase{}
	}
	info := w.phaseInfo[title]
	if detail != "" {
		info.detail = detail
	}
	info.dynamic = dynamic
	w.phaseInfo[title] = info
}

func (w *acpWorkflow) addLog(line string) {
	w.logs = append(w.logs, line)
	if over := len(w.logs) - maxWorkflowLogLines; over > 0 {
		w.logs = append([]string(nil), w.logs[over:]...)
		w.dropped += over
	}
}

// title is the card's title: the workflow's name, then its size tier and
// token budget when the run reported them, so a collapsed card already says
// how big the run was meant to be.
func (w *acpWorkflow) title() string {
	title := "workflow " + w.name
	if w.size != "" {
		title += " · " + w.size
	}
	if w.budget > 0 {
		title += " · budget " + compactTokens(w.budget)
	}
	return title
}

// compactTokens renders a token count the way a budget directive spells it
// (500k, 1.5m): that is how the user asked for it.
func compactTokens(n int) string {
	switch {
	case n >= 1_000_000:
		s := strings.TrimSuffix(fmt.Sprintf("%.2f", float64(n)/1e6), "0")
		return strings.TrimSuffix(s, ".0") + "m"
	case n >= 1_000:
		s := strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e3), "0")
		return strings.TrimSuffix(s, ".") + "k"
	}
	return fmt.Sprint(n)
}

// phaseOrder lists declared phases first, then any the script entered without
// declaring, then a bucket for agents dispatched outside a phase.
func (w *acpWorkflow) phaseOrder() []string {
	order := append([]string(nil), w.phases...)
	seen := map[string]bool{}
	for _, p := range order {
		seen[p] = true
	}
	loose := false
	for _, a := range w.agents {
		switch {
		case a.Phase == "":
			loose = true
		case !seen[a.Phase]:
			seen[a.Phase] = true
			order = append(order, a.Phase)
		}
	}
	if loose {
		order = append(order, "")
	}
	return order
}

// agentGlyph maps a member's status to its marker. Only "success" is done: a
// status this code does not know (one a newer runtime adds) must not read as
// a finished agent.
func agentGlyph(status string) string {
	switch status {
	case "success":
		return "✓"
	case "error":
		return "✗"
	case "running":
		return "▶"
	}
	return "·"
}

// acpWorkflowPhaseView is one phase as the card shows it: its members and
// their tally. render() writes it as text and meta() as structure, so the two
// cannot disagree about what a phase holds.
type acpWorkflowPhaseView struct {
	// title is "" for the bucket of agents dispatched outside a phase.
	title   string
	detail  string
	dynamic bool
	members []acpWorkflowAgent
	// finished counts members that ended either way, failed included: the
	// "d/n done" the text shows. failed counts those that errored.
	finished int
	failed   int
}

func (w *acpWorkflow) phaseViews() []acpWorkflowPhaseView {
	order := w.phaseOrder()
	views := make([]acpWorkflowPhaseView, 0, len(order))
	for _, phase := range order {
		info := w.phaseInfo[phase]
		v := acpWorkflowPhaseView{title: phase, detail: info.detail, dynamic: info.dynamic}
		for _, a := range w.agents {
			if a.Phase != phase {
				continue
			}
			v.members = append(v.members, a)
			switch a.Status {
			case "error":
				v.failed++
				v.finished++
			case "success":
				v.finished++
			}
		}
		views = append(views, v)
	}
	return views
}

func (w *acpWorkflow) render() string {
	var b strings.Builder
	if w.description != "" {
		b.WriteString(w.description + "\n\n")
	}
	if w.size != "" {
		if w.sizeAgents > 0 {
			fmt.Fprintf(&b, "size: %s (~%d agents, a guideline)", w.size, w.sizeAgents)
		} else {
			fmt.Fprintf(&b, "size: %s (no guideline)", w.size)
		}
		if w.budget > 0 {
			fmt.Fprintf(&b, " · budget %s tokens", compactTokens(w.budget))
		}
		b.WriteString("\n\n")
	}
	switch w.status {
	case "", "running", "success", "error":
	case "stopped":
		// A deliberate stop, not a failure: the card closes as completed,
		// so this line is what tells it apart from a run that finished.
		b.WriteString("■ stopped")
		if w.stopReason != "" {
			b.WriteString(": " + w.stopReason)
		}
		b.WriteString("\n\n")
	case "paused":
		// The run is alive but idle until the orchestrating model answers
		// its checkpoint, which may only happen in a later turn. Saying so
		// keeps a card that stopped moving from reading as hung.
		b.WriteString("⏸ ")
		if w.checkpointID != "" {
			b.WriteString("paused at " + w.checkpointID + " — ")
		}
		b.WriteString("waiting for orchestrator")
		if w.waiting != "" {
			b.WriteString(": " + w.waiting)
		}
		b.WriteString("\n\n")
	default:
		fmt.Fprintf(&b, "status: %s\n\n", w.status)
	}
	for _, p := range w.phaseViews() {
		title := p.title
		if title == "" {
			title = "(no phase)"
		}
		if p.dynamic {
			title += " (added at runtime)"
		}
		if len(p.members) == 0 {
			fmt.Fprintf(&b, "○ %s — pending\n", title)
		} else {
			fmt.Fprintf(&b, "▸ %s — %d/%d done", title, p.finished, len(p.members))
			if p.failed > 0 {
				fmt.Fprintf(&b, ", %d failed", p.failed)
			}
			b.WriteString("\n")
		}
		if p.detail != "" {
			fmt.Fprintf(&b, "    ↳ %s\n", p.detail)
		}
		for _, a := range p.members {
			label := a.Label
			if a.Cached {
				label = "replayed · " + label
			}
			fmt.Fprintf(&b, "    %s %s  %s\n", agentGlyph(a.Status), a.Instance, label)
		}
	}
	if len(w.logs) > 0 {
		b.WriteString("\nlog:\n")
		if w.dropped > 0 {
			fmt.Fprintf(&b, "  … %d earlier lines\n", w.dropped)
		}
		for _, line := range w.logs {
			b.WriteString("  " + line + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// workflowMetaKey carries the card's state as structure in the _meta of every
// tool_call and tool_call_update the card sends. The text content stays what
// it was, for clients (and the TUI) that only read it; a client that knows
// this key reads the run from here instead of parsing render()'s lines.
// Clients detect it by its presence, so it does not bump extensionsVersion.
// workflowMetaVersion is the payload's own version, raised only by a change
// an older reader would misread.
const (
	workflowMetaKey     = "spettro.app/workflow"
	workflowMetaVersion = 1
)

// acpWorkflowMeta is the `_meta["spettro.app/workflow"]` payload. Fields the
// card always has are always sent, empty or zero when the run gave none, so a
// client need not tell a missing field from an empty one.
type acpWorkflowMeta struct {
	Version     int    `json:"version"`
	RunID       string `json:"runId"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Size        string `json:"size"`
	// SizeAgents is the size tier's agent guideline, 0 for none.
	SizeAgents int `json:"sizeAgents"`
	// BudgetTokens is the token pool the run draws on, 0 for none.
	BudgetTokens int `json:"budgetTokens"`
	// Status is the run's lifecycle status (see workflowMetaStatus).
	Status        string                `json:"status"`
	PausedAt      *acpWorkflowMetaPause `json:"pausedAt,omitempty"`
	StoppedReason string                `json:"stoppedReason,omitempty"`
	// ContinuedFrom is the ID of the card an earlier turn closed when this
	// turn continued the run; ContinuedIn, on that closed card, is the ID of
	// the card that took over.
	ContinuedFrom string `json:"continuedFrom,omitempty"`
	ContinuedIn   string `json:"continuedIn,omitempty"`
	// Attach counts the turns that have shown this run's card, this one
	// included: 1 for the card that opened it.
	Attach int `json:"attach"`
	// Summary is the run's closing line ("3 agents · 0 failed · 1 replayed"),
	// on the update that closes a run that finished.
	Summary         string                  `json:"summary,omitempty"`
	Phases          []acpWorkflowMetaPhase  `json:"phases"`
	Members         []acpWorkflowMetaMember `json:"members"`
	Counts          acpWorkflowMetaCounts   `json:"counts"`
	LogTail         []string                `json:"logTail"`
	DroppedLogLines int                     `json:"droppedLogLines"`
}

type acpWorkflowMetaPause struct {
	CheckpointID string `json:"checkpointId"`
	Message      string `json:"message"`
}

// acpWorkflowMetaPhase is one phase. Title is "" for the agents dispatched
// outside any phase (the text calls them "(no phase)"). Done counts members
// that finished either way, failed ones included, as the text's "d/n done"
// does; Failed counts those that errored and Total all of them.
type acpWorkflowMetaPhase struct {
	Title   string `json:"title"`
	Detail  string `json:"detail,omitempty"`
	Dynamic bool   `json:"dynamic"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Failed  int    `json:"failed"`
}

// acpWorkflowMetaMember is one agent() call. Status is "running", "done",
// "failed" or "pending"; a status the runtime reports that this code does
// not know is "pending", as its "·" glyph in the text is.
type acpWorkflowMetaMember struct {
	Instance string `json:"instance"`
	Task     string `json:"task"`
	Phase    string `json:"phase"`
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
}

type acpWorkflowMetaCounts struct {
	Agents   int `json:"agents"`
	Failed   int `json:"failed"`
	Replayed int `json:"replayed"`
}

// workflowMetaStatus names a run's lifecycle status for the meta: "running",
// "paused", "stopped", "success", "failed" or "cancelled". A status this code
// does not know passes through as it came, as the text's "status: <s>" line
// shows it, so a client must treat any other value as an open run.
func workflowMetaStatus(status string) string {
	switch status {
	case "", "running":
		return "running"
	case "error", "failed":
		return "failed"
	case "cancelled", "canceled":
		return "cancelled"
	}
	return status
}

func workflowMemberStatus(status string) string {
	switch status {
	case "success":
		return "done"
	case "error":
		return "failed"
	case "running":
		return "running"
	}
	return "pending"
}

// metaView is the card's state as the meta payload, read from the same fields
// and phase views render() uses.
func (w *acpWorkflow) metaView() acpWorkflowMeta {
	m := acpWorkflowMeta{
		Version:         workflowMetaVersion,
		RunID:           w.runID,
		Name:            w.name,
		Description:     w.description,
		Size:            w.size,
		SizeAgents:      w.sizeAgents,
		BudgetTokens:    w.budget,
		Status:          workflowMetaStatus(w.status),
		ContinuedFrom:   w.continuedFrom,
		Attach:          w.attach,
		Phases:          []acpWorkflowMetaPhase{},
		Members:         []acpWorkflowMetaMember{},
		LogTail:         append([]string{}, w.logs...),
		DroppedLogLines: w.dropped,
	}
	switch m.Status {
	case "paused":
		m.PausedAt = &acpWorkflowMetaPause{CheckpointID: w.checkpointID, Message: w.waiting}
	case "stopped":
		m.StoppedReason = w.stopReason
	}
	for _, p := range w.phaseViews() {
		m.Phases = append(m.Phases, acpWorkflowMetaPhase{
			Title: p.title, Detail: p.detail, Dynamic: p.dynamic,
			Done: p.finished, Total: len(p.members), Failed: p.failed,
		})
		// Members are listed phase by phase, in the order the text shows them.
		for _, a := range p.members {
			m.Members = append(m.Members, acpWorkflowMetaMember{
				Instance: a.Instance, Task: a.Label, Phase: a.Phase,
				Status: workflowMemberStatus(a.Status), Replayed: a.Cached,
			})
			m.Counts.Agents++
			if a.Status == "error" {
				m.Counts.Failed++
			}
			if a.Cached {
				m.Counts.Replayed++
			}
		}
	}
	return m
}

// meta is the card's _meta as a tool call carries it.
func (w *acpWorkflow) meta() map[string]any {
	return workflowMeta(w.metaView())
}

func workflowMeta(m acpWorkflowMeta) map[string]any {
	return map[string]any{workflowMetaKey: m}
}

// The SDK has helpers for every tool-call field but _meta.
func withStartMeta(meta map[string]any) acpsdk.ToolCallStartOpt {
	return func(tc *acpsdk.SessionUpdateToolCall) { tc.Meta = meta }
}

func withUpdateMeta(meta map[string]any) acpsdk.ToolCallUpdateOpt {
	return func(tu *acpsdk.SessionToolCallUpdate) { tu.Meta = meta }
}

// cardID is the tool call ID of the attach-th card shown for a run with an
// ID: the run's ID for the first, plus the attach count for later ones.
func (w *acpWorkflow) cardID(attach int) acpsdk.ToolCallId {
	if attach <= 1 {
		return acpsdk.ToolCallId("workflow-" + w.runID)
	}
	return acpsdk.ToolCallId(fmt.Sprintf("workflow-%s-%d", w.runID, attach))
}

// acpWorkflowCards is a session's workflow cards, keyed by run_id. It
// outlives single prompt turns because a run can: one paused at a checkpoint
// waits for the orchestrating model, which may continue it only in a later
// turn. That turn's "running" trace carries resumed:true and the same run_id,
// and finds the phases, members and log the run built so far here instead of
// starting an empty card. A card leaves the map when its run finishes.
type acpWorkflowCards struct {
	mu   sync.Mutex
	runs map[string]*acpWorkflow
}

func newACPWorkflowCards() *acpWorkflowCards {
	return &acpWorkflowCards{runs: map[string]*acpWorkflow{}}
}

func (c *acpWorkflowCards) getLocked(runID string) *acpWorkflow {
	if runID == "" {
		return nil
	}
	return c.runs[runID]
}

func (c *acpWorkflowCards) putLocked(w *acpWorkflow) {
	if w.runID == "" {
		return
	}
	if c.runs == nil {
		c.runs = map[string]*acpWorkflow{}
	}
	c.runs[w.runID] = w
}

// stoppedCard is what closing a stopped run's card sends: the card's tool
// call and its final content, rendered while the cards' lock was held.
type stoppedCard struct {
	callID acpsdk.ToolCallId
	body   string
	meta   map[string]any
}

// stopCardLocked marks w stopped for reason, takes it out of the session and
// renders what closing its card shows. Every field a trace writes is written
// under c.mu (onWorkflowTool holds it), so marking and rendering under it
// too is safe even while the turn that last showed the card still runs.
// Caller holds c.mu.
func (c *acpWorkflowCards) stopCardLocked(w *acpWorkflow, reason string) stoppedCard {
	if c.runs[w.runID] == w {
		delete(c.runs, w.runID)
	}
	w.status = "stopped"
	w.stopReason = reason
	return stoppedCard{callID: w.callID, body: w.render(), meta: w.meta()}
}

// stop takes runID's card out of the session, marked stopped for reason. ok
// is false when there is none: the run finished, or its card was taken.
func (c *acpWorkflowCards) stop(runID, reason string) (card stoppedCard, ok bool) {
	if c == nil || runID == "" {
		return stoppedCard{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.runs[runID]
	if w == nil {
		return stoppedCard{}, false
	}
	return c.stopCardLocked(w, reason), true
}

// stopAll takes every card out of the session, each marked stopped for
// reason, for a session whose live runs are being stopped (/clear, the
// session closing, the connection ending): nothing is left for a later trace
// to re-attach to.
func (c *acpWorkflowCards) stopAll(reason string) []stoppedCard {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]stoppedCard, 0, len(c.runs))
	for _, w := range c.runs {
		out = append(out, c.stopCardLocked(w, reason))
	}
	return out
}

// close closes a stopped run's card through notify. Such a run was stopped
// while no turn was driving it — paused at a checkpoint when the idle reaper
// or /clear stopped it — so no turn's sessionUpdate can carry this. The card
// closes as completed, not failed: a deliberate stop is not an error, and its
// "■ stopped: <reason>" line says what happened. A card no turn ever
// announced, or a nil notify, sends nothing.
func (sc stoppedCard) close(notify func(acpsdk.SessionUpdate)) {
	if sc.callID == "" || notify == nil {
		return
	}
	notify(acpsdk.UpdateToolCall(
		sc.callID,
		acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusCompleted),
		acpsdk.WithUpdateContent(toolOutputContent(sc.body, nil)),
		withUpdateMeta(sc.meta),
	))
}

// Three different payloads travel under these trace names, and they disagree
// about the type of "cached": a member reports whether it was replayed (a
// bool), while the lifecycle's finish payload reports how many calls were
// replayed (a count). Decoding all three into one struct meant the finish
// payload failed to unmarshal, onWorkflowTool bailed out, and the workflow
// tool call was never closed — editors showed a run spinning forever with a
// duplicate completed call appended after it. Each payload gets its own type.

// acpWorkflowArgs is the shape common to every workflow trace: enough to tell
// which run a trace belongs to. Fields that only some payloads carry live in
// the specific types below.
type acpWorkflowArgs struct {
	RunID    string `json:"run_id"`
	Workflow string `json:"workflow"`
}

// acpWorkflowStartArgs is the lifecycle trace's opening payload. The finish
// payload deliberately decodes with this same type: its extra counters are
// summarised in the trace output, and ignoring them here keeps the finish path
// from depending on fields whose types have already drifted once.
//
// A continued run sends this payload again with Resumed set; the fields it
// carries then refresh the card rather than replace it.
type acpWorkflowStartArgs struct {
	acpWorkflowArgs
	Description string `json:"description"`
	Phases      []struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	} `json:"phases"`
	Size         string `json:"size"`
	SizeAgents   int    `json:"size_agents"`
	BudgetTokens int    `json:"budget_tokens"`
	Resumed      bool   `json:"resumed"`
}

// acpWorkflowPausedArgs is the lifecycle trace a run sends when the tool call
// returns with the run waiting at a checkpoint.
type acpWorkflowPausedArgs struct {
	acpWorkflowArgs
	CheckpointID string `json:"checkpoint_id"`
	Message      string `json:"message"`
}

// acpWorkflowStoppedArgs is the lifecycle trace of a run stopped on purpose
// (status "stopped"): Reason says by whom or why — the orchestrator's stop,
// the idle reaper, the session ending.
type acpWorkflowStoppedArgs struct {
	acpWorkflowArgs
	Reason string `json:"reason"`
}

// acpWorkflowProgressArgs is a phase(), log() or checkpoint() notification.
type acpWorkflowProgressArgs struct {
	acpWorkflowArgs
	Kind  string `json:"kind"`
	Phase string `json:"phase"`
	// Detail and Dynamic qualify a phase: its detail line, and whether the
	// script entered it without declaring it in meta.phases.
	Detail  string `json:"detail"`
	Dynamic bool   `json:"dynamic"`
	// CheckpointID and Message describe a checkpoint; Cached marks one whose
	// answer was replayed from the journal instead of asked again.
	CheckpointID string `json:"checkpoint_id"`
	Message      string `json:"message"`
	Cached       bool   `json:"cached"`
}

// acpWorkflowMemberArgs is one agent() call's lifecycle.
type acpWorkflowMemberArgs struct {
	acpWorkflowArgs
	Agent  string `json:"agent"`
	Task   string `json:"task"`
	Phase  string `json:"phase"`
	Cached bool   `json:"cached"`
}

// workflowCardsLocked returns the session's workflow cards, or a turn-local
// set for a turn built without one (tests, and any host path that does not
// thread the session's through). Caller holds t.mu.
func (t *turnState) workflowCardsLocked() *acpWorkflowCards {
	if t.workflows == nil {
		t.workflows = newACPWorkflowCards()
	}
	return t.workflows
}

// workflowForLocked finds the card a progress or member trace belongs to: the
// session's card for its run_id, or else this turn's current card when the
// trace names no run (or the card knows none). Caller holds t.mu and cards.mu.
func (t *turnState) workflowForLocked(cards *acpWorkflowCards, runID string) *acpWorkflow {
	if w := cards.getLocked(runID); w != nil {
		return w
	}
	if w := t.workflow; w != nil && (runID == "" || w.runID == "" || w.runID == runID) {
		return w
	}
	return nil
}

// announceWorkflowLocked makes w's card a tool call of this turn. A card
// another turn announced — a run paused at a checkpoint and continued now —
// is closed there with a pointer forward and started afresh here with
// everything recorded so far, under a new ID: tool call IDs are per turn
// elsewhere in this package, so the run's ID plus an attach count keeps the
// two cards (and any card of another turn) apart. Caller holds t.mu.
func (t *turnState) announceWorkflowLocked(w *acpWorkflow, rawInput any) {
	if w.turn == t && w.callID != "" {
		return
	}
	if !t.takeWorkflowLocked(w) {
		return
	}
	w.attach++
	if w.runID == "" {
		w.callID = t.nextToolCallIDLocked("wf")
	} else {
		w.callID = w.cardID(w.attach)
	}
	w.turn = t
	t.workflow = w
	opts := []acpsdk.ToolCallStartOpt{
		acpsdk.WithStartKind(acpsdk.ToolKindThink),
		acpsdk.WithStartStatus(acpsdk.ToolCallStatusInProgress),
		acpsdk.WithStartContent(toolOutputContent(w.render(), nil)),
		withStartMeta(w.meta()),
	}
	if rawInput != nil {
		opts = append(opts, acpsdk.WithStartRawInput(rawInput))
	}
	t.sessionUpdate(acpsdk.StartToolCall(w.callID, w.title(), opts...))
}

// takeWorkflowLocked closes w's card in the turn that announced it, if that
// is another turn, as the first half of announcing it here; it is called
// before the trace's own change is applied, so the closed card shows the run
// as that turn left it (paused at its checkpoint). It reports whether this
// turn may take the card: a turn that has ended can still receive a trace (a
// run whose turn was cancelled under it reports its stop through that turn),
// but it cannot show anything, so it must not take the card from a turn that
// can. A paused run stopped while detached sends no trace at all; its card is
// closed through the registry's stop hook instead (see stoppedCard.close).
// Caller holds t.mu.
func (t *turnState) takeWorkflowLocked(w *acpWorkflow) bool {
	if w.callID == "" || w.turn == t {
		return true
	}
	if t.ctx != nil && t.ctx.Err() != nil {
		return false
	}
	// The closed card points at the one about to replace it, and that one
	// back at it. Only a run with an ID can reach here from another turn
	// (the session keys its cards by run ID), so the next ID is known.
	meta := w.metaView()
	meta.ContinuedIn = string(w.cardID(w.attach + 1))
	t.sessionUpdate(acpsdk.UpdateToolCall(
		w.callID,
		acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusCompleted),
		acpsdk.WithUpdateContent(toolOutputContent("continued in a later turn\n\n"+w.render(), nil)),
		withUpdateMeta(workflowMeta(meta)),
	))
	w.continuedFrom = string(w.callID)
	w.callID = ""
	return true
}

// refreshWorkflowLocked re-sends w's card, still in progress, with its
// current text and meta. Caller holds t.mu.
func (t *turnState) refreshWorkflowLocked(w *acpWorkflow) {
	t.sessionUpdate(acpsdk.UpdateToolCall(
		w.callID,
		acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusInProgress),
		acpsdk.WithUpdateContent(toolOutputContent(w.render(), nil)),
		withUpdateMeta(w.meta()),
	))
}

// onWorkflowTool folds a workflow trace into the live tool call. It reports
// whether the trace was fully handled: workflow lifecycle and progress traces
// are (they have no standalone meaning), while a member's agent trace is only
// recorded here and still travels the normal path so the editor gets a tool
// call for the sub-agent itself.
//
// The model's own call of the workflow tool also arrives as a "workflow"
// trace; its arguments are the tool's (name, script, continue_run_id, ...)
// and carry no "workflow" key, so it is left to the generic card path.
func (t *turnState) onWorkflowTool(tr agent.ToolTrace) (handled bool) {
	switch tr.Name {
	case "workflow", "workflow-progress", "agent":
	default:
		return false
	}
	// Only the two fields every payload shares are required here. Decoding a
	// payload-specific field is done below, per trace kind, so a field one
	// payload spells differently can never disown the whole trace.
	var args acpWorkflowArgs
	if json.Unmarshal([]byte(tr.Args), &args) != nil || args.Workflow == "" {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	cards := t.workflowCardsLocked()
	cards.mu.Lock()
	defer cards.mu.Unlock()

	if tr.Name == "workflow" {
		switch tr.Status {
		case "running":
			t.startWorkflowLocked(cards, args, tr)
			return true
		case "paused":
			return t.pauseWorkflowLocked(cards, args, tr)
		}
		return t.finishWorkflowLocked(cards, args, tr)
	}

	w := t.workflowForLocked(cards, args.RunID)
	if w == nil {
		return false
	}
	// A run continued without a resumed start trace first (or one whose
	// start this turn missed) still reports next to this turn's conversation.
	t.announceWorkflowLocked(w, nil)
	switch tr.Name {
	case "workflow-progress":
		var prog acpWorkflowProgressArgs
		_ = json.Unmarshal([]byte(tr.Args), &prog)
		switch prog.Kind {
		case "phase":
			w.addPhase(prog.Phase)
			w.notePhase(prog.Phase, prog.Detail, prog.Dynamic)
		case "log":
			if msg := strings.TrimSpace(tr.Output); msg != "" {
				w.addLog(msg)
			}
		case "checkpoint":
			// The trace's output is the checkpoint's data; the card shows
			// only the message, since the data is the orchestrator's to
			// read and may run to thousands of characters.
			line := "⏸ "
			if prog.CheckpointID != "" {
				line += prog.CheckpointID + " "
			}
			if prog.Cached {
				line += "(answer replayed) "
			}
			w.addLog(strings.TrimSpace(line + prog.Message))
		}
		handled = true
	case "agent":
		var mem acpWorkflowMemberArgs
		if json.Unmarshal([]byte(tr.Args), &mem) != nil || mem.Agent == "" {
			return false
		}
		found := false
		for i := range w.agents {
			if w.agents[i].Instance == mem.Agent {
				w.agents[i].Status = tr.Status
				found = true
				break
			}
		}
		if !found {
			w.agents = append(w.agents, acpWorkflowAgent{
				Instance: mem.Agent, Label: mem.Task, Phase: mem.Phase,
				Status: tr.Status, Cached: mem.Cached,
			})
		}
		// Not handled: the sub-agent still deserves its own tool call.
		handled = false
	}
	t.refreshWorkflowLocked(w)
	return handled
}

// startWorkflowLocked handles a "running" lifecycle trace: a new run opens a
// card, and a continued one (resumed:true) re-attaches to the card the
// session already holds for its run_id, keeping what it recorded. Caller
// holds t.mu and cards.mu.
func (t *turnState) startWorkflowLocked(cards *acpWorkflowCards, args acpWorkflowArgs, tr agent.ToolTrace) {
	var start acpWorkflowStartArgs
	_ = json.Unmarshal([]byte(tr.Args), &start)

	w := cards.getLocked(args.RunID)
	if w == nil || !start.Resumed {
		prev := w
		w = &acpWorkflow{runID: args.RunID, name: args.Workflow}
		if prev != nil {
			// The same run_id started over: carry the attach count so the
			// new card's ID cannot repeat the old one's.
			w.attach = prev.attach
		}
	} else {
		// Close the earlier turn's card while it still shows the pause.
		t.takeWorkflowLocked(w)
	}
	w.status = "running"
	w.checkpointID, w.waiting = "", ""
	if start.Description != "" {
		w.description = start.Description
	}
	for _, p := range start.Phases {
		w.addPhase(p.Title)
		w.notePhase(p.Title, p.Detail, false)
	}
	if start.Size != "" {
		w.size = start.Size
		w.sizeAgents = start.SizeAgents
	}
	if start.BudgetTokens > 0 {
		w.budget = start.BudgetTokens
	}
	cards.putLocked(w)

	if w.turn == t && w.callID != "" {
		// Continued in the turn that paused it: the card is already here.
		t.workflow = w
		t.refreshWorkflowLocked(w)
		return
	}
	t.announceWorkflowLocked(w, rawJSON(tr.Args))
}

// pauseWorkflowLocked handles the trace a run sends when the tool call returns
// with the run waiting at a checkpoint. The card stays in progress — the run
// is not over, and the next workflow call may continue it — with the
// checkpoint it waits at said up front. Caller holds t.mu and cards.mu.
func (t *turnState) pauseWorkflowLocked(cards *acpWorkflowCards, args acpWorkflowArgs, tr agent.ToolTrace) bool {
	w := t.workflowForLocked(cards, args.RunID)
	if w == nil {
		return false
	}
	var paused acpWorkflowPausedArgs
	_ = json.Unmarshal([]byte(tr.Args), &paused)
	t.announceWorkflowLocked(w, nil)
	w.status = "paused"
	w.checkpointID = paused.CheckpointID
	w.waiting = strings.TrimSpace(paused.Message)
	t.refreshWorkflowLocked(w)
	return true
}

// finishWorkflowLocked handles every other lifecycle status. "success" and
// "error" close the card. "stopped" is a deliberate stop (the orchestrator
// answered a checkpoint with stop, the idle reaper, the session ending): it
// closes the card as completed — the run did what it was told — with a
// "■ stopped: <reason>" line, so it reads neither as done nor as a failure.
// Statuses that plainly mean the run ended without finishing close it as
// failed; anything else is a status this code does not know, which must
// neither close the card nor read as done, so it is shown and the card stays
// open. Caller holds t.mu and cards.mu.
func (t *turnState) finishWorkflowLocked(cards *acpWorkflowCards, args acpWorkflowArgs, tr agent.ToolTrace) bool {
	w := t.workflowForLocked(cards, args.RunID)
	if w == nil {
		return false
	}
	t.announceWorkflowLocked(w, nil)
	var status acpsdk.ToolCallStatus
	switch tr.Status {
	case "success":
		status = acpsdk.ToolCallStatusCompleted
	case "stopped":
		status = acpsdk.ToolCallStatusCompleted
		var stopped acpWorkflowStoppedArgs
		_ = json.Unmarshal([]byte(tr.Args), &stopped)
		w.stopReason = strings.TrimSpace(stopped.Reason)
	case "error", "failed", "cancelled", "canceled":
		status = acpsdk.ToolCallStatusFailed
	default:
		w.status = tr.Status
		t.refreshWorkflowLocked(w)
		return true
	}
	w.status = tr.Status
	body := w.render()
	meta := w.metaView()
	// A stopped card's render already ends with its "■ stopped: <reason>"
	// line; the trace's output says the same thing again.
	if summary := strings.TrimSpace(tr.Output); summary != "" && tr.Status != "stopped" {
		body = summary + "\n\n" + body
		meta.Summary = summary
	}
	t.sessionUpdate(acpsdk.UpdateToolCall(
		w.callID,
		acpsdk.WithUpdateStatus(status),
		acpsdk.WithUpdateContent(toolOutputContent(body, nil)),
		acpsdk.WithUpdateRawOutput(map[string]any{"output": tr.Output}),
		withUpdateMeta(workflowMeta(meta)),
	))
	if cards.getLocked(w.runID) == w {
		delete(cards.runs, w.runID)
	}
	if t.workflow == w {
		t.workflow = nil
	}
	return true
}
