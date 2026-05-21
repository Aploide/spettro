package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SubmitKind tells the TUI how to route a message that arrived through
// Telegram. The relay assigns the kind based on its own state (e.g. it
// knows when an ask-user reply is pending) so the TUI does not need to
// duplicate that bookkeeping.
type SubmitKind string

const (
	// SubmitPrompt asks the TUI to treat the message as a normal user prompt
	// (or a slash command if it starts with "/"). This is the default.
	SubmitPrompt SubmitKind = "prompt"
	// SubmitAnswer routes the message into the currently pending ask-user
	// dialog. The TUI is responsible for clearing the "expecting answer"
	// flag once the dialog resolves.
	SubmitAnswer SubmitKind = "answer"
	// SubmitBotCommand is reserved for Telegram-side commands that should
	// run without ever reaching the agent loop (e.g. /status, /help). The
	// relay handles these locally — the TUI never sees them.
	SubmitBotCommand SubmitKind = "bot"
)

// SubmitRequest is delivered to the TUI for every incoming Telegram
// message that should drive the session. Reply must be answered exactly
// once so the relay can ack the original chat with a short confirmation.
type SubmitRequest struct {
	Message string
	Kind    SubmitKind
	ChatID  int64
	UserID  int64
	From    string
	Reply   chan<- SubmitResponse
}

// SubmitResponse mirrors remote.SubmitResponse: the TUI tells the relay
// whether it accepted the prompt and whether it was queued.
type SubmitResponse struct {
	Accepted bool
	Queued   bool
	Note     string
	Error    string
}

// Relay owns the Telegram poll loop and the in/out channels that hook into
// the TUI's bubbletea program. Its surface mirrors remote.Server so the
// wiring in model_telegram.go looks like model_remote.go.
type Relay struct {
	client      *BotClient
	botUsername string

	// Inbound channels (TUI consumes).
	submitCh    chan SubmitRequest
	interruptCh chan struct{}

	// Lifecycle.
	mu            sync.Mutex
	cancel        context.CancelFunc
	running       bool
	stopped       bool
	pollErr       error
	pollDoneCh    chan struct{}
	expectAnswers map[int64]bool

	// sendMu only guards the bookkeeping fields below (bound chats + last
	// observed send error). It is intentionally NOT held during HTTP
	// round-trips so concurrent broadcasts can fan out and observability
	// reads (AnySubscriber, BoundChats) never block on the network.
	sendMu      sync.Mutex
	boundChats  map[int64]struct{}
	boundOrder  []int64
	lastSendErr error

	// Config snapshot owned by the relay. Mutators (allow/deny, autostart)
	// go through Mutate which persists to disk inside the lock.
	cfgMu sync.RWMutex
	cfg   PersistedConfig

	// Observer callback: every interesting event is also forwarded here so
	// the TUI can publish it on the SSE control plane and log it inside
	// the chat. Optional.
	observer func(kind string, data map[string]interface{})
}

// Options controls relay construction.
type Options struct {
	// Token is the BotFather API token. Required.
	Token string
	// BotUsername is the bot's own @username (without leading @). Optional
	// — used only for help text. The relay calls getMe on Start anyway and
	// will overwrite this value if it can.
	BotUsername string
	// Config is the persisted relay config (allowlist, last-seen update,
	// etc.). Pass an empty value when the user has not configured anything
	// yet.
	Config PersistedConfig
	// Client lets callers (mainly tests) inject a pre-built BotClient.
	// Production callers leave this nil and let NewRelay build its own.
	Client *BotClient
	// SubmitBuffer is how many inbound messages can be in flight before
	// the relay blocks. 8 mirrors remote.Server's default.
	SubmitBuffer int
}

// NewRelay returns a relay ready to Start. It does not perform any network
// I/O.
func NewRelay(opts Options) (*Relay, error) {
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil, errors.New("telegram: empty bot token")
	}
	buf := opts.SubmitBuffer
	if buf <= 0 {
		buf = 8
	}
	client := opts.Client
	if client == nil {
		client = NewBotClient(token)
	}
	r := &Relay{
		client:        client,
		botUsername:   strings.TrimPrefix(strings.TrimSpace(opts.BotUsername), "@"),
		submitCh:      make(chan SubmitRequest, buf),
		interruptCh:   make(chan struct{}, 8),
		expectAnswers: map[int64]bool{},
		boundChats:    map[int64]struct{}{},
		cfg:           NormaliseConfig(opts.Config),
	}
	return r, nil
}

// SetObserver installs an optional callback that receives the same event
// stream the relay sends to Telegram, for cross-publication on the SSE
// control plane.
func (r *Relay) SetObserver(fn func(kind string, data map[string]interface{})) {
	r.observer = fn
}

