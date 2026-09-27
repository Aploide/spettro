package acp

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
	"spettro/internal/session"
)

// turnState carries the per-prompt streaming/tool bookkeeping shared by the
// LLMAgent callbacks. Callbacks fire concurrently (tools run in parallel
// goroutines), so all mutable state is behind mu.
type turnState struct {
	bridge    *bridge
	ctx       context.Context
	sessionID acpsdk.SessionId
	// cwd is the session's working directory. Relative paths in tool
	// arguments are resolved against it, because ACP locations must be
	// absolute.
	cwd string
	// agentID is the agent the turn runs (the session's mode). Only its own
	// words reach the chat as agent messages; a sub-agent's narration would
	// read as the main agent talking.
	agentID string

	mu  sync.Mutex
	seq int
	// open maps a running tool call (agent, name and args) to the ACP tool
	// calls announced for it, oldest first, so the completion trace, which
	// repeats agent, name and args, updates the right card. Identical calls
	// running at once share a key and complete in announcement order.
	open map[string][]openToolCall
	// awaiting holds the cards currently showing a permission prompt (see
	// approvalToolCallID and settleApprovalCard).
	awaiting map[acpsdk.ToolCallId]bool
	// lastNarration is the most recent narration of the turn's own agent
	// sent to the chat, so a run's final content that repeats it is not
	// sent twice (see repeatsNarration).
	lastNarration string
	// workflow is the in-flight workflow run whose tool call is rewritten as
	// the run progresses; nil outside a workflow.
	workflow *acpWorkflow
}

// openToolCall is a tool call the editor has been told is running.
type openToolCall struct {
	id acpsdk.ToolCallId
	// seq orders calls by announcement; the most recent call wins when a
	// permission request cannot tell candidates apart (approvalToolCallID).
	seq int
	// agentID is the agent that made the call, as its trace names it;
	// name is the tool's canonical name and args its decoded arguments (nil
	// when not JSON). The approval flow matches on all three.
	agentID string
	name    string
	args    toolArgs
}

// sessionUpdate sends a session/update notification, dropping it silently if
// the turn has been cancelled (the connection may be mid-teardown).
func (t *turnState) sessionUpdate(update acpsdk.SessionUpdate) {
	if t.ctx.Err() != nil {
		return
	}
	_ = t.bridge.conn.SessionUpdate(t.ctx, acpsdk.SessionNotification{
		SessionId: t.sessionID,
		Update:    update,
	})
}

// onStream forwards thinking deltas as agent_thought_chunk notifications.
// Answer chunks are NOT streamed: in Spettro's stream protocol they form a
// replaceable draft (a Reset discards the current draft at step boundaries),
// which cannot be expressed over ACP's append-only message chunks — streaming
// them would duplicate intermediate step prose. The authoritative answer is
// sent once from RunResult.Content when the turn completes.
func (t *turnState) onStream(c agent.StreamChunk) {
	if c.Kind == agent.StreamKindThinking && c.Delta != "" {
		t.sessionUpdate(acpsdk.UpdateAgentThoughtText(c.Delta))
	}
}

// onUsage forwards per-request token accounting as an ACP usage_update
// notification so the client can render a live context gauge while the run is
// still executing. Used carries the context occupancy (largest single
// request), matching what the size-relative gauge needs; the cumulative turn
// cost travels in the final PromptResponse.Usage instead.
func (t *turnState) onUsage(ev agent.UsageEvent, contextWindow int) {
	if contextWindow <= 0 {
		contextWindow = 128000 // same fallback the in-loop compactor assumes
	}
	t.sessionUpdate(acpsdk.SessionUpdate{
		UsageUpdate: &acpsdk.SessionUsageUpdate{
			Size: contextWindow,
			Used: ev.ContextTokens,
			Meta: map[string]any{"spettro.app/tokensUsed": ev.TotalTokens},
		},
	})
}

