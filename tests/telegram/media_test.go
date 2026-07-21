package telegram_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"spettro/internal/telegram"
)

// bindAuthedChat pushes one allowlisted message through the relay so the
// chat gets added to BoundChats(). It consumes the resulting submission
// and ack so subsequent assertions only see fresh state. Use this when a
// test needs the relay to know about a chat without exercising the full
// inbound flow.
func bindAuthedChat(t *testing.T, fb *fakeBot, r *telegram.Relay, chatID int64, username string) {
	t.Helper()
	fb.pushUpdate(telegram.Update{
		UpdateID: chatID,
		Message: &telegram.Message{
			MessageID: chatID,
			Date:      time.Now().Unix(),
			Text:      "bind",
			From:      &telegram.User{ID: chatID, Username: username},
			Chat:      &telegram.Chat{ID: chatID, Type: "private"},
		},
	})
	select {
	case req := <-r.Submissions():
		req.Reply <- telegram.SubmitResponse{Accepted: true, Note: "bound"}
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout binding chat %d", chatID)
	}
	// Wait for the bound bookkeeping to flush.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(r.BoundChats(), chatID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("chat %d never appeared in BoundChats: %+v", chatID, r.BoundChats())
}

// TestSendMediaFile_RoutesByExtensionAndSize covers the small routing
// matrix in mediaEndpoint plus an end-to-end happy path: we point a real
// BotClient at the fakeBot, upload an in-memory file, and assert the
// resulting multipart payload reached the right endpoint with the right
// field name and bytes.
func TestSendMediaFile_RoutesByExtensionAndSize(t *testing.T) {
	fb := newFakeBot(t)
	c := telegram.NewBotClient("token", telegram.WithBaseURL(fb.URL()))
	dir := t.TempDir()

	imgPath := writeMedia(t, dir, "logo.png", "image-bytes")
	vidPath := writeMedia(t, dir, "clip.mp4", "video-bytes")
	docPath := writeMedia(t, dir, "readme.txt", "doc-bytes")

	cases := []struct {
		name      string
		path      string
		kind      telegram.MediaKind
		caption   string
		wantField string
		wantMthd  string
	}{
		{"image goes via sendPhoto", imgPath, telegram.MediaKindImage, "🖼 hello", "photo", "sendPhoto"},
		{"video goes via sendVideo", vidPath, telegram.MediaKindVideo, "", "video", "sendVideo"},
		{"explicit document forces sendDocument", docPath, telegram.MediaKindDocument, "log.txt", "document", "sendDocument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(fb.sentMediaCalls())
			if _, err := c.SendMediaFile(context.Background(), 4242, tc.kind, tc.path, tc.caption); err != nil {
				t.Fatalf("SendMediaFile: %v", err)
			}
			calls := fb.sentMediaCalls()
			if len(calls) != before+1 {
				t.Fatalf("expected one new media call, got %d", len(calls)-before)
			}
			got := calls[before]
			if got.Method != tc.wantMthd {
				t.Fatalf("method: got %q want %q", got.Method, tc.wantMthd)
			}
			if got.Field != tc.wantField {
				t.Fatalf("field: got %q want %q", got.Field, tc.wantField)
			}
			if got.ChatID != 4242 {
				t.Fatalf("chat id: got %d want 4242", got.ChatID)
			}
			if got.Caption != strings.TrimSpace(tc.caption) {
				t.Fatalf("caption: got %q want %q", got.Caption, tc.caption)
			}
			if want := filepath.Base(tc.path); got.Filename != want {
				t.Fatalf("filename: got %q want %q", got.Filename, want)
			}
			wantBody, _ := os.ReadFile(tc.path)
			if !bytes.Equal(got.Body, wantBody) {
				t.Fatalf("upload body mismatch: got %q want %q", got.Body, wantBody)
			}
		})
	}
}

// TestSendMediaFile_FallsBackToDocumentOnAPIError verifies that when the
// API returns an explicit error the caller observes a Telegram APIError
// (so the relay can record it for /telegram status).
func TestSendMediaFile_FallsBackToDocumentOnAPIError(t *testing.T) {
	fb := newFakeBot(t)
	fb.setError("sendMedia", "Bad Request: photo too big")
	c := telegram.NewBotClient("token", telegram.WithBaseURL(fb.URL()))
	path := writeMedia(t, t.TempDir(), "logo.png", "x")

	_, err := c.SendMediaFile(context.Background(), 1, telegram.MediaKindImage, path, "")
	if err == nil {
		t.Fatal("expected API error")
	}
	apiErr, ok := telegram.IsAPIError(err)
	if !ok {
		t.Fatalf("expected APIError, got %T (%v)", err, err)
	}
	if !strings.Contains(apiErr.Description, "photo too big") {
		t.Fatalf("unexpected error description: %q", apiErr.Description)
	}
}

// TestSendMediaFile_RejectsMissingAndDirectories ensures the client fails
// fast before contacting Telegram when the local path doesn't make sense.
func TestSendMediaFile_RejectsMissingAndDirectories(t *testing.T) {
	fb := newFakeBot(t)
	c := telegram.NewBotClient("token", telegram.WithBaseURL(fb.URL()))
	dir := t.TempDir()
	cases := []struct {
		name string
		path string
	}{
		{"empty path", ""},
		{"missing file", filepath.Join(dir, "nope.png")},
		{"directory", dir},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.SendMediaFile(context.Background(), 1, telegram.MediaKindImage, tc.path, "")
			if err == nil {
				t.Fatalf("expected error for %q", tc.path)
			}
		})
	}
	if len(fb.sentMediaCalls()) != 0 {
		t.Fatal("no network calls should have happened")
	}
}

