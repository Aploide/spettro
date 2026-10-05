// Package workflow runs deterministic multi-agent orchestration scripts.
//
// A workflow is a small JavaScript program that decides — in ordinary control
// flow, not by asking a model — which sub-agents run, in what order, and how
// their results combine. The script gets a handful of globals (agent, parallel,
// pipeline, phase, log, args, budget, size, workflow, checkpoint, plan,
// untilDry) and returns a value; everything else is plain JS.
//
// The engine is deliberately decoupled from Spettro's agent package: it talks
// to a Runner interface for sub-agent execution and pushes progress through an
// Observer, so it can be driven (and tested) without a provider.
package workflow

import "time"

// EventKind classifies a progress event emitted while a script runs.
type EventKind string

const (
	// EventStart fires once, before the first line of the script executes.
	EventStart EventKind = "start"
	// EventLog carries a log() call from the script.
	EventLog EventKind = "log"
	// EventPhase fires when phase() starts a new progress group.
	EventPhase EventKind = "phase"
	// EventAgentStart fires when an agent() call is dispatched.
	EventAgentStart EventKind = "agent_start"
	// EventAgentDone fires when an agent() call resolves.
	EventAgentDone EventKind = "agent_done"
	// EventAgentError fires when an agent() call fails or is rejected.
	EventAgentError EventKind = "agent_error"
	// EventFinish fires once the script settles, successfully or not.
	EventFinish EventKind = "finish"
	// EventCheckpoint fires when the run pauses at a checkpoint and hands the
	// decision to the orchestrator: Message is the script's question, Output
	// the checkpoint's data as JSON, CheckpointID its id. It fires when the
	// pause is surfaced (every in-flight agent has finished), not when the
	// script called checkpoint(). On a resumed run whose journal already holds
	// the answer it fires with Cached set, immediately followed by the
	// matching EventResume — the run does not actually pause.
	EventCheckpoint EventKind = "checkpoint"
	// EventResume fires when a checkpoint is answered; Output is the reply as
	// JSON.
	EventResume EventKind = "resume"
)

// Event is one progress notification. Hosts render these (TUI workflow panel,
// ACP tool calls) and are free to ignore kinds they do not display.
type Event struct {
	Kind EventKind
	At   time.Time
	// Phase is the progress group the event belongs to ("" outside any phase).
	Phase string
	// Label is the display label of an agent call, defaulting to a trimmed
	// prompt when the script did not set one.
	Label string
	// Instance is the unique per-call agent identity ("review#3"), stable
	// across the start/done pair so hosts can update a row in place.
	Instance string
	// AgentType is the manifest agent the call ran as.
	AgentType string
	// Index is the 1-based global agent counter at dispatch time.
	Index int
	// Message carries log() text, the workflow name on start/finish, or the
	// error text on failure.
	Message string
	// Output is the agent's final text (truncated by the host, not here).
	Output string
	// Cached is true when a resumed run replayed this call from the journal
	// instead of executing it.
	Cached bool
	// Nested marks events produced by a workflow() sub-run.
	Nested bool
	// Detail is a phase's one-line description on EventPhase: the one passed
	// to phase(title, {detail}), else the one declared in meta.phases.
	Detail string
	// Dynamic is set on EventPhase when the title was not declared in the
	// calling script's meta.phases — a phase the script added at runtime, so
	// hosts that drew the declared plan up front can mark it as new.
	Dynamic bool
	// CheckpointID identifies the checkpoint on EventCheckpoint/EventResume
	// ("cp-1", "cp-2", … across the whole run).
	CheckpointID string
	// Auto marks an automatic phase-boundary checkpoint (Options.AutoCheckpoint)
	// as opposed to one the script raised with checkpoint().
	Auto bool
}

// Observer receives progress events. It is called from the script goroutines,
// from agent dispatch goroutines and from the goroutine calling Handle.Next or
// Handle.Resume, so implementations must be safe for concurrent use and must
// not block for long.
type Observer func(Event)