// Submissions returns the inbound channel. The TUI re-arms a tea.Cmd from
// it each time a message is delivered.
func (r *Relay) Submissions() <-chan SubmitRequest { return r.submitCh }

// Interrupts returns the channel that fires when a Telegram client requests
// the equivalent of pressing Esc. Coalesced.
func (r *Relay) Interrupts() <-chan struct{} { return r.interruptCh }

// BotUsername returns the cached bot username (may be empty if getMe has
// not yet run).
func (r *Relay) BotUsername() string {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	if r.botUsername != "" {
		return r.botUsername
	}
	return r.cfg.BotUsername
}

// Config returns a snapshot of the persisted config.
func (r *Relay) Config() PersistedConfig {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.cfg
}

// Mutate runs fn against the persisted config under a lock, then writes
// the result to disk and updates the in-memory snapshot. The mutated value
// is returned for caller convenience.
func (r *Relay) Mutate(fn func(*PersistedConfig)) (PersistedConfig, error) {
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	cfg := r.cfg
	if fn != nil {
		fn(&cfg)
	}
	cfg = NormaliseConfig(cfg)
	if err := SaveConfig(cfg); err != nil {
		return r.cfg, err
	}
	r.cfg = cfg
	return cfg, nil
}

// IsRunning reports whether the poll loop is active.
func (r *Relay) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// LastError returns the error, if any, that caused the last poll loop to
// exit. Reset on the next Start.
func (r *Relay) LastError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pollErr
}

// Start spins up the poll loop. It calls getMe once to validate the token
// and capture the bot username, then returns. The poll loop runs in the
// background until Stop is called or an unrecoverable error occurs.
func (r *Relay) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return errors.New("telegram: relay has been stopped; create a new one")
	}
	if r.running {
		r.mu.Unlock()
		return errors.New("telegram: already running")
	}
	r.pollErr = nil
	loopCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.running = true
	r.pollDoneCh = make(chan struct{})
	r.mu.Unlock()

	probeCtx, probeCancel := context.WithTimeout(ctx, 15*time.Second)
	defer probeCancel()
	me, err := r.client.GetMe(probeCtx)
	if err != nil {
		r.mu.Lock()
		r.running = false
		r.cancel = nil
		close(r.pollDoneCh)
		r.pollDoneCh = nil
		r.mu.Unlock()
		cancel()
		return fmt.Errorf("telegram: getMe: %w", err)
	}
	if me != nil && me.Username != "" {
		r.cfgMu.Lock()
		r.botUsername = me.Username
		r.cfg.BotUsername = me.Username
		_ = SaveConfig(NormaliseConfig(r.cfg))
		r.cfgMu.Unlock()
	}

	// Always delete any registered webhook before long-polling so the
	// Bot API does not refuse getUpdates with "409 Conflict".
	_ = r.client.DeleteWebhook(probeCtx)

	go r.runPollLoop(loopCtx)
	return nil
}

// Stop tears down the poll loop. Safe to call multiple times. The function
// blocks (up to 5 seconds) waiting for the goroutine to exit so its
// resources are released before the caller continues.
func (r *Relay) Stop() {
	r.mu.Lock()
	if !r.running {
		r.stopped = true
		// Idempotent: if the loop is already done we still want subsequent
		// callers to get a closed Submissions/Interrupts channel.
		if r.submitCh != nil {
			close(r.submitCh)
			r.submitCh = nil
		}
		if r.interruptCh != nil {
			close(r.interruptCh)
			r.interruptCh = nil
		}
		r.mu.Unlock()
		return
	}
	cancel := r.cancel
	done := r.pollDoneCh
	r.cancel = nil
	r.running = false
	r.stopped = true
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	r.mu.Lock()
	if r.submitCh != nil {
		close(r.submitCh)
		r.submitCh = nil
	}
	if r.interruptCh != nil {
		close(r.interruptCh)
		r.interruptCh = nil
	}
	r.mu.Unlock()
}

// runPollLoop is the long-polling goroutine. It exits when ctx is cancelled.
func (r *Relay) runPollLoop(ctx context.Context) {
	defer func() {
		r.mu.Lock()
		if r.pollDoneCh != nil {
			close(r.pollDoneCh)
			r.pollDoneCh = nil
		}
		r.running = false
		r.mu.Unlock()
	}()

	r.publishObs("telegram_started", map[string]interface{}{
		"bot_username": r.BotUsername(),
	})

	// Backoff state when getUpdates errors out repeatedly.
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			r.publishObs("telegram_stopped", map[string]interface{}{
				"bot_username": r.BotUsername(),
			})
			return
		default:
		}

		offset := r.cfg.LastUpdateID + 1
		updates, err := r.client.GetUpdates(ctx, offset, 25)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			r.mu.Lock()
			r.pollErr = err
			r.mu.Unlock()
			r.publishObs("telegram_error", map[string]interface{}{
				"error": err.Error(),
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = time.Second

		for _, u := range updates {
			r.processUpdate(ctx, u)
		}
	}
}

