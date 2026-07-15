package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The view-image tool feeds an image file to the model as vision input, so
// the agent can look at anything it can get onto disk — a screenshot it took
// itself (headless chromium / playwright via the shell tools), a rendered
// chart, a design asset — without the user hand-feeding images.
//
// The tool does not talk to a provider directly: it validates the file and
// registers its path on the per-call image sink; the tool loop moves sink
// paths onto provider.ToolResult.Images (native tools) or the feedback user
// message (text protocol), and the provider layer turns them into image
// blocks the model can see.

// maxAttachBytes caps the image size fed to the model. Providers reject
// oversized base64 payloads (Anthropic caps around 5MB decoded).
const maxAttachBytes = 4 << 20

// --- image sink --------------------------------------------------------------

// imageSink collects image paths a tool attaches during one call. It travels
// via the call context because tools in a batch run in parallel goroutines and
// a shared runtime field could not tell their attachments apart.
type imageSink struct {
	mu    sync.Mutex
	paths []string
}

type imageSinkKey struct{}

// withImageSink derives a context carrying a fresh sink for one tool call.
func withImageSink(ctx context.Context) (context.Context, *imageSink) {
	s := &imageSink{}
	return context.WithValue(ctx, imageSinkKey{}, s), s
}

// attachResultImage registers an image file for the model to see alongside the
// current tool call's result. No-op when the context carries no sink (e.g.
// direct runtime calls in tests).
func attachResultImage(ctx context.Context, path string) {
	s, ok := ctx.Value(imageSinkKey{}).(*imageSink)
	if !ok || s == nil {
		return
	}
	s.mu.Lock()
	s.paths = append(s.paths, path)
	s.mu.Unlock()
}

func (s *imageSink) list() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// modelSupportsVision reports whether the model the loop is currently calling
// can consume image input. visionCheck is a test seam.
func (r *toolRuntime) modelSupportsVision() bool {
	if r.visionCheck != nil {
		return r.visionCheck()
	}
	if r.providerMgr == nil || r.providerName == nil || r.modelName == nil {
		return false
	}
	m := r.effectiveModel()
	return r.providerMgr.SupportsVision(m.Provider, m.Model)
}

// --- view-image tool ---------------------------------------------------------

type viewImageArgs struct {
	Path string `json:"path"`
}

// imageMediaTypes gates view-image to formats every vision provider accepts.
var imageMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

func (r *toolRuntime) runViewImage(ctx context.Context, rawArgs []byte) (string, error) {
	var args viewImageArgs
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("view-image args: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return "", fmt.Errorf("view-image: path is required")
	}
	abs, rel, err := r.resolvePath(args.Path)
	if err != nil {
		return "", fmt.Errorf("view-image: %w", err)
	}
	mediaType, ok := imageMediaTypes[strings.ToLower(filepath.Ext(abs))]
	if !ok {
		return "", fmt.Errorf("view-image: unsupported image type %q (png, jpg, webp, gif)", filepath.Ext(abs))
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("view-image: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("view-image: %s is a directory", rel)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("view-image: %s is empty", rel)
	}
	if info.Size() > maxAttachBytes {
		return "", fmt.Errorf("view-image: %s is %d bytes (limit %d) — resize or re-capture it smaller first", rel, info.Size(), maxAttachBytes)
	}
	if !r.modelSupportsVision() {
		return "", fmt.Errorf("view-image: the active model has no vision support; switch to a vision-capable model to look at images")
	}
	attachResultImage(ctx, abs)
	r.mu.Lock()
	r.readSet[rel] = struct{}{}
	r.mu.Unlock()
	out := map[string]any{
		"tool":       "view-image",
		"file":       rel,
		"media_type": mediaType,
		"size_bytes": info.Size(),
		"attached":   true,
	}
	raw, _ := json.Marshal(out)
	return string(raw), nil
}
