package agent

import (
	"time"

	"spettro/internal/hooks"
	"spettro/internal/jobs"
	"spettro/internal/pty"
)

// Snapshots prepared while the model generates.
//
// A snapshot runs several git processes over the whole tree (tens of
// milliseconds on a small repository, hundreds on a large one), and taking
// it when the step's first mutating call arrived put that cost in front of
// every mutating step. Nothing is supposed to change the tree while the
// model generates a step, so the runtime asks the host to prepare the
// snapshot as soon as the step's request is sent (prepareStepCheckpoint),
// and the step's first mutating call claims it (claimSpeculativeLocked):
// recording a checkpoint entry, no git process. The claim falls back to
// the synchronous snapshot whenever the preparation cannot be trusted:
//
//   - a background job or pty session is running: it can change any file
//     at any time, so nothing is prepared and nothing is claimed;
//   - hooks are configured: a tool hook can change files right before the
//     mutating call, so the run never prepares ahead;
//   - preparing failed, or the host snapshotted or restored something in
//     between (a sub-agent sharing the checkout): the host's Claim reports
//     false;
//   - a file the agent knows (one it holds a stamp for: it read or wrote
//     it) changed after preparation started: the tracked files are then
//     re-staged on top of the preparation (ClaimRefreshed), which picks up
//     every change to a tracked file.
//
// A preparation nobody claimed is kept for the next step when that step
// only ran tools that cannot change the tree (treeReadOnlyCall) and it is
// younger than speculativeReuseMaxAge; otherwise the next step prepares
// anew and the host unpins the unclaimed one.
//
// Residual risk, accepted and documented in docs/checkpointing.md: a file
// the agent never touched that someone else creates or edits during that
// window is captured by the next checkpoint instead of this one, so
// rewinding to this checkpoint reverts that edit (or deletes that new
// file). The window is one step's generation time, at most
// speculativeReuseMaxAge when a preparation is reused.

// PreparedCheckpoint is a working-tree snapshot the host prepared ahead of
// a step's first mutating tool call (see LLMAgent.CheckpointPrepare).
type PreparedCheckpoint interface {
	// Claim records the prepared snapshot as the checkpoint before tool. It
	// reports false when the snapshot can no longer be used; the runtime
	// then snapshots synchronously through Checkpoint.
	Claim(tool string) bool
	// ClaimRefreshed is Claim after re-staging the tracked files: for when
	// a file the agent knows changed after the snapshot was prepared.
	ClaimRefreshed(tool string) bool
}

// speculativeReuseMaxAge bounds how long an unclaimed preparation is
// reused across read-only steps, and with it the window in which an
// outside edit to a file the agent never touched can be missed. Long
// enough to span a few quick read-only steps, short against the time a
// user takes to switch to an editor and change something.
const speculativeReuseMaxAge = 30 * time.Second

// speculativeCheckpoint is one prepared (or preparing) snapshot.
//
// Ordering: prepareStepCheckpoint runs on the run-loop goroutine before the
// step's request is sent and starts the host's preparation on a goroutine
// of its own, which stores prepared and then closes done. A claim runs on
// the goroutine of the step's first mutating call, holding
// stepCheckpointMu, and reads prepared only after done is closed. The run
// loop sends a request only after the previous step's tools returned, so a
// claim never overlaps a newer preparation of the same runtime.
type speculativeCheckpoint struct {
	started time.Time
	done    chan struct{}
	// prepared is the host's result; nil when preparing failed.
	prepared PreparedCheckpoint
	// known holds the identity of every stamped file, taken before the
	// preparation started; knownIncomplete is set when there were more
	// than maxShellStampFiles of them.
	known           map[string]knownFileState
	knownIncomplete bool
}

// knownFileState is a stamped file's identity, or its absence.
type knownFileState struct {
	id     fileIdentity
	exists bool
}

// prepareStepCheckpoint starts preparing this step's snapshot, unless an
// earlier preparation can be reused or preparing ahead is off.
func (r *toolRuntime) prepareStepCheckpoint() {
	if r.checkpoint == nil || r.checkpointPrepare == nil {
		return
	}
	r.stepCheckpointMu.Lock()
	reusable := r.speculative != nil && !r.speculativeDirty && r.speculative.reusable()
	r.stepCheckpointMu.Unlock()
	if reusable {
		return
	}
	var sc *speculativeCheckpoint
	if !backgroundWorkRunning() {
		sc = &speculativeCheckpoint{started: time.Now(), done: make(chan struct{})}
		sc.known, sc.knownIncomplete = r.knownFileStates()
		prepare := r.checkpointPrepare
		go func() {
			defer close(sc.done)
			sc.prepared = prepare()
		}()
	}
	r.stepCheckpointMu.Lock()
	r.speculative = sc
	r.speculativeDirty = false
	r.stepCheckpointMu.Unlock()
}