// TestMediaEndpoint_Routing covers the pure routing helper independent of
// HTTP plumbing.
func TestMediaEndpoint_Routing(t *testing.T) {
	cases := []struct {
		kind      telegram.MediaKind
		size      int64
		wantMthd  string
		wantField string
	}{
		{telegram.MediaKindImage, 1024, "sendPhoto", "photo"},
		{telegram.MediaKindImage, telegram.MaxPhotoBytesForTesting(), "sendPhoto", "photo"},
		{telegram.MediaKindImage, telegram.MaxPhotoBytesForTesting() + 1, "sendDocument", "document"},
		{telegram.MediaKindVideo, 1024, "sendVideo", "video"},
		{telegram.MediaKindVideo, telegram.MaxVideoBytesForTesting() + 1, "sendDocument", "document"},
		{telegram.MediaKindDocument, 1024, "sendDocument", "document"},
	}
	for _, tc := range cases {
		gotMthd, gotField := telegram.MediaEndpointForTesting(tc.kind, tc.size)
		if gotMthd != tc.wantMthd || gotField != tc.wantField {
			t.Errorf("kind=%s size=%d: got (%q, %q) want (%q, %q)", tc.kind, tc.size, gotMthd, gotField, tc.wantMthd, tc.wantField)
		}
	}
}

// TestClassifyMedia covers the extension-to-MediaKind helper used by the
// TUI dispatcher to pick the right upload method.
func TestClassifyMedia(t *testing.T) {
	cases := map[string]telegram.MediaKind{
		"foo.png":           telegram.MediaKindImage,
		"foo.PNG":           telegram.MediaKindImage,
		"x.jpg":             telegram.MediaKindImage,
		"x.jpeg":            telegram.MediaKindImage,
		"x.webp":            telegram.MediaKindImage,
		"x.gif":             telegram.MediaKindImage,
		"clip.mp4":          telegram.MediaKindVideo,
		"clip.webm":         telegram.MediaKindVideo,
		"clip.mov":          telegram.MediaKindVideo,
		"thing.zip":         telegram.MediaKindDocument,
		"no-extension-file": telegram.MediaKindDocument,
		"":                  telegram.MediaKindDocument,
	}
	for path, want := range cases {
		if got := telegram.ClassifyMedia(path); got != want {
			t.Errorf("ClassifyMedia(%q) = %s, want %s", path, got, want)
		}
	}
}

// TestRelay_BroadcastMedia_FanOutToBoundChats wires the fake bot into a
// real Relay, binds two chats, then calls BroadcastMedia and asserts each
// chat received exactly one matching upload.
func TestRelay_BroadcastMedia_FanOutToBoundChats(t *testing.T) {
	fb := newFakeBot(t)
	r := startRelay(t, fb, telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{
			{ChatID: 10}, {ChatID: 20},
		},
	})
	defer r.Stop()
	bindAuthedChat(t, fb, r, 10, "u10")
	bindAuthedChat(t, fb, r, 20, "u20")

	path := writeMedia(t, t.TempDir(), "logo.png", "logo-bytes")
	r.BroadcastMedia(path, telegram.MediaKindImage, "hero")

	calls := fb.sentMediaCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 media uploads, got %d (%+v)", len(calls), calls)
	}
	seen := map[int64]bool{}
	for _, c := range calls {
		if c.Method != "sendPhoto" || c.Field != "photo" {
			t.Errorf("unexpected routing for chat %d: method=%q field=%q", c.ChatID, c.Method, c.Field)
		}
		if c.Caption != "hero" {
			t.Errorf("expected caption=hero, got %q", c.Caption)
		}
		if c.Filename != "logo.png" {
			t.Errorf("expected filename=logo.png, got %q", c.Filename)
		}
		seen[c.ChatID] = true
	}
	if !seen[10] || !seen[20] {
		t.Fatalf("expected both bound chats to receive a copy, got %v", seen)
	}
}

// TestRelay_BroadcastMediaTo_RecordsSendError surfaces upload failures
// through LastSendError() so /telegram status can show them.
func TestRelay_BroadcastMediaTo_RecordsSendError(t *testing.T) {
	fb := newFakeBot(t)
	fb.setError("sendMedia", "Bad Request: payload empty")
	r := startRelay(t, fb, telegram.PersistedConfig{
		Allowlist: []telegram.AllowEntry{{ChatID: 99}},
	})
	defer r.Stop()
	bindAuthedChat(t, fb, r, 99, "u99")

	path := writeMedia(t, t.TempDir(), "logo.png", "x")
	r.BroadcastMediaTo(99, path, telegram.MediaKindImage, "")

	err := r.LastSendError()
	if err == nil {
		t.Fatal("expected LastSendError to be populated after upload failure")
	}
	if !strings.Contains(err.Error(), "payload empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// writeMedia is a thin helper that materialises an in-memory blob to a
// temp file so the upload code path has a real os.File to read from.
func writeMedia(t *testing.T, dir, name, contents string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}
