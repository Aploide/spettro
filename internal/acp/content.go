package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
)

// turnState carries the per-prompt streaming/tool bookkeeping shared by the
// LLMAgent callbacks. Callbacks fire concurrently (tools run in parallel
// goroutines), so all mutable state is behind mu.
type turnState struct {
	bridge    *bridge
	ctx       context.Context
	sessionID acpsdk.SessionId

	mu  sync.Mutex
	seq int
	// open maps a running tool call (name+args) to its ACP tool call IDs so
	// the completion trace, which repeats name+args, updates the right call.
	open map[string][]acpsdk.ToolCallId
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

// onTool translates ToolTrace events into ACP tool_call / tool_call_update
// notifications. "comment" traces are transient progress notes already
// covered by the tool updates themselves, so they are dropped.
func (t *turnState) onTool(tr agent.ToolTrace) {
	if tr.Name == "comment" {
		return
	}
	key := tr.AgentID + "\x00" + tr.Name + "\x00" + tr.Args
	if tr.Status == "running" {
		id := t.nextToolCallID("call")
		t.mu.Lock()
		t.open[key] = append(t.open[key], id)
		t.mu.Unlock()
		t.sessionUpdate(acpsdk.StartToolCall(
			id,
			toolCallTitle(tr),
			acpsdk.WithStartKind(toolKind(tr.Name)),
			acpsdk.WithStartStatus(acpsdk.ToolCallStatusInProgress),
			acpsdk.WithStartLocations(toolLocations(tr.Args)),
			acpsdk.WithStartRawInput(rawJSON(tr.Args)),
		))
		return
	}

	status := acpsdk.ToolCallStatusCompleted
	if tr.Status == "error" {
		status = acpsdk.ToolCallStatusFailed
	}
	t.mu.Lock()
	stack := t.open[key]
	var id acpsdk.ToolCallId
	known := len(stack) > 0
	if known {
		id = stack[len(stack)-1]
		t.open[key] = stack[:len(stack)-1]
	}
	t.mu.Unlock()

	if !known {
		// Completion without a matching start (e.g. a call rejected before
		// execution): emit a single already-finished tool call.
		t.sessionUpdate(acpsdk.StartToolCall(
			t.nextToolCallID("call"),
			toolCallTitle(tr),
			acpsdk.WithStartKind(toolKind(tr.Name)),
			acpsdk.WithStartStatus(status),
			acpsdk.WithStartLocations(toolLocations(tr.Args)),
			acpsdk.WithStartRawInput(rawJSON(tr.Args)),
			acpsdk.WithStartContent(toolOutputContent(tr.Output)),
		))
		return
	}
	t.sessionUpdate(acpsdk.UpdateToolCall(
		id,
		acpsdk.WithUpdateStatus(status),
		acpsdk.WithUpdateContent(toolOutputContent(tr.Output)),
		acpsdk.WithUpdateRawOutput(map[string]any{"output": tr.Output}),
	))
}

func (t *turnState) nextToolCallID(prefix string) acpsdk.ToolCallId {
	t.mu.Lock()
	t.seq++
	n := t.seq
	t.mu.Unlock()
	return acpsdk.ToolCallId(fmt.Sprintf("%s-%d", prefix, n))
}

// openToolCallID returns the most recent in-flight tool call for the given
// tool name (used to attach a shell permission request to the tool call the
// editor is already rendering), or a fresh ID when none is open.
func (t *turnState) openToolCallID(toolName string) acpsdk.ToolCallId {
	t.mu.Lock()
	for key, stack := range t.open {
		parts := strings.SplitN(key, "\x00", 3)
		if len(parts) == 3 && parts[1] == toolName && len(stack) > 0 {
			id := stack[len(stack)-1]
			t.mu.Unlock()
			return id
		}
	}
	t.mu.Unlock()
	return t.nextToolCallID("perm")
}

func toolCallTitle(tr agent.ToolTrace) string {
	title := tr.Name
	if tr.Args != "" {
		args := tr.Args
		const maxArgs = 120
		if len(args) > maxArgs {
			args = args[:maxArgs] + "…"
		}
		title += " " + args
	}
	return title
}

// toolKind classifies Spettro tool IDs into ACP tool kinds so editors pick
// the right icon/affordance.
func toolKind(name string) acpsdk.ToolKind {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "edit"), strings.Contains(n, "write"), strings.Contains(n, "patch"):
		return acpsdk.ToolKindEdit
	case strings.Contains(n, "read"):
		return acpsdk.ToolKindRead
	case strings.Contains(n, "search"), strings.Contains(n, "grep"), strings.Contains(n, "glob"), n == "ls":
		return acpsdk.ToolKindSearch
	case strings.Contains(n, "shell"), strings.Contains(n, "bash"), strings.Contains(n, "exec"):
		return acpsdk.ToolKindExecute
	case strings.Contains(n, "http"), strings.Contains(n, "fetch"), strings.Contains(n, "web"):
		return acpsdk.ToolKindFetch
	case strings.Contains(n, "agent"), strings.Contains(n, "todo"), strings.Contains(n, "plan"):
		return acpsdk.ToolKindThink
	default:
		return acpsdk.ToolKindOther
	}
}

