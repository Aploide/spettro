package tui

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"spettro/internal/agent"
	"spettro/internal/remote"
)

// remoteSubmitMsg is delivered to Update when an HTTP client posts a prompt
// to /messages. The TUI must reply on req.Reply exactly once.
type remoteSubmitMsg struct {
	req remote.SubmitRequest
}

// remoteInterruptMsg is sent by the remote server when an HTTP client posts
// to /interrupt. The TUI cancels any active run.
type remoteInterruptMsg struct{}

// waitForRemoteSubmit re-arms a cmd that consumes one submission from the
// server channel. We re-issue it after each delivery so the loop continues.
func waitForRemoteSubmit(server *remote.Server) tea.Cmd {
	if server == nil {
		return nil
	}
	ch := server.Submissions()
	return func() tea.Msg {
		req, ok := <-ch
		if !ok {
			return nil
		}
		return remoteSubmitMsg{req: req}
	}
}

// waitForRemoteInterrupt re-arms a cmd that consumes one interrupt signal.
func waitForRemoteInterrupt(server *remote.Server) tea.Cmd {
	if server == nil {
		return nil
	}
	ch := server.Interrupts()
	return func() tea.Msg {
		_, ok := <-ch
		if !ok {
			return nil
		}
		return remoteInterruptMsg{}
	}
}

// remoteListenCmds returns the pair of cmds that pump submissions and
// interrupts from the remote server into the bubbletea program loop.
func remoteListenCmds(server *remote.Server) []tea.Cmd {
	if server == nil {
		return nil
	}
	return []tea.Cmd{
		waitForRemoteSubmit(server),
		waitForRemoteInterrupt(server),
	}
}

// handleRemoteCommand implements `/remote`, `/remote :PORT`, `/remote local`,
// and `/remote stop`.
//
// All branches must call m.refreshViewport() before returning because the
// outer handleCommand dispatch returns early for /remote and so doesn't run
// the trailing refresh itself. Without this the system message that contains
// the bearer token would only become visible after the next event repaints
// the viewport.
func (m Model) handleRemoteCommand(input string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(input)
	if len(fields) >= 2 {
		switch strings.ToLower(fields[1]) {
		case "stop", "off", "shutdown":
			return m.stopRemote()
		case "status":
			return m.printRemoteStatus()
		}
	}

	if m.remoteServer != nil {
		m.showBanner(fmt.Sprintf("remote already listening on %s — use /remote stop first", m.remoteAddress()), "warn")
		m.refreshViewport()
		return m, nil
	}

	preferredPort := 0
	preferredHost := "127.0.0.1"
	if len(fields) >= 2 {
		switch strings.ToLower(fields[1]) {
		case "local":
			preferredHost = "0.0.0.0"
			if len(fields) >= 3 {
				_, port, err := parseRemoteArg(fields[2])
				if err != nil {
					m.showBanner("remote: "+err.Error(), "error")
					m.refreshViewport()
					return m, nil
				}
				preferredPort = port
			}
		default:
			host, port, err := parseRemoteArg(fields[1])
			if err != nil {
				m.showBanner("remote: "+err.Error(), "error")
				m.refreshViewport()
				return m, nil
			}
			preferredPort = port
			preferredHost = host
		}
	}

	server, err := remote.NewServer(remote.Options{BindHost: preferredHost})
	if err != nil {
		m.showBanner("remote: "+err.Error(), "error")
		m.refreshViewport()
		return m, nil
	}
	server.SetStatus(m.remoteStatusSnapshot())

	port, fellBack, err := server.Start(preferredPort)
	if err != nil {
		m.showBanner("remote: "+err.Error(), "error")
		m.refreshViewport()
		return m, nil
	}

	m.remoteServer = server
	m.remoteRequestedPort = preferredPort

	// When binding to all interfaces, show the detected LAN IP so it can be
	// pasted directly into Spettro Remote on Android.
	displayHost := server.Host()
	if displayHost == "0.0.0.0" {
		if lan := detectLANIP(); lan != "" {
			displayHost = lan
		}
	}

	if fellBack {
		switch {
		case preferredPort > 0:
			m.showBanner(fmt.Sprintf("port %d unavailable — listening on %d instead", preferredPort, port), "warn")
		default:
			m.showBanner(fmt.Sprintf("default port %d unavailable — listening on %d instead", remote.DefaultPort, port), "warn")
		}
	} else {
		m.showBanner(fmt.Sprintf("remote listening on http://%s:%d", displayHost, port), "success")
	}

	addr := fmt.Sprintf("http://%s:%d", displayHost, port)
	lanNote := ""
	if server.Host() == "0.0.0.0" {
		lanNote = "\n  lan:    accessible from other devices on this network"
	}
	m.pushSystemMsg(strings.Join([]string{
		"remote control enabled",
		"  url:    " + addr + lanNote,
		"  token:  " + server.Token(),
		"  send:   POST " + addr + "/messages    {\"message\":\"...\"}",
		"  events: GET  " + addr + "/events     (text/event-stream)",
		"  status: GET  " + addr + "/status",
		"  cancel: POST " + addr + "/interrupt",
		"  stop:   /remote stop",
		"  auth:   Authorization: Bearer <token>  (or ?token=<token>)",
	}, "\n"))

	server.Publish("remote_started", map[string]interface{}{
		"port":         port,
		"requested":    preferredPort,
		"fell_back":    fellBack,
		"default_port": remote.DefaultPort,
		"started_at":   time.Now(),
	})

	m.refreshViewport()
	cmds := remoteListenCmds(server)
	return m, tea.Batch(cmds...)
}

