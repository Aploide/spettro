package acp

// Session persistence and the ACP session lifecycle methods beyond
// session/new: session/load (replay a stored conversation), session/list
// (enumerate stored sessions), and session/resume (reattach without replay).
// Sessions are stored in the same on-disk store the TUI's /resume uses, so
// conversations started in either front-end are visible to both.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/session"
)

// maxACPHistoryBytes caps the flattened transcript injected on the first
// prompt turn after a load, mirroring the TUI's resume budget.
const maxACPHistoryBytes = 32 * 1024

// The SDK only dispatches session/load when the agent implements AgentLoader.
var _ acpsdk.AgentLoader = (*bridge)(nil)

// persistState snapshots the session's transcript under the bridge lock and
// returns the state to save. Saving itself happens outside the lock.
func (s *acpSession) persistState() session.State {
	return session.State{
		Metadata: session.Metadata{
			ID:          s.id,
			ProjectPath: s.cwd,
			ProjectHash: session.ProjectHash(s.cwd),
			StartedAt:   s.startedAt,
		},
		Messages: append([]session.Message(nil), s.transcript...),
	}
}

// flattenTranscript renders stored messages as the bounded "role: line"
// transcript the runtime's History field expects — the degraded fallback for
// the first turn after a load, before structured history exists again.
func flattenTranscript(msgs []session.Message) string {
	var lines []string
	for _, msg := range msgs {
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		switch msg.Role {
		case "user":
			lines = append(lines, "user: "+singleLineTranscript(content))
		case "assistant":
			lines = append(lines, "assistant: "+singleLineTranscript(content))
		}
	}
	// Keep the most recent turns within the byte cap (oldest dropped first).
	kept := make([]string, 0, len(lines))
	total := 0
	for _, line := range slices.Backward(lines) {
		size := len(line) + 1
		if total+size > maxACPHistoryBytes && len(kept) > 0 {
			break
		}
		kept = append(kept, line)
		total += size
	}
	for l, r := 0, len(kept)-1; l < r; l, r = l+1, r-1 {
		kept[l], kept[r] = kept[r], kept[l]
	}
	return strings.Join(kept, "\n")
}

// singleLineTranscript collapses a turn to a single bounded line so no entry
// dominates the history budget.
func singleLineTranscript(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const maxPerTurn = 4000
	if len(s) > maxPerTurn {
		s = s[:maxPerTurn] + " …(truncated)"
	}
	return s
}

// restoreSession loads a stored session and registers it as a live ACP
// session under its original ID. If the ID is already live on this
// connection, the in-memory session wins: it carries structured history the
// store does not, so replacing it would degrade the conversation.
func (b *bridge) restoreSession(sessionID acpsdk.SessionId, reqCwd string) (*acpSession, session.State, error) {
	b.mu.Lock()
	if existing, ok := b.sessions[string(sessionID)]; ok {
		b.mu.Unlock()
		return existing, session.State{Messages: existing.transcript}, nil
	}
	b.mu.Unlock()

	state, err := session.Load(b.opts.GlobalDir, string(sessionID))
	if err != nil {
		return nil, session.State{}, acpsdk.NewInvalidParams(map[string]any{"error": "session not found: " + string(sessionID)})
	}
	cwd := state.Metadata.ProjectPath
	if cwd == "" {
		cwd = reqCwd
	}
	if !filepath.IsAbs(cwd) {
		return nil, session.State{}, acpsdk.NewInvalidParams(map[string]any{"error": "cwd must be an absolute path"})
	}

	manifest, err := config.LoadAgentManifestForProject(cwd)
	if err != nil {
		manifest = b.opts.Manifest
	}
	agentID := manifest.DefaultAgent
	if agentID == "" {
		agentID = "plan"
	}
	startedAt := state.Metadata.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}

	s := &acpSession{
		id:         string(sessionID),
		cwd:        cwd,
		agentID:    agentID,
		manifest:   manifest,
		mediaDir:   filepath.Join(session.SessionDir(b.opts.GlobalDir, string(sessionID)), "acp-media"),
		transcript: state.Messages,
		startedAt:  startedAt,
	}
	b.mu.Lock()
	b.sessions[string(sessionID)] = s
	b.mu.Unlock()
	return s, state, nil
}

