package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/config"
)

// newWebToolRuntime builds a minimal runtime for exercising web-fetch and
// download against a local httptest server: YOLO permission skips approvals
// and the injected plain client bypasses the SSRF loopback block (which is
// tested separately in hardening_test.go).
func newWebToolRuntime(t *testing.T) *toolRuntime {
	t.Helper()
	return &toolRuntime{
		cwd:          t.TempDir(),
		permission:   config.PermissionYOLO,
		toolPolicies: map[string]config.ToolSpec{},
		readSet:      map[string]struct{}{},
		httpClient:   &http.Client{},
	}
}

func TestConvertHTMLPageBasics(t *testing.T) {
	in := `<!doctype html><html><head><title>My Page</title>
<meta property="article:published_time" content="2026-01-02T03:04:05Z">
<style>body{color:red}</style></head>
<body><script>alert(1)</script><h1>Main &amp; Title</h1>
<p>Hello <a href="/doc">docs link</a> world, this paragraph is long enough to be treated as real content by the extractor.</p>
<ul><li>first</li><li>second</li></ul>
<ol><li>one</li><li>two</li></ol>
<pre><code class="language-go">fmt.Println("hi")</code></pre>
<blockquote><p>quoted wisdom</p></blockquote>
<img src="/img/logo.png" alt="logo">
<a href="javascript:alert(1)">bad</a></body></html>`
	page := convertHTMLPage(in, "https://example.com/base/")
	if page.Title != "My Page" {
		t.Errorf("title = %q", page.Title)
	}
	if page.Published != "2026-01-02T03:04:05Z" {
		t.Errorf("published = %q", page.Published)
	}
	out := page.Markdown
	for _, want := range []string{
		"# Main & Title",
		"[docs link](https://example.com/doc)",
		"- first",
		"- second",
		"1. one",
		"2. two",
		"```go",
		`fmt.Println("hi")`,
		"> quoted wisdom",
		"![logo](https://example.com/img/logo.png)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	for _, banned := range []string{"alert(1)", "color:red", "<p>", "javascript:"} {
		if strings.Contains(out, banned) {
			t.Errorf("unexpected %q in output:\n%s", banned, out)
		}
	}
}

func TestResolveURLSanitizesSchemes(t *testing.T) {
	base, _ := url.Parse("https://example.com/base/")
	r := &mdRenderer{base: base}

	// Dangerous schemes are rejected, including obfuscated variants that use
	// control characters browsers would strip before interpreting the scheme.
	for _, raw := range []string{
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)",
		"VBScript:msgbox(1)",
		"java\tscript:alert(1)",
		"java\nscript:alert(1)",
		"  javascript:alert(1)  ",
		"file:///etc/passwd",
	} {
		if got := r.resolveURL(raw); got != "" {
			t.Errorf("resolveURL(%q) = %q, want empty", raw, got)
		}
	}

	// Safe absolute and relative URLs are preserved and resolved against base.
	cases := map[string]string{
		"https://other.com/x":  "https://other.com/x",
		"http://other.com/x":   "http://other.com/x",
		"mailto:a@example.com": "mailto:a@example.com",
		"/doc":                 "https://example.com/doc",
		"page.html":            "https://example.com/base/page.html",
		"#frag":                "https://example.com/base/#frag",
	}
	for raw, want := range cases {
		if got := r.resolveURL(raw); got != want {
			t.Errorf("resolveURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestConvertHTMLPageTable(t *testing.T) {
	in := `<html><body><table>
<tr><th>Name</th><th>Age</th></tr>
<tr><td>Alice</td><td>30</td></tr>
<tr><td>Bob</td><td>41</td></tr>
</table></body></html>`
	out := convertHTMLPage(in, "https://example.com").Markdown
	for _, want := range []string{
		"| Name | Age |",
		"| --- | --- |",
		"| Alice | 30 |",
		"| Bob | 41 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in table output:\n%s", want, out)
		}
	}
}

func TestConvertHTMLPageExtractsMainContent(t *testing.T) {
	nav := strings.Repeat(`<li><a href="/x">Nav item</a></li>`, 30)
	body := strings.Repeat(`<p>This is a long meaningful paragraph of article text, with commas, clauses, and enough length to score well in extraction.</p>`, 10)
	in := `<html><body><div class="sidebar"><ul>` + nav + `</ul></div><article>` + body + `</article></body></html>`
	out := convertHTMLPage(in, "https://example.com").Markdown
	if !strings.Contains(out, "meaningful paragraph") {
		t.Fatalf("article content missing:\n%s", out)
	}
	if strings.Contains(out, "Nav item") {
		t.Fatalf("navigation leaked into extracted content:\n%s", out)
	}
}