// onTool translates ToolTrace events into ACP tool_call / tool_call_update
// notifications:
//
//   - a "running" trace announces a tool_call (in_progress) with its kind,
//     title, locations and rawInput (see tools.go for how each is derived
//     and bounded);
//   - the completion trace ("success" or "error") sends a tool_call_update
//     with the final status and the result as content: a diff per file the
//     call changed, then the text output and any attached images;
//   - "comment" traces never become cards: see onComment;
//   - "approval" traces record a decision the editor either made itself
//     (session/request_permission) or sees as the tool call failing with
//     the policy's reason, so they are dropped.
func (t *turnState) onTool(tr agent.ToolTrace) {
	switch tr.Name {
	case "comment":
		t.onComment(tr)
		return
	case "approval":
		return
	}
	// Workflow traces drive a single long-lived tool call; the helper reports
	// whether the trace has been fully consumed by it.
	if t.onWorkflowTool(tr) {
		return
	}
	key := tr.AgentID + "\x00" + tr.Name + "\x00" + tr.Args
	if tr.Status == "running" {
		t.mu.Lock()
		call := openToolCall{
			id:      t.nextToolCallIDLocked("call"),
			seq:     t.seq,
			agentID: tr.AgentID,
			name:    agent.CanonicalToolName(tr.Name),
			args:    decodeToolArgs(tr.Args),
		}
		if t.open == nil {
			t.open = map[string][]openToolCall{}
		}
		t.open[key] = append(t.open[key], call)
		t.mu.Unlock()
		t.sessionUpdate(acpsdk.StartToolCall(
			call.id,
			toolCallTitle(tr),
			acpsdk.WithStartKind(toolKind(tr.Name)),
			acpsdk.WithStartStatus(acpsdk.ToolCallStatusInProgress),
			acpsdk.WithStartLocations(toolLocations(tr.Args, t.cwd)),
			acpsdk.WithStartRawInput(boundedRawInput(tr.Args)),
		))
		return
	}

	status := acpsdk.ToolCallStatusCompleted
	if tr.Status == "error" {
		status = acpsdk.ToolCallStatusFailed
	}
	content := append(fileChangeContent(tr.FileChanges), toolOutputContent(tr.Output, tr.Images)...)
	rawOutput := map[string]any{"output": clipBytes(tr.Output, maxToolTextBytes)}

	t.mu.Lock()
	queue := t.open[key]
	var id acpsdk.ToolCallId
	known := len(queue) > 0
	if known {
		id = queue[0].id
		if len(queue) == 1 {
			delete(t.open, key)
		} else {
			t.open[key] = queue[1:]
		}
	}
	t.mu.Unlock()

	if !known {
		// Completion without a matching start (a call rejected before it
		// ran, e.g. a retired name whose arguments do not convert): emit a
		// single already-finished tool call.
		t.sessionUpdate(acpsdk.StartToolCall(
			t.nextToolCallID("call"),
			toolCallTitle(tr),
			acpsdk.WithStartKind(toolKind(tr.Name)),
			acpsdk.WithStartStatus(status),
			acpsdk.WithStartLocations(toolLocations(tr.Args, t.cwd)),
			acpsdk.WithStartRawInput(boundedRawInput(tr.Args)),
			acpsdk.WithStartContent(content),
			acpsdk.WithStartRawOutput(rawOutput),
		))
	} else {
		opts := []acpsdk.ToolCallUpdateOpt{
			acpsdk.WithUpdateStatus(status),
			acpsdk.WithUpdateContent(content),
			acpsdk.WithUpdateRawOutput(rawOutput),
		}
		if len(tr.FileChanges) > 0 {
			// The changed files' absolute paths are authoritative (a sub-agent
			// in a worktree names paths relative to the worktree), so they
			// replace the locations guessed from the arguments.
			opts = append(opts, acpsdk.WithUpdateLocations(fileChangeLocations(tr.FileChanges)))
		}
		t.sessionUpdate(acpsdk.UpdateToolCall(id, opts...))
	}
	if status == acpsdk.ToolCallStatusCompleted {
		t.publishPlanIfTaskTool(tr.Name)
	}
}

