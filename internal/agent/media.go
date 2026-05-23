package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"spettro/internal/config"
)

// Grok / xAI media-generation endpoints.
//
// All requests share the same `https://api.x.ai/v1` base, authenticated with
// `Authorization: Bearer $XAI_API_KEY`. The key is resolved from the encrypted
// `~/.spettro/keys.json` (entries `x-ai` / `xai`) and falls back to the
// `XAI_API_KEY` environment variable. We do NOT require the user to have xAI
// selected as their active chat provider — only that the key exists.
const (
	xaiImagesEndpoint    = "https://api.x.ai/v1/images/generations"
	xaiVideosEndpoint    = "https://api.x.ai/v1/videos/generations"
	xaiVideoStatusBase   = "https://api.x.ai/v1/videos/"
	defaultGrokImage     = "grok-imagine-image-quality"
	defaultGrokVideo     = "grok-imagine-video"
	defaultMediaDir      = "assets"
	nextjsPublicDir      = "public"
	maxVideoPollDuration = 12 * time.Minute
	videoPollInterval    = 5 * time.Second
)

// xaiAPIKey resolves the xAI API key from Spettro's encrypted keystore, the
// active manager (when wired), or the environment. The empty string means no
// key was found.
func xaiAPIKey() string {
	keys, _ := config.LoadAPIKeys()
	for _, id := range []string{"x-ai", "xai"} {
		if v := strings.TrimSpace(keys[id]); v != "" {
			return v
		}
	}
	return strings.TrimSpace(os.Getenv("XAI_API_KEY"))
}

// --- image generation -------------------------------------------------------

type grokImageArgs struct {
	Prompt         string `json:"prompt"`
	Path           string `json:"path"`
	Model          string `json:"model"`
	N              int    `json:"n"`
	AspectRatio    string `json:"aspect_ratio"`
	Resolution     string `json:"resolution"`
	ResponseFormat string `json:"response_format"`
}

type grokImageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`
	AspectRatio    string `json:"aspect_ratio,omitempty"`
	Resolution     string `json:"resolution,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

type grokImageData struct {
	URL      string `json:"url"`
	B64JSON  string `json:"b64_json"`
	MimeType string `json:"mime_type"`
}

type grokImageResponse struct {
	Data []grokImageData `json:"data"`
}

func (r *toolRuntime) runGrokImage(ctx context.Context, rawArgs []byte) (string, error) {
	var args grokImageArgs
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("grok-image args: %w", err)
	}
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return "", fmt.Errorf("grok-image: prompt is required")
	}

	apiKey := xaiAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("grok-image: no xAI API key configured (run /connect x-ai or set XAI_API_KEY)")
	}

	model := strings.TrimSpace(args.Model)
	if model == "" {
		model = defaultGrokImage
	}
	if args.N < 0 {
		return "", fmt.Errorf("grok-image: n must be >= 0")
	}
	if args.N == 0 {
		args.N = 1
	}
	// b64_json is easier to write reliably to disk than a temporary URL that
	// must be re-fetched, so default to that when the caller didn't specify.
	respFormat := strings.TrimSpace(args.ResponseFormat)
	if respFormat == "" {
		respFormat = "b64_json"
	}

	payload := grokImageRequest{
		Model:          model,
		Prompt:         prompt,
		N:              args.N,
		AspectRatio:    strings.TrimSpace(args.AspectRatio),
		Resolution:     strings.TrimSpace(args.Resolution),
		ResponseFormat: respFormat,
	}

	body, err := postXAIJSON(ctx, xaiImagesEndpoint, apiKey, payload)
	if err != nil {
		return "", fmt.Errorf("grok-image: %w", err)
	}
	var parsed grokImageResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("grok-image: decode response: %w", err)
	}
	if len(parsed.Data) == 0 {
		return "", fmt.Errorf("grok-image: empty response")
	}

	saved, err := r.saveMediaBatch(ctx, args.Path, prompt, "image", parsed.Data)
	if err != nil {
		return "", err
	}
	out := map[string]any{
		"tool":   "grok-image",
		"model":  model,
		"prompt": truncate(prompt, 200),
		"count":  len(saved),
		"files":  saved,
	}
	raw, _ := json.Marshal(out)
	return string(raw), nil
}

// --- video generation -------------------------------------------------------

type grokVideoArgs struct {
	Prompt              string   `json:"prompt"`
	Path                string   `json:"path"`
	Model               string   `json:"model"`
	Duration            int      `json:"duration"`
	AspectRatio         string   `json:"aspect_ratio"`
	Resolution          string   `json:"resolution"`
	ImageURL            string   `json:"image_url"`
	ReferenceImageURLs  []string `json:"reference_image_urls"`
}

