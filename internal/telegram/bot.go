// Package telegram implements a Telegram Bot API relay that lets users drive
// a running Spettro session from a chat client: send prompts, observe agent
// output, answer ask-user dialogs and interrupt runs without sitting in
// front of the TUI.
//
// The package is intentionally dependency-free (only the Go standard library
// plus the existing config/secret helpers) so it can be embedded into the
// TUI without expanding the binary much. The Bot API is consumed via long
// polling, so the user does not need a public HTTPS endpoint or a webhook.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the canonical Telegram Bot API root. It is exposed as a
// constructor option so tests can point a BotClient at an httptest server.
const DefaultBaseURL = "https://api.telegram.org"

// User mirrors a subset of the Telegram User object.
type User struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name,omitempty"`
	Username     string `json:"username,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
}

// Chat mirrors a subset of the Telegram Chat object.
type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title,omitempty"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

// Message mirrors a subset of the Telegram Message object.
type Message struct {
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text,omitempty"`
	From      *User  `json:"from,omitempty"`
	Chat      *Chat  `json:"chat,omitempty"`
}

// Update mirrors a subset of the Telegram Update object.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
	// EditedMessage is intentionally surfaced as Message so callers can treat
	// edits as fresh inputs without a separate code path.
	EditedMessage *Message `json:"edited_message,omitempty"`
}

// BotClient is a minimal Telegram Bot API client (long polling + send).
type BotClient struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

// ClientOption tunes a BotClient at construction time.
type ClientOption func(*BotClient)

// WithBaseURL overrides the API root (used by tests).
func WithBaseURL(u string) ClientOption {
	return func(c *BotClient) {
		if u != "" {
			c.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithHTTPClient overrides the underlying http.Client (used by tests or
// when a caller wants a custom transport).
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *BotClient) {
		if h != nil {
			c.httpClient = h
		}
	}
}

// NewBotClient constructs a BotClient bound to the given bot token.
func NewBotClient(token string, opts ...ClientOption) *BotClient {
	c := &BotClient{
		token:   strings.TrimSpace(token),
		baseURL: DefaultBaseURL,
		httpClient: &http.Client{
			// The long-poll endpoint takes up to ~timeout seconds plus
			// some Telegram-side slack, so the transport timeout is set
			// generously. Per-call deadlines come from the context.
			Timeout: 60 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Token returns the bot token. Callers must not log it.
func (c *BotClient) Token() string { return c.token }

// apiResponse is the standard Telegram Bot API envelope.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// call POSTs JSON to the given method and decodes the OK envelope.
func (c *BotClient) call(ctx context.Context, method string, payload any, out any) error {
	if strings.TrimSpace(c.token) == "" {
		return errors.New("telegram: bot token is empty")
	}
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("telegram: marshal %s: %w", method, err)
		}
		body = bytes.NewReader(buf)
	}
	endpoint := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("telegram: build %s: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("telegram: read %s: %w", method, err)
	}
	var env apiResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram: decode %s: %w (body=%q)", method, err, truncateForLog(raw))
	}
	if !env.OK {
		desc := strings.TrimSpace(env.Description)
		if desc == "" {
			desc = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return &APIError{Method: method, Code: env.ErrorCode, Description: desc}
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("telegram: decode %s result: %w", method, err)
	}
	return nil
}

// APIError is a non-OK envelope returned by the Telegram Bot API.
type APIError struct {
	Method      string
	Code        int
	Description string
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("telegram: %s failed: %d %s", e.Method, e.Code, e.Description)
	}
	return fmt.Sprintf("telegram: %s failed: %s", e.Method, e.Description)
}

// IsAPIError reports whether err is a Telegram API error and returns the
// description for convenience.
func IsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// GetMe validates the bot token by calling getMe and returns the bot's
// own user record.
func (c *BotClient) GetMe(ctx context.Context) (*User, error) {
	var u User
	if err := c.call(ctx, "getMe", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// DeleteWebhook clears any registered webhook so getUpdates can long-poll
// successfully. It is safe to call when no webhook is set.
func (c *BotClient) DeleteWebhook(ctx context.Context) error {
	payload := map[string]any{"drop_pending_updates": false}
	return c.call(ctx, "deleteWebhook", payload, nil)
}

// GetUpdates performs one long-poll round. offset must be the next update_id
// the caller expects (i.e. lastSeen+1). timeoutSec is the server-side
// long-poll timeout in seconds; the server may close the connection earlier
// if updates arrive.
func (c *BotClient) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	if timeoutSec < 0 {
		timeoutSec = 0
	}
	if timeoutSec > 50 {
		timeoutSec = 50
	}
	payload := map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"message", "edited_message"},
	}
	var updates []Update
	if err := c.call(ctx, "getUpdates", payload, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// SendMessage posts a plain-text message to the given chat.
//
// Spettro does not use Telegram markdown by default: agent output can
// contain arbitrary characters and MarkdownV2 escaping is error-prone, so
// plain text avoids ever getting a 400 from the Bot API. A future caller
// can extend this by adding a parse_mode argument.
func (c *BotClient) SendMessage(ctx context.Context, chatID int64, text string) (*Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("telegram: SendMessage with empty text")
	}
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	var msg Message
	if err := c.call(ctx, "sendMessage", payload, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// truncateForLog clamps a byte buffer for inclusion in an error message.
func truncateForLog(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// ParseChatTarget resolves a user-supplied allowlist entry into a stable
// form. It accepts "@username", "username", or a numeric chat ID (positive
// for users, negative for groups). Whitespace and leading "@" are stripped.
//
// Returned values:
//   - id != 0 for numeric IDs (username = "")
//   - username != "" for textual identifiers, normalised lowercase without
//     leading "@" (id = 0)
//
// An empty/garbage input returns ("", 0, err).
func ParseChatTarget(raw string) (username string, id int64, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, errors.New("empty identifier")
	}
	raw = strings.TrimPrefix(raw, "@")
	if raw == "" {
		return "", 0, errors.New("identifier must not be just @")
	}
	// numeric chat IDs are at most 19 digits (signed int64); allow a leading
	// '-' for supergroup/channel IDs.
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n == 0 {
			return "", 0, errors.New("chat id must not be zero")
		}
		return "", n, nil
	}
	// Telegram usernames are 5–32 chars, ASCII letters/digits/underscore.
	// We don't strictly enforce that here so the user can paste anything
	// reasonable; matching is case-insensitive.
	return strings.ToLower(raw), 0, nil
}

// FormatChatTarget renders a (username, id) pair back into the canonical
// display form ("@username" or "12345").
func FormatChatTarget(username string, id int64) string {
	switch {
	case username != "":
		return "@" + username
	case id != 0:
		return strconv.FormatInt(id, 10)
	default:
		return ""
	}
}