// onComment handles "comment" traces. Three of them are words for the user
// and are sent as agent_message_chunk text, each ending in a blank line so
// the next message or tool card starts on its own:
//
//   - narration, the prose the model wrote in a step that also called tools
//     (without this the editor would only ever see the final answer);
//   - a call of the comment tool, whose message the model addressed to the
//     user ("one short line shown to the user");
//   - the note that a mid-run steering message reached the model.
//
// Only the turn's own agent speaks in the chat. A sub-agent's narration and
// comment-tool messages would read as the main agent talking, and its
// steering notes are not the user's: a sub-agent's private steering queue
// carries only the runtime's time-limit wrap-up notice. All of those are
// dropped, like the runtime's own progress notes ("Starting bash (...)"),
// which repeat what the tool cards already show.
func (t *turnState) onComment(tr agent.ToolTrace) {
	text := t.commentChatText(tr)
	if text == "" {
		return
	}
	if tr.Narration {
		t.mu.Lock()
		t.lastNarration = text
		t.mu.Unlock()
	}
	t.sessionUpdate(acpsdk.UpdateAgentMessageText(text + "\n\n"))
}

// commentChatText is the chat message a "comment" trace becomes, or "" when
// it stays out of the chat (see onComment for which ones are shown).
func (t *turnState) commentChatText(tr agent.ToolTrace) string {
	if t.agentID != "" && tr.AgentID != t.agentID {
		// A sub-agent's comment: never the turn's own words.
		return ""
	}
	text := ""
	switch {
	case strings.HasPrefix(tr.Output, "steering delivered"):
		text = "✔ " + tr.Output
	case tr.Narration:
		text = tr.Output
	case tr.Status == "running":
		// Only a call of the comment tool announces itself as running; the
		// runtime's notes arrive already finished.
		text = decodeToolArgs(tr.Args).str("message")
	}
	return strings.TrimSpace(text)
}

// repeatsNarration reports whether content is the narration the chat was
// sent last. A /goal run that ends with goal-complete and no summary
// returns the prose of its last step as its content, and that prose was
// already narrated.
func (t *turnState) repeatsNarration(content string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastNarration != "" && t.lastNarration == strings.TrimSpace(content)
}

// publishPlanIfTaskTool mirrors the persistent session task graph to the ACP
// client as a plan update whenever a task-mutating tool succeeds, so editors
// render the agent's live task list.
func (t *turnState) publishPlanIfTaskTool(toolName string) {
	if agent.CanonicalToolName(toolName) != "todo-write" {
		return
	}
	todos, err := session.LoadTodos(t.bridge.opts.GlobalDir, string(t.sessionID))
	if err != nil {
		return
	}
	// An empty list is still published: deleting the last task must clear the
	// editor's plan view rather than leave it stale.
	t.sessionUpdate(acpsdk.UpdatePlan(planEntriesFromTodos(todos)...))
}

// planEntriesFromTodos maps the session task graph onto ACP plan entries in
// dependency order, folding the derived blocked state into the entry text
// (ACP plans have no blocked status).
func planEntriesFromTodos(todos []session.Todo) []acpsdk.PlanEntry {
	byID := make(map[string]session.Todo, len(todos))
	for _, td := range todos {
		byID[td.ID] = td
	}
	blocked := session.BlockedIDs(todos)
	entries := make([]acpsdk.PlanEntry, 0, len(todos))
	for _, id := range session.TopoOrder(todos) {
		td := byID[id]
		st := acpsdk.PlanEntryStatusPending
		switch td.Status {
		case session.TaskStatusInProgress:
			st = acpsdk.PlanEntryStatusInProgress
		case session.TaskStatusCompleted, session.TaskStatusCancelled:
			st = acpsdk.PlanEntryStatusCompleted
		}
		pr := acpsdk.PlanEntryPriorityMedium
		switch strings.ToLower(td.Priority) {
		case "high", "urgent":
			pr = acpsdk.PlanEntryPriorityHigh
		case "low":
			pr = acpsdk.PlanEntryPriorityLow
		}
		content := td.Content
		if _, gated := blocked[td.ID]; gated && st == acpsdk.PlanEntryStatusPending {
			content += " (blocked)"
		}
		entries = append(entries, acpsdk.PlanEntry{Content: content, Priority: pr, Status: st})
	}
	return entries
}