// processUpdate routes a single Telegram update.
func (r *Relay) processUpdate(ctx context.Context, u Update) {
	// Persist the offset eagerly so a crash mid-batch does not replay
	// already-processed updates.
	defer func() {
		_, _ = r.Mutate(func(cfg *PersistedConfig) {
			if u.UpdateID >= cfg.LastUpdateID {
				cfg.LastUpdateID = u.UpdateID
			}
		})
	}()

	msg := u.Message
	if msg == nil {
		msg = u.EditedMessage
	}
	if msg == nil || msg.Chat == nil {
		return
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	var username string
	var userID int64
	if msg.From != nil {
		username = strings.ToLower(strings.TrimPrefix(msg.From.Username, "@"))
		userID = msg.From.ID
	}
	chatID := msg.Chat.ID

	if !IsAllowed(r.Config(), username, userID, chatID) {
		r.publishObs("telegram_denied", map[string]interface{}{
			"chat_id":  chatID,
			"username": username,
		})
		r.sendRaw(ctx, chatID, "🚫 This chat is not allowed to drive Spettro. Ask the operator to run `/telegram allow @"+username+"` (or `/telegram allow "+itoa(chatID)+"`) in the TUI.")
		return
	}

	// Authorised: remember this chat so outbound events go here.
	r.bindChat(chatID)

	// Intercept bot-side commands first so they never reach the agent loop.
	if handled, reply := r.handleBotCommand(text); handled {
		if reply != "" {
			r.sendRaw(ctx, chatID, reply)
		}
		return
	}

	kind := SubmitPrompt
	r.cfgMu.RLock()
	if r.expectAnswers[chatID] && !strings.HasPrefix(text, "/") {
		kind = SubmitAnswer
	}
	r.cfgMu.RUnlock()

	reply := make(chan SubmitResponse, 1)
	req := SubmitRequest{
		Message: text,
		Kind:    kind,
		ChatID:  chatID,
		UserID:  userID,
		From:    formatFromName(msg.From),
		Reply:   reply,
	}
	r.publishObs("telegram_submission", map[string]interface{}{
		"chat_id":  chatID,
		"username": username,
		"kind":     string(kind),
		"preview":  Truncate(text, 200),
	})

	r.mu.Lock()
	closed := r.submitCh == nil
	ch := r.submitCh
	r.mu.Unlock()
	if closed {
		return
	}
	select {
	case ch <- req:
	case <-ctx.Done():
		return
	}
	// Wait for the TUI's ack so the user gets a fast confirmation in the
	// chat. We don't block forever — if the TUI is wedged, fall back to
	// an "accepted (no response)" message after 30s.
	select {
	case resp := <-reply:
		r.sendAck(ctx, chatID, resp)
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
		r.sendRaw(ctx, chatID, "(submitted — Spettro did not ack within 30s, still running)")
	}
}

func (r *Relay) sendAck(ctx context.Context, chatID int64, resp SubmitResponse) {
	var prefix string
	switch {
	case resp.Error != "":
		prefix = "❌ " + resp.Error
	case resp.Queued:
		prefix = "🕒 queued"
	case resp.Accepted:
		prefix = "✅ running"
	default:
		prefix = "❌ rejected"
	}
	note := strings.TrimSpace(resp.Note)
	if note != "" {
		prefix = prefix + " — " + note
	}
	r.sendRaw(ctx, chatID, prefix)
}

// handleBotCommand intercepts simple "/cancel", "/status" etc. that should
// not reach the agent. Returns (true, reply) if the relay handled the
// command itself, (false, "") otherwise.
func (r *Relay) handleBotCommand(text string) (bool, string) {
	if !strings.HasPrefix(text, "/") {
		return false, ""
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false, ""
	}
	// Some Telegram clients append "@botname" to commands sent in groups.
	head := strings.ToLower(fields[0])
	if at := strings.Index(head, "@"); at > 0 {
		head = head[:at]
	}
	switch head {
	case "/start", "/help":
		return true, r.helpText()
	case "/cancel", "/stop":
		select {
		case r.interruptCh <- struct{}{}:
		default:
		}
		return true, "🛑 interrupt requested"
	case "/whoami":
		cfg := r.Config()
		return true, fmt.Sprintf("bot: @%s\nallowlist size: %d", r.BotUsername(), len(cfg.Allowlist))
	}
	return false, ""
}

func (r *Relay) helpText() string {
	bot := r.BotUsername()
	if bot == "" {
		bot = "your bot"
	}
	return strings.Join([]string{
		"👋 Spettro Telegram relay",
		"",
		"Send any message and it becomes a prompt for the active agent.",
		"Send a /-command (like /plan) and it runs as a slash command.",
		"",
		"Bot-side commands:",
		"  /cancel — interrupt the running agent",
		"  /whoami — show bot identity",
		"  /help — this message",
		"",
		"When Spettro asks a question, just reply with plain text.",
	}, "\n")
}

// ExpectAnswer flips an internal flag so non-slash messages from chatID
// are routed as SubmitAnswer until ClearExpectAnswer is called. The TUI
// invokes this when it shows an ask-user dialog whose origin is Telegram.
func (r *Relay) ExpectAnswer(chatID int64, expect bool) {
	if chatID == 0 {
		return
	}
	r.cfgMu.Lock()
	if expect {
		r.expectAnswers[chatID] = true
	} else {
		delete(r.expectAnswers, chatID)
	}
	r.cfgMu.Unlock()
}

// AnySubscriber reports whether any chat has bound to the relay this
// session. The TUI uses it to avoid wasting bandwidth on outbound events
// when nobody is listening.
func (r *Relay) AnySubscriber() bool {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	return len(r.boundChats) > 0
}

// BoundChats returns the current set of chat IDs that have authenticated
// at least once during this run. Used for /telegram status output.
func (r *Relay) BoundChats() []int64 {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	out := make([]int64, 0, len(r.boundOrder))
	out = append(out, r.boundOrder...)
	return out
}

func (r *Relay) bindChat(chatID int64) {
	r.sendMu.Lock()
	if _, ok := r.boundChats[chatID]; !ok {
		r.boundChats[chatID] = struct{}{}
		r.boundOrder = append(r.boundOrder, chatID)
	}
	r.sendMu.Unlock()
}

// Broadcast sends text to every currently bound chat. Chunks long
// payloads. Failures are surfaced through observer events but do not stop
// the relay.
func (r *Relay) Broadcast(text string) {
	chunks := SplitForTelegram(text)
	if len(chunks) == 0 {
		return
	}
	chats := r.BoundChats()
	if len(chats) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, chatID := range chats {
		for _, chunk := range chunks {
			r.sendRaw(ctx, chatID, chunk)
		}
	}
}

// BroadcastTo sends text only to the given chat. Useful for ack/replies
// scoped to the chat that triggered the run.
func (r *Relay) BroadcastTo(chatID int64, text string) {
	if chatID == 0 {
		return
	}
	chunks := SplitForTelegram(text)
	if len(chunks) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, chunk := range chunks {
		r.sendRaw(ctx, chatID, chunk)
	}
}

func (r *Relay) sendRaw(ctx context.Context, chatID int64, text string) {
	text = strings.TrimSpace(text)
	if chatID == 0 || text == "" {
		return
	}
	if _, err := r.client.SendMessage(ctx, chatID, text); err != nil {
		r.recordSendErr(chatID, err)
	}
}

// recordSendErr stashes the most recent send failure for diagnostics
// surfaced via /telegram status. Held under sendMu only briefly — the HTTP
// call must not hold this lock or AnySubscriber would block on the
// network.
func (r *Relay) recordSendErr(chatID int64, err error) {
	r.sendMu.Lock()
	r.lastSendErr = err
	r.sendMu.Unlock()
	r.publishObs("telegram_send_error", map[string]interface{}{
		"chat_id": chatID,
		"error":   err.Error(),
	})
}

// LastSendError reports the most recent send failure, if any.
func (r *Relay) LastSendError() error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	return r.lastSendErr
}

func (r *Relay) publishObs(kind string, data map[string]interface{}) {
	fn := r.observer
	if fn == nil {
		return
	}
	if data == nil {
		data = map[string]interface{}{}
	}
	fn(kind, data)
}

// formatFromName renders a "First Last (@username)" tag for log/event
// payloads. Falls back to "anonymous" if everything is empty.
func formatFromName(u *User) string {
	if u == nil {
		return "anonymous"
	}
	name := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
	switch {
	case u.Username != "" && name != "":
		return name + " (@" + u.Username + ")"
	case u.Username != "":
		return "@" + u.Username
	case name != "":
		return name
	default:
		return "anonymous"
	}
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}