func (m Model) stopRemote() (tea.Model, tea.Cmd) {
	if m.remoteServer == nil {
		m.showBanner("remote not running", "info")
		m.refreshViewport()
		return m, nil
	}
	addr := m.remoteAddress()
	// Publish before tearing down so any subscribed client sees the
	// shutdown notice in the same SSE response stream.
	m.remoteServer.Publish("remote_stopped", map[string]interface{}{"address": addr})
	_ = m.remoteServer.Stop()
	m.remoteServer = nil
	m.remoteRequestedPort = 0
	m.showBanner("remote stopped ("+addr+")", "success")
	m.refreshViewport()
	return m, nil
}

func (m Model) printRemoteStatus() (tea.Model, tea.Cmd) {
	if m.remoteServer == nil {
		m.pushSystemMsg("remote: not running")
		m.refreshViewport()
		return m, nil
	}
	host := m.remoteServer.Host()
	if host == "0.0.0.0" {
		if lan := detectLANIP(); lan != "" {
			host = lan
		}
	}
	port := m.remoteServer.Port()
	m.pushSystemMsg(strings.Join([]string{
		"remote: running",
		fmt.Sprintf("  url:   http://%s:%d", host, port),
		"  token: " + m.remoteServer.Token(),
	}, "\n"))
	m.refreshViewport()
	return m, nil
}

func (m Model) remoteAddress() string {
	if m.remoteServer == nil {
		return ""
	}
	host := m.remoteServer.Host()
	if host == "0.0.0.0" {
		if lan := detectLANIP(); lan != "" {
			host = lan
		}
	}
	return fmt.Sprintf("http://%s:%d", host, m.remoteServer.Port())
}

// parseRemoteArg accepts ":7878", "7878", or "0.0.0.0:7878" and returns
// (host, port). host defaults to "127.0.0.1" when no host is specified.
func parseRemoteArg(arg string) (host string, port int, err error) {
	arg = strings.TrimSpace(arg)
	// HOST:PORT form
	if strings.Contains(arg, ".") || strings.Contains(arg, "[") {
		h, p, splitErr := net.SplitHostPort(arg)
		if splitErr != nil {
			return "", 0, fmt.Errorf("invalid address %q (use HOST:PORT or :PORT)", arg)
		}
		v, convErr := strconv.Atoi(p)
		if convErr != nil || v <= 0 || v > 65535 {
			return "", 0, fmt.Errorf("invalid port %q", p)
		}
		return h, v, nil
	}
	// :PORT or PORT form
	arg = strings.TrimPrefix(arg, ":")
	if arg == "" {
		return "", 0, fmt.Errorf("missing port number")
	}
	v, convErr := strconv.Atoi(arg)
	if convErr != nil {
		return "", 0, fmt.Errorf("invalid port %q", arg)
	}
	if v <= 0 || v > 65535 {
		return "", 0, fmt.Errorf("port out of range: %d", v)
	}
	return "127.0.0.1", v, nil
}

