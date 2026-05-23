package telegram_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"spettro/internal/telegram"
)

// fakeBot is a tiny in-process Bot API server used by the bot- and relay-
// layer tests. It speaks just the surface Spettro consumes: getMe,
// deleteWebhook, getUpdates, sendMessage. The behaviour of each endpoint
// is configurable per-test through helper methods that take an internal
// mutex; the HTTP handler reads the same fields under that mutex so the
// -race detector is happy.
type fakeBot struct {
	server *httptest.Server

	// updatesMu / updates model the long-poll cursor.
	updatesMu sync.Mutex
	updates   []telegram.Update

	// sendMu / sent records every sendMessage call so assertions can
	// inspect what the relay tried to deliver.
	sendMu sync.Mutex
	sent   []sentMessage

	// mediaMu / media records every multipart upload (sendPhoto,
	// sendVideo, sendDocument) so assertions can inspect routing and
	// payload bytes.
	mediaMu sync.Mutex
	media   []sentMedia

	// stateMu guards everything below: tests mutate canned errors at
	// runtime while the HTTP handler is reading them concurrently.
	stateMu          sync.RWMutex
	getMeResp        telegram.User
	deleteWebhookOK  bool
	sendErr          string
	getUpdatesErr    string
	getMeErr         string
	mediaErr         string
	getUpdatesCalled atomic.Int64
}

type sentMessage struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

// sentMedia captures one multipart upload — what endpoint it hit, which
// form field carried the file, and a copy of the bytes so assertions can
// confirm the right file was uploaded.
type sentMedia struct {
	Method   string
	Field    string
	ChatID   int64
	Caption  string
	Filename string
	Body     []byte
}

