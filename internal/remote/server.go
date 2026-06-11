// Package remote exposes a small HTTP/SSE control plane so that an external
// process can drive a running Spettro session: submit prompts, observe live
// progress (tool calls, comments, agent output, banners) and request an
// interrupt. The server binds to 127.0.0.1 by default (loopback only), but can
// be configured to listen on 0.0.0.0 when LAN access is desired. It is gated by
// a bearer token printed when /remote is invoked.
package remote

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultPort is the first port tried when /remote is invoked without an
// explicit port. It is intentionally an uncommon, easy-to-remember number.
const DefaultPort = 7878

// portScanLimit caps how many sequential ports we probe before letting the OS
// pick a free one.
const portScanLimit = 10

// recentEventsLimit bounds the per-server replay buffer sent to clients on
// connect.
const recentEventsLimit = 64

// maxRequestBytes caps the size of any request body the control server reads,
// so a client cannot exhaust memory with an unbounded POST.
const maxRequestBytes = 1 << 20 // 1 MiB

// SubmitRequest is delivered to the TUI when an HTTP client posts a prompt.
// The TUI must send exactly one response on Reply.
type SubmitRequest struct {
	Message string                `json:"message"`
	Reply   chan<- SubmitResponse `json:"-"`
}

// SubmitResponse describes how the TUI handled an incoming prompt.
type SubmitResponse struct {
	Accepted bool   `json:"accepted"`
	Queued   bool   `json:"queued"`
	Note     string `json:"note,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Event is broadcast to every connected /events subscriber and stored in a
// short replay buffer.
type Event struct {
	Seq  uint64                 `json:"seq"`
	Kind string                 `json:"kind"`
	At   time.Time              `json:"at"`
	Data map[string]interface{} `json:"data,omitempty"`
}

// Status is the JSON shape returned by GET /status. It mirrors the bits of
// runtime state external clients care about.
type Status struct {
	Thinking      bool      `json:"thinking"`
	Mode          string    `json:"mode"`
	ActiveAgent   string    `json:"active_agent,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	MessagesCount int       `json:"messages_count"`
	TokensUsed    int       `json:"tokens_used"`
	StartedAt     time.Time `json:"started_at"`
}

// ApprovalDecision is the client-provided answer to a shell-approval request.
type ApprovalDecision struct {
	Decision string `json:"decision"` // "allow-once" | "allow-always" | "deny"
	Instead  string `json:"instead,omitempty"`
}

// Server is the local HTTP control plane.
type Server struct {
	token     string
	startedAt time.Time

	submitCh    chan SubmitRequest
	interruptCh chan struct{}

	listener   net.Listener
	httpServer *http.Server
	port       int
	bindHost   string

	mu      sync.RWMutex
	subs    map[chan Event]struct{}
	recent  []Event
	seq     uint64
	closed  bool
	running bool

	statusMu sync.RWMutex
	status   Status

	// pendingApprovals maps tool_id -> channel awaiting approval decision.
	pendingApprovals sync.Map
	// pendingAskUsers maps question_id -> channel awaiting user answer.
	pendingAskUsers sync.Map
}

// Options controls server creation.
type Options struct {
	// SubmitBuffer is how many submissions can be in flight before the HTTP
	// handlers block. 8 is plenty for an interactive CLI.
	SubmitBuffer int
	// Token, if empty, will be generated automatically. Pass a fixed value
	// only for tests.
	Token string
	// BindHost is the interface address to listen on. Defaults to "127.0.0.1"
	// (loopback only). Set to "0.0.0.0" to accept connections from the LAN.
	BindHost string
}

// NewServer constructs a server but does not bind a port yet.
func NewServer(opts Options) (*Server, error) {
	buf := opts.SubmitBuffer
	if buf <= 0 {
		buf = 8
	}
	token := opts.Token
	if strings.TrimSpace(token) == "" {
		generated, err := generateToken(16)
		if err != nil {
			return nil, fmt.Errorf("remote: generate token: %w", err)
		}
		token = generated
	}
	host := opts.BindHost
	if host == "" {
		host = "127.0.0.1"
	}
	return &Server{
		token:       token,
		bindHost:    host,
		submitCh:    make(chan SubmitRequest, buf),
		interruptCh: make(chan struct{}, 8),
		subs:        map[chan Event]struct{}{},
	}, nil
}

// Host returns the interface address the server is bound to (e.g. "127.0.0.1" or "0.0.0.0").
func (s *Server) Host() string { return s.bindHost }

// SetStatus updates the snapshot returned by GET /status. It is safe for
// concurrent callers.
func (s *Server) SetStatus(st Status) {
	s.statusMu.Lock()
	s.status = st
	s.statusMu.Unlock()
}

// Token returns the bearer token clients must send on every request.
func (s *Server) Token() string { return s.token }

// Submissions returns the channel from which the TUI reads incoming prompts.
// It is closed by Stop.
func (s *Server) Submissions() <-chan SubmitRequest { return s.submitCh }