// detectLANIP returns the machine's outbound LAN IP by probing a UDP route.
// No actual packet is sent; this just asks the OS which source IP it would use.
func detectLANIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// handleRemoteSubmission accepts a prompt that arrived through the remote
// HTTP API and routes it through the same handlePrompt / handleCommand
// pipeline used for local input. Reply is always sent on req.Reply (exactly
// once) so the HTTP handler can return.
func (m Model) handleRemoteSubmission(req remote.SubmitRequest) (tea.Model, tea.Cmd) {
	reply := req.Reply
	text := strings.TrimSpace(req.Message)
	if text == "" {
		sendRemoteReply(reply, remote.SubmitResponse{Accepted: false, Error: "empty message"})
		return m, nil
	}

	// Slash commands: only honored when no run is in flight (mirrors local
	// behaviour — see handleCommand wiring in updateMain).
	if strings.HasPrefix(text, "/") {
		if m.thinking {
			sendRemoteReply(reply, remote.SubmitResponse{
				Accepted: false,
				Error:    "commands cannot be queued while an agent is running",
			})
			return m, nil
		}
		m.publishRemote("remote_command", map[string]interface{}{"command": text})
		sendRemoteReply(reply, remote.SubmitResponse{Accepted: true, Note: "command dispatched"})
		return m.handleCommand(text)
	}

	// Plain prompt: synthesise a user-message echo and forward through the
	// regular routing.
	m.publishRemote("remote_prompt", map[string]interface{}{"prompt": text})

	if m.thinking {
		mentionedFiles := m.extractMentionedFiles(text)
		prompt := injectMentionGuidance(text, mentionedFiles)
		m.queuePrompt(text, prompt, mentionedFiles, nil)
		m.pushSystemMsg(fmt.Sprintf("queued remote request: %s", truncateLabel(text, 140)))
		m.showBanner("remote request queued", "info")
		m.refreshViewport()
		sendRemoteReply(reply, remote.SubmitResponse{Accepted: true, Queued: true, Note: "queued behind active run"})
		return m, nil
	}

	sendRemoteReply(reply, remote.SubmitResponse{Accepted: true, Note: "running"})
	return m.handlePrompt(text)
}

func sendRemoteReply(ch chan<- remote.SubmitResponse, resp remote.SubmitResponse) {
	if ch == nil {
		return
	}
	select {
	case ch <- resp:
	default:
		// Reply channel is buffered with capacity 1 by the HTTP handler, so
		// a default branch only fires if the client disconnected before the
		// TUI got around to answering.
	}
}

func (m Model) remoteStatusSnapshot() remote.Status {
	return remote.Status{
		Thinking:      m.thinking,
		Mode:          m.mode,
		ActiveAgent:   m.activeAgentID,
		SessionID:     m.sessionID,
		MessagesCount: len(m.messages),
		TokensUsed:    m.totalTokensUsed,
	}
}

// publishRemote forwards an event to the optional remote server AND the
// optional Telegram relay. It is the single funnel through which all
// observability events leave the TUI.
func (m *Model) publishRemote(kind string, data map[string]interface{}) {
	if data == nil {
		data = map[string]interface{}{}
	}
	if _, ok := data["mode"]; !ok && m.mode != "" {
		data["mode"] = m.mode
	}
	if m.remoteServer != nil {
		m.remoteServer.Publish(kind, data)
	}
	m.dispatchTelegramEvent(kind, data)
}

func (m *Model) publishRemoteState(reason string) {
	if m.remoteServer == nil {
		return
	}
	st := m.remoteStatusSnapshot()
	m.remoteServer.SetStatus(st)
	data := map[string]interface{}{
		"thinking":       st.Thinking,
		"mode":           st.Mode,
		"active_agent":   st.ActiveAgent,
		"session_id":     st.SessionID,
		"messages_count": st.MessagesCount,
		"tokens_used":    st.TokensUsed,
	}
	if reason != "" {
		data["reason"] = reason
	}
	m.publishRemote("state", data)
}

func (m *Model) publishRemoteToolTrace(t agent.ToolTrace) {
	if m.remoteServer == nil {
		return
	}
	data := map[string]interface{}{
		"name":   t.Name,
		"status": t.Status,
		"agent":  t.AgentID,
	}
	if t.Args != "" {
		// Try to surface structured args; fall back to the raw string.
		var parsed any
		if err := json.Unmarshal([]byte(t.Args), &parsed); err == nil {
			data["args"] = parsed
		} else {
			data["args_raw"] = t.Args
		}
	}
	if t.Output != "" {
		data["output"] = t.Output
	}
	m.publishRemote("tool", data)
}