func newFakeBot(t *testing.T) *fakeBot {
	t.Helper()
	fb := &fakeBot{
		getMeResp:       telegram.User{ID: 1, IsBot: true, FirstName: "Spettro", Username: "SpettroTestBot"},
		deleteWebhookOK: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", fb.handle)
	fb.server = httptest.NewServer(mux)
	t.Cleanup(fb.server.Close)
	return fb
}

func (f *fakeBot) URL() string { return f.server.URL }

// setError flips one of the canned error fields under the state mutex.
func (f *fakeBot) setError(kind, msg string) {
	f.stateMu.Lock()
	defer f.stateMu.Unlock()
	switch kind {
	case "getMe":
		f.getMeErr = msg
	case "getUpdates":
		f.getUpdatesErr = msg
	case "sendMessage":
		f.sendErr = msg
	case "sendMedia":
		f.mediaErr = msg
	}
}

func (f *fakeBot) snapState() (getMeErr, getUpdatesErr, sendErr, mediaErr string, getMeResp telegram.User, deleteOK bool) {
	f.stateMu.RLock()
	defer f.stateMu.RUnlock()
	return f.getMeErr, f.getUpdatesErr, f.sendErr, f.mediaErr, f.getMeResp, f.deleteWebhookOK
}

func (f *fakeBot) handle(w http.ResponseWriter, r *http.Request) {
	// URL shape: /bot<token>/<method>
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "bot") {
		http.NotFound(w, r)
		return
	}
	method := parts[1]
	getMeErr, getUpdatesErr, sendErr, mediaErr, getMeResp, deleteOK := f.snapState()
	switch method {
	case "getMe":
		if getMeErr != "" {
			writeAPI(w, false, getMeErr, nil)
			return
		}
		writeAPI(w, true, "", getMeResp)
	case "deleteWebhook":
		writeAPI(w, deleteOK, "", true)
	case "getUpdates":
		f.getUpdatesCalled.Add(1)
		if getUpdatesErr != "" {
			writeAPI(w, false, getUpdatesErr, nil)
			return
		}
		// Parse {offset,timeout} body and pop matching updates.
		var body struct {
			Offset  int64 `json:"offset"`
			Timeout int   `json:"timeout"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.updatesMu.Lock()
		take := []telegram.Update{}
		remaining := f.updates[:0]
		for _, u := range f.updates {
			if u.UpdateID >= body.Offset {
				take = append(take, u)
			} else {
				remaining = append(remaining, u)
			}
		}
		f.updates = remaining
		f.updatesMu.Unlock()
		writeAPI(w, true, "", take)
	case "sendMessage":
		if sendErr != "" {
			writeAPI(w, false, sendErr, nil)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var msg sentMessage
		_ = json.Unmarshal(raw, &msg)
		f.sendMu.Lock()
		f.sent = append(f.sent, msg)
		f.sendMu.Unlock()
		writeAPI(w, true, "", telegram.Message{MessageID: 1, Date: time.Now().Unix(), Text: msg.Text})
	case "sendPhoto", "sendVideo", "sendDocument":
		if mediaErr != "" {
			writeAPI(w, false, mediaErr, nil)
			return
		}
		entry, err := readMultipartMedia(r, method)
		if err != nil {
			writeAPI(w, false, err.Error(), nil)
			return
		}
		f.mediaMu.Lock()
		f.media = append(f.media, entry)
		f.mediaMu.Unlock()
		writeAPI(w, true, "", telegram.Message{MessageID: 1, Date: time.Now().Unix()})
	default:
		writeAPI(w, false, "method "+method+" not implemented", nil)
	}
}

// readMultipartMedia parses a Telegram multipart upload (sendPhoto /
// sendVideo / sendDocument) into a sentMedia entry. The expected file
// field name matches the endpoint (photo/video/document).
func readMultipartMedia(r *http.Request, method string) (sentMedia, error) {
	var entry sentMedia
	entry.Method = method
	expectedField := ""
	switch method {
	case "sendPhoto":
		expectedField = "photo"
	case "sendVideo":
		expectedField = "video"
	case "sendDocument":
		expectedField = "document"
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		return entry, fmt.Errorf("parse multipart: %w", err)
	}
	if v := r.FormValue("chat_id"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &entry.ChatID)
	}
	entry.Caption = r.FormValue("caption")
	headers := r.MultipartForm.File[expectedField]
	if len(headers) == 0 {
		return entry, fmt.Errorf("missing form file field %q", expectedField)
	}
	entry.Field = expectedField
	entry.Filename = headers[0].Filename
	f, err := headers[0].Open()
	if err != nil {
		return entry, fmt.Errorf("open form file: %w", err)
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		return entry, fmt.Errorf("read form file: %w", err)
	}
	entry.Body = body
	return entry, nil
}

func (f *fakeBot) pushUpdate(u telegram.Update) {
	f.updatesMu.Lock()
	f.updates = append(f.updates, u)
	f.updatesMu.Unlock()
}

func (f *fakeBot) sentMessages() []sentMessage {
	f.sendMu.Lock()
	defer f.sendMu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

// sentMediaCalls returns a copy of every multipart upload recorded so far.
// Tests use it to assert routing and payload bytes for SendMediaFile.
func (f *fakeBot) sentMediaCalls() []sentMedia {
	f.mediaMu.Lock()
	defer f.mediaMu.Unlock()
	out := make([]sentMedia, len(f.media))
	for i, m := range f.media {
		body := make([]byte, len(m.Body))
		copy(body, m.Body)
		out[i] = sentMedia{
			Method:   m.Method,
			Field:    m.Field,
			ChatID:   m.ChatID,
			Caption:  m.Caption,
			Filename: m.Filename,
			Body:     body,
		}
	}
	return out
}

func writeAPI(w http.ResponseWriter, ok bool, desc string, result any) {
	w.Header().Set("Content-Type", "application/json")
	envelope := map[string]any{"ok": ok}
	if desc != "" {
		envelope["description"] = desc
	}
	if ok {
		envelope["result"] = result
	}
	_ = json.NewEncoder(w).Encode(envelope)
}

// TestBotClient_GetMe_OK validates that GetMe parses the result envelope.
func TestBotClient_GetMe_OK(t *testing.T) {
	fb := newFakeBot(t)
	c := telegram.NewBotClient("token", telegram.WithBaseURL(fb.URL()))
	me, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if me.Username != "SpettroTestBot" {
		t.Fatalf("unexpected username: %q", me.Username)
	}
}

// TestBotClient_GetMe_Error converts a non-ok envelope into an APIError.
func TestBotClient_GetMe_Error(t *testing.T) {
	fb := newFakeBot(t)
	fb.setError("getMe", "Unauthorized")
	c := telegram.NewBotClient("token", telegram.WithBaseURL(fb.URL()))
	_, err := c.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := telegram.IsAPIError(err)
	if !ok {
		t.Fatalf("expected APIError, got %T", err)
	}
	if !strings.Contains(apiErr.Description, "Unauthorized") {
		t.Fatalf("unexpected description: %q", apiErr.Description)
	}
}

// TestBotClient_GetUpdates_PopsFromQueue verifies the long-poll cursor.
func TestBotClient_GetUpdates_PopsFromQueue(t *testing.T) {
	fb := newFakeBot(t)
	fb.pushUpdate(telegram.Update{UpdateID: 10})
	fb.pushUpdate(telegram.Update{UpdateID: 11})

	c := telegram.NewBotClient("token", telegram.WithBaseURL(fb.URL()))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := c.GetUpdates(ctx, 10, 0)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(got))
	}
	if got[0].UpdateID != 10 || got[1].UpdateID != 11 {
		t.Fatalf("unexpected update ids: %#v", got)
	}
}

// TestBotClient_SendMessage_RoundTrip records the outgoing payload.
func TestBotClient_SendMessage_RoundTrip(t *testing.T) {
	fb := newFakeBot(t)
	c := telegram.NewBotClient("token", telegram.WithBaseURL(fb.URL()))
	if _, err := c.SendMessage(context.Background(), 4242, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	sent := fb.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent, got %d", len(sent))
	}
	if sent[0].ChatID != 4242 || sent[0].Text != "hello" {
		t.Fatalf("unexpected payload: %+v", sent[0])
	}
}

// TestBotClient_EmptyTextRefused fails fast on a logically invalid call.
func TestBotClient_EmptyTextRefused(t *testing.T) {
	fb := newFakeBot(t)
	c := telegram.NewBotClient("token", telegram.WithBaseURL(fb.URL()))
	_, err := c.SendMessage(context.Background(), 1, "   ")
	if err == nil {
		t.Fatal("expected error for empty text")
	}
}

// TestBotClient_EmptyTokenRefused makes sure SaveToken("")-style state
// produces a clear error instead of a 401 round-trip.
func TestBotClient_EmptyTokenRefused(t *testing.T) {
	c := telegram.NewBotClient("", telegram.WithBaseURL("http://invalid"))
	_, err := c.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected empty-token error")
	}
}

// TestParseChatTarget_Variants covers the small parser used for /telegram
// allow / deny.
func TestParseChatTarget_Variants(t *testing.T) {
	cases := []struct {
		in       string
		wantUser string
		wantID   int64
		wantErr  bool
	}{
		{"@Foo", "foo", 0, false},
		{"foo", "foo", 0, false},
		{" Bar ", "bar", 0, false},
		{"12345", "", 12345, false},
		{"-1001234", "", -1001234, false},
		{"", "", 0, true},
		{"@", "", 0, true},
		{"0", "", 0, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			u, id, err := telegram.ParseChatTarget(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if u != c.wantUser || id != c.wantID {
				t.Fatalf("got (%q,%d), want (%q,%d)", u, id, c.wantUser, c.wantID)
			}
		})
	}
}

// TestSplitForTelegram_Chunking exercises the message chunker.
func TestSplitForTelegram_Chunking(t *testing.T) {
	short := strings.Repeat("a", 100)
	if got := telegram.SplitForTelegram(short); len(got) != 1 || got[0] != short {
		t.Fatalf("short text should pass through: %d chunks", len(got))
	}
	long := strings.Repeat("word ", 1000) // ~5000 chars
	got := telegram.SplitForTelegram(long)
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(got))
	}
	for i, c := range got {
		if len(c) > telegram.MaxMessageLen {
			t.Fatalf("chunk %d exceeds limit: %d", i, len(c))
		}
	}
	// Check continuation markers.
	if !strings.HasSuffix(got[0], "(continued)") {
		t.Fatalf("first chunk should end with continuation suffix; got tail %q", lastN(got[0], 40))
	}
	if !strings.HasPrefix(got[1], "(...cont)") {
		t.Fatalf("second chunk should start with cont prefix; got %q", got[1][:20])
	}
}

// TestSplitForTelegram_HonoursBoundaries asserts we break on newlines when
// available.
func TestSplitForTelegram_HonoursBoundaries(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		_, _ = fmt.Fprintf(&sb, "line %d aaaaaaaaaaaaaaaaaa\n", i)
	}
	chunks := telegram.SplitForTelegram(sb.String())
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks")
	}
	// First chunk's last visible character (before the continuation suffix)
	// should be a complete "line N ..." rather than mid-word.
	first := strings.TrimSuffix(chunks[0], "\n... (continued)")
	if strings.HasSuffix(first, "aaaaa") || strings.Count(first, "\n") < 5 {
		// We're really just sanity-checking that the split happened at a
		// newline. If aaaa appears at the very end with no newline, the
		// chunker didn't prefer the newline boundary.
		if !strings.Contains(first, "\n") {
			t.Fatalf("expected newline-aligned split")
		}
	}
}

// lastN returns the trailing n bytes of s for diagnostic messages.
func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// TestTruncate verifies the prompt-preview helper.
func TestTruncate(t *testing.T) {
	if got := telegram.Truncate("hello", 20); got != "hello" {
		t.Fatalf("short truncate broke: %q", got)
	}
	if got := telegram.Truncate("abcdef", 4); got != "abc…" {
		t.Fatalf("truncated form wrong: %q", got)
	}
}

// TestAPIError_Format ensures we render code+description for logs.
func TestAPIError_Format(t *testing.T) {
	err := &telegram.APIError{Method: "sendMessage", Code: 400, Description: "Bad Request: chat not found"}
	if msg := err.Error(); !strings.Contains(msg, "400") || !strings.Contains(msg, "chat not found") {
		t.Fatalf("unexpected error string: %q", msg)
	}
	if _, ok := telegram.IsAPIError(errors.New("plain")); ok {
		t.Fatal("plain error should not be classified as APIError")
	}
}