// Interrupts returns the channel that fires once for every POST /interrupt
// the server receives. It is closed by Stop.
func (s *Server) Interrupts() <-chan struct{} { return s.interruptCh }

// Port reports the bound TCP port (0 before Start).
func (s *Server) Port() int { return s.port }

// Start binds and serves on a TCP port.
//
// If preferredPort > 0 we try that first. On failure we fall back to an
// OS-assigned port. If preferredPort == 0 we sequentially probe DefaultPort,
// DefaultPort+1, ... up to portScanLimit before falling back. The returned
// fellBack flag is true whenever the bound port differs from the caller's
// first preference (DefaultPort or preferredPort).
func (s *Server) Start(preferredPort int) (port int, fellBack bool, err error) {
	if preferredPort < 0 || preferredPort > 65535 {
		return 0, false, fmt.Errorf("remote: invalid port %d", preferredPort)
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return s.port, false, errors.New("remote: already running")
	}
	s.mu.Unlock()

	bind := s.bindHost
	var ln net.Listener
	if preferredPort > 0 {
		ln, err = net.Listen("tcp", fmt.Sprintf("%s:%d", bind, preferredPort))
		if err != nil {
			altLn, altErr := net.Listen("tcp", bind+":0")
			if altErr != nil {
				return 0, false, fmt.Errorf("remote: listen fallback: %w", altErr)
			}
			ln = altLn
			fellBack = true
		}
	} else {
		for i := 0; i < portScanLimit; i++ {
			candidate := DefaultPort + i
			scanLn, scanErr := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, candidate))
			if scanErr == nil {
				ln = scanLn
				fellBack = i > 0
				break
			}
		}
		if ln == nil {
			altLn, altErr := net.Listen("tcp", bind+":0")
			if altErr != nil {
				return 0, false, fmt.Errorf("remote: listen: %w", altErr)
			}
			ln = altLn
			fellBack = true
		}
	}

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return 0, false, errors.New("remote: unexpected listener type")
	}
	s.listener = ln
	s.port = addr.Port

	s.httpServer = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	s.mu.Lock()
	s.running = true
	s.startedAt = time.Now()
	s.mu.Unlock()

	go func() {
		_ = s.httpServer.Serve(ln)
	}()

	return s.port, fellBack, nil
}

// Stop tears down the HTTP server and the submission/interrupt channels.
// It is idempotent.
//
// Per-subscriber event channels are intentionally NOT closed here. They are
// owned by their respective /events handler goroutines and will be released
// once httpServer.Shutdown cancels their request contexts (which makes the
// goroutines exit and unreference the channels). Closing them from Stop
// would race with concurrent Publish calls that already snapshotted the
// subscriber slice.
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.closed || !s.running {
		s.closed = true
		s.running = false
		s.subs = nil
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.running = false
	s.subs = nil
	srv := s.httpServer
	s.mu.Unlock()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}

	// Now that all in-flight HTTP handlers have exited (Shutdown blocks
	// until they do), it is safe to close the writer-owned channels.
	close(s.submitCh)
	close(s.interruptCh)
	return nil
}

// Publish records an event in the replay buffer and fans it out to every
// connected /events subscriber. Slow subscribers are skipped (we never block
// the TUI).
func (s *Server) Publish(kind string, data map[string]interface{}) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.seq++
	ev := Event{Seq: s.seq, Kind: kind, At: time.Now(), Data: data}
	s.recent = append(s.recent, ev)
	if len(s.recent) > recentEventsLimit {
		s.recent = s.recent[len(s.recent)-recentEventsLimit:]
	}
	subs := make([]chan Event, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			// Subscriber is slow; drop rather than blocking the TUI loop.
		}
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/messages", s.handleMessages)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/interrupt", s.handleInterrupt)
	mux.HandleFunc("/approval", s.handleApproval)
	mux.HandleFunc("/ask-user", s.handleAskUser)
	return s.applyAuth(mux)
}

func (s *Server) applyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// DNS-rebinding defense: when bound to loopback, only serve requests
		// addressed to a loopback Host. A browser page that rebinds a hostname
		// to 127.0.0.1 carries its own Host header and is rejected here.
		if !s.hostAllowed(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		got := strings.TrimSpace(r.Header.Get("Authorization"))
		if got != "" {
			got = strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
		} else if r.URL.Path == "/events" {
			// EventSource clients cannot set headers, so the token may ride in
			// a query parameter — but only on the read-only SSE stream, never
			// on the state-changing POST endpoints (where it would leak into
			// logs/history/Referer).
			got = strings.TrimSpace(r.URL.Query().Get("token"))
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		next.ServeHTTP(w, r)
	})
}

