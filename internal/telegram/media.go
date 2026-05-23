package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MediaKind classifies what kind of upload a caller is performing. The
// relay/bot pick the right Telegram method (`sendPhoto`, `sendVideo`, or
// `sendDocument` fallback) based on this and the file size.
type MediaKind string

const (
	// MediaKindImage routes through `sendPhoto` when the file fits the
	// 10 MB Bot API photo cap. Larger files fall back to `sendDocument`
	// so the user always receives something instead of an upload error.
	MediaKindImage MediaKind = "image"
	// MediaKindVideo routes through `sendVideo` when the file fits the
	// 50 MB cap; larger files fall back to `sendDocument`.
	MediaKindVideo MediaKind = "video"
	// MediaKindDocument routes through `sendDocument` directly, no
	// fallback. Useful when the caller already knows the media isn't a
	// photo or video.
	MediaKindDocument MediaKind = "document"
)

// Bot API hard limits on uploaded file size, per method, in bytes.
//
// Telegram occasionally tweaks these numbers; we use the documented
// values so we never get surprised by a confusing API error. When the
// content exceeds the cap we transparently degrade to `sendDocument`
// (which can carry up to ~2 GB) instead of failing the whole send.
const (
	maxPhotoBytes    = 10 * 1024 * 1024
	maxVideoBytes    = 50 * 1024 * 1024
	maxDocumentBytes = int64(2 * 1024 * 1024 * 1024)
)

// SendMediaFile uploads localPath to chatID using the most specific Bot API
// method available for kind. When the file exceeds the Telegram cap for
// that method, it transparently falls back to `sendDocument` so the user
// still receives the asset rather than a hard upload failure.
//
// `caption` is optional; if empty, no caption is included. Captions are
// truncated to the Telegram limit (1024 chars) — anything beyond is
// silently dropped to avoid 400 errors.
func (c *BotClient) SendMediaFile(ctx context.Context, chatID int64, kind MediaKind, localPath, caption string) (*Message, error) {
	if strings.TrimSpace(c.token) == "" {
		return nil, errors.New("telegram: bot token is empty")
	}
	if chatID == 0 {
		return nil, errors.New("telegram: chat id is zero")
	}
	localPath = strings.TrimSpace(localPath)
	if localPath == "" {
		return nil, errors.New("telegram: local path is empty")
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return nil, fmt.Errorf("telegram: stat %s: %w", localPath, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("telegram: %s is a directory", localPath)
	}
	if info.Size() > maxDocumentBytes {
		return nil, fmt.Errorf("telegram: %s is %d bytes, exceeds 2 GB cap", localPath, info.Size())
	}

	method, field := mediaEndpoint(kind, info.Size())
	return c.uploadMedia(ctx, method, field, chatID, localPath, caption)
}

// mediaEndpoint maps (kind, size) to the (method, multipart-field) pair we
// must hit on the Bot API. The field name is the parameter Telegram expects
// for the file part (e.g. `photo` for sendPhoto, `video` for sendVideo,
// `document` for sendDocument).
func mediaEndpoint(kind MediaKind, size int64) (method, field string) {
	switch kind {
	case MediaKindImage:
		if size <= maxPhotoBytes {
			return "sendPhoto", "photo"
		}
		return "sendDocument", "document"
	case MediaKindVideo:
		if size <= maxVideoBytes {
			return "sendVideo", "video"
		}
		return "sendDocument", "document"
	default:
		return "sendDocument", "document"
	}
}

// uploadMedia performs the actual multipart/form-data POST. The Bot API
// envelope decoding matches the JSON `call` path so callers see the same
// `APIError` type on failure.
func (c *BotClient) uploadMedia(ctx context.Context, method, field string, chatID int64, localPath, caption string) (*Message, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("telegram: open %s: %w", localPath, err)
	}
	defer f.Close()

	// Stream the upload through an io.Pipe so we don't have to buffer
	// the entire file in memory. The writer goroutine builds the
	// multipart body; if it errors out it closes the pipe with that
	// error so the HTTP request fails cleanly.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		if err := mw.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if cap := strings.TrimSpace(caption); cap != "" {
			if err := mw.WriteField("caption", Truncate(cap, 1024)); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		part, err := mw.CreateFormFile(field, filepath.Base(localPath))
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, f); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := mw.Close(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}()

	endpoint := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pr)
	if err != nil {
		return nil, fmt.Errorf("telegram: build %s: %w", method, err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("telegram: read %s: %w", method, err)
	}
	var env apiResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("telegram: decode %s: %w (body=%q)", method, err, truncateForLog(raw))
	}
	if !env.OK {
		desc := strings.TrimSpace(env.Description)
		if desc == "" {
			desc = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return nil, &APIError{Method: method, Code: env.ErrorCode, Description: desc}
	}
	var msg Message
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &msg); err != nil {
			return nil, fmt.Errorf("telegram: decode %s result: %w", method, err)
		}
	}
	return &msg, nil
}

// classifyMedia returns a MediaKind based on the file extension. Anything
// the function doesn't recognise as image/video defaults to a document
// upload so the user still receives the file.
func classifyMedia(localPath string) MediaKind {
	ext := strings.ToLower(filepath.Ext(localPath))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".heic", ".heif":
		return MediaKindImage
	case ".mp4", ".webm", ".mov", ".mkv", ".m4v":
		return MediaKindVideo
	default:
		return MediaKindDocument
	}
}

// ClassifyMedia exposes classifyMedia to callers who only have a local
// path and want the default routing.
func ClassifyMedia(localPath string) MediaKind {
	return classifyMedia(localPath)
}

// MaxPhotoBytesForTesting / MaxVideoBytesForTesting return the documented
// Bot API caps so tests can exercise the fallback path without redeclaring
// magic numbers in two places.
func MaxPhotoBytesForTesting() int64    { return maxPhotoBytes }
func MaxVideoBytesForTesting() int64    { return maxVideoBytes }
func MaxDocumentBytesForTesting() int64 { return maxDocumentBytes }

// MediaEndpointForTesting is a tiny shim exposing mediaEndpoint to tests
// so they can assert routing without going through the full HTTP path.
func MediaEndpointForTesting(kind MediaKind, size int64) (string, string) {
	return mediaEndpoint(kind, size)
}