type grokVideoRequest struct {
	Model              string   `json:"model"`
	Prompt             string   `json:"prompt"`
	Duration           int      `json:"duration,omitempty"`
	AspectRatio        string   `json:"aspect_ratio,omitempty"`
	Resolution         string   `json:"resolution,omitempty"`
	ImageURL           string   `json:"image_url,omitempty"`
	ReferenceImageURLs []string `json:"reference_image_urls,omitempty"`
}

type grokVideoStartResponse struct {
	RequestID string `json:"request_id"`
}

type grokVideoStatusResponse struct {
	Status string `json:"status"`
	Video  struct {
		URL      string `json:"url"`
		MimeType string `json:"mime_type"`
	} `json:"video"`
	Error string `json:"error"`
}

func (r *toolRuntime) runGrokVideo(ctx context.Context, rawArgs []byte) (string, error) {
	var args grokVideoArgs
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("grok-video args: %w", err)
	}
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return "", fmt.Errorf("grok-video: prompt is required")
	}
	apiKey := xaiAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("grok-video: no xAI API key configured (run /connect x-ai or set XAI_API_KEY)")
	}
	model := strings.TrimSpace(args.Model)
	if model == "" {
		model = defaultGrokVideo
	}

	payload := grokVideoRequest{
		Model:              model,
		Prompt:             prompt,
		Duration:           args.Duration,
		AspectRatio:        strings.TrimSpace(args.AspectRatio),
		Resolution:         strings.TrimSpace(args.Resolution),
		ImageURL:           strings.TrimSpace(args.ImageURL),
		ReferenceImageURLs: args.ReferenceImageURLs,
	}

	body, err := postXAIJSON(ctx, xaiVideosEndpoint, apiKey, payload)
	if err != nil {
		return "", fmt.Errorf("grok-video: %w", err)
	}
	var start grokVideoStartResponse
	if err := json.Unmarshal(body, &start); err != nil {
		return "", fmt.Errorf("grok-video: decode start response: %w", err)
	}
	if strings.TrimSpace(start.RequestID) == "" {
		return "", fmt.Errorf("grok-video: missing request_id in response: %s", truncate(string(body), 240))
	}

	status, err := pollGrokVideo(ctx, apiKey, start.RequestID)
	if err != nil {
		return "", fmt.Errorf("grok-video: %w", err)
	}
	if status.Status != "done" || strings.TrimSpace(status.Video.URL) == "" {
		return "", fmt.Errorf("grok-video: generation finished with status %q (%s)", status.Status, truncate(status.Error, 200))
	}

	// Treat the result as a single-item batch so saveMediaBatch handles
	// slugging + extension selection consistently with image flows.
	saved, err := r.saveMediaBatch(ctx, args.Path, prompt, "video", []grokImageData{{URL: status.Video.URL, MimeType: status.Video.MimeType}})
	if err != nil {
		return "", err
	}
	out := map[string]any{
		"tool":       "grok-video",
		"model":      model,
		"prompt":     truncate(prompt, 200),
		"request_id": start.RequestID,
		"files":      saved,
	}
	raw, _ := json.Marshal(out)
	return string(raw), nil
}