func TestConvertHTMLPageFallsBackWhenExtractionTooAggressive(t *testing.T) {
	// A clean page with several sibling sections: extraction that keeps only
	// one section drops below the 30% threshold, so the full page is kept.
	var secs []string
	for i := 0; i < 6; i++ {
		secs = append(secs, fmt.Sprintf(`<div><p>Standalone section %d with plenty of prose, commas, and general substance to matter.</p></div>`, i))
	}
	in := `<html><body>` + strings.Join(secs, "") + `</body></html>`
	out := convertHTMLPage(in, "https://example.com").Markdown
	for i := 0; i < 6; i++ {
		if !strings.Contains(out, fmt.Sprintf("Standalone section %d", i)) {
			t.Fatalf("section %d lost:\n%s", i, out)
		}
	}
}

func TestIsTextualContentType(t *testing.T) {
	for ct, want := range map[string]bool{
		"":                         true,
		"text/html; charset=utf-8": true,
		"text/plain":               true,
		"application/json":         true,
		"application/vnd.api+json": true,
		"application/rss+xml":      true,
		"application/octet-stream": false,
		"image/png":                false,
		"application/pdf":          false,
		"application/x-gzip":       false,
	} {
		if got := isTextualContentType(ct); got != want {
			t.Errorf("isTextualContentType(%q) = %v, want %v", ct, got, want)
		}
	}
}

func TestRunWebFetchConvertsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><script>x()</script></head><body><h2>Guide</h2><p>Read <a href="https://example.com/a">this</a>.</p></body></html>`)
	}))
	defer srv.Close()
	rt := newWebToolRuntime(t)
	raw, _ := json.Marshal(map[string]any{"url": srv.URL})
	out, err := rt.runWebFetch(context.Background(), raw)
	if err != nil {
		t.Fatalf("web-fetch: %v", err)
	}
	if !strings.Contains(out, "## Guide") || !strings.Contains(out, "[this](https://example.com/a)") {
		t.Fatalf("unexpected output: %q", out)
	}
	if !strings.Contains(out, "URL Source: "+srv.URL) || !strings.Contains(out, "Markdown Content:") {
		t.Fatalf("missing front matter: %q", out)
	}
	if strings.Contains(out, "x()") {
		t.Fatalf("script leaked into output: %q", out)
	}
}

func TestRunWebFetchTruncatesToBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("a", 5000))
	}))
	defer srv.Close()
	rt := newWebToolRuntime(t)
	raw, _ := json.Marshal(map[string]any{"url": srv.URL, "max_length": 100})
	out, err := rt.runWebFetch(context.Background(), raw)
	if err != nil {
		t.Fatalf("web-fetch: %v", err)
	}
	if !strings.Contains(out, "[truncated:") || !strings.Contains(out, "job-output") {
		t.Fatalf("expected spool truncation footer, got %q", out)
	}
	if len(out) > 200 {
		t.Fatalf("output not truncated: %d chars", len(out))
	}
}

func TestRunWebFetchRejectsBinary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0x00, 0x01})
	}))
	defer srv.Close()
	rt := newWebToolRuntime(t)
	raw, _ := json.Marshal(map[string]any{"url": srv.URL})
	if _, err := rt.runWebFetch(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "unsupported content type") {
		t.Fatalf("expected content-type error, got %v", err)
	}
}

func TestRunWebFetchRejectsBadScheme(t *testing.T) {
	rt := newWebToolRuntime(t)
	raw, _ := json.Marshal(map[string]any{"url": "file:///etc/passwd"})
	if _, err := rt.runWebFetch(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("expected scheme error, got %v", err)
	}
}

func TestRunWebFetchRedirectCap(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.Path+"x", http.StatusFound)
	}))
	defer srv.Close()
	rt := newWebToolRuntime(t)
	raw, _ := json.Marshal(map[string]any{"url": srv.URL})
	if _, err := rt.runWebFetch(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("expected redirect cap error, got %v", err)
	}
}

func TestRunDownloadWritesFile(t *testing.T) {
	payload := []byte("binary\x00payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(payload)
	}))
	defer srv.Close()
	rt := newWebToolRuntime(t)
	raw, _ := json.Marshal(map[string]any{"url": srv.URL, "path": "assets/blob.bin"})
	out, err := rt.runDownload(context.Background(), raw)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !strings.Contains(out, "assets/blob.bin") || !strings.Contains(out, fmt.Sprintf("%d bytes", len(payload))) {
		t.Fatalf("unexpected output: %q", out)
	}
	got, err := os.ReadFile(filepath.Join(rt.cwd, "assets", "blob.bin"))
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestRunDownloadEnforcesSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Repeat("z", 2048)))
	}))
	defer srv.Close()
	rt := newWebToolRuntime(t)
	raw, _ := json.Marshal(map[string]any{"url": srv.URL, "path": "big.bin", "max_bytes": 1024})
	if _, err := rt.runDownload(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected size limit error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(rt.cwd, "big.bin")); !os.IsNotExist(err) {
		t.Fatalf("partial file left behind: %v", err)
	}
}

func TestRunDownloadRejectsEscapingPath(t *testing.T) {
	rt := newWebToolRuntime(t)
	raw, _ := json.Marshal(map[string]any{"url": "https://example.com/x", "path": "../outside.bin"})
	if _, err := rt.runDownload(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("expected workspace escape error, got %v", err)
	}
}