func (t *turnState) nextToolCallID(prefix string) acpsdk.ToolCallId {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nextToolCallIDLocked(prefix)
}

// nextToolCallIDLocked is the same, for callers already holding t.mu.
func (t *turnState) nextToolCallIDLocked(prefix string) acpsdk.ToolCallId {
	t.seq++
	return acpsdk.ToolCallId(fmt.Sprintf("%s-%d", prefix, t.seq))
}

// promptContent is an ACP prompt's content blocks, read. The typed text and
// the attached contexts are kept apart because a skill command must be
// parsed from what the user typed alone: an attached file is context for
// the turn, never part of the skill's arguments (see Prompt).
type promptContent struct {
	// typed is what the user typed: the text blocks in order, each resource
	// link written as an @path mention.
	typed string
	// contexts are the files the editor embedded as resources, each
	// rendered as a "Context from <path>:" fenced block.
	contexts []string
	// images are the image blocks, decoded to files for the vision channel.
	images []string
	// mentioned are the resource links' paths (the run's RequiredReads).
	mentioned []string
}

// task is the prompt the model receives for this content: the typed text,
// then the attached contexts.
func (p promptContent) task() string {
	return p.withContexts(p.typed)
}

// withContexts appends the attached contexts to text, which is the typed
// text or a prompt built from it (a skill's instructions).
func (p promptContent) withContexts(text string) string {
	if len(p.contexts) == 0 {
		return text
	}
	return strings.TrimSpace(text) + "\n\n" + strings.Join(p.contexts, "\n\n")
}

// readPromptContent reads the ACP prompt content blocks: text blocks in
// order, resource links surfaced as @-mentions (and RequiredReads),
// embedded text resources kept as fenced context, images decoded to files
// in mediaDir.
func readPromptContent(blocks []acpsdk.ContentBlock, mediaDir string) (promptContent, error) {
	var p promptContent
	var text strings.Builder
	imgN := 0
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			text.WriteString(block.Text.Text)
		case block.ResourceLink != nil:
			path := uriToPath(block.ResourceLink.Uri)
			text.WriteString("@")
			text.WriteString(path)
			p.mentioned = append(p.mentioned, path)
		case block.Resource != nil:
			if tr := block.Resource.Resource.TextResourceContents; tr != nil {
				p.contexts = append(p.contexts, fmt.Sprintf("Context from %s:\n```\n%s\n```", uriToPath(tr.Uri), tr.Text))
			}
		case block.Image != nil:
			if block.Image.Data == "" {
				continue
			}
			if err := ensureMediaDir(mediaDir); err != nil {
				return promptContent{}, fmt.Errorf("media dir: %w", err)
			}
			raw, derr := base64.StdEncoding.DecodeString(block.Image.Data)
			if derr != nil {
				return promptContent{}, fmt.Errorf("decode image: %w", derr)
			}
			imgN++
			path := filepath.Join(mediaDir, fmt.Sprintf("prompt-img-%d%s", imgN, imageExt(block.Image.MimeType)))
			if werr := os.WriteFile(path, raw, 0o600); werr != nil {
				return promptContent{}, fmt.Errorf("write image: %w", werr)
			}
			p.images = append(p.images, path)
		}
	}
	p.typed = text.String()
	return p, nil
}

func uriToPath(uri string) string {
	return strings.TrimPrefix(uri, "file://")
}

func imageExt(mime string) string {
	switch mime {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}