// hostAllowed implements DNS-rebinding protection. When the server is bound to
// a loopback address the request Host must also be loopback. When the operator
// has explicitly opted into LAN exposure (0.0.0.0 or a specific interface) we
// do not constrain Host — the bearer token remains the gate.
func (s *Server) hostAllowed(host string) bool {
	if !isLoopbackBind(s.bindHost) {
		return true
	}
	h := host
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		h = hostname
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	switch strings.ToLower(h) {
	case "", "localhost":
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func isLoopbackBind(bind string) bool {
	switch bind {
	case "", "localhost":
		return true
	}
	if ip := net.ParseIP(bind); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "spettro-remote",
		"endpoints": []string{
			"POST /messages",
			"GET /events (text/event-stream)",
			"GET /status",
			"POST /interrupt",
			"POST /approval",
			"POST /ask-user",
		},
	})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Message = strings.TrimSpace(body.Message)
	if body.Message == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	reply := make(chan SubmitResponse, 1)
	req := SubmitRequest{Message: body.Message, Reply: reply}

	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		http.Error(w, "remote server stopped", http.StatusServiceUnavailable)
		return
	}

	select {
	case s.submitCh <- req:
	case <-r.Context().Done():
		http.Error(w, "client disconnected", http.StatusRequestTimeout)
		return
	}

	select {
	case resp := <-reply:
		status := http.StatusOK
		if !resp.Accepted {
			status = http.StatusConflict
		}
		writeJSON(w, status, resp)
	case <-r.Context().Done():
		http.Error(w, "client disconnected", http.StatusRequestTimeout)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.statusMu.RLock()
	st := s.status
	s.statusMu.RUnlock()
	if st.StartedAt.IsZero() {
		st.StartedAt = s.startedAt
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		http.Error(w, "remote server stopped", http.StatusServiceUnavailable)
		return
	}
	select {
	case s.interruptCh <- struct{}{}:
	default:
		// Already pending — coalesce.
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan Event, 32)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.subs[ch] = struct{}{}
	backlog := append([]Event(nil), s.recent...)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		// Just unregister; do NOT close ch. A concurrent Publish may have
		// snapshotted this subscriber slice while still holding ch, and
		// closing it would race the `select { case ch <- ev: default: }`
		// fan-out (sends to a closed channel panic; the default branch
		// does not rescue that). The goroutine that owned ch is already
		// returning, so the channel will be garbage-collected once the
		// publisher's snapshot is released.
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	for _, ev := range backlog {
		if err := writeSSE(w, ev); err != nil {
			return
		}
	}
	flusher.Flush()

	pulse := time.NewTicker(15 * time.Second)
	defer pulse.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			if err := writeSSE(w, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-pulse.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// RequestApproval publishes an approval_request event and blocks until the
// Android client responds via POST /approval. ctx cancellation returns deny.
func (s *Server) RequestApproval(ctx context.Context, toolID, command, reason string) (ApprovalDecision, error) {
	ch := make(chan ApprovalDecision, 1)
	s.pendingApprovals.Store(toolID, ch)
	defer s.pendingApprovals.Delete(toolID)

	s.Publish("approval_request", map[string]interface{}{
		"tool_id": toolID,
		"command": command,
		"reason":  reason,
	})

	select {
	case dec := <-ch:
		return dec, nil
	case <-ctx.Done():
		return ApprovalDecision{Decision: "deny"}, ctx.Err()
	}
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ToolID   string `json:"tool_id"`
		Decision string `json:"decision"`
		Instead  string `json:"instead,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	val, ok := s.pendingApprovals.Load(body.ToolID)
	if !ok {
		http.Error(w, "no pending approval for tool_id", http.StatusNotFound)
		return
	}
	ch := val.(chan ApprovalDecision)
	select {
	case ch <- ApprovalDecision{Decision: body.Decision, Instead: body.Instead}:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "approval already answered", http.StatusConflict)
	}
}

// RequestAskUser publishes an ask_user event and blocks until the Android
// client responds via POST /ask-user. ctx cancellation returns empty string.
func (s *Server) RequestAskUser(ctx context.Context, questionID, question string, options []string, allowFreeResponse bool) (string, error) {
	ch := make(chan string, 1)
	s.pendingAskUsers.Store(questionID, ch)
	defer s.pendingAskUsers.Delete(questionID)

	data := map[string]interface{}{
		"question_id":         questionID,
		"question":            question,
		"options":             options,
		"allow_free_response": allowFreeResponse,
	}
	s.Publish("ask_user", data)

	select {
	case answer := <-ch:
		return answer, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *Server) handleAskUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		QuestionID string `json:"question_id"`
		Answer     string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	val, ok := s.pendingAskUsers.Load(body.QuestionID)
	if !ok {
		http.Error(w, "no pending ask-user for question_id", http.StatusNotFound)
		return
	}
	ch := val.(chan string)
	select {
	case ch <- body.Answer:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "question already answered", http.StatusConflict)
	}
}

func writeSSE(w http.ResponseWriter, ev Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", sanitizeKind(ev.Kind)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\n", ev.Seq); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	return nil
}

func sanitizeKind(kind string) string {
	if kind == "" {
		return "message"
	}
	out := make([]byte, 0, len(kind))
	for i := 0; i < len(kind); i++ {
		c := kind[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func generateToken(byteLen int) (string, error) {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