// LoadSession restores a stored session and replays its conversation to the
// client as session/update notifications before returning, per the ACP spec:
// the client rebuilds its transcript UI from the replayed stream.
func (b *bridge) LoadSession(ctx context.Context, params acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	s, state, err := b.restoreSession(params.SessionId, params.Cwd)
	if err != nil {
		return acpsdk.LoadSessionResponse{}, err
	}

	for _, msg := range state.Messages {
		content := strings.TrimSpace(msg.Content)
		switch msg.Role {
		case "user":
			if content == "" {
				continue
			}
			_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
				SessionId: params.SessionId,
				Update:    acpsdk.UpdateUserMessageText(content),
			})
		case "assistant":
			if thinking := strings.TrimSpace(msg.Thinking); thinking != "" {
				_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
					SessionId: params.SessionId,
					Update:    acpsdk.UpdateAgentThoughtText(thinking),
				})
			}
			if content == "" {
				continue
			}
			_ = b.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
				SessionId: params.SessionId,
				Update:    acpsdk.UpdateAgentMessageText(content),
			})
		}
	}

	cfg := b.opts.Cfg
	if fresh, err := config.LoadFull(); err == nil {
		cfg = fresh
		b.opts.Providers.SetAPIKeys(cfg.APIKeys)
	}

	// Same ordering hazard as NewSession: defer the commands announcement past
	// the response so clients that register the session on response arrival do
	// not drop it. Prompt re-announces as a fallback.
	go func() {
		time.Sleep(200 * time.Millisecond)
		b.announceCommands(context.Background(), params.SessionId)
	}()

	b.mu.Lock()
	options := buildConfigOptions(s, &cfg, b.opts.Providers)
	b.mu.Unlock()
	return acpsdk.LoadSessionResponse{ConfigOptions: options}, nil
}

// ResumeSession reattaches to a stored session without transcript replay:
// the client declares it already holds the conversation view.
func (b *bridge) ResumeSession(_ context.Context, params acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	s, _, err := b.restoreSession(params.SessionId, params.Cwd)
	if err != nil {
		return acpsdk.ResumeSessionResponse{}, err
	}

	cfg := b.opts.Cfg
	if fresh, err := config.LoadFull(); err == nil {
		cfg = fresh
		b.opts.Providers.SetAPIKeys(cfg.APIKeys)
	}
	b.mu.Lock()
	options := buildConfigOptions(s, &cfg, b.opts.Providers)
	b.mu.Unlock()
	return acpsdk.ResumeSessionResponse{ConfigOptions: options}, nil
}

// ListSessions enumerates the on-disk session store, optionally filtered to
// the requested working directory, newest first. Pagination is not needed at
// this scale, so every response is a single page.
func (b *bridge) ListSessions(_ context.Context, params acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	entries, err := os.ReadDir(session.SessionsDir(b.opts.GlobalDir))
	if err != nil && !os.IsNotExist(err) {
		return acpsdk.ListSessionsResponse{}, err
	}

	var metas []session.Metadata
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := session.LoadMetadata(b.opts.GlobalDir, entry.Name())
		if err != nil {
			continue
		}
		if params.Cwd != nil {
			if meta.ProjectPath != *params.Cwd && meta.ProjectHash != session.ProjectHash(*params.Cwd) {
				continue
			}
		}
		metas = append(metas, meta)
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
	})

	sessions := make([]acpsdk.SessionInfo, 0, len(metas))
	for _, meta := range metas {
		info := acpsdk.SessionInfo{
			SessionId: acpsdk.SessionId(meta.ID),
			Cwd:       meta.ProjectPath,
		}
		if meta.Preview != "" {
			info.Title = new(meta.Preview)
		}
		if !meta.UpdatedAt.IsZero() {
			info.UpdatedAt = new(meta.UpdatedAt.Format(time.RFC3339))
		}
		sessions = append(sessions, info)
	}
	return acpsdk.ListSessionsResponse{Sessions: sessions}, nil
}