// toolLocations extracts a file path from the tool's JSON args, if present,
// so "follow the agent" editors can jump to the file being touched.
func toolLocations(args string) []acpsdk.ToolCallLocation {
	m, ok := rawJSON(args).(map[string]any)
	if !ok {
		return nil
	}
	for _, k := range []string{"path", "file", "file_path", "filename"} {
		if p, ok := m[k].(string); ok && p != "" {
			return []acpsdk.ToolCallLocation{{Path: p}}
		}
	}
	return nil
}

// rawJSON parses the single-line args string for rawInput; on failure the
// original string is passed through so nothing is lost.
func rawJSON(args string) any {
	if args == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return args
	}
	return v
}

func toolOutputContent(output string) []acpsdk.ToolCallContent {
	if output == "" {
		return nil
	}
	return []acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(output))}
}

// promptFromBlocks flattens the ACP prompt content into the single task
// string LLMAgent consumes: text blocks in order, resource links surfaced as
// @-mentions (and RequiredReads), embedded text resources appended as fenced
// context, images decoded to files for the vision channel.
func promptFromBlocks(blocks []acpsdk.ContentBlock, mediaDir string) (task string, images []string, mentioned []string, err error) {
	var text strings.Builder
	var contexts []string
	imgN := 0
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			text.WriteString(block.Text.Text)
		case block.ResourceLink != nil:
			path := uriToPath(block.ResourceLink.Uri)
			text.WriteString("@")
			text.WriteString(path)
			mentioned = append(mentioned, path)
		case block.Resource != nil:
			if tr := block.Resource.Resource.TextResourceContents; tr != nil {
				contexts = append(contexts, fmt.Sprintf("Context from %s:\n```\n%s\n```", uriToPath(tr.Uri), tr.Text))
			}
		case block.Image != nil:
			if block.Image.Data == "" {
				continue
			}
			if err := ensureMediaDir(mediaDir); err != nil {
				return "", nil, nil, fmt.Errorf("media dir: %w", err)
			}
			raw, derr := base64.StdEncoding.DecodeString(block.Image.Data)
			if derr != nil {
				return "", nil, nil, fmt.Errorf("decode image: %w", derr)
			}
			imgN++
			p := filepath.Join(mediaDir, fmt.Sprintf("prompt-img-%d%s", imgN, imageExt(block.Image.MimeType)))
			if werr := os.WriteFile(p, raw, 0o600); werr != nil {
				return "", nil, nil, fmt.Errorf("write image: %w", werr)
			}
			images = append(images, p)
		}
	}
	task = text.String()
	if len(contexts) > 0 {
		task = strings.TrimSpace(task) + "\n\n" + strings.Join(contexts, "\n\n")
	}
	return task, images, mentioned, nil
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