// reusable reports whether an unclaimed preparation may serve a later step:
// it is young enough and did not fail.
func (sc *speculativeCheckpoint) reusable() bool {
	if time.Since(sc.started) >= speculativeReuseMaxAge {
		return false
	}
	select {
	case <-sc.done:
		return sc.prepared != nil
	default:
		return true // still preparing
	}
}

// claimSpeculativeLocked claims the prepared snapshot as the checkpoint
// before tool and reports whether it did; false means the caller must
// snapshot synchronously. The caller holds stepCheckpointMu. A preparation
// is claimed at most once.
func (r *toolRuntime) claimSpeculativeLocked(tool string) bool {
	sc := r.speculative
	if sc == nil {
		return false
	}
	r.speculative = nil
	<-sc.done
	if sc.prepared == nil || backgroundWorkRunning() {
		return false
	}
	if !sc.knownIncomplete && r.knownFilesUnchanged(sc.known) {
		return sc.prepared.Claim(tool)
	}
	return sc.prepared.ClaimRefreshed(tool)
}

// noteTreeUse records that call is about to run: any call that might change
// the working tree makes an unclaimed preparation unusable for later steps.
func (r *toolRuntime) noteTreeUse(call toolCall) {
	if r.checkpointPrepare == nil || treeReadOnlyCall(call) {
		return
	}
	r.stepCheckpointMu.Lock()
	r.speculativeDirty = true
	r.stepCheckpointMu.Unlock()
}

// treeReadOnlyTools are the built-in tools that never write to the working
// tree. Anything else (including tools of the operator's own and MCP tools)
// is assumed to write.
var treeReadOnlyTools = map[string]bool{
	"file-read": true, "grep": true, "glob": true, "web-fetch": true, "web-search": true,
	"view-image": true, "ask-user": true, "tool-search": true, "skill": true, "todo-write": true,
	"job-output": true, "tool-output": true, "comment": true, "goal-complete": true,
}

// treeReadOnlyCall reports whether call (named by its built-in) cannot
// change the working tree: a read-only tool, or a shell call the checkpoint
// policy exempts (a provably read-only command, or polling a job).
func treeReadOnlyCall(call toolCall) bool {
	if treeReadOnlyTools[call.Tool] {
		return true
	}
	switch call.Tool {
	case "bash", "shell-exec":
		return !needsCheckpoint(call)
	}
	return false
}

// knownFileStates stats every file the agent holds a stamp for.
func (r *toolRuntime) knownFileStates() (map[string]knownFileState, bool) {
	r.mu.Lock()
	keys := make([]string, 0, min(len(r.fileStamps), maxShellStampFiles))
	incomplete := false
	for key := range r.fileStamps {
		if len(keys) == maxShellStampFiles {
			incomplete = true
			break
		}
		keys = append(keys, key)
	}
	r.mu.Unlock()
	states := make(map[string]knownFileState, len(keys))
	for _, key := range keys {
		id, ok := statIdentity(key)
		states[key] = knownFileState{id: id, exists: ok}
	}
	return states, incomplete
}

// knownFilesUnchanged reports whether every file in known still has the
// recorded identity (size, mtime, ctime, inode). A file first stamped after
// known was taken is not covered: like any file the agent had not touched,
// its state before preparation is unknown (see the residual risk above).
func (r *toolRuntime) knownFilesUnchanged(known map[string]knownFileState) bool {
	for key, before := range known {
		id, ok := statIdentity(key)
		if ok != before.exists || id != before.id {
			return false
		}
	}
	return true
}

// backgroundWorkRunning reports whether a background shell job or a pty
// session is running: either can change the tree at any moment.
func backgroundWorkRunning() bool {
	return jobs.Default().RunningCount() > 0 || pty.Default().RunningCount() > 0
}

// mayCheckpoint reports whether any allowed tool takes a checkpoint (a
// mutating built-in, or a tool that runs agents in this checkout), so that a
// read-only agent never prepares one.
func mayCheckpoint(allowed map[string]struct{}) bool {
	for tool := range allowed {
		switch name := CanonicalToolName(tool); {
		case isMutatingTool(name), name == "agent", name == ultraToolID, name == workflowToolID:
			return true
		}
	}
	return false
}

// hooksMayWriteFiles reports whether cfg has an enabled hook that runs
// around a tool call (before or after it, or on its approval): its command
// could change files between the preparation and the claim.
func hooksMayWriteFiles(cfg hooks.EffectiveConfig) bool {
	for _, rule := range cfg.Rules {
		if !rule.Enabled {
			continue
		}
		switch rule.Event {
		case hooks.EventPreToolUse, hooks.EventPostToolUse, hooks.EventPermissionRequest:
			return true
		}
	}
	return false
}
