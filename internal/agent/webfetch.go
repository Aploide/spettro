package agent

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// webFetchReadLimit caps how many response bytes web-fetch reads before
	// converting/truncating for the model.
	webFetchReadLimit = 2 * 1024 * 1024
	// webFetchDefaultBudget / webFetchMaxBudget bound the text returned to the
	// model after conversion.
	webFetchDefaultBudget = 20000
	webFetchMaxBudget     = 50000
	// downloadDefaultLimit / downloadMaxLimit bound file downloads.
	downloadDefaultLimit = 20 * 1024 * 1024
	downloadMaxLimit     = 200 * 1024 * 1024
	// maxHTTPRedirects caps redirect chains for model-chosen URLs.
	maxHTTPRedirects = 5
)

// fetchClient returns the HTTP client used for model-chosen URLs: the injected
// test client when set, otherwise a fresh SSRF-safe client. Either way the
// redirect chain is capped and every hop is re-validated as http(s).
func (r *toolRuntime) fetchClient(timeout time.Duration) *http.Client {
	base := r.httpClient
	if base == nil {
		base = newSafeHTTPClient(timeout)
	}
	c := *base // shallow copy so the redirect policy doesn't mutate shared state
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxHTTPRedirects {
			return fmt.Errorf("stopped after %d redirects", maxHTTPRedirects)
		}
		return validatePublicURL(req.URL.String())
	}
	return &c
}

func (r *toolRuntime) runWebFetch(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		URL       string `json:"url"`
		MaxLength int    `json:"max_length"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("web-fetch args: %w", err)
	}
	urlText := strings.TrimSpace(args.URL)
	if urlText == "" {
		return "", fmt.Errorf("web-fetch: url is required")
	}
	if err := validatePublicURL(urlText); err != nil {
		return "", fmt.Errorf("web-fetch: %w", err)
	}
	if err := r.authorizeNetworkAccess(ctx, "web-fetch", urlText); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlText, nil)
	if err != nil {
		return "", fmt.Errorf("web-fetch: %w", err)
	}
	req.Header.Set("User-Agent", "Spettro Agent/1.0")
	req.Header.Set("Accept", "text/html, text/plain, application/json, application/xml;q=0.9, */*;q=0.5")
	resp, err := r.fetchClient(30 * time.Second).Do(req)
	if err != nil {
		return "", fmt.Errorf("web-fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("web-fetch failed: %s", resp.Status)
	}
	ctype := resp.Header.Get("Content-Type")
	if !isTextualContentType(ctype) {
		return "", fmt.Errorf("web-fetch: unsupported content type %q (use the download tool for binary content)", ctype)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchReadLimit))
	if err != nil {
		return "", fmt.Errorf("web-fetch: read: %w", err)
	}
	text := string(body)
	if isHTMLContentType(ctype) || looksLikeHTML(text) {
		finalURL := urlText
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
		text = convertHTMLPage(text, finalURL).render(finalURL)
	}
	budget := args.MaxLength
	if budget <= 0 {
		budget = webFetchDefaultBudget
	}
	if budget > webFetchMaxBudget {
		budget = webFetchMaxBudget
	}
	if len(text) > budget {
		text = text[:budget] + "\n\n[content truncated at " + fmt.Sprintf("%d", budget) + " chars]"
	}
	return text, nil
}

func (r *toolRuntime) runDownload(ctx context.Context, rawArgs []byte) (string, error) {
	var args struct {
		URL      string `json:"url"`
		Path     string `json:"path"`
		MaxBytes int64  `json:"max_bytes"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("download args: %w", err)
	}
	urlText := strings.TrimSpace(args.URL)
	if urlText == "" {
		return "", fmt.Errorf("download: url is required")
	}
	if err := validatePublicURL(urlText); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	abs, rel, err := r.resolvePath(args.Path)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	limit := args.MaxBytes
	if limit <= 0 {
		limit = downloadDefaultLimit
	}
	if limit > downloadMaxLimit {
		limit = downloadMaxLimit
	}
	if err := r.authorizeNetworkAccess(ctx, "download", urlText); err != nil {
		return "", err
	}
	if err := r.authorizeWriteAccess(ctx, "download", rel, fmt.Sprintf("download %s -> %s (max %d bytes)", urlText, rel, limit)); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlText, nil)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	req.Header.Set("User-Agent", "Spettro Agent/1.0")
	resp, err := r.fetchClient(120 * time.Second).Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}
	if resp.ContentLength > limit {
		return "", fmt.Errorf("download: content length %d exceeds limit %d bytes", resp.ContentLength, limit)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	// Stream to a temp file next to the destination, then rename: a failed or
	// oversized download never leaves a partial file at the target path.
	tmp, err := os.CreateTemp(filepath.Dir(abs), "."+filepath.Base(abs)+".part-*")
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, limit+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download: write: %w", err)
	}
	if n > limit {
		return "", fmt.Errorf("download: response exceeds limit of %d bytes", limit)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	r.mu.Lock()
	r.readSet[rel] = struct{}{}
	r.mu.Unlock()
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "unknown"
	}
	return fmt.Sprintf("downloaded %s (%d bytes, %s)", rel, n, ctype), nil
}

// isTextualContentType reports whether a Content-Type is safe to return to the
// model as text. An empty header is allowed (some plain-text endpoints omit
// it); binary types must go through the download tool instead.
func isTextualContentType(ctype string) bool {
	if strings.TrimSpace(ctype) == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(ctype)
	if err != nil {
		return false
	}
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	switch mt {
	case "application/json", "application/xml", "application/javascript",
		"application/xhtml+xml", "application/x-yaml", "application/yaml":
		return true
	}
	return strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml")
}

func isHTMLContentType(ctype string) bool {
	mt, _, err := mime.ParseMediaType(ctype)
	if err != nil {
		return false
	}
	return mt == "text/html" || mt == "application/xhtml+xml"
}

func looksLikeHTML(s string) bool {
	head := strings.ToLower(s[:min(len(s), 1024)])
	return strings.Contains(head, "<!doctype html") || strings.Contains(head, "<html")
}

var blankLinesRE = regexp.MustCompile(`\n{3,}`)