func pollGrokVideo(ctx context.Context, apiKey, requestID string) (grokVideoStatusResponse, error) {
	deadline := time.Now().Add(maxVideoPollDuration)
	statusURL := xaiVideoStatusBase + url.PathEscape(requestID)
	for {
		if err := ctx.Err(); err != nil {
			return grokVideoStatusResponse{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
		if err != nil {
			return grokVideoStatusResponse{}, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return grokVideoStatusResponse{}, err
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil {
			return grokVideoStatusResponse{}, err
		}
		if resp.StatusCode >= 300 {
			return grokVideoStatusResponse{}, fmt.Errorf("status %s: %s", resp.Status, truncate(string(raw), 240))
		}
		var st grokVideoStatusResponse
		if err := json.Unmarshal(raw, &st); err != nil {
			return grokVideoStatusResponse{}, fmt.Errorf("decode status: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(st.Status)) {
		case "done":
			return st, nil
		case "failed":
			return st, fmt.Errorf("video generation failed: %s", truncate(st.Error, 200))
		case "expired":
			return st, fmt.Errorf("video generation request expired")
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("timed out polling video request after %s", maxVideoPollDuration)
		}
		select {
		case <-ctx.Done():
			return grokVideoStatusResponse{}, ctx.Err()
		case <-time.After(videoPollInterval):
		}
	}
}

// --- shared HTTP helper -----------------------------------------------------

func postXAIJSON(ctx context.Context, endpoint, apiKey string, body any) ([]byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("xai api %s: %s", resp.Status, truncate(string(raw), 480))
	}
	return raw, nil
}

// --- file persistence -------------------------------------------------------

// saveMediaBatch writes a batch of generated media items to disk under the
// caller's chosen path (or the workspace default if none was supplied) and
// returns the relative paths it wrote.
func (r *toolRuntime) saveMediaBatch(ctx context.Context, requestedPath, prompt, kind string, items []grokImageData) ([]string, error) {
	if len(items) == 0 {
		return nil, nil
	}

	baseDir, baseName, fixedExt, hasExt, dirOnly := resolveMediaPath(r.cwd, requestedPath, prompt, kind)
	saved := make([]string, 0, len(items))
	for i, item := range items {
		ext := fixedExt
		if !hasExt {
			ext = pickExtension(item.MimeType, kind)
		}
		var name string
		switch {
		case hasExt && len(items) == 1:
			name = baseName + ext
		case hasExt:
			name = fmt.Sprintf("%s-%d%s", baseName, i+1, ext)
		case dirOnly && len(items) == 1:
			name = baseName + ext
		default:
			name = fmt.Sprintf("%s-%d%s", baseName, i+1, ext)
		}
		rel, err := r.writeMediaFile(ctx, filepath.Join(baseDir, name), item)
		if err != nil {
			return saved, err
		}
		saved = append(saved, rel)
	}
	return saved, nil
}

// resolveMediaPath turns an optional caller-supplied path + prompt into a
// concrete directory + base filename, defaulting to the project's natural
// asset folder (Next.js `public/` if detected, else `assets/`) when nothing
// was provided.
func resolveMediaPath(cwd, requested, prompt, kind string) (dir, baseName, fixedExt string, hasExt, dirOnly bool) {
	slug := slugifyPrompt(prompt)
	if slug == "" {
		slug = fmt.Sprintf("%s-%d", kind, time.Now().Unix())
	}

	cleaned := strings.TrimSpace(requested)
	if cleaned == "" {
		return filepath.Join(cwd, defaultMediaDirFor(cwd)), slug, "", false, true
	}

	if !filepath.IsAbs(cleaned) {
		cleaned = filepath.Clean(filepath.Join(cwd, cleaned))
	}
	// Treat trailing separator or an existing directory as "save into this folder".
	if strings.HasSuffix(requested, "/") || strings.HasSuffix(requested, string(filepath.Separator)) {
		return cleaned, slug, "", false, true
	}
	if info, err := os.Stat(cleaned); err == nil && info.IsDir() {
		return cleaned, slug, "", false, true
	}

	ext := strings.ToLower(filepath.Ext(cleaned))
	if ext == "" {
		return cleaned, slug, "", false, true
	}
	base := strings.TrimSuffix(filepath.Base(cleaned), filepath.Ext(cleaned))
	return filepath.Dir(cleaned), base, ext, true, false
}

// defaultMediaDirFor picks the conventional output folder for the project.
// Next.js sites expect static assets under `public/`; everything else gets
// `assets/`.
func defaultMediaDirFor(cwd string) string {
	if isNextJSProject(cwd) {
		return nextjsPublicDir
	}
	return defaultMediaDir
}

func isNextJSProject(cwd string) bool {
	for _, name := range []string{"next.config.js", "next.config.mjs", "next.config.ts", "next.config.cjs"} {
		if _, err := os.Stat(filepath.Join(cwd, name)); err == nil {
			return true
		}
	}
	pkg := filepath.Join(cwd, "package.json")
	data, err := os.ReadFile(pkg)
	if err != nil {
		return false
	}
	var parsed struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return false
	}
	if _, ok := parsed.Dependencies["next"]; ok {
		return true
	}
	_, ok := parsed.DevDependencies["next"]
	return ok
}

func pickExtension(mime, kind string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	}
	if kind == "video" {
		return ".mp4"
	}
	return ".png"
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slugifyPrompt(prompt string) string {
	s := strings.ToLower(strings.TrimSpace(prompt))
	s = slugRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
		s = strings.Trim(s, "-")
	}
	return s
}

// writeMediaFile materializes a single image/video item to absPath, decoding
// base64 payloads inline or downloading a presigned URL otherwise. Returns
// the workspace-relative path it wrote.
func (r *toolRuntime) writeMediaFile(ctx context.Context, absPath string, item grokImageData) (string, error) {
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return "", fmt.Errorf("create media dir: %w", err)
	}
	if strings.TrimSpace(item.B64JSON) != "" {
		raw, err := base64.StdEncoding.DecodeString(item.B64JSON)
		if err != nil {
			return "", fmt.Errorf("decode b64_json: %w", err)
		}
		if err := os.WriteFile(absPath, raw, 0o644); err != nil {
			return "", err
		}
	} else if strings.TrimSpace(item.URL) != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.URL, nil)
		if err != nil {
			return "", err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return "", fmt.Errorf("download %s: %s", item.URL, resp.Status)
		}
		f, err := os.Create(absPath)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
	} else {
		return "", fmt.Errorf("response item had no url or b64_json")
	}

	if r != nil && r.cwd != "" {
		if rel, err := filepath.Rel(r.cwd, absPath); err == nil {
			r.mu.Lock()
			r.readSet[filepath.ToSlash(rel)] = struct{}{}
			r.mu.Unlock()
			return filepath.ToSlash(rel), nil
		}
	}
	return absPath, nil
}
